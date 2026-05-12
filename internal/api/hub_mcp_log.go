// hub_mcp_log.go — Phase 2 Task 2.5 (G4 unified hub MCP).
//
// Structured JSON-Lines event emitter for hub-mcp.log. Every event
// flows through RedactToken before reaching disk, terminal, or
// upstream-error wrappers — concentrating the redaction at these
// choke-points so Phase 4 + 5 emit sites cannot leak tokens by
// composing strings that bypass the helper.
//
// Surfaces covered:
//
//   - LogHubMcpEvent(level, event, fields) → writes one JSON line to
//     <state-dir>/hub-mcp.log (10 MB rotation to .log.1).
//   - wrapHubMcpFileError(op, path, cause) → wraps a syscall error
//     with token-redacted path context. The path arg may contain a
//     basename of a token-bearing file (spec §"Logging hygiene"
//     bullet "Syscall error wrappers").
//   - redactArgvForLog(argv) → emits a copy of argv with each token-
//     shaped element replaced by `<token>`. Used by command-not-found
//     / unknown-flag paths that would otherwise echo the whole argv.
//   - formatInstallStatusForLog(status, client, url) → produces the
//     install-status string Phase 5 uses in its progress / failure
//     output. The URL may carry a token in the path or query string.
//
// Per spec §"Logging hygiene" the failed-auth / control-token /
// instance-id rotation log lines must NOT include any plain token
// bytes. The golden test in hub_mcp_log_redaction_test.go (this same
// file) drives a token through each surface and asserts no plain-token
// bytes survive.
//
// Spec: docs/superpowers/specs/2026-05-12-g4-unified-hub-mcp-design-v3.md
// §"Logging hygiene + golden test" (F-S2 closure). Plan: Task 2.5.

package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/gofrs/flock"
)

// hubMcpLogFileLeaf names the JSON-Lines log under <state-dir>. The
// .log.1 rotation suffix follows the same convention as
// watchdog.log.1 + intent-audit.log.1.
const hubMcpLogFileLeaf = "hub-mcp.log"

// hubMcpLogLockFileLeaf serializes appends across concurrent emitters.
// A dedicated lock leaf (separate from hub-mcp.lock) so log writes
// never queue behind a long-running token-rotation transaction.
const hubMcpLogLockFileLeaf = "hub-mcp.log.lock"

// hubMcpLogRotatedSuffix matches watchdog.log's `.1` convention. Only
// the newest rotated file is retained; older rotations are
// overwritten on the next rotation.
const hubMcpLogRotatedSuffix = ".1"

// HubMcpLogRotateSizeBytes mirrors the watchdog 10 MB ceiling. Two log
// files × 10 MB each = ~20 MB ceiling per log family.
const HubMcpLogRotateSizeBytes int64 = 10 * 1024 * 1024

// hubMcpLogAppendMu is a package-level mutex for in-process
// serialization. The on-disk flock handles cross-process ordering;
// the in-process mutex avoids gofrs/flock's repeated acquire/release
// cycles when several emit calls fire back-to-back from the same
// process.
var hubMcpLogAppendMu sync.Mutex

// LogHubMcpEvent marshals one structured event to hub-mcp.log. The
// rendered JSON Line is passed through RedactToken before writing
// so any token bytes the caller dropped into `fields` are scrubbed.
//
// Field semantics (subset; Phase 4 + 5 add domain-specific keys):
//
//   - "ts"      string  — RFC3339Nano UTC, auto-populated.
//   - "level"   string  — caller-provided: "info"|"warn"|"error".
//   - "event"   string  — caller-provided event name (e.g.
//     "tokens-reloaded", "internal-reload-rejected").
//
// `fields` carries any additional key/value pairs. The caller does
// NOT need to pre-redact — every value passes through RedactToken
// before reaching disk.
//
// Failures to open or write the log are best-effort: this is the
// observability path, not a load-bearing data path. A returned error
// is informational; callers do not branch on it (Phase 4 handlers
// should not 500 because the log is full).
//
// Envelope keys "ts", "level", "event" are reserved and overwrite any
// caller-supplied `fields` entries of the same name — the merge runs
// FIRST so envelope assignment is authoritative. This guarantees an
// observability envelope a misbehaving emit site cannot rewrite.
func LogHubMcpEvent(level, event string, fields map[string]any) error {
	if level == "" {
		level = "info"
	}
	rec := make(map[string]any, len(fields)+3)
	// Merge caller fields FIRST so the envelope assignments below
	// remain authoritative on collision (see doc comment above).
	for k, v := range fields {
		rec[k] = v
	}
	rec["ts"] = time.Now().UTC().Format(time.RFC3339Nano)
	rec["level"] = level
	rec["event"] = event
	raw, err := json.Marshal(rec)
	if err != nil {
		return fmt.Errorf("marshal hub-mcp event: %w", err)
	}
	redacted := RedactToken(string(raw))
	return appendHubMcpLogLine([]byte(redacted))
}

// wrapHubMcpFileError wraps a syscall error with token-redacted path
// context. Use at every adapter/install/state path that surfaces a
// syscall error whose path arg may contain a token-bearing basename.
//
// The returned error has the form `hub-mcp %s %s: %v` where the
// path component is run through RedactToken. errors.Is on the
// returned value still matches the underlying cause via the %w
// wrapper, so callers can branch on os.ErrPermission / os.ErrNotExist
// across the redacted surface.
func wrapHubMcpFileError(op, path string, cause error) error {
	if cause == nil {
		return nil
	}
	return fmt.Errorf("hub-mcp %s %s: %w", op, RedactToken(path), cause)
}

// redactArgvForLog returns a copy of argv with each element passed
// through RedactToken. Used by command-not-found / unknown-flag
// echo paths so a token mistakenly passed on the command line never
// reaches stdout/stderr or hub-mcp.log.
//
// The returned slice has the same length as argv; elements with no
// 64-hex run are returned verbatim. Callers should NEVER log the
// original argv after this wrapping (the redacted slice is the only
// safe surface).
func redactArgvForLog(argv []string) []string {
	out := make([]string, len(argv))
	for i, a := range argv {
		out[i] = RedactToken(a)
	}
	return out
}

// formatInstallStatusForLog returns a human-readable install-status
// line for Phase 5 CLI output + hub-mcp.log records. The url argument
// MAY contain a token in the query string (e.g.
// "http://127.0.0.1:9120/clients/claude-code/mcp?t=<64-hex>"); the
// redaction strips it before the string composes.
//
// Shape: "install <status> client=<client> url=<redacted-url>". One
// line per call. The choke-point design — every install-status emit
// goes through this function — guarantees no plain-token bytes
// reach the log even if a future caller forgets to pre-redact.
func formatInstallStatusForLog(status, client, url string) string {
	return RedactToken(fmt.Sprintf("install %s client=%s url=%s", status, client, url))
}

// appendHubMcpLogLine writes line + "\n" to hub-mcp.log under flock.
// 10 MB rotation handled at the top of the call: if the active file
// is at-or-above the ceiling, rotate to .1 (overwriting any existing
// .1) before appending.
func appendHubMcpLogLine(line []byte) error {
	hubMcpLogAppendMu.Lock()
	defer hubMcpLogAppendMu.Unlock()

	dir, err := DaemonStateDir()
	if err != nil {
		return err
	}
	path := filepath.Join(dir, hubMcpLogFileLeaf)
	lockPath := filepath.Join(dir, hubMcpLogLockFileLeaf)

	lk := flock.New(lockPath)
	if err := lk.Lock(); err != nil {
		return fmt.Errorf("hub-mcp log flock: %w", err)
	}
	defer func() { _ = lk.Unlock() }()

	if size, ok := hubMcpLogFileSize(path); ok && size >= HubMcpLogRotateSizeBytes {
		if rotErr := rotateHubMcpLogFile(path); rotErr != nil {
			return rotErr
		}
	}
	return appendHubMcpLogLineUnlocked(path, line)
}

// hubMcpLogFileSize returns the size of path. Missing-file → (0,false)
// so the rotation check treats a fresh state-dir as "below the
// ceiling".
func hubMcpLogFileSize(path string) (int64, bool) {
	st, err := os.Stat(path)
	if err != nil {
		return 0, false
	}
	return st.Size(), true
}

// rotateHubMcpLogFile renames the active log to ${path}.1, replacing
// any existing .1 backup. Returns the rename error verbatim on
// failure. Caller must already hold the hub-mcp.log flock.
func rotateHubMcpLogFile(path string) error {
	target := path + hubMcpLogRotatedSuffix
	if err := os.Remove(target); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("remove existing rotated %s: %w", target, err)
	}
	if err := os.Rename(path, target); err != nil {
		return fmt.Errorf("rotate %s -> %s: %w", path, target, err)
	}
	return nil
}

// appendHubMcpLogLineUnlocked writes line + "\n" to path. Caller MUST
// already hold the hub-mcp.log flock. File is opened with
// O_APPEND|O_CREATE|0600 + a defensive Chmod (POSIX umask drift
// defense; Windows no-op).
func appendHubMcpLogLineUnlocked(path string, line []byte) error {
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY|os.O_CREATE, 0o600)
	if err != nil {
		return fmt.Errorf("open hub-mcp log %s: %w", path, err)
	}
	defer func() { _ = f.Close() }()

	if err := os.Chmod(path, 0o600); err != nil {
		return fmt.Errorf("chmod hub-mcp log %s: %w", path, err)
	}
	if _, err := f.Write(append(line, '\n')); err != nil {
		return fmt.Errorf("write hub-mcp log %s: %w", path, err)
	}
	// Sync to match watchdog_log.go durability — avoids losing the last
	// appended line on power-cut/crash mid-rotation. Best-effort: the
	// log path is observability-only, so sync errors are swallowed.
	_ = f.Sync()
	return nil
}

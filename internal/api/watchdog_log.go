// Package api — Task 9 watchdog decision log (watchdog plan v13 §10,
// §20, §28, §31, §35, §38, §44, §45, §49, §54).
//
// watchdog.log is the SECOND JSON-Lines log file owned by the watchdog
// stack — distinct from intent-audit.log (Task 3). The two logs serve
// different purposes:
//
//   - intent-audit.log: append-only audit trail of intent FILE writes
//     (set-intent, clear-intent, watchdog-self-quarantined, audit-rotated).
//     Owned by Task 3 (intent_audit.go).
//   - watchdog.log: append-only DECISION trail of `mcphub watchdog --once`
//     ticks (already-running-skipped, restart, restart-verified-running,
//     restart-not-yet-running-after-30s, ctx-budget-exhausted,
//     stop-race-aborted, suspicious-xml, restart-pending-stale-cleared,
//     audit-degraded, etc.). Owned by Task 9 (this file).
//
// Schema (plan §10 v8) — JSON Lines, one entry per line:
//
//	{"ts":"...","task":"...","state":"...","last_result":"...",
//	 "intent":"...","attempts":N,"cooldown_due":bool,"action":"...","err":""}
//
// Per §10 the action carries the canonical event vocabulary; the optional
// fields populate when relevant (e.g. attempts only on restart entries).
//
// Per-entry size cap: 16KB with identity preservation per §35 (same rules
// as intent-audit.log; identity field here is `task`). The Task 3 helpers
// (taskHash12, AuditEntryMaxBytes constant) are intentionally re-used so
// both logs share one ceiling and one truncation contract.
//
// Rotation: 10MB → ${path}.1, only the .1 backup retained (older ones
// overwritten). No self-event emitted for watchdog.log rotation — the
// rotation is recorded ONLY by being followed by a fresh log; status
// command surfaces it via the size+age delta.
//
// Concurrency: holds gofrs/flock on watchdog.log.lock for the duration
// of every Append call. Concurrent appenders serialize.
package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/gofrs/flock"
)

// ---------------------------------------------------------------------------
// File layout constants.
// ---------------------------------------------------------------------------

// watchdogLogFileLeaf is the canonical file name (relative to
// DaemonStateDir) holding the JSON-Lines decision log.
const watchdogLogFileLeaf = "watchdog.log"

// watchdogLogLockLeaf is the sibling file used by gofrs/flock so
// concurrent WatchdogLogAppend callers serialize.
const watchdogLogLockLeaf = "watchdog.log.lock"

// watchdogLogRotatedSuffix is appended to the rotated file name. Plan
// §10 implicit: only ${leaf}.1 is retained, older rotations are
// overwritten on next rotation.
const watchdogLogRotatedSuffix = ".1"

// WatchdogLogRotateSizeBytes mirrors §10's 10MB rotation contract. Kept
// in sync with AuditRotateSizeBytes so both logs share one ceiling.
const WatchdogLogRotateSizeBytes int64 = 10 * 1024 * 1024

// ---------------------------------------------------------------------------
// Test seams.
// ---------------------------------------------------------------------------

// watchdogLogAppendWriteFn, when non-nil, replaces the disk-append path
// inside AppendWatchdogLog. Tests inject targeted failures (e.g.,
// simulate disk-full) without exercising the OS write path.
var watchdogLogAppendWriteFn func(path string, line []byte) error

// ---------------------------------------------------------------------------
// Schema.
// ---------------------------------------------------------------------------

// WatchdogLogEntry is one JSON Lines row in watchdog.log. Field semantics
// (plan §10 v8):
//
//   - TS: UTC RFC3339Nano. Auto-populated when zero.
//   - Task: identity field (>1KB → ErrIdentityOversize per §35). Carries
//     the scheduler task name verbatim (with leading backslash) when the
//     entry pertains to a specific daemon; sentinel literals like
//     "<once-driver>" / "<rotation-system>" are used for entries that
//     don't bind to a single task.
//   - State: optional snapshot of the daemon's scheduler State at the
//     time of the decision (Running / Ready / Stopped / Failed). Empty
//     when not relevant (e.g. "ctx-deadline-exceeded" on the global path).
//   - LastResult: stringified last-result code at decision time (kept as
//     string so the JSON wire shape stays stable across positive/negative
//     int32 values).
//   - Intent: free-form intent context (e.g. "user-stop", "default-
//     running"). Empty when not relevant.
//   - Attempts: per-cycle attempt count carried by restart entries only.
//   - CooldownDue: snapshot of CooldownReader.Due at decision time.
//   - Action: canonical event vocabulary. Plan §10 lists ~12 values.
//   - Err: error context for negative outcomes; empty on success.
//   - Note: free-form auxiliary detail (e.g. "likely process kill mid-
//     restart on prior tick" for stale-clear entries).
//   - PendingAt / ClearedAt: pair of timestamps used by stale-clear
//     entries to give operators an exact lockout window.
type WatchdogLogEntry struct {
	TS          time.Time // UTC RFC3339Nano
	Task        string    // identity field (>1KB → reject)
	State       string    // scheduler State at decision time (omitempty)
	LastResult  string    // stringified last-result (omitempty)
	Intent      string    // intent context (omitempty)
	Attempts    int       // per-cycle attempt count (omitempty)
	CooldownDue bool      // CooldownReader.Due snapshot (omitempty)
	Action      string    // canonical event vocabulary
	Err         string    // negative-outcome context (omitempty)
	Note        string    // free-form auxiliary detail (omitempty)
	PendingAt   time.Time // restart-pending-stale-cleared (omitempty)
	ClearedAt   time.Time // restart-pending-stale-cleared (omitempty)
	Priority    string    // "high" | "" — plan §38, §44 high-priority entries
}

// watchdogLogWire is the JSON-side projection. Mirrors auditWire's
// design — string field names match plan §10 v8 schema.
type watchdogLogWire struct {
	TS          time.Time `json:"ts"`
	Task        string    `json:"task"`
	State       string    `json:"state,omitempty"`
	LastResult  string    `json:"last_result,omitempty"`
	Intent      string    `json:"intent,omitempty"`
	Attempts    int       `json:"attempts,omitempty"`
	CooldownDue bool      `json:"cooldown_due,omitempty"`
	Action      string    `json:"action"`
	Err         string    `json:"err,omitempty"`
	Note        string    `json:"note,omitempty"`
	PendingAt   time.Time `json:"pending_at,omitempty"`
	ClearedAt   time.Time `json:"cleared_at,omitempty"`
	Priority    string    `json:"priority,omitempty"`
}

// MarshalJSON projects WatchdogLogEntry onto the wire shape. Time fields
// are auto-serialized as RFC3339Nano; the constructor normalizes to UTC
// at fill-time so the on-disk representation is stable.
func (e WatchdogLogEntry) MarshalJSON() ([]byte, error) {
	w := watchdogLogWire{
		TS:          e.TS,
		Task:        e.Task,
		State:       e.State,
		LastResult:  e.LastResult,
		Intent:      e.Intent,
		Attempts:    e.Attempts,
		CooldownDue: e.CooldownDue,
		Action:      e.Action,
		Err:         e.Err,
		Note:        e.Note,
		PendingAt:   e.PendingAt,
		ClearedAt:   e.ClearedAt,
		Priority:    e.Priority,
	}
	return json.Marshal(w)
}

// UnmarshalJSON populates an entry from the wire shape. Used by the
// `mcphub watchdog status` recent-events tail reader.
func (e *WatchdogLogEntry) UnmarshalJSON(data []byte) error {
	var w watchdogLogWire
	if err := json.Unmarshal(data, &w); err != nil {
		return err
	}
	e.TS = w.TS
	e.Task = w.Task
	e.State = w.State
	e.LastResult = w.LastResult
	e.Intent = w.Intent
	e.Attempts = w.Attempts
	e.CooldownDue = w.CooldownDue
	e.Action = w.Action
	e.Err = w.Err
	e.Note = w.Note
	e.PendingAt = w.PendingAt
	e.ClearedAt = w.ClearedAt
	e.Priority = w.Priority
	return nil
}

// ---------------------------------------------------------------------------
// AppendWatchdogLog — the production write path.
// ---------------------------------------------------------------------------

// AppendWatchdogLog serializes the entry as a JSON Line and appends it
// to watchdog.log under flock. Returns ErrIdentityOversize when Task
// exceeds AuditIdentityFieldByteCap (>1KB) so callers can refuse to
// log the entry rather than truncate identity context. Identity-
// preserving 16KB cap is applied via the Task 3 marshaller.
//
// Rotation: at the start of each call, if the existing log is
// >= WatchdogLogRotateSizeBytes, os.Rename(*.log, *.log.1). No self-
// event is emitted (rotation for watchdog.log is implicit; the next
// successful append goes through normally).
func (a *API) AppendWatchdogLog(entry WatchdogLogEntry) error {
	// 1. Auto-fill TS if unset; normalize to UTC.
	if entry.TS.IsZero() {
		entry.TS = time.Now().UTC()
	} else {
		entry.TS = entry.TS.UTC()
	}
	if !entry.PendingAt.IsZero() {
		entry.PendingAt = entry.PendingAt.UTC()
	}
	if !entry.ClearedAt.IsZero() {
		entry.ClearedAt = entry.ClearedAt.UTC()
	}

	// 2. Identity oversize gate.
	if len(entry.Task) > AuditIdentityFieldByteCap {
		return ErrIdentityOversize
	}

	// 3. Resolve paths.
	dir, err := DaemonStateDir()
	if err != nil {
		return err
	}
	path := filepath.Join(dir, watchdogLogFileLeaf)
	lockPath := filepath.Join(dir, watchdogLogLockLeaf)

	// 4. Acquire flock for serialization.
	lock := flock.New(lockPath)
	if err := lock.Lock(); err != nil {
		return fmt.Errorf("watchdog log flock: %w", err)
	}
	defer func() { _ = lock.Unlock() }()

	// 5. Rotation check.
	if size, ok := watchdogLogFileSize(path); ok && size >= WatchdogLogRotateSizeBytes {
		if rotErr := rotateWatchdogLogFile(path); rotErr != nil {
			return rotErr
		}
	}

	// 6. Marshal entry within the 16KB budget. Reuse the Task 3
	//    marshaller adapted for the watchdog-log entry shape — same
	//    cap, identity preservation, and _task_hash semantics.
	line, marshalErr := marshalWatchdogLogLineWithCap(entry)
	if marshalErr != nil {
		return marshalErr
	}

	// 7. Append.
	return appendWatchdogLogLine(path, line)
}

// watchdogLogFileSize returns the size of `path` in bytes plus a "found"
// flag. ErrNotExist returns (0, false) so the caller treats a fresh
// file as below the rotation threshold.
func watchdogLogFileSize(path string) (int64, bool) {
	st, err := os.Stat(path)
	if err != nil {
		return 0, false
	}
	return st.Size(), true
}

// rotateWatchdogLogFile renames the active log to ${path}.1, replacing
// any existing .1 backup. Returns the rename error verbatim on failure.
func rotateWatchdogLogFile(path string) error {
	target := path + watchdogLogRotatedSuffix
	if err := os.Remove(target); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("remove existing rotated %s: %w", target, err)
	}
	if err := os.Rename(path, target); err != nil {
		return fmt.Errorf("rotate %s -> %s: %w", path, target, err)
	}
	return nil
}

// appendWatchdogLogLine writes line + newline to path under the caller's
// already-held flock. Routes through watchdogLogAppendWriteFn when set.
func appendWatchdogLogLine(path string, line []byte) error {
	if watchdogLogAppendWriteFn != nil {
		return watchdogLogAppendWriteFn(path, line)
	}
	return defaultAppendWatchdogLogLine(path, line)
}

// defaultAppendWatchdogLogLine is the production disk write — opens the
// file with O_APPEND|O_CREATE|O_WRONLY 0o600 and writes line + "\n".
// Caller MUST already hold the watchdog-log flock.
func defaultAppendWatchdogLogLine(path string, line []byte) error {
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY|os.O_CREATE, 0o600)
	if err != nil {
		return fmt.Errorf("open watchdog log %s: %w", path, err)
	}
	defer func() { _ = f.Close() }()

	// Defense vs umask drift on POSIX (no-op on Windows).
	if err := os.Chmod(path, 0o600); err != nil {
		return fmt.Errorf("chmod watchdog log %s: %w", path, err)
	}

	if _, err := f.Write(append(line, '\n')); err != nil {
		return fmt.Errorf("write watchdog log line: %w", err)
	}
	if err := f.Sync(); err != nil {
		return fmt.Errorf("sync watchdog log: %w", err)
	}
	return nil
}

// ---------------------------------------------------------------------------
// 16KB cap with identity preservation for watchdog log entries.
// ---------------------------------------------------------------------------

// marshalWatchdogLogLineWithCap marshals e into JSON Lines bytes under
// the 16KB cap. Task is identity (never truncated; >1KB rejected
// upstream). Longest non-identity STRING field truncated when the
// entry exceeds AuditEntryMaxBytes; entry gains _truncated:true,
// _truncated_field, _task_hash markers via the Task 3 injection helper.
//
// If post-truncation the entry STILL exceeds the cap, replace with a
// placeholder line carrying _task_hash for forensic correlation.
func marshalWatchdogLogLineWithCap(e WatchdogLogEntry) ([]byte, error) {
	raw, err := json.Marshal(e)
	if err != nil {
		return nil, fmt.Errorf("marshal watchdog log entry: %w", err)
	}
	if len(raw) <= AuditEntryMaxBytes {
		return raw, nil
	}
	// Oversize path: identify the longest non-identity string field and
	// truncate so the marshaled entry fits.
	field, _, ok := truncateLongestWatchdogLogNonIdentity(&e, len(raw))
	if ok {
		raw2, err2 := json.Marshal(e)
		if err2 != nil {
			return nil, fmt.Errorf("re-marshal truncated watchdog log entry: %w", err2)
		}
		injected := injectTruncationMarkers(raw2, field, taskHash12(e.Task))
		if len(injected) <= AuditEntryMaxBytes {
			return injected, nil
		}
	}
	// Drop with placeholder — same shape as intent-audit's drop branch.
	placeholder := buildWatchdogLogDropPlaceholder(e)
	return placeholder, nil
}

// truncateLongestWatchdogLogNonIdentity finds the longest non-identity
// string field on e and shortens it so the next marshal lands within
// budget. Returns (fieldName, replacedText, ok). The field set is:
// {err, note, intent, last_result}. Identity (task) is excluded.
func truncateLongestWatchdogLogNonIdentity(e *WatchdogLogEntry, currentSize int) (string, string, bool) {
	type cand struct {
		name string
		ptr  *string
	}
	cands := []cand{
		{"err", &e.Err},
		{"note", &e.Note},
		{"intent", &e.Intent},
		{"last_result", &e.LastResult},
	}
	var longest *cand
	for i := range cands {
		c := &cands[i]
		if longest == nil || len(*c.ptr) > len(*longest.ptr) {
			longest = c
		}
	}
	if longest == nil || len(*longest.ptr) == 0 {
		return "", "", false
	}
	overBy := currentSize - AuditEntryMaxBytes + auditFixedSchemaOverhead
	if overBy < 0 {
		overBy = 0
	}
	curLen := len(*longest.ptr)
	if overBy >= curLen {
		return "", "", false
	}
	newLen := curLen - overBy
	if newLen < 0 {
		newLen = 0
	}
	original := *longest.ptr
	*longest.ptr = original[:newLen]
	return longest.name, original, true
}

// buildWatchdogLogDropPlaceholder returns the placeholder line emitted
// when even truncation cannot fit the entry within AuditEntryMaxBytes.
// Includes _task_hash for forensic correlation per §35 / §54.
func buildWatchdogLogDropPlaceholder(e WatchdogLogEntry) []byte {
	hash := taskHash12(e.Task)
	ts := e.TS.Format(time.RFC3339Nano)
	body := fmt.Sprintf(
		`{"ts":%q,"action":"log-entry-dropped-oversize","note":"watchdog.log entry exceeded 16KB after truncation","_truncated":true,"_task_hash":%q}`,
		ts, hash,
	)
	return []byte(body)
}

// ---------------------------------------------------------------------------
// ReadWatchdogLogTail — used by `mcphub watchdog status`.
// ---------------------------------------------------------------------------

// ReadWatchdogLogTail reads the last `n` JSON Lines from watchdog.log,
// returning them in CHRONOLOGICAL ORDER (oldest first). Lines that fail
// to parse are skipped silently; this is a display-only reader and we
// prefer surfacing what we can rather than failing the whole status
// command on one malformed line.
//
// Returns an empty slice (not nil) when the log is absent — `mcphub
// watchdog status` then prints a "no recent events" line.
func (a *API) ReadWatchdogLogTail(n int) []WatchdogLogEntry {
	if n <= 0 {
		return []WatchdogLogEntry{}
	}
	dir, err := DaemonStateDir()
	if err != nil {
		return []WatchdogLogEntry{}
	}
	path := filepath.Join(dir, watchdogLogFileLeaf)
	raw, err := os.ReadFile(path)
	if err != nil {
		return []WatchdogLogEntry{}
	}
	return parseWatchdogLogTail(raw, n)
}

// parseWatchdogLogTail extracts the last `n` JSON Lines from raw. Pure
// for testability — tests pass synthesized log content directly.
func parseWatchdogLogTail(raw []byte, n int) []WatchdogLogEntry {
	if n <= 0 || len(raw) == 0 {
		return []WatchdogLogEntry{}
	}
	// Split on newlines. Walk backward, parse each line, accumulate up to n.
	lines := splitJSONLines(raw)
	if len(lines) == 0 {
		return []WatchdogLogEntry{}
	}
	start := 0
	if len(lines) > n {
		start = len(lines) - n
	}
	out := make([]WatchdogLogEntry, 0, len(lines)-start)
	for _, line := range lines[start:] {
		if len(line) == 0 {
			continue
		}
		var entry WatchdogLogEntry
		if err := json.Unmarshal(line, &entry); err != nil {
			continue
		}
		out = append(out, entry)
	}
	return out
}

// splitJSONLines splits raw on '\n', returning each non-empty line as a
// fresh slice (no aliasing of raw). Cheap O(n) walk; avoids bufio.Scanner
// to keep the hot path allocation-light.
func splitJSONLines(raw []byte) [][]byte {
	var out [][]byte
	start := 0
	for i := 0; i < len(raw); i++ {
		if raw[i] == '\n' {
			if i > start {
				cp := make([]byte, i-start)
				copy(cp, raw[start:i])
				out = append(out, cp)
			}
			start = i + 1
		}
	}
	if start < len(raw) {
		cp := make([]byte, len(raw)-start)
		copy(cp, raw[start:])
		out = append(out, cp)
	}
	return out
}

// ---------------------------------------------------------------------------
// ReadIntentAuditTail — used by `mcphub watchdog status`.
// ---------------------------------------------------------------------------

// ReadIntentAuditTail reads the last `n` audit-log JSON Lines, returning
// them in CHRONOLOGICAL ORDER (oldest first). Like ReadWatchdogLogTail,
// malformed lines are skipped silently; absent file returns empty slice.
//
// Per §34 v9: callers in the status path MUST run each entry through
// RedactIntentAuditEntryForNonOwner before display. This reader does
// NOT redact — it returns raw entries so other callers (e.g. tests)
// can verify what's on disk.
func (a *API) ReadIntentAuditTail(n int) []IntentAuditEntry {
	if n <= 0 {
		return []IntentAuditEntry{}
	}
	dir, err := DaemonStateDir()
	if err != nil {
		return []IntentAuditEntry{}
	}
	path := filepath.Join(dir, intentAuditFileLeaf)
	raw, err := os.ReadFile(path)
	if err != nil {
		return []IntentAuditEntry{}
	}
	return parseIntentAuditTail(raw, n)
}

// parseIntentAuditTail extracts the last `n` JSON Lines from raw audit
// bytes. System entries on disk are rehydrated via the trusted-source
// rehydrator (same-package trust per §48); external untrusted JSON
// callers cannot reach this path because the helper is on (*API) and
// reads directly from the per-user state file.
func parseIntentAuditTail(raw []byte, n int) []IntentAuditEntry {
	if n <= 0 || len(raw) == 0 {
		return []IntentAuditEntry{}
	}
	lines := splitJSONLines(raw)
	if len(lines) == 0 {
		return []IntentAuditEntry{}
	}
	start := 0
	if len(lines) > n {
		start = len(lines) - n
	}
	out := make([]IntentAuditEntry, 0, len(lines)-start)
	for _, line := range lines[start:] {
		if len(line) == 0 {
			continue
		}
		var entry IntentAuditEntry
		if err := json.Unmarshal(line, &entry); err != nil {
			continue
		}
		// Re-derive systemEntry from the lowercased "system_entry" wire
		// field. The stock UnmarshalJSON discards it (sealed pattern,
		// §48); we re-set via the trusted rehydrator because this
		// reader runs in-process from the per-user state path.
		var probe struct {
			SystemEntry bool `json:"system_entry"`
		}
		if err := json.Unmarshal(line, &probe); err == nil && probe.SystemEntry {
			rehydrateSystemEntryFromTrustedSource(&entry, true)
		}
		out = append(out, entry)
	}
	return out
}

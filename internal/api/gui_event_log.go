// Package api — G9 GUI/API event log (Phase 3B-II backlog G9
// "Structured JSON event envelopes").
//
// gui-events.log is the THIRD JSON-Lines log file in the state-dir
// family (alongside watchdog.log and intent-audit.log). It records
// GUI/API lifecycle events with a consistent envelope so external
// monitoring tools can tail one file and reason about both human-
// triggered actions (bulk-action) and background poller signals
// (daemon-state, poller-error) without scraping the SSE wire.
//
// Goals per backlog row G9:
//   - Consistent JSON envelope for every event (schema_version, ts,
//     type, source, severity, body).
//   - Preserve the existing human-readable log surface (this log is
//     ADDITIVE — daemon stdout/stderr stays untouched).
//   - Useful for the unified health endpoint (G2) — same on-disk
//     pattern operators already know from watchdog.log.
//
// Schema (v1):
//
//	{"schema_version":"1","ts":"...","type":"...","source":"...",
//	 "severity":"info"|"warn"|"error","body":{...}}
//
// Concurrency: flock'd on gui-events.log.lock for the duration of
// every append, mirroring watchdog.log. Rotation: 10MB → .log.1.
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

const (
	// guiEventLogFileLeaf is the canonical file name relative to
	// DaemonStateDir for the GUI/API event log.
	guiEventLogFileLeaf = "gui-events.log"

	// guiEventLogLockLeaf is the sibling lock file used by gofrs/flock
	// so concurrent appenders serialize.
	guiEventLogLockLeaf = "gui-events.log.lock"

	// guiEventLogRotatedSuffix mirrors watchdog log convention — only
	// the .1 backup is retained.
	guiEventLogRotatedSuffix = ".1"

	// GUIEventLogRotateSizeBytes mirrors WatchdogLogRotateSizeBytes
	// (10MB). Two GUI logs (active + .1) bound disk usage to ~20MB.
	GUIEventLogRotateSizeBytes int64 = 10 * 1024 * 1024

	// GUIEventLogSchemaVersion is the envelope schema version. Bumped
	// when any field is added/removed or semantics change. Tools
	// reading the log should branch on this value.
	GUIEventLogSchemaVersion = "1"

	// GUIEventSeverityInfo / Warn / Error — canonical severity values.
	GUIEventSeverityInfo  = "info"
	GUIEventSeverityWarn  = "warn"
	GUIEventSeverityError = "error"
)

// GUIEventEntry is the in-memory shape of one event line.
//
// SchemaVersion is auto-filled by AppendGUIEventLog if empty — callers
// pass zero-value to get the current version. TS is auto-filled to
// time.Now().UTC() when zero.
//
// Body uses map[string]any so existing emit sites that already build a
// payload map can pass it through unchanged (see gui/poller.go,
// gui/servers.go).
type GUIEventEntry struct {
	SchemaVersion string         // "1"
	TS            time.Time      // UTC RFC3339Nano
	Type          string         // canonical event vocabulary
	Source        string         // emit-site identifier ("poller", "servers", etc.)
	Severity      string         // info | warn | error
	Body          map[string]any // arbitrary JSON-serializable payload
}

type guiEventLogWire struct {
	SchemaVersion string         `json:"schema_version"`
	TS            time.Time      `json:"ts"`
	Type          string         `json:"type"`
	Source        string         `json:"source,omitempty"`
	Severity      string         `json:"severity,omitempty"`
	Body          map[string]any `json:"body,omitempty"`
}

// MarshalJSON projects GUIEventEntry onto the wire shape.
func (e GUIEventEntry) MarshalJSON() ([]byte, error) {
	w := guiEventLogWire{
		SchemaVersion: e.SchemaVersion,
		TS:            e.TS,
		Type:          e.Type,
		Source:        e.Source,
		Severity:      e.Severity,
		Body:          e.Body,
	}
	return json.Marshal(w)
}

// UnmarshalJSON populates an entry from the wire shape.
func (e *GUIEventEntry) UnmarshalJSON(data []byte) error {
	var w guiEventLogWire
	if err := json.Unmarshal(data, &w); err != nil {
		return err
	}
	e.SchemaVersion = w.SchemaVersion
	e.TS = w.TS
	e.Type = w.Type
	e.Source = w.Source
	e.Severity = w.Severity
	e.Body = w.Body
	return nil
}

// guiEventLogAppendWriteFn is the test seam for disk writes. Production
// path uses defaultGuiEventLogAppend.
var guiEventLogAppendWriteFn func(path string, line []byte) error

// AppendGUIEventLog serializes entry as a JSON Line and appends it to
// gui-events.log under flock. Auto-fills SchemaVersion + TS + Severity
// when the caller leaves them blank.
//
// The Type field is required; callers must pass it. Empty Type returns
// ErrGUIEventLogMissingType. Empty Body is allowed (entries are still
// useful for type-only signals like "gui-server-started").
//
// Rotation: at the start of each call, if the existing log is >=
// GUIEventLogRotateSizeBytes, os.Rename(*.log, *.log.1). No self-event
// is emitted (mirrors watchdog.log).
func (a *API) AppendGUIEventLog(entry GUIEventEntry) error {
	if entry.Type == "" {
		return ErrGUIEventLogMissingType
	}
	if entry.SchemaVersion == "" {
		entry.SchemaVersion = GUIEventLogSchemaVersion
	}
	if entry.TS.IsZero() {
		entry.TS = time.Now().UTC()
	} else {
		entry.TS = entry.TS.UTC()
	}
	if entry.Severity == "" {
		entry.Severity = GUIEventSeverityInfo
	}

	dir, err := DaemonStateDir()
	if err != nil {
		return err
	}
	path := filepath.Join(dir, guiEventLogFileLeaf)
	lockPath := filepath.Join(dir, guiEventLogLockLeaf)

	lock := flock.New(lockPath)
	if err := lock.Lock(); err != nil {
		return fmt.Errorf("gui event log flock: %w", err)
	}
	defer func() { _ = lock.Unlock() }()

	if size, ok := guiEventLogFileSize(path); ok && size >= GUIEventLogRotateSizeBytes {
		if rotErr := rotateGUIEventLogFile(path); rotErr != nil {
			return rotErr
		}
	}

	line, err := json.Marshal(entry)
	if err != nil {
		return fmt.Errorf("gui event log marshal: %w", err)
	}
	line = append(line, '\n')

	return appendGUIEventLogLine(path, line)
}

// ErrGUIEventLogMissingType is returned by AppendGUIEventLog when the
// entry's Type field is empty. Callers must supply a non-empty Type so
// log consumers can categorize the row.
var ErrGUIEventLogMissingType = errors.New("gui event log: missing type")

func guiEventLogFileSize(path string) (int64, bool) {
	st, err := os.Stat(path)
	if err != nil {
		return 0, false
	}
	return st.Size(), true
}

func rotateGUIEventLogFile(path string) error {
	target := path + guiEventLogRotatedSuffix
	if err := os.Remove(target); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("remove existing rotated %s: %w", target, err)
	}
	if err := os.Rename(path, target); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return fmt.Errorf("rotate gui event log %s: %w", path, err)
	}
	return nil
}

func appendGUIEventLogLine(path string, line []byte) error {
	if guiEventLogAppendWriteFn != nil {
		return guiEventLogAppendWriteFn(path, line)
	}
	return defaultGUIEventLogAppend(path, line)
}

func defaultGUIEventLogAppend(path string, line []byte) error {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("open gui event log %s: %w", path, err)
	}
	defer func() { _ = f.Close() }()
	if _, err := f.Write(line); err != nil {
		return fmt.Errorf("write gui event log %s: %w", path, err)
	}
	return nil
}

// ReadGUIEventLogTail returns the last n entries from gui-events.log,
// parsing each JSON Line back into a GUIEventEntry. Used by the status
// surface and tests. Lines that fail to unmarshal are skipped (best-
// effort tail — corrupt lines should not block status output).
//
// Returns an empty slice (NOT nil) on every non-happy path so JSON
// encoders emit `[]` not `null` — consumers that expect a stable
// array shape don't break (Codex P3 on PR #150 line 236).
func (a *API) ReadGUIEventLogTail(n int) []GUIEventEntry {
	empty := []GUIEventEntry{}
	if n <= 0 {
		return empty
	}
	dir, err := DaemonStateDir()
	if err != nil {
		return empty
	}
	path := filepath.Join(dir, guiEventLogFileLeaf)
	raw, err := os.ReadFile(path)
	if err != nil {
		return empty
	}
	return parseGUIEventLogTail(raw, n)
}

func parseGUIEventLogTail(raw []byte, n int) []GUIEventEntry {
	// Tail-parse: scan from the end, collect up to n lines.
	out := make([]GUIEventEntry, 0, n)
	// Split by '\n' to get individual lines.
	lines := splitJSONLines(raw)
	start := 0
	if len(lines) > n {
		start = len(lines) - n
	}
	for _, line := range lines[start:] {
		if len(line) == 0 {
			continue
		}
		var e GUIEventEntry
		if err := json.Unmarshal(line, &e); err != nil {
			continue
		}
		out = append(out, e)
	}
	return out
}

// splitJSONLines is defined in watchdog_log.go — reused here to keep
// one JSON-Lines tokenizer across the api package.

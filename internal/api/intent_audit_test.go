// Tests for Task 3 (intent_audit.go) — append-only JSON Lines audit log
// with sealed SystemEntry, identity-preserving 16KB cap, idempotent
// audit-rotated retry, Priority field, and per-OS caller_start_time.
//
// Tests run with the production state-path resolver bypassed via the
// daemonStateRootOverride seam from state_paths.go (Task 1) so each
// test owns its own per-test directory under t.TempDir(). The
// quarantine/log seams from daemon_intent.go are reset by reusing
// daemonIntentTestHelper.
package api

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

// intentAuditTestHelper resets the audit-log seams + reuses
// daemonIntentTestHelper for the shared state-path override. Returns the
// resolved state directory so tests can read the audit file directly.
func intentAuditTestHelper(t *testing.T) string {
	t.Helper()
	dir := daemonIntentTestHelper(t)

	prevWrite := auditAppendWriteFn
	prevRotFail := auditRotatedFailureLogFn
	t.Cleanup(func() {
		auditAppendWriteFn = prevWrite
		auditRotatedFailureLogFn = prevRotFail
	})
	return dir
}

// readAuditLines reads every JSON Line in the audit log; returns the
// raw bytes of each line (without the trailing newline) so tests can
// json.Unmarshal each into the shape they care about.
func readAuditLines(t *testing.T, dir string) [][]byte {
	t.Helper()
	path := filepath.Join(dir, intentAuditFileLeaf)
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read audit log %s: %v", path, err)
	}
	if len(raw) == 0 {
		return nil
	}
	parts := strings.Split(strings.TrimRight(string(raw), "\n"), "\n")
	out := make([][]byte, 0, len(parts))
	for _, p := range parts {
		if p == "" {
			continue
		}
		out = append(out, []byte(p))
	}
	return out
}

// ---------------------------------------------------------------------------
// Append → readable line
// ---------------------------------------------------------------------------

func TestIntentAudit_Append_ReadableLine(t *testing.T) {
	a := NewAPI()
	dir := intentAuditTestHelper(t)

	entry := NewIntentAuditEntry(
		WithAction("set-intent"),
		WithTask("\\mcp-local-hub-time-default"),
		WithWho("tester"),
		WithReason(IntentReasonInstall),
	)
	if err := a.AppendIntentAudit(entry); err != nil {
		t.Fatalf("AppendIntentAudit: %v", err)
	}

	lines := readAuditLines(t, dir)
	if len(lines) != 1 {
		t.Fatalf("expected 1 audit line, got %d", len(lines))
	}
	var got map[string]any
	if err := json.Unmarshal(lines[0], &got); err != nil {
		t.Fatalf("audit line not valid JSON: %v\nraw=%s", err, string(lines[0]))
	}
	if got["action"] != "set-intent" {
		t.Errorf("action: got %v, want %q", got["action"], "set-intent")
	}
	if got["task"] != "\\mcp-local-hub-time-default" {
		t.Errorf("task: got %v", got["task"])
	}
	if got["who"] != "tester" {
		t.Errorf("who: got %v", got["who"])
	}
	if got["reason"] != IntentReasonInstall {
		t.Errorf("reason: got %v", got["reason"])
	}
}

// ---------------------------------------------------------------------------
// Caller fields populated; caller_start_time UTC and within ±2s of fresh process start
// ---------------------------------------------------------------------------

func TestIntentAudit_CallerFieldsPopulated(t *testing.T) {
	a := NewAPI()
	dir := intentAuditTestHelper(t)

	entry := NewIntentAuditEntry(
		WithAction("set-intent"),
		WithTask("\\mcp-local-hub-time-default"),
	)
	if err := a.AppendIntentAudit(entry); err != nil {
		t.Fatalf("AppendIntentAudit: %v", err)
	}

	lines := readAuditLines(t, dir)
	if len(lines) != 1 {
		t.Fatalf("expected 1 audit line, got %d", len(lines))
	}
	var got map[string]any
	if err := json.Unmarshal(lines[0], &got); err != nil {
		t.Fatalf("json: %v", err)
	}
	if pidF, ok := got["caller_pid"].(float64); !ok || int(pidF) != os.Getpid() {
		t.Errorf("caller_pid: got %v, want %d", got["caller_pid"], os.Getpid())
	}
	if exe, ok := got["caller_exe"].(string); !ok || exe == "" {
		t.Errorf("caller_exe missing/empty: got %v", got["caller_exe"])
	}
	if user, ok := got["caller_user"].(string); !ok || user == "" {
		t.Errorf("caller_user missing/empty: got %v", got["caller_user"])
	}
	tsRaw, ok := got["caller_start_time"].(string)
	if !ok || tsRaw == "" {
		t.Fatalf("caller_start_time missing: got %v", got["caller_start_time"])
	}
	parsed, err := time.Parse(time.RFC3339Nano, tsRaw)
	if err != nil {
		t.Fatalf("caller_start_time not RFC3339Nano: %v (raw=%q)", err, tsRaw)
	}
	if parsed.Location() != time.UTC {
		t.Errorf("caller_start_time location: got %v, want UTC", parsed.Location())
	}
	// Compare against the SAME source the production writer uses, not against
	// time.Now().
	//
	// This assertion used to read `time.Since(parsed) > 2*time.Minute` with the
	// comment "process just started". That is not a clock tolerance — `parsed`
	// is THIS test binary's own start time, so `time.Since(parsed)` is simply
	// how long the package has been running. It was a hidden budget on SUITE
	// DURATION, and it began failing deterministically once internal/api grew
	// past two minutes (observed deltas 4m10s / 4m34s / 4m16s across runs). The
	// failure was repeatedly dismissed as a flake; it never was one.
	//
	// The real invariant is that the field carries this process's start time,
	// serialized as UTC RFC3339Nano. Comparing to CallerStartTime() tests
	// exactly that and is immune to how long the suite takes.
	//
	// The tolerance absorbs per-OS whole-second truncation in the start-time
	// conversion, not elapsed time: both sides read the same fixed instant, so
	// any difference is representational.
	const startTimeTolerance = 2 * time.Second
	want := CallerStartTime()
	if delta := parsed.Sub(want).Abs(); delta > startTimeTolerance {
		t.Errorf("caller_start_time = %v, want CallerStartTime() = %v (delta %v > %v)",
			parsed, want, delta, startTimeTolerance)
	}
	// ts must also be UTC RFC3339Nano.
	tsTopRaw, ok := got["ts"].(string)
	if !ok {
		t.Fatalf("ts missing")
	}
	tsTop, err := time.Parse(time.RFC3339Nano, tsTopRaw)
	if err != nil {
		t.Fatalf("ts not RFC3339Nano: %v", err)
	}
	if tsTop.Location() != time.UTC {
		t.Errorf("ts location: got %v, want UTC", tsTop.Location())
	}
}

// ---------------------------------------------------------------------------
// File perms 0600 on POSIX
// ---------------------------------------------------------------------------

func TestIntentAudit_FilePerms0600(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX-only perm semantics")
	}
	a := NewAPI()
	dir := intentAuditTestHelper(t)

	if err := a.AppendIntentAudit(NewIntentAuditEntry(
		WithAction("set-intent"),
		WithTask("\\mcp-local-hub-x"),
	)); err != nil {
		t.Fatalf("AppendIntentAudit: %v", err)
	}

	path := filepath.Join(dir, intentAuditFileLeaf)
	st, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat %s: %v", path, err)
	}
	if perm := st.Mode().Perm(); perm != 0o600 {
		t.Errorf("audit log perms = %o, want 0o600", perm)
	}
}

// ---------------------------------------------------------------------------
// Rotation: write >10MB → .log.1 exists; fresh .log first entry is audit-rotated
// ---------------------------------------------------------------------------

func TestIntentAudit_Rotation_AuditRotatedSelfEvent(t *testing.T) {
	a := NewAPI()
	dir := intentAuditTestHelper(t)

	// Seed an existing log with 10MB+1 of bytes to force rotation on next call.
	path := filepath.Join(dir, intentAuditFileLeaf)
	if err := os.WriteFile(path, make([]byte, 10*1024*1024+1024), 0o600); err != nil {
		t.Fatalf("seed log: %v", err)
	}

	if err := a.AppendIntentAudit(NewIntentAuditEntry(
		WithAction("set-intent"),
		WithTask("\\mcp-local-hub-after-rotation"),
	)); err != nil {
		t.Fatalf("AppendIntentAudit: %v", err)
	}

	// .log.1 must exist (rotation target).
	rotPath := filepath.Join(dir, intentAuditFileLeaf+".1")
	if _, err := os.Stat(rotPath); err != nil {
		t.Fatalf("rotated file %s missing: %v", rotPath, err)
	}

	// Fresh .log: first line must be audit-rotated self-event; second line
	// is the caller's entry.
	lines := readAuditLines(t, dir)
	if len(lines) < 2 {
		t.Fatalf("expected >=2 lines after rotation (audit-rotated + caller entry), got %d: %s", len(lines), lines)
	}
	var first map[string]any
	if err := json.Unmarshal(lines[0], &first); err != nil {
		t.Fatalf("first line not JSON: %v", err)
	}
	if first["action"] != "audit-rotated" {
		t.Errorf("first line after rotation: action = %v, want %q", first["action"], "audit-rotated")
	}
	// audit-rotated must be a system entry on the wire.
	if seVal, ok := first["system_entry"].(bool); !ok || !seVal {
		t.Errorf("audit-rotated entry missing system_entry=true: got %v", first["system_entry"])
	}
}

// ---------------------------------------------------------------------------
// Control-char + invalid UTF-8 escape: yields valid JSON
// ---------------------------------------------------------------------------

func TestIntentAudit_ControlAndInvalidUTF8_StillValidJSON(t *testing.T) {
	a := NewAPI()
	dir := intentAuditTestHelper(t)

	// Use a non-identity field for the control + invalid UTF-8 because
	// identity fields (Task, CallerUser) are length-rejected before
	// JSON encoding. Reason is non-identity and free-form here.
	wild := "\x00\n\t\\u" + string([]byte{0xff, 0xfe, 0xfd})
	entry := NewIntentAuditEntry(
		WithAction("set-intent"),
		WithTask("\\mcp-local-hub-x"),
		WithReason(wild),
	)
	if err := a.AppendIntentAudit(entry); err != nil {
		t.Fatalf("AppendIntentAudit: %v", err)
	}

	lines := readAuditLines(t, dir)
	if len(lines) != 1 {
		t.Fatalf("expected 1 line, got %d", len(lines))
	}
	// Must round-trip through json.Unmarshal — i.e., escape sequences
	// produced by encoding/json keep the line valid JSON.
	var got map[string]any
	if err := json.Unmarshal(lines[0], &got); err != nil {
		t.Fatalf("audit line not valid JSON after control/invalid-UTF-8 reason: %v\nraw=%s", err, string(lines[0]))
	}
	// No raw NUL byte in the line — encoding/json escapes .
	if bytesContains(lines[0], 0x00) {
		t.Errorf("audit line contains raw NUL byte; encoding/json should escape as \\u0000")
	}
}

func bytesContains(b []byte, c byte) bool {
	for _, x := range b {
		if x == c {
			return true
		}
	}
	return false
}

// ---------------------------------------------------------------------------
// Audit-write failure surfaces error to caller
// ---------------------------------------------------------------------------

func TestIntentAudit_WriteFailure_SurfacesError(t *testing.T) {
	a := NewAPI()
	intentAuditTestHelper(t)

	wantErr := errors.New("synthetic disk full")
	auditAppendWriteFn = func(path string, line []byte) error {
		return wantErr
	}

	err := a.AppendIntentAudit(NewIntentAuditEntry(
		WithAction("set-intent"),
		WithTask("\\mcp-local-hub-x"),
	))
	if err == nil {
		t.Fatal("expected non-nil err on disk-full simulation, got nil")
	}
	if !errors.Is(err, wantErr) && !strings.Contains(err.Error(), wantErr.Error()) {
		t.Errorf("err = %v, want chain containing %v", err, wantErr)
	}
}

// ---------------------------------------------------------------------------
// audit-rotated idempotent retry: self-event write failure does NOT
// re-rotate; next successful append goes through normally
// ---------------------------------------------------------------------------

func TestIntentAudit_Rotation_SelfEventFailure_IsIdempotent(t *testing.T) {
	a := NewAPI()
	dir := intentAuditTestHelper(t)

	// Seed existing log to force rotation on next call.
	path := filepath.Join(dir, intentAuditFileLeaf)
	if err := os.WriteFile(path, make([]byte, 10*1024*1024+512), 0o600); err != nil {
		t.Fatalf("seed log: %v", err)
	}

	var (
		failedRotErr error
		callCount    int
		mu           sync.Mutex
	)
	auditRotatedFailureLogFn = func(e error) {
		mu.Lock()
		defer mu.Unlock()
		failedRotErr = e
	}
	auditAppendWriteFn = func(path string, line []byte) error {
		mu.Lock()
		callCount++
		idx := callCount
		mu.Unlock()
		// Fail only the audit-rotated self-event (call #1 — the rotation
		// entry). Subsequent writes succeed.
		if idx == 1 && strings.Contains(string(line), `"audit-rotated"`) {
			return errors.New("synthetic disk-full on self-event")
		}
		// Any other call: write through to disk.
		return defaultAppendAuditLine(path, line)
	}

	// First call: triggers rotation; self-event fails; user entry STILL
	// succeeds because the user-entry append is the next call after the
	// failed self-event.
	if err := a.AppendIntentAudit(NewIntentAuditEntry(
		WithAction("set-intent"),
		WithTask("\\mcp-local-hub-after-rotation"),
	)); err != nil {
		t.Fatalf("first AppendIntentAudit (after rotation): %v", err)
	}

	// .log.1 exists.
	rotPath := filepath.Join(dir, intentAuditFileLeaf+".1")
	if _, err := os.Stat(rotPath); err != nil {
		t.Fatalf("rotated file missing: %v", err)
	}
	// failure was reported via the watchdog-log seam.
	if failedRotErr == nil {
		t.Errorf("auditRotatedFailureLogFn was not called; expected the self-event failure to be reported")
	}

	// Second call: must NOT re-rotate (file is well below 10MB after
	// rotation). The user entry from call #1 plus the new entry should
	// both be present in the active .log; no second .log.1 rotation.
	if err := a.AppendIntentAudit(NewIntentAuditEntry(
		WithAction("set-intent"),
		WithTask("\\mcp-local-hub-second"),
	)); err != nil {
		t.Fatalf("second AppendIntentAudit: %v", err)
	}

	// Read the active log: should have the two user entries, no new
	// audit-rotated lines (since the size is well under 10MB).
	lines := readAuditLines(t, dir)
	rotatedCount := 0
	for _, l := range lines {
		if strings.Contains(string(l), `"audit-rotated"`) {
			rotatedCount++
		}
	}
	if rotatedCount != 0 {
		t.Errorf("active log contains %d audit-rotated entries after failed self-event; want 0 (idempotent retry should not re-rotate)", rotatedCount)
	}
	// Must contain at least the second user entry.
	foundSecond := false
	for _, l := range lines {
		if strings.Contains(string(l), "\\\\mcp-local-hub-second") {
			foundSecond = true
		}
	}
	if !foundSecond {
		t.Errorf("second user entry not found in active log: lines=%s", lines)
	}
}

// ---------------------------------------------------------------------------
// Identity field oversize REJECTION (task)
// ---------------------------------------------------------------------------

func TestIntentAudit_IdentityOversize_TaskRejected(t *testing.T) {
	a := NewAPI()
	intentAuditTestHelper(t)

	// 1024 bytes is the cap; 1025 rejects.
	bigTask := strings.Repeat("a", 1025)
	err := a.AppendIntentAudit(NewIntentAuditEntry(
		WithAction("set-intent"),
		WithTask(bigTask),
	))
	if !errors.Is(err, ErrIdentityOversize) {
		t.Fatalf("expected ErrIdentityOversize, got %v", err)
	}
}

// ---------------------------------------------------------------------------
// Identity field oversize REJECTION (caller_user)
// ---------------------------------------------------------------------------

func TestIntentAudit_IdentityOversize_CallerUserRejected(t *testing.T) {
	a := NewAPI()
	intentAuditTestHelper(t)

	bigUser := strings.Repeat("u", 1025)
	entry := NewIntentAuditEntry(
		WithAction("set-intent"),
		WithTask("\\mcp-local-hub-x"),
	)
	// Force CallerUser explicitly so AppendIntentAudit's auto-fill does
	// not overwrite our oversize string.
	entry.CallerUser = bigUser

	err := a.AppendIntentAudit(entry)
	if !errors.Is(err, ErrIdentityOversize) {
		t.Fatalf("expected ErrIdentityOversize on >1KB caller_user, got %v", err)
	}
}

// ---------------------------------------------------------------------------
// Non-identity truncation: 32KB reason field, 100-byte task
// ---------------------------------------------------------------------------

func TestIntentAudit_NonIdentityTruncation(t *testing.T) {
	a := NewAPI()
	dir := intentAuditTestHelper(t)

	smallTask := "\\mcp-local-hub-" + strings.Repeat("t", 50) // 65 bytes
	bigReason := strings.Repeat("r", 32*1024)                 // 32KB
	entry := NewIntentAuditEntry(
		WithAction("set-intent"),
		WithTask(smallTask),
		WithReason(bigReason),
	)
	if err := a.AppendIntentAudit(entry); err != nil {
		t.Fatalf("AppendIntentAudit: %v", err)
	}

	lines := readAuditLines(t, dir)
	if len(lines) != 1 {
		t.Fatalf("expected 1 line, got %d", len(lines))
	}
	if got := len(lines[0]); got > 16*1024 {
		t.Errorf("audit line size = %d, want <=16KB after truncation", got)
	}

	var got map[string]any
	if err := json.Unmarshal(lines[0], &got); err != nil {
		t.Fatalf("json: %v\nraw=%s", err, string(lines[0]))
	}
	// task must be intact (identity field not truncated).
	if got["task"] != smallTask {
		t.Errorf("task: got %v, want intact %q", got["task"], smallTask)
	}
	// _truncated marker present.
	if got["_truncated"] != true {
		t.Errorf("_truncated marker missing/false: got %v", got["_truncated"])
	}
	// _truncated_field must name the truncated field.
	if got["_truncated_field"] != "reason" {
		t.Errorf("_truncated_field: got %v, want %q", got["_truncated_field"], "reason")
	}
	// _task_hash must be 12 hex chars matching SHA-256 of original task.
	wantHash := hex.EncodeToString(sha256.New().Sum([]byte(smallTask))[:0]) // placeholder
	_ = wantHash                                                            // we compute below
	hashRaw, ok := got["_task_hash"].(string)
	if !ok {
		t.Fatalf("_task_hash missing or not string: got %v", got["_task_hash"])
	}
	sum := sha256.Sum256([]byte(smallTask))
	wantHashHex := hex.EncodeToString(sum[:])[:12]
	if hashRaw != wantHashHex {
		t.Errorf("_task_hash: got %q, want %q (sha256-first-12-hex of %q)", hashRaw, wantHashHex, smallTask)
	}
}

// ---------------------------------------------------------------------------
// Non-identity multi-field oversize → drop with placeholder
// ---------------------------------------------------------------------------

func TestIntentAudit_NonIdentityMultiOversize_DropsWithPlaceholder(t *testing.T) {
	a := NewAPI()
	dir := intentAuditTestHelper(t)

	smallTask := "\\mcp-local-hub-" + strings.Repeat("t", 50)
	// All non-identity fields are limited (Reason omitempty + Note + CallerExe).
	// Build an entry where multiple fields each individually fit but the
	// total marshaled size exceeds 16KB even after truncating the longest.
	// Easiest path: use the maximum CallerExe (auto-populated; can't be
	// trivially controlled) plus a giant Reason. But the truncation step
	// reduces *one* longest field. To force "drop with placeholder," set
	// the entry's CallerExe to a big string that pushes total size over
	// budget after truncating Reason to a tiny value.
	bigExe := strings.Repeat("E", 30*1024)
	bigReason := strings.Repeat("R", 30*1024)

	entry := NewIntentAuditEntry(
		WithAction("set-intent"),
		WithTask(smallTask),
		WithReason(bigReason),
	)
	entry.CallerExe = bigExe // direct field set — bypasses auto-fill defaults

	if err := a.AppendIntentAudit(entry); err != nil {
		t.Fatalf("AppendIntentAudit: %v", err)
	}

	lines := readAuditLines(t, dir)
	if len(lines) != 1 {
		t.Fatalf("expected 1 line, got %d", len(lines))
	}
	if got := len(lines[0]); got > 16*1024 {
		t.Errorf("dropped placeholder line size = %d, want <=16KB", got)
	}

	var got map[string]any
	if err := json.Unmarshal(lines[0], &got); err != nil {
		t.Fatalf("json: %v\nraw=%s", err, string(lines[0]))
	}
	if got["action"] != "log-entry-dropped-oversize" {
		t.Errorf("action: got %v, want %q", got["action"], "log-entry-dropped-oversize")
	}
	if got["_truncated"] != true {
		t.Errorf("_truncated marker missing/false: got %v", got["_truncated"])
	}
	hashRaw, ok := got["_task_hash"].(string)
	if !ok {
		t.Fatalf("_task_hash missing: got %v", got["_task_hash"])
	}
	sum := sha256.Sum256([]byte(smallTask))
	wantHashHex := hex.EncodeToString(sum[:])[:12]
	if hashRaw != wantHashHex {
		t.Errorf("_task_hash: got %q, want %q", hashRaw, wantHashHex)
	}
}

// ---------------------------------------------------------------------------
// Sealed systemEntry constructor
// ---------------------------------------------------------------------------

func TestIntentAudit_SealedSystemEntry_Constructors(t *testing.T) {
	intentAuditTestHelper(t)

	// Public constructor → false.
	pub := NewIntentAuditEntry(
		WithAction("set-intent"),
		WithTask("\\mcp-local-hub-x"),
	)
	if pub.IsSystemEntry() {
		t.Errorf("NewIntentAuditEntry: IsSystemEntry want false, got true")
	}

	// Package-private constructor → true.
	sys := newSystemAuditEntry(
		"audit-rotated",
		WithTask("\\mcp-local-hub-x"),
	)
	if !sys.IsSystemEntry() {
		t.Errorf("newSystemAuditEntry: IsSystemEntry want true, got false")
	}
}

// ---------------------------------------------------------------------------
// JSON unmarshal IGNORES system_entry (sealed pattern)
// ---------------------------------------------------------------------------

func TestIntentAudit_UnmarshalDiscardsSystemEntry(t *testing.T) {
	intentAuditTestHelper(t)

	raw := `{"ts":"2026-05-07T12:00:00Z","who":"forger","action":"forged","task":"x","caller_pid":1,"caller_exe":"e","caller_start_time":"2026-05-07T11:59:00Z","caller_user":"forger","system_entry":true}`
	var e IntentAuditEntry
	if err := json.Unmarshal([]byte(raw), &e); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if e.IsSystemEntry() {
		t.Errorf("IsSystemEntry after Unmarshal: got true; sealed pattern requires JSON-side system_entry to be discarded")
	}
}

// ---------------------------------------------------------------------------
// Sentinel <rotation-system> + sealed system entry → no redaction
// ---------------------------------------------------------------------------

func TestIntentAudit_RedactionExemption_SystemEntry(t *testing.T) {
	intentAuditTestHelper(t)

	sys := newSystemAuditEntry(
		"audit-rotated",
		WithTask("\\mcp-local-hub-x"),
	)
	sys.CallerUser = "<rotation-system>"

	out := RedactIntentAuditEntryForNonOwner(sys)
	if out.CallerUser != "<rotation-system>" {
		t.Errorf("system entry CallerUser must be preserved verbatim: got %q, want %q", out.CallerUser, "<rotation-system>")
	}
	if !out.IsSystemEntry() {
		t.Errorf("system flag lost across redaction call")
	}
}

// ---------------------------------------------------------------------------
// <rotation-system> name without sealed system flag → still redacted
// ---------------------------------------------------------------------------

func TestIntentAudit_RedactionRejection_NonSystemEntryUsingSentinelName(t *testing.T) {
	intentAuditTestHelper(t)

	// Forge an entry: sentinel-name caller_user but NOT a sealed system
	// entry (constructor returns IsSystemEntry()=false).
	forged := NewIntentAuditEntry(
		WithAction("forged"),
		WithTask("\\mcp-local-hub-x"),
	)
	forged.CallerUser = "<rotation-system>" // sentinel value but no seal

	out := RedactIntentAuditEntryForNonOwner(forged)
	// Non-owner test: forged.CallerUser != current OS user → must redact.
	// (current OS user is unlikely to literally be "<rotation-system>".)
	if out.CallerUser == "<rotation-system>" {
		t.Errorf("non-system entry with sentinel-shaped CallerUser must be redacted; got verbatim sentinel back")
	}
	if out.CallerUser != "<redacted-non-owner>" {
		t.Errorf("non-system entry CallerUser: got %q, want %q", out.CallerUser, "<redacted-non-owner>")
	}
}

// ---------------------------------------------------------------------------
// Owner match → no redaction
// ---------------------------------------------------------------------------

func TestIntentAudit_RedactionOwnerMatch_NoChange(t *testing.T) {
	intentAuditTestHelper(t)

	owner := currentOSUser()
	if owner == "" || owner == "<unknown>" {
		t.Skip("currentOSUser() unavailable; skipping owner-match check")
	}
	entry := NewIntentAuditEntry(
		WithAction("set-intent"),
		WithTask("\\mcp-local-hub-x"),
	)
	entry.CallerUser = owner

	out := RedactIntentAuditEntryForNonOwner(entry)
	if out.CallerUser != owner {
		t.Errorf("owner-match: CallerUser got %q, want unchanged %q", out.CallerUser, owner)
	}
}

// ---------------------------------------------------------------------------
// Priority field with omitempty
// ---------------------------------------------------------------------------

func TestIntentAudit_PriorityField_HighEmitsToJSON(t *testing.T) {
	a := NewAPI()
	dir := intentAuditTestHelper(t)

	if err := a.AppendIntentAudit(NewIntentAuditEntry(
		WithAction("watchdog-install-elevated-override"),
		WithTask("\\mcp-local-hub-x"),
		WithPriority("high"),
	)); err != nil {
		t.Fatalf("AppendIntentAudit: %v", err)
	}
	lines := readAuditLines(t, dir)
	if len(lines) != 1 {
		t.Fatalf("expected 1 line, got %d", len(lines))
	}
	var got map[string]any
	if err := json.Unmarshal(lines[0], &got); err != nil {
		t.Fatalf("json: %v", err)
	}
	if got["priority"] != "high" {
		t.Errorf("priority: got %v, want %q", got["priority"], "high")
	}
}

func TestIntentAudit_PriorityField_DefaultEmptyOmitted(t *testing.T) {
	a := NewAPI()
	dir := intentAuditTestHelper(t)

	if err := a.AppendIntentAudit(NewIntentAuditEntry(
		WithAction("set-intent"),
		WithTask("\\mcp-local-hub-x"),
	)); err != nil {
		t.Fatalf("AppendIntentAudit: %v", err)
	}
	lines := readAuditLines(t, dir)
	if len(lines) != 1 {
		t.Fatalf("expected 1 line, got %d", len(lines))
	}
	// Raw byte search: omitempty must drop the priority field entirely.
	if strings.Contains(string(lines[0]), `"priority"`) {
		t.Errorf("priority field present despite default-empty Priority: line=%s", lines[0])
	}
}

// ---------------------------------------------------------------------------
// Wired through the appendIntentAuditFn seam: it binds the production
// AppendIntentAudit so the dispatcher reaches real disk.
// ---------------------------------------------------------------------------

func TestIntentAudit_WiresTask0Seam(t *testing.T) {
	a := NewAPI()
	dir := intentAuditTestHelper(t)

	// Reset the seam so init() binding is the only source. We directly
	// test that the production AppendIntentAudit is now what the
	// dispatcher reaches.
	if appendIntentAuditFn == nil {
		t.Fatal("appendIntentAuditFn nil; Task 3 init() must wire production AppendIntentAudit")
	}
	// Call through the dispatcher.
	err := appendIntentAuditFn(NewIntentAuditEntry(
		WithAction("seam-probe"),
		WithTask("\\mcp-local-hub-probe"),
	))
	if err != nil {
		t.Fatalf("appendIntentAuditFn returned err: %v", err)
	}
	lines := readAuditLines(t, dir)
	found := false
	for _, l := range lines {
		if strings.Contains(string(l), "seam-probe") {
			found = true
		}
	}
	if !found {
		t.Errorf("seam-probe entry not present on disk; production AppendIntentAudit not wired through Task 0 seam")
	}
	_ = a // a is unused but kept so the helper-unwind path is consistent
}

// ---------------------------------------------------------------------------
// Task 2 wiring: WriteDaemonIntent + ClearDaemonIntent emit set-intent /
// clear-intent audit entries with Before/After
// ---------------------------------------------------------------------------

func TestIntentAudit_WriteDaemonIntentEmitsSetIntentAudit(t *testing.T) {
	a := NewAPI()
	dir := intentAuditTestHelper(t)

	now := time.Now().UTC()
	if err := a.WriteDaemonIntent("\\mcp-local-hub-x", DaemonIntent{
		Desired:   IntentDesiredStopped,
		Reason:    IntentReasonUserStop,
		UpdatedAt: now,
	}, "tester"); err != nil {
		t.Fatalf("WriteDaemonIntent: %v", err)
	}

	lines := readAuditLines(t, dir)
	if len(lines) != 1 {
		t.Fatalf("expected 1 audit line after WriteDaemonIntent, got %d: %s", len(lines), lines)
	}
	var got map[string]any
	if err := json.Unmarshal(lines[0], &got); err != nil {
		t.Fatalf("json: %v", err)
	}
	if got["action"] != "set-intent" {
		t.Errorf("action: got %v, want %q", got["action"], "set-intent")
	}
	if got["task"] != "\\mcp-local-hub-x" {
		t.Errorf("task: got %v", got["task"])
	}
	if got["who"] != "tester" {
		t.Errorf("who: got %v", got["who"])
	}
	// "after" should reflect the new intent.
	after, ok := got["after"].(map[string]any)
	if !ok {
		t.Fatalf("after missing or not object: got %v", got["after"])
	}
	if after["desired"] != IntentDesiredStopped {
		t.Errorf("after.desired: got %v", after["desired"])
	}
	if after["reason"] != IntentReasonUserStop {
		t.Errorf("after.reason: got %v", after["reason"])
	}
}

func TestIntentAudit_ClearDaemonIntentEmitsClearIntentAudit(t *testing.T) {
	a := NewAPI()
	dir := intentAuditTestHelper(t)

	now := time.Now().UTC()
	if err := a.WriteDaemonIntent("\\mcp-local-hub-x", DaemonIntent{
		Desired:   IntentDesiredStopped,
		Reason:    IntentReasonUserStop,
		UpdatedAt: now,
	}, "tester"); err != nil {
		t.Fatalf("WriteDaemonIntent: %v", err)
	}
	if err := a.ClearDaemonIntent("\\mcp-local-hub-x", "tester"); err != nil {
		t.Fatalf("ClearDaemonIntent: %v", err)
	}

	lines := readAuditLines(t, dir)
	if len(lines) < 2 {
		t.Fatalf("expected >=2 audit lines (set + clear), got %d: %s", len(lines), lines)
	}
	last := lines[len(lines)-1]
	var got map[string]any
	if err := json.Unmarshal(last, &got); err != nil {
		t.Fatalf("json: %v", err)
	}
	if got["action"] != "clear-intent" {
		t.Errorf("action: got %v, want %q", got["action"], "clear-intent")
	}
	if got["task"] != "\\mcp-local-hub-x" {
		t.Errorf("task: got %v", got["task"])
	}
	// "before" should reflect the stop intent that was being cleared.
	before, ok := got["before"].(map[string]any)
	if !ok {
		t.Fatalf("before missing or not object: got %v", got["before"])
	}
	if before["desired"] != IntentDesiredStopped {
		t.Errorf("before.desired: got %v", before["desired"])
	}
}

// ---------------------------------------------------------------------------
// Task 0 IntentAuditEntry stub migrated: full schema fields exist
// ---------------------------------------------------------------------------

func TestIntentAudit_FullSchemaFields(t *testing.T) {
	intentAuditTestHelper(t)

	// All public fields per plan §35 v9. Compile-time assertion that
	// every plan-required field exists on the struct. If a future
	// refactor renames or drops one this test fails to compile.
	var e IntentAuditEntry
	_ = e.TS
	_ = e.Who
	_ = e.Action
	_ = e.Task
	_ = e.Before
	_ = e.After
	_ = e.CallerPID
	_ = e.CallerExe
	_ = e.CallerStartTime
	_ = e.CallerUser
	_ = e.Reason
	_ = e.Priority
	// Type assertions on a few of the nontrivial types.
	var _ time.Time = e.TS
	var _ time.Time = e.CallerStartTime
	var _ *DaemonIntent = e.Before
	var _ *DaemonIntent = e.After
}

// ---------------------------------------------------------------------------
// CallerStartTime helper returns UTC RFC3339Nano-parseable instant
// ---------------------------------------------------------------------------

func TestIntentAudit_CallerStartTimeHelper_UTC(t *testing.T) {
	got := CallerStartTime()
	if got.Location() != time.UTC {
		t.Errorf("CallerStartTime location: got %v, want UTC", got.Location())
	}
	if got.IsZero() {
		t.Errorf("CallerStartTime returned zero value")
	}
	// Round-trip through RFC3339Nano string.
	s := got.Format(time.RFC3339Nano)
	parsed, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		t.Errorf("RFC3339Nano roundtrip: %v", err)
	}
	if !parsed.Equal(got) {
		t.Errorf("RFC3339Nano roundtrip mismatch: %v vs %v", parsed, got)
	}
}

// ---------------------------------------------------------------------------
// Compile-time assertions: contract guards. If a future refactor renames a
// JSON method, the build fails rather than silently dropping the sealed
// pattern.
// ---------------------------------------------------------------------------

var _ json.Marshaler = IntentAuditEntry{}
var _ json.Unmarshaler = (*IntentAuditEntry)(nil)

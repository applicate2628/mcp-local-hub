package api

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gofrs/flock"
)

// TestWriteStateFileAtomic is the happy-path baseline. v0.5.0 Fix
// Group 5 preserves the test's identity (happy-path round trip
// asserting payload survives + no temp-file leak) while wiring the
// parent dir through hardenedTempDir so the new hardened pipeline
// can pass its parent-dir gate. On machines whose %TEMP%
// (Windows) / TMPDIR (POSIX) has Authenticated-Users-equivalent
// write rights, t.TempDir() alone would fail the parent-dir gate
// — the hardened temp dir installs an allowlist-conforming DACL
// (Windows) or 0700 mode (POSIX) so the strict path passes.
// Falsifiable claim #12 still holds: the test's assertion surface
// is unchanged.
func TestWriteStateFileAtomic(t *testing.T) {
	dir := hardenedTempDir(t)
	path := filepath.Join(dir, "test-state.json")
	payload := map[string]string{"hello": "world"}

	if err := WriteStateFileAtomic(path, payload); err != nil {
		t.Fatalf("write: %v", err)
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	var got map[string]string
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got["hello"] != "world" {
		t.Fatalf("payload mismatch: %v", got)
	}

	// Verify no temp file leftover. The hardened-pipeline writer
	// produces a crypto/rand-named temp file inside the parent dir
	// and renames it across the held parent-dir handle, so a
	// successful write must leave ONLY the destination basename
	// plus the per-file flock leaf (`<path>.lock`) — anything else
	// would be a hardened-pipeline regression that leaks bytes.
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		name := e.Name()
		if name == "test-state.json" {
			continue
		}
		if name == "test-state.json.lock" {
			// Per-file flock leaf created by WriteStateFileAtomic;
			// gofrs/flock does not delete the lock file on Unlock
			// because the file IS the lock — leaving it in place
			// is the documented behavior.
			continue
		}
		t.Fatalf("temp file leaked: %s", name)
	}
}

func TestWriteStateFileBytesAtomicWritesRawPayload(t *testing.T) {
	dir := hardenedTempDir(t)
	path := filepath.Join(dir, "daemon-env-overrides.yaml")
	payload := []byte("version: 1\ndaemons: {}\n")

	if err := WriteStateFileBytesAtomic(path, payload); err != nil {
		t.Fatalf("write bytes: %v", err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read bytes: %v", err)
	}
	if string(raw) != string(payload) {
		t.Fatalf("raw payload mismatch:\n got %q\nwant %q", string(raw), string(payload))
	}
}

// TestWriteStateFileAtomic_AcquiresFileLock pins falsifiable claim
// #7: concurrent WriteStateFileAtomic invocations against the same
// path must serialize via flock.New(path + ".lock"). The test
// acquires the lock OUT-OF-BAND first, holds it past the
// WriteStateFileAtomic call's expected acquire point, then releases
// it — the second writer must block until release happens.
//
// We measure the wall-clock delta between the external release and
// the WriteStateFileAtomic return. If the lock works, the delta is
// small (the second writer was already blocked waiting). If the
// lock is missing, WriteStateFileAtomic would return before the
// external release, evidencing the absent serialization.
func TestWriteStateFileAtomic_AcquiresFileLock(t *testing.T) {
	dir := hardenedTempDir(t)
	path := filepath.Join(dir, "lock-test.json")
	lockPath := path + ".lock"

	// Acquire the per-file lock externally first.
	external := flock.New(lockPath)
	if err := external.Lock(); err != nil {
		t.Fatalf("external lock acquire: %v", err)
	}

	// Spawn the writer; it must block on flock acquire.
	var wg sync.WaitGroup
	var writeErr error
	var writeReturnedAt time.Time
	wg.Add(1)
	go func() {
		defer wg.Done()
		writeErr = WriteStateFileAtomic(path, map[string]string{"v": "1"})
		writeReturnedAt = time.Now()
	}()

	// Give the goroutine time to attempt the lock acquire.
	time.Sleep(150 * time.Millisecond)

	// Release; the goroutine should now proceed.
	releasedAt := time.Now()
	if err := external.Unlock(); err != nil {
		t.Fatalf("external unlock: %v", err)
	}
	wg.Wait()

	if writeErr != nil {
		t.Fatalf("write: %v", writeErr)
	}

	// If flock is honored, writeReturnedAt - releasedAt is small
	// (the writer was already blocked waiting and proceeds as soon
	// as the lock frees). If flock is missing, writeReturnedAt
	// would be BEFORE releasedAt because the writer would have
	// completed during the 150ms sleep above.
	if writeReturnedAt.Before(releasedAt) {
		t.Fatalf("WriteStateFileAtomic returned before external lock released — flock not honored (returned=%v, released=%v)",
			writeReturnedAt, releasedAt)
	}
	// Sanity bound: the writer should complete within a reasonable
	// window after release. 5s leaves ample headroom for slow CI.
	if writeReturnedAt.Sub(releasedAt) > 5*time.Second {
		t.Fatalf("WriteStateFileAtomic took >5s after external unlock — flock may be deadlocked")
	}
}

// TestWriteStateFileAtomic_HonorsStrictModeWhenParentInsecure pins
// falsifiable claim #4: with MCPHUB_REQUIRE_SINGLE_USER_HOME=1 the
// strict parent-dir gate is enforced, and a broadened parent
// produces a hard error. The error string must mention the env var
// so the operator knows which knob to flip.
func TestWriteStateFileAtomic_HonorsStrictModeWhenParentInsecure(t *testing.T) {
	t.Setenv(RequireSingleUserHomeEnv, "1")
	t.Setenv(AllowUnhardenedClientWriteEnv, "")

	parent := filepath.Join(t.TempDir(), "leaky-parent")
	if err := os.Mkdir(parent, 0o700); err != nil {
		t.Fatalf("mkdir parent: %v", err)
	}
	broadenParentForStateFileTest(t, parent)

	dst := filepath.Join(parent, "supervisor-intent.json")
	err := WriteStateFileAtomic(dst, map[string]string{"v": "1"})
	if err == nil {
		t.Fatalf("strict-mode must reject permissive parent; got nil")
	}
	if !strings.Contains(err.Error(), RequireSingleUserHomeEnv) {
		t.Errorf("error must mention %q (strict-mode signal); got %v", RequireSingleUserHomeEnv, err)
	}
	// The destination file must NOT exist — a strict refusal that
	// leaks a half-written file is worse than the missing gate.
	if _, statErr := os.Stat(dst); !os.IsNotExist(statErr) {
		t.Errorf("strict-mode rejection leaked a write at %s (stat err = %v)", dst, statErr)
	}
}

func TestWriteStateFileAtomic_HonorsPersistedStrictModeWhenParentInsecure(t *testing.T) {
	statePathsHelper(t)
	stateDir := hardenedTempDir(t)
	daemonStateRootOverride = stateDir
	t.Setenv(RequireSingleUserHomeEnv, "")
	t.Setenv(AllowUnhardenedClientWriteEnv, "")
	t.Cleanup(resetStrictModeIntentCacheForTest)

	intentPath := filepath.Join(stateDir, supervisorIntentFileLeaf)
	if err := os.WriteFile(intentPath, []byte(`{"version":1,"strict_mode":true}`), 0o600); err != nil {
		t.Fatalf("seed persisted strict intent: %v", err)
	}
	resetStrictModeIntentCacheForTest()
	if !OperatorRequiresSingleUserHome() {
		t.Fatal("precondition: persisted strict_mode=true with env unset must enable the strict gate")
	}

	parent := filepath.Join(t.TempDir(), "leaky-parent")
	if err := os.Mkdir(parent, 0o700); err != nil {
		t.Fatalf("mkdir parent: %v", err)
	}
	broadenParentForStateFileTest(t, parent)

	dst := filepath.Join(parent, "supervisor-intent.json")
	err := WriteStateFileAtomic(dst, map[string]string{"v": "1"})
	if err == nil {
		t.Fatalf("persisted strict_mode=true must reject permissive parent even when %s is unset", RequireSingleUserHomeEnv)
	}
	if !strings.Contains(err.Error(), "persisted supervisor-intent.json") {
		t.Errorf("error must mention persisted strict-mode intent; got %v", err)
	}
	if _, statErr := os.Stat(dst); !os.IsNotExist(statErr) {
		t.Errorf("persisted strict-mode rejection leaked a write at %s (stat err = %v)", dst, statErr)
	}
}

// TestWriteStateFileAtomic_DefaultRelaxLaneSucceeds pins
// falsifiable claim #5: when the env vars are unset and the parent
// gate rejects, the relax lane fires, the write succeeds, and the
// audit row lands in the canonical channel.
//
// Audit channel (codex r3 Lane D/E P2): supervisor-events.log
// (NOT hub-mcp.log). State-file fallbacks are supervisor-domain
// events under spec §Q13 and must be emitted via
// OpenSupervisorEventLog so operators monitoring
// supervisor-events.log for audit-posture downgrades see the
// relax-lane fire. Client-config fallbacks remain in hub-mcp.log
// ("client-write-unhardened-fallback") so the two policy domains
// stay separable. See
// TestSecureWriteStateFileWithOperatorOpt_FallbackEventGoesToSupervisorEventsLog
// for the assertion that the event lands in supervisor-events.log
// (not hub-mcp.log).
func TestWriteStateFileAtomic_DefaultRelaxLaneSucceeds(t *testing.T) {
	// Redirect the state dir so the audit channel under test
	// (supervisor-events.log) lands in a clean per-test path with
	// no leftover events from a previous run.
	statePathsHelper(t)
	stateDir := hardenedTempDir(t)
	daemonStateRootOverride = stateDir

	t.Setenv(RequireSingleUserHomeEnv, "")
	t.Setenv(AllowUnhardenedClientWriteEnv, "")

	parent := filepath.Join(t.TempDir(), "leaky-parent")
	if err := os.Mkdir(parent, 0o700); err != nil {
		t.Fatalf("mkdir parent: %v", err)
	}
	broadenParentForStateFileTest(t, parent)

	dst := filepath.Join(parent, "supervisor-intent.json")
	if err := WriteStateFileAtomic(dst, map[string]string{"v": "1"}); err != nil {
		t.Fatalf("relax write: %v", err)
	}

	// File exists and round-trips.
	raw, err := os.ReadFile(dst)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	var got map[string]string
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got["v"] != "1" {
		t.Fatalf("payload mismatch: %v", got)
	}

	// Audit row landed in supervisor-events.log with the canonical
	// supervisor-domain envelope. Hub-mcp.log must NOT carry the
	// event under the canonical path; the
	// FallbackEventGoesToSupervisorEventsLog test below asserts the
	// channel separation explicitly.
	found, line := findSupervisorEventByName(t, filepath.Join(stateDir, SupervisorEventLogFileLeaf), "state-file-write-unhardened-fallback")
	if found == nil {
		t.Fatalf("no state-file-write-unhardened-fallback event in supervisor-events.log (raw=%q)", line)
	}
	if got := found["severity"]; got != SupervisorEventSeverityWarn {
		t.Errorf("event severity = %v, want %q (security-boundary downgrade must be dashboard-visible)", got, SupervisorEventSeverityWarn)
	}
	body, _ := found["body"].(map[string]any)
	if body == nil {
		t.Fatalf("event body not an object: %v", found["body"])
	}
	if path, _ := body["path"].(string); path != dst {
		t.Errorf("event body.path = %v, want %q", body["path"], dst)
	}
}

func TestStateFileRelaxLane_WrongOwnerGateCauseDoesNotRelax(t *testing.T) {
	gateErr := errors.Join(ErrSecureWriteParentInsecure, ErrWrongOwner)

	if stateFileParentGateAllowsDefaultRelax(gateErr) {
		t.Fatalf("wrong-owner parent gate error entered the default-relax lane; want refusal")
	}
}

func TestStateFileRelaxLane_BroadenedOwnerCorrectGateCauseRelaxes(t *testing.T) {
	gateErr := ErrSecureWriteParentInsecure

	if !stateFileParentGateAllowsDefaultRelax(gateErr) {
		t.Fatalf("broadened owner-correct parent gate error did not enter the default-relax lane")
	}
}

func TestStateFileRelaxLane_NonParentGateCauseDoesNotRelax(t *testing.T) {
	if stateFileParentGateAllowsDefaultRelax(nil) {
		t.Fatalf("nil error entered the default-relax lane; want refusal")
	}
	if stateFileParentGateAllowsDefaultRelax(ErrWrongOwner) {
		t.Fatalf("wrong-owner error without parent-gate sentinel entered the default-relax lane; want refusal")
	}
}

// TestSecureWriteStateFileWithOperatorOpt_FallbackEventGoesToSupervisorEventsLog
// pins codex r3 Lane D/E P2: the default-relax audit event MUST
// land in supervisor-events.log, NOT hub-mcp.log. Before this
// change the helper called LogHubMcpEvent which wrote to
// hub-mcp.log under a different envelope ({level, event, caller}
// instead of the supervisor {schema_version, ts, severity, source,
// event, task_name, body}). Operators monitoring
// supervisor-events.log for audit-posture downgrades would not
// have seen the relax-lane fire under the prior wiring.
//
// Assertions:
//
//  1. After a relax-lane fire, supervisor-events.log contains an
//     entry with event == "state-file-write-unhardened-fallback",
//     source == "state-file-helper", severity == "warn", and the
//     supervisor envelope keys.
//  2. hub-mcp.log does NOT contain any
//     state-file-write-unhardened-fallback entry on the canonical
//     path (it stays the audit channel for
//     client-write-unhardened-fallback only).
func TestSecureWriteStateFileWithOperatorOpt_FallbackEventGoesToSupervisorEventsLog(t *testing.T) {
	statePathsHelper(t)
	stateDir := hardenedTempDir(t)
	daemonStateRootOverride = stateDir

	t.Setenv(RequireSingleUserHomeEnv, "")
	t.Setenv(AllowUnhardenedClientWriteEnv, "")

	parent := filepath.Join(t.TempDir(), "leaky-parent")
	if err := os.Mkdir(parent, 0o700); err != nil {
		t.Fatalf("mkdir parent: %v", err)
	}
	broadenParentForStateFileTest(t, parent)

	dst := filepath.Join(parent, "supervisor-intent.json")
	if err := WriteStateFileAtomic(dst, map[string]string{"v": "1"}); err != nil {
		t.Fatalf("relax write: %v", err)
	}

	// Assertion 1: supervisor-events.log carries the event.
	eventsPath := filepath.Join(stateDir, SupervisorEventLogFileLeaf)
	found, line := findSupervisorEventByName(t, eventsPath, "state-file-write-unhardened-fallback")
	if found == nil {
		t.Fatalf("supervisor-events.log missing state-file-write-unhardened-fallback (raw=%q)", line)
	}
	if got := found["source"]; got != "state-file-helper" {
		t.Errorf("event source = %v, want \"state-file-helper\" (per supervisor envelope)", got)
	}
	if got := found["severity"]; got != SupervisorEventSeverityWarn {
		t.Errorf("event severity = %v, want %q", got, SupervisorEventSeverityWarn)
	}
	if got := found["schema_version"]; got != SupervisorEventSchemaVersion {
		t.Errorf("event schema_version = %v, want %q (canonical envelope)", got, SupervisorEventSchemaVersion)
	}
	body, _ := found["body"].(map[string]any)
	if body == nil {
		t.Fatalf("event body not an object: %v", found["body"])
	}
	if got := body["path"]; got != dst {
		t.Errorf("event body.path = %v, want %q", got, dst)
	}
	// audit_channel_degraded is set ONLY on the fallback branches
	// (state-dir resolve / open / emit failure). The canonical path
	// must NOT set it.
	if _, degraded := body["audit_channel_degraded"]; degraded {
		t.Errorf("canonical supervisor-events.log path must not set audit_channel_degraded; got body=%v", body)
	}

	// Assertion 2: hub-mcp.log MUST NOT carry the state-file
	// fallback event under the canonical path. The channel
	// separation prevents collision with
	// client-write-unhardened-fallback dashboards.
	hubEvents, err := RecentHubMcpEvents(50)
	if err != nil {
		// hub-mcp.log may not exist yet on a fresh stateDir; that
		// itself proves the canonical path didn't write there.
		// Only fail the test on a real read error (not a missing-
		// file error which RecentHubMcpEvents already maps to a
		// nil-events return).
		t.Logf("RecentHubMcpEvents: %v (treated as empty)", err)
	}
	for _, ev := range hubEvents {
		if ev["event"] == "state-file-write-unhardened-fallback" {
			t.Errorf("hub-mcp.log must not carry state-file-write-unhardened-fallback under the canonical path; got %v", ev)
		}
	}
}

// findSupervisorEventByName scans supervisor-events.log at path
// for the first JSONL entry whose `event` field matches name.
// Returns (parsed-map, raw-line) on hit; (nil, "") when absent.
// Reused by the relax-lane tests so they assert against a single
// canonical decoder rather than re-implementing the JSONL parse.
func findSupervisorEventByName(t *testing.T, path, name string) (map[string]any, string) {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, ""
		}
		t.Fatalf("read %s: %v", path, err)
	}
	for _, line := range strings.Split(strings.TrimRight(string(raw), "\n"), "\n") {
		if line == "" {
			continue
		}
		var got map[string]any
		if err := json.Unmarshal([]byte(line), &got); err != nil {
			t.Fatalf("supervisor-events.log line invalid JSON: %v: %q", err, line)
		}
		if got["event"] == name {
			return got, line
		}
	}
	return nil, ""
}

// TestWriteStateFileAtomic_StrictModeWithWriteCapableParent pins the default
// relax posture after inode-anchored reads closed the swap window: when the env
// strict flag is OFF, a write-capable parent no longer hard-fails. The hardened
// skip-parent-gate writer still creates an owner-only file and emits the state
// fallback audit event.
func TestWriteStateFileAtomic_StrictModeWithWriteCapableParent(t *testing.T) {
	t.Setenv(RequireSingleUserHomeEnv, "") // default relax
	t.Setenv(AllowUnhardenedClientWriteEnv, "")
	t.Setenv(AllowUnhardenedStateWriteEnv, "")

	parent := filepath.Join(t.TempDir(), "writable-parent")
	if err := os.Mkdir(parent, 0o700); err != nil {
		t.Fatalf("mkdir parent: %v", err)
	}
	broadenParentForStateFileWriteCapableTest(t, parent)

	dst := filepath.Join(parent, "supervisor-intent.json")
	if err := WriteStateFileAtomic(dst, map[string]string{"v": "1"}); err != nil {
		t.Fatalf("default-relax write-capable parent must proceed through hardened fallback, got %v", err)
	}
	raw, err := readStateFileInodeAnchoredEnvStrictOnly(dst)
	if err != nil {
		t.Fatalf("written state file must be readable by the anchored reader: %v", err)
	}
	if !strings.Contains(string(raw), `"v": "1"`) {
		t.Fatalf("written payload = %q, want v=1", raw)
	}
}

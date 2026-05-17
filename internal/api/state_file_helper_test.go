package api

import (
	"encoding/json"
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

// TestWriteStateFileAtomic_DefaultRelaxLaneSucceeds pins
// falsifiable claim #5: when the env vars are unset and the parent
// gate rejects, the relax lane fires, the write succeeds, and a
// warn event "state-file-write-unhardened-fallback" lands in
// hub-mcp.log so log-monitoring dashboards keep visibility on the
// security-boundary downgrade.
func TestWriteStateFileAtomic_DefaultRelaxLaneSucceeds(t *testing.T) {
	// Redirect the state dir so RecentHubMcpEvents reads from a
	// clean per-test path (no leftover events from production
	// hub-mcp.log).
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

	// Warn event landed in hub-mcp.log with the new distinct event
	// name (separate from "client-write-unhardened-fallback" so
	// audit-log filters can distinguish state-file from
	// client-config write fallbacks).
	events, err := RecentHubMcpEvents(20)
	if err != nil {
		t.Fatalf("read recent events: %v", err)
	}
	var found map[string]any
	for _, ev := range events {
		if ev["event"] == "state-file-write-unhardened-fallback" {
			found = ev
			break
		}
	}
	if found == nil {
		t.Fatalf("no state-file-write-unhardened-fallback event in last %d entries (got %d events)", 20, len(events))
	}
	if found["level"] != "warn" {
		t.Errorf("event level = %v, want \"warn\" (security-boundary downgrade must be dashboard-visible)", found["level"])
	}
	if path, _ := found["path"].(string); path != dst {
		t.Errorf("event path = %v, want %q", found["path"], dst)
	}
}

// TestWriteStateFileAtomic_StrictModeWithWriteCapableParent pins
// falsifiable claim #6: even when strict mode is OFF (default
// relax), a parent that grants write/delete to a non-allowlisted
// principal is refused with a "TOCTOU swap risk" error. The
// asymmetry between accepting the write (under default-relax) and
// having the read side refuse the same parent would strand state
// files in unreadable directories; this test guards the symmetry.
//
// Test-name "StrictMode" in the design memo is the public test
// name — the actual scenario is default-relax + write-capable
// parent (per memo step 5 description). The check fires from
// secureWriteStateFileWithOperatorOpt's relax branch via
// checkStateDirParentWriteSafe.
func TestWriteStateFileAtomic_StrictModeWithWriteCapableParent(t *testing.T) {
	t.Setenv(RequireSingleUserHomeEnv, "") // default relax
	t.Setenv(AllowUnhardenedClientWriteEnv, "")

	parent := filepath.Join(t.TempDir(), "writable-parent")
	if err := os.Mkdir(parent, 0o700); err != nil {
		t.Fatalf("mkdir parent: %v", err)
	}
	broadenParentForStateFileWriteCapableTest(t, parent)

	dst := filepath.Join(parent, "supervisor-intent.json")
	err := WriteStateFileAtomic(dst, map[string]string{"v": "1"})
	if err == nil {
		t.Fatalf("default-relax must still refuse write-capable parent (TOCTOU swap risk); got nil")
	}
	if !strings.Contains(err.Error(), "TOCTOU swap risk") {
		t.Errorf("error must mention \"TOCTOU swap risk\"; got %v", err)
	}
	// File must NOT exist — refusing then leaking is the worst of
	// both worlds.
	if _, statErr := os.Stat(dst); !os.IsNotExist(statErr) {
		t.Errorf("write-capable rejection leaked a file at %s (stat err = %v)", dst, statErr)
	}
}

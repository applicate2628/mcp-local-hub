// overlay_hardening_test.go — tests for the two deep-security overlay
// hardening fixes on the Load path:
//
//   - Sec-F1: Load now calls the read-side parent-DACL gate
//     (checkStateDirParentReadSafe) after the regular-file check and
//     before the size read. Strict mode
//     (MCPHUB_REQUIRE_SINGLE_USER_HOME=1) over a broadened parent makes
//     Load REFUSE the read; the explicit relax opt-out leaves Load
//     succeeding. This closes the silent-no-op where the documented
//     strict mitigation never fired on the read path, letting a
//     co-resident principal who swapped the overlay file inject
//     attacker-controlled daemon env.
//
//   - Conc-F6: Load retries exactly once on a transient open/stat I/O
//     failure (consistent with a writer's in-flight atomic rename) so
//     the spawn-time live-reload reads the just-applied edit instead of
//     degrading to the stale startup snapshot. The writer publishes via
//     atomic rename, so a concurrent reader observes the COMPLETE old or
//     COMPLETE new file — never a torn/partial parse. The concurrency
//     test asserts that property under -race.
//
// Spec: docs/superpowers/specs/2026-05-19-servers-matrix-lsp-and-env-revamp-design.md
// §"Read-side hardening" (B-V4-1, B-V4-4); plan §"Load ... ->
// checkStateDirParentReadSafe(parentDir) -> size cap".

package daemon_env_overlay

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	api "mcp-local-hub/internal/api"
	"mcp-local-hub/internal/api/apitest"
)

// broadenedParentForGateRejection returns a directory that the
// write-side parent gate (and therefore the symmetric read-side gate)
// REJECTS, contriving a "broadened parent" the way a corp-managed
// %LOCALAPPDATA% or a chmod-broadened POSIX dir looks. The test SKIPs if
// the host cannot produce such a parent.
//
//   - Windows: the raw t.TempDir() under %TEMP% inherits an
//     Authenticated-Users (S-1-5-11) ACE, which the gate refuses — no
//     elevation needed.
//   - POSIX: chmod the temp dir to 0777 so the group/world write bits
//     trip checkStateDirParentWriteSafe.
func broadenedParentForGateRejection(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if runtime.GOOS != "windows" {
		if err := os.Chmod(dir, 0o0777); err != nil {
			t.Fatalf("chmod parent broadened: %v", err)
		}
		t.Cleanup(func() { _ = os.Chmod(dir, 0o0700) })
	}
	// Precondition: the gate must actually reject this parent. If a
	// hardened CI runner / unusual %TEMP% ACL / umask prevents the
	// broadening from sticking, skip rather than assert a false negative.
	if err := api.CheckStateDirParentWriteSafe(dir); err == nil {
		t.Skipf("parent %s is not gate-rejecting on this host (cannot contrive a broadened parent without elevation): write-side gate accepted it", dir)
	}
	return dir
}

// writeOverlayFileDirect writes an overlay file body straight to disk
// with os.WriteFile (NOT through WriteOverlay's hardened writer, which
// would run the WRITE-side gate and could itself refuse a broadened
// parent). The Sec-F1 tests need a complete, readable overlay file
// sitting inside a broadened parent so the READ gate is the only thing
// under test.
func writeOverlayFileDirect(t *testing.T, dir, body string) string {
	t.Helper()
	path := filepath.Join(dir, "daemon-env-overrides.yaml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write overlay file: %v", err)
	}
	return path
}

// TestLoad_StrictMode_BroadenedParent_Refuses proves Sec-F1: with
// MCPHUB_REQUIRE_SINGLE_USER_HOME=1 set and a parent the gate rejects,
// Load returns the strict gate error instead of silently reading the
// file.
func TestLoad_StrictMode_BroadenedParent_Refuses(t *testing.T) {
	dir := broadenedParentForGateRejection(t)
	path := writeOverlayFileDirect(t, dir, "version: 1\ndaemons: {}\n")

	t.Setenv("MCPHUB_REQUIRE_SINGLE_USER_HOME", "1")
	t.Setenv(AllowUnhardenedStateReadEnv, "") // strict must win even if relax were set

	_, err := Load(path)
	if err == nil {
		t.Fatalf("Load(broadened parent, strict mode): expected refusal, got nil error")
	}
	low := strings.ToLower(err.Error())
	if !strings.Contains(low, "single-user") && !strings.Contains(low, "parent gate") {
		t.Errorf("Load strict-refusal error %q does not look like the parent-gate strict error", err)
	}
}

// TestLoad_DefaultMode_BroadenedParent_Refuses proves the default-mode
// (neither env var set) read-side refusal also fires through Load — the
// read side defaults to REFUSE on a broadened parent (unlike the write
// side's default-relax), so the gate must reject here too.
func TestLoad_DefaultMode_BroadenedParent_Refuses(t *testing.T) {
	dir := broadenedParentForGateRejection(t)
	path := writeOverlayFileDirect(t, dir, "version: 1\ndaemons: {}\n")

	t.Setenv("MCPHUB_REQUIRE_SINGLE_USER_HOME", "")
	t.Setenv(AllowUnhardenedStateReadEnv, "")

	_, err := Load(path)
	if err == nil {
		t.Fatalf("Load(broadened parent, default mode): expected refusal, got nil error")
	}
	if !strings.Contains(strings.ToLower(err.Error()), "parent gate") {
		t.Errorf("Load default-refuse error %q does not look like the read-side parent-gate error", err)
	}
}

// TestLoad_RelaxOptOut_BroadenedParent_Succeeds proves the explicit
// relax opt-out (MCPHUB_ALLOW_UNHARDENED_STATE_READ=1, strict NOT set)
// makes Load succeed on a broadened parent — the operator has accepted
// the broadening. Guards against the Sec-F1 fix over-blocking the
// legitimate corp-host-with-opt-out path.
func TestLoad_RelaxOptOut_BroadenedParent_Succeeds(t *testing.T) {
	dir := broadenedParentForGateRejection(t)
	body := "version: 1\ndaemons:\n  \"\\\\mcp-local-hub-memory-default\":\n    env:\n      FOO: bar\n"
	path := writeOverlayFileDirect(t, dir, body)

	t.Setenv("MCPHUB_REQUIRE_SINGLE_USER_HOME", "")
	t.Setenv(AllowUnhardenedStateReadEnv, "1")

	ov, err := Load(path)
	if err != nil {
		t.Fatalf("Load(broadened parent, relax opt-out): unexpected error: %v", err)
	}
	if ov == nil || len(ov.Daemons) != 1 {
		t.Fatalf("Load(relax): want 1 daemon row, got %+v", ov)
	}
}

// TestLoad_StrictMode_OverridesRelax_BroadenedParent_Refuses proves the
// strict-over-relax precedence: when BOTH MCPHUB_REQUIRE_SINGLE_USER_HOME=1
// and MCPHUB_ALLOW_UNHARDENED_STATE_READ=1 are set, the strict gate wins
// and Load still refuses. This mirrors the gate's documented precedence
// (parent_check.go) and prevents a per-operator relax env var from
// silently downgrading a corp-mandated strict posture.
func TestLoad_StrictMode_OverridesRelax_BroadenedParent_Refuses(t *testing.T) {
	dir := broadenedParentForGateRejection(t)
	path := writeOverlayFileDirect(t, dir, "version: 1\ndaemons: {}\n")

	t.Setenv("MCPHUB_REQUIRE_SINGLE_USER_HOME", "1")
	t.Setenv(AllowUnhardenedStateReadEnv, "1") // ignored under strict

	_, err := Load(path)
	if err == nil {
		t.Fatalf("Load(strict+relax both set): expected strict refusal, got nil error")
	}
	if !strings.Contains(strings.ToLower(err.Error()), "single-user") {
		t.Errorf("Load strict-over-relax error %q does not mention the strict single-user gate", err)
	}
}

// TestLoad_DefaultMode_SafeParent_Succeeds is the regression guard that
// the Sec-F1 gate does NOT break the legitimate solo-host path: an
// owner-only parent (apitest.HardenedTempDir applies a PROTECTED
// single-user DACL on Windows / 0700 on POSIX) passes the gate, so Load
// succeeds with neither env var set.
func TestLoad_DefaultMode_SafeParent_Succeeds(t *testing.T) {
	dir := apitest.HardenedTempDir(t)

	// Sanity: the hardened parent must pass the write-side gate;
	// otherwise the helper regressed and the assertion below is vacuous.
	if err := api.CheckStateDirParentWriteSafe(dir); err != nil {
		t.Fatalf("apitest.HardenedTempDir produced a gate-rejecting parent (helper regression?): %v", err)
	}

	path := writeOverlayFileDirect(t, dir, "version: 1\ndaemons: {}\n")

	t.Setenv("MCPHUB_REQUIRE_SINGLE_USER_HOME", "")
	t.Setenv(AllowUnhardenedStateReadEnv, "")

	ov, err := Load(path)
	if err != nil {
		t.Fatalf("Load(safe parent, default mode): unexpected error: %v", err)
	}
	if ov == nil || ov.Daemons == nil {
		t.Fatalf("Load(safe parent): want non-nil overlay with non-nil Daemons, got %+v", ov)
	}
}

// TestLoad_TransientRetry_ErrorClassification pins the Conc-F6 retry
// decision logic that Load relies on: a transientReadError satisfies
// errors.Is(err, errTransientOverlayRead) (so Load retries), and
// underlyingReadError strips the tag (so the surfaced error after retry
// is the clean open/stat error). A non-transient error matches neither.
func TestLoad_TransientRetry_ErrorClassification(t *testing.T) {
	inner := fmt.Errorf("some/path: open: sharing violation")
	tr := &transientReadError{inner: inner}

	if !errors.Is(tr, errTransientOverlayRead) {
		t.Errorf("transientReadError should match errTransientOverlayRead via Is()")
	}
	if got := underlyingReadError(tr); got != inner {
		t.Errorf("underlyingReadError(transient) = %v, want the inner error %v", got, inner)
	}

	plain := fmt.Errorf("some/path: decode: bad yaml")
	if errors.Is(plain, errTransientOverlayRead) {
		t.Errorf("plain error should not match errTransientOverlayRead")
	}
	if got := underlyingReadError(plain); got != plain {
		t.Errorf("underlyingReadError(plain) = %v, want unchanged %v", got, plain)
	}
}

// TestLoad_ConcurrentWithWriteOverlay_NeverPartial proves Conc-F6's
// core property: a Load racing a WriteOverlay observes either the
// COMPLETE old or COMPLETE new overlay, never a torn/partial parse. The
// writer publishes via an atomic rename, so no reader can ever see a
// half-written file. Run with -race.
//
// Each WriteOverlay flips the row's FOO value between two known complete
// states; each concurrent Load must return one of those two states (or a
// clean non-decode error — never a YAML decode / UTF-8 error, which
// would indicate a partial read).
func TestLoad_ConcurrentWithWriteOverlay_NeverPartial(t *testing.T) {
	dir := apitest.HardenedTempDir(t)
	if err := api.CheckStateDirParentWriteSafe(dir); err != nil {
		t.Fatalf("apitest.HardenedTempDir produced a gate-rejecting parent (helper regression?): %v", err)
	}
	path := filepath.Join(dir, "daemon-env-overrides.yaml")

	t.Setenv("MCPHUB_REQUIRE_SINGLE_USER_HOME", "")
	t.Setenv(AllowUnhardenedStateReadEnv, "")

	const key = "\\mcp-local-hub-memory-default"

	// Seed an initial valid overlay so the first Loads see a complete file.
	if err := WriteOverlay(path, func(ov *Overlay) error {
		ov.Daemons[key] = DaemonRow{Env: map[string]string{"FOO": "stateA"}}
		return nil
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}

	const iters = 200
	var stop atomic.Bool
	var wg sync.WaitGroup

	// Writer goroutine: churn the FOO value between two complete states.
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < iters && !stop.Load(); i++ {
			want := "stateA"
			if i%2 == 1 {
				want = "stateB"
			}
			_ = WriteOverlay(path, func(ov *Overlay) error {
				row := ov.Daemons[key]
				if row.Env == nil {
					row.Env = map[string]string{}
				}
				row.Env["FOO"] = want
				ov.Daemons[key] = row
				return nil
			})
		}
	}()

	// Reader goroutines: hammer Load and assert every successful read is
	// a COMPLETE overlay (FOO is one of the two known states), never a
	// partial/garbled parse.
	readerErr := make(chan error, 4)
	for r := 0; r < 4; r++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < iters && !stop.Load(); i++ {
				ov, err := Load(path)
				if err != nil {
					// A decode / UTF-8 error would mean a torn/partial read
					// slipped through — exactly what Conc-F6 must prevent.
					low := strings.ToLower(err.Error())
					if strings.Contains(low, "decode") || strings.Contains(low, "utf-8") {
						readerErr <- fmt.Errorf("Load saw a partial/garbled file: %w", err)
						stop.Store(true)
						return
					}
					// Any other transient error after the retry is tolerated
					// (the writer may be mid-rename on a slow host); the
					// contract is "never partial", not "never errors".
					continue
				}
				row, ok := ov.Daemons[key]
				if !ok {
					// The seed row exists in both states; a missing row would
					// indicate a structurally wrong (partial) read.
					readerErr <- fmt.Errorf("Load returned overlay missing seed row: %+v", ov.Daemons)
					stop.Store(true)
					return
				}
				if v := row.Env["FOO"]; v != "stateA" && v != "stateB" {
					readerErr <- fmt.Errorf("Load returned FOO=%q, want a complete stateA/stateB value", v)
					stop.Store(true)
					return
				}
			}
		}()
	}

	wg.Wait()
	close(readerErr)
	for err := range readerErr {
		t.Error(err)
	}
}

// TestLoad_TransientNotFound_RetriesThenReadsPresentFile pins the
// Conc-F6 refinement (Codex bot #268 overlay.go:170-171/173 P2): a
// FIRST fs.ErrNotExist — the momentary not-found the Windows
// atomic-replace window can surface — is retried, and the retry reads
// the COMPLETE present file rather than degrading to emptyOverlay().
//
// Determinism on every OS comes from the openOverlayFile seam: the first
// open returns fs.ErrNotExist (simulating the in-kernel replace window);
// every later open delegates to the real hardenedOpen, which finds the
// seeded file. Without the fix, loadOnce short-circuited the first
// fs.ErrNotExist straight to emptyOverlay() and the present override was
// silently dropped.
func TestLoad_TransientNotFound_RetriesThenReadsPresentFile(t *testing.T) {
	dir := apitest.HardenedTempDir(t)
	if err := api.CheckStateDirParentWriteSafe(dir); err != nil {
		t.Fatalf("apitest.HardenedTempDir produced a gate-rejecting parent (helper regression?): %v", err)
	}
	path := filepath.Join(dir, "daemon-env-overrides.yaml")
	t.Setenv("MCPHUB_REQUIRE_SINGLE_USER_HOME", "")
	t.Setenv(AllowUnhardenedStateReadEnv, "")

	const key = "\\mcp-local-hub-memory-default"
	if err := WriteOverlay(path, func(ov *Overlay) error {
		ov.Daemons[key] = DaemonRow{Env: map[string]string{"FOO": "present"}}
		return nil
	}); err != nil {
		t.Fatalf("seed overlay: %v", err)
	}

	// Inject: first open → ErrNotExist (replace window); subsequent opens
	// → real hardenedOpen (present file). Restore on cleanup.
	var calls atomic.Int32
	orig := openOverlayFile
	t.Cleanup(func() { openOverlayFile = orig })
	openOverlayFile = func(p string) (*os.File, error) {
		if calls.Add(1) == 1 {
			return nil, &fs.PathError{Op: "open", Path: p, Err: fs.ErrNotExist}
		}
		return orig(p)
	}

	ov, err := Load(path)
	if err != nil {
		t.Fatalf("Load after transient not-found: unexpected error: %v", err)
	}
	if ov == nil {
		t.Fatalf("Load returned nil overlay")
	}
	// The retry must have read the COMPLETE present file, NOT emptyOverlay().
	row, ok := ov.Daemons[key]
	if !ok {
		t.Fatalf("Load degraded to emptyOverlay() on a transient not-found; overlay=%+v (the present override was dropped)", ov.Daemons)
	}
	if row.Env["FOO"] != "present" {
		t.Fatalf("Load FOO = %q, want the present override %q", row.Env["FOO"], "present")
	}
	if got := calls.Load(); got != 2 {
		t.Fatalf("openOverlayFile call count = %d, want exactly 2 (one failed open + one retry)", got)
	}
}

// TestLoad_PersistentNotFound_ReturnsEmptyOverlayAfterRetry proves the
// other half of the classification: a file that is STILL absent after the
// single retry is the legitimate "no overlay file" case and returns
// emptyOverlay() + nil — the pre-retry missing-file contract is
// preserved. The seam forces fs.ErrNotExist on EVERY open so the retry
// also misses; Load must NOT surface an error and must return the empty
// overlay. It must also retry exactly once (two open attempts, not an
// unbounded loop).
func TestLoad_PersistentNotFound_ReturnsEmptyOverlayAfterRetry(t *testing.T) {
	dir := apitest.HardenedTempDir(t)
	path := filepath.Join(dir, "daemon-env-overrides.yaml")

	var calls atomic.Int32
	orig := openOverlayFile
	t.Cleanup(func() { openOverlayFile = orig })
	openOverlayFile = func(p string) (*os.File, error) {
		calls.Add(1)
		return nil, &fs.PathError{Op: "open", Path: p, Err: fs.ErrNotExist}
	}

	ov, err := Load(path)
	if err != nil {
		t.Fatalf("Load on persistently-absent file: want nil error (empty-overlay contract), got %v", err)
	}
	if ov == nil || ov.Version != 1 || ov.Daemons == nil {
		t.Fatalf("Load on absent file: want emptyOverlay() {Version:1, Daemons:{}}, got %+v", ov)
	}
	if len(ov.Daemons) != 0 {
		t.Fatalf("Load on absent file: want 0 daemons, got %d", len(ov.Daemons))
	}
	// Exactly one retry: two open attempts total, never an infinite loop.
	if got := calls.Load(); got != 2 {
		t.Fatalf("openOverlayFile call count = %d, want exactly 2 (initial + one retry)", got)
	}
}

// TestLoad_PersistentNotFound_RealMissingFile is the seam-free companion:
// a genuinely-missing real file (no injection) returns emptyOverlay() +
// nil through the real hardenedOpen, proving the production missing-file
// path still behaves as the contract promises after the retry rework.
func TestLoad_PersistentNotFound_RealMissingFile(t *testing.T) {
	dir := apitest.HardenedTempDir(t)
	path := filepath.Join(dir, "this-overlay-does-not-exist.yaml")

	ov, err := Load(path)
	if err != nil {
		t.Fatalf("Load on real missing file: want nil error, got %v", err)
	}
	if ov == nil || len(ov.Daemons) != 0 {
		t.Fatalf("Load on real missing file: want emptyOverlay(), got %+v", ov)
	}
}

// TestLoad_TransientNotFound_NeverEmptyWhilePresent_Race is the
// concurrent property test: a Load racing a churning WriteOverlay across
// the rename window must NEVER return emptyOverlay() while the file
// actually exists with content. The file is seeded once and only ever
// mutated (never removed), so the row is present in EVERY committed
// state; any empty/missing-row read would mean a transient open miss
// degraded to emptyOverlay() instead of retrying. On Windows the
// atomic-replace window can surface that miss; on POSIX rename(2) never
// does, so the assertion is simply always-satisfied there. Run with
// -race.
func TestLoad_TransientNotFound_NeverEmptyWhilePresent_Race(t *testing.T) {
	dir := apitest.HardenedTempDir(t)
	if err := api.CheckStateDirParentWriteSafe(dir); err != nil {
		t.Fatalf("apitest.HardenedTempDir produced a gate-rejecting parent (helper regression?): %v", err)
	}
	path := filepath.Join(dir, "daemon-env-overrides.yaml")
	t.Setenv("MCPHUB_REQUIRE_SINGLE_USER_HOME", "")
	t.Setenv(AllowUnhardenedStateReadEnv, "")

	const key = "\\mcp-local-hub-memory-default"
	if err := WriteOverlay(path, func(ov *Overlay) error {
		ov.Daemons[key] = DaemonRow{Env: map[string]string{"FOO": "stateA"}}
		return nil
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}

	const iters = 300
	var stop atomic.Bool
	var wg sync.WaitGroup

	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < iters && !stop.Load(); i++ {
			want := "stateA"
			if i%2 == 1 {
				want = "stateB"
			}
			_ = WriteOverlay(path, func(ov *Overlay) error {
				row := ov.Daemons[key]
				if row.Env == nil {
					row.Env = map[string]string{}
				}
				row.Env["FOO"] = want
				ov.Daemons[key] = row
				return nil
			})
		}
	}()

	readerErr := make(chan error, 4)
	for r := 0; r < 4; r++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < iters && !stop.Load(); i++ {
				ov, err := Load(path)
				if err != nil {
					// A non-nil error after the retry is tolerated (the
					// writer may be mid-rename on a slow host); the property
					// under test is "never silently empty while present".
					continue
				}
				if _, ok := ov.Daemons[key]; !ok {
					readerErr <- fmt.Errorf("Load returned emptyOverlay()/missing-row while the file exists with content: %+v", ov.Daemons)
					stop.Store(true)
					return
				}
			}
		}()
	}

	wg.Wait()
	close(readerErr)
	for err := range readerErr {
		t.Error(err)
	}
}

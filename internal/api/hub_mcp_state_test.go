// hub_mcp_state_test.go — Phase 2 Task 2.2 (G4 unified hub MCP).
//
// Atomic write + load helpers for the hub-mcp state files
// (hub-mcp.endpoint.json, hub-mcp-tokens.json, hub-mcp-control.token,
// hub-mcp.log). The write path delegates to SecureWriteClientConfig
// (Phase 1 Task 1.3/1.4) so callers inherit the handle-relative,
// DACL-verified pipeline. The read path delegates to the inode-anchored
// reader before any bytes are returned.
//
// Tests use the daemonStateRootOverride seam (state_paths.go) + the
// hardenedTempDir test fixture (hardened_tempdir_*_test.go) so the
// state dir's DACL / mode passes the load-time gate.
//
// Spec: docs/superpowers/specs/2026-05-12-g4-unified-hub-mcp-design-v3.md
// §"Token + endpoint state hardening" (atomic-write + load-time
// validation blocks).
// Plan: docs/superpowers/plans/2026-05-12-g4-unified-hub-mcp.md Task 2.2.

package api

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gofrs/flock"
)

// hubMcpStateTestHelper installs the state-dir override pointing at a
// freshly hardened temp dir (DACL allowlist-conforming on Windows,
// 0700 on POSIX). The caller then writes/reads hub-mcp.* files at the
// returned absolute path. statePathsHelper(t) restores the override on
// cleanup.
func hubMcpStateTestHelper(t *testing.T) string {
	t.Helper()
	statePathsHelper(t)
	root := hardenedTempDir(t)
	// Restore daemonStateRootOverride after this test exits so
	// subsequent tests in the same `go test` invocation (e.g.
	// Register / WriteDaemonIntent paths in register_test.go) don't
	// observe a leaked override pointing at this test's already-
	// deleted temp dir, which can produce huge stale on-disk state
	// or hangs when those tests reuse the path.
	prevOverride := daemonStateRootOverride
	t.Cleanup(func() { daemonStateRootOverride = prevOverride })
	daemonStateRootOverride = root
	// Sanity: DaemonStateDir should now return the hardened root
	// directly (the override path bypasses the platform resolver, so
	// the hardened DACL/mode on `root` is what every state-file
	// open will inspect).
	got, err := DaemonStateDir()
	if err != nil {
		t.Fatalf("DaemonStateDir after override: %v", err)
	}
	if got != root {
		t.Fatalf("override mismatch: got %q, want %q", got, root)
	}
	return root
}

// TestWriteHubMcpStateAtomicRoundTrip pins the write -> read roundtrip
// contract: writeHubMcpStateFile(name, payload) followed by
// readHubMcpStateFile(name) returns the exact bytes. Verifies the
// write hit the expected on-disk location via filepath.Join.
func TestWriteHubMcpStateAtomicRoundTrip(t *testing.T) {
	dir := hubMcpStateTestHelper(t)

	payload := []byte(`{"foo":"bar"}`)
	if err := writeHubMcpStateFile("hub-mcp.endpoint.json", payload); err != nil {
		t.Fatalf("writeHubMcpStateFile: %v", err)
	}

	target := filepath.Join(dir, "hub-mcp.endpoint.json")
	gotDisk, err := os.ReadFile(target)
	if err != nil {
		t.Fatalf("read back from disk: %v", err)
	}
	if !bytes.Equal(gotDisk, payload) {
		t.Errorf("on-disk content = %q, want %q", gotDisk, payload)
	}

	gotHelper, err := readHubMcpStateFile("hub-mcp.endpoint.json")
	if err != nil {
		t.Fatalf("readHubMcpStateFile: %v", err)
	}
	if !bytes.Equal(gotHelper, payload) {
		t.Errorf("readHubMcpStateFile = %q, want %q", gotHelper, payload)
	}
}

// TestReadHubMcpStateRejectsSymlink asserts the load-time gate refuses
// to read a state-file whose target is a symlink. The inode-anchored
// reader's reparse-defeat flags are the relevant defense.
//
// Symlink creation on Windows requires SeCreateSymbolicLinkPrivilege;
// the test skips when the OS denies the call.
func TestReadHubMcpStateRejectsSymlink(t *testing.T) {
	dir := hubMcpStateTestHelper(t)

	target := filepath.Join(dir, "hub-mcp.endpoint.json")
	real := filepath.Join(dir, "real.json")
	if err := os.WriteFile(real, []byte("{}"), 0o600); err != nil {
		t.Fatalf("seed real: %v", err)
	}
	if err := os.Symlink(real, target); err != nil {
		t.Skipf("symlink unsupported on this host: %v", err)
	}

	_, err := readHubMcpStateFile("hub-mcp.endpoint.json")
	if err == nil {
		t.Fatalf("readHubMcpStateFile must reject symlink target")
	}
	// The inode-anchored reader surfaces ErrIrregularFile (POSIX leg) or
	// a reparse-point error (Windows leg). Either is acceptable here —
	// the contract is "non-nil err".
	_ = errors.Is(err, ErrIrregularFile)
}

// TestWriteHubMcpStateRejectsBadName covers the validateStateFileName
// boundary: paths containing separators or traversal segments are
// rejected before any disk I/O. Defense-in-depth — production callers
// pass hardcoded leaves, but a future caller must not be able to
// escape DaemonStateDir() by composing the name.
func TestWriteHubMcpStateRejectsBadName(t *testing.T) {
	hubMcpStateTestHelper(t)

	for _, bad := range []string{
		"../escape.json",
		"hub-mcp/../escape",
		"hub-mcp.lock/../etc",
		"",
		".",
		"..",
	} {
		t.Run(bad, func(t *testing.T) {
			err := writeHubMcpStateFile(bad, []byte("{}"))
			if err == nil {
				t.Fatalf("writeHubMcpStateFile(%q) must reject; returned nil", bad)
			}
			if !errors.Is(err, errStateNameInvalid) {
				t.Fatalf("writeHubMcpStateFile(%q) err = %v, want errStateNameInvalid", bad, err)
			}
			rerr := func() error {
				_, e := readHubMcpStateFile(bad)
				return e
			}()
			if rerr == nil {
				t.Fatalf("readHubMcpStateFile(%q) must reject; returned nil", bad)
			}
			if !errors.Is(rerr, errStateNameInvalid) {
				t.Fatalf("readHubMcpStateFile(%q) err = %v, want errStateNameInvalid", bad, rerr)
			}
		})
	}
}

func TestWriteHubMcpStateFile_HonorsPersistedStrictModeWhenParentInsecure(t *testing.T) {
	statePathsHelper(t)
	t.Setenv(RequireSingleUserHomeEnv, "")
	t.Cleanup(resetStrictModeIntentCacheForTest)

	stateDir := filepath.Join(t.TempDir(), "leaky-state")
	if err := os.Mkdir(stateDir, 0o700); err != nil {
		t.Fatalf("mkdir state dir: %v", err)
	}
	intentPath := filepath.Join(stateDir, supervisorIntentFileLeaf)
	if err := os.WriteFile(intentPath, []byte(`{"version":1,"strict_mode":true}`), 0o600); err != nil {
		t.Fatalf("seed persisted strict intent: %v", err)
	}
	broadenParentForStateFileTest(t, stateDir)
	daemonStateRootOverride = stateDir
	resetStrictModeIntentCacheForTest()
	if !OperatorRequiresSingleUserHome() {
		t.Fatal("precondition: persisted strict_mode=true with env unset must enable the strict gate")
	}

	err := writeHubMcpStateFile(hubMcpEndpointFileLeaf, []byte(`{"port":9125}`))
	if err == nil {
		t.Fatalf("persisted strict_mode=true must reject hub-mcp write under permissive state dir even when %s is unset", RequireSingleUserHomeEnv)
	}
	if !strings.Contains(err.Error(), "persisted supervisor-intent.json") {
		t.Errorf("error must mention persisted strict-mode intent; got %v", err)
	}
	if _, statErr := os.Stat(filepath.Join(stateDir, hubMcpEndpointFileLeaf)); !os.IsNotExist(statErr) {
		t.Errorf("persisted strict-mode rejection leaked hub-mcp state file (stat err = %v)", statErr)
	}
}

// TestAcquireHubMcpLockSerializes pins the flock contract: a held
// hub-mcp.lock is observable. We use TryLock on a second flock handle
// to detect contention without blocking the test.
func TestAcquireHubMcpLockSerializes(t *testing.T) {
	hubMcpStateTestHelper(t)

	lk1, err := acquireHubMcpLock()
	if err != nil {
		t.Fatalf("acquireHubMcpLock #1: %v", err)
	}
	defer func() { _ = lk1.Unlock() }()

	// A second acquire from a fresh flock handle (NOT just calling
	// acquireHubMcpLock again — gofrs/flock is reentrant per-process
	// when the same *Flock is shared, but TWO distinct *Flock objects
	// on the same path serialize). Use TryLock to avoid a deadlock
	// when the contract is satisfied.
	dir, err := DaemonStateDir()
	if err != nil {
		t.Fatalf("DaemonStateDir: %v", err)
	}
	probe := flock.New(filepath.Join(dir, hubMcpLockFileLeaf))
	defer func() { _ = probe.Unlock() }()

	locked, err := probe.TryLock()
	if err != nil {
		t.Fatalf("probe.TryLock: %v", err)
	}
	if locked {
		t.Errorf("probe acquired the lock while it should be held by lk1")
	}
}

// TestAcquireHubMcpLockContextRespectsCancellation pins codex bot
// phase4 r10 P1 closure on PR #158: the ctx-aware variant must return
// promptly with a wrapped ctx error when the lock is held by an
// out-of-process holder AND ctx is canceled or times out.
//
// Without this contract, gui-server startup / shutdown can stall
// indefinitely if a sibling CLI (mcphub hub-mcp regenerate-token /
// install reconciler) is holding hub-mcp.lock — the bot's stated
// concern that shutdown budgets become unenforceable.
//
// Pattern adapted from internal/api/daemon_intent_test.go where the
// same TryLockContext shape is pinned for the tray aggregator. Uses a
// raw *flock.Flock as the "out-of-process" holder so the test doesn't
// depend on a real sibling process — gofrs/flock LockFileEx on
// Windows / flock(2) on POSIX serializes ACROSS *Flock objects on the
// same path, so two distinct handles emulate the sibling-process race.
func TestAcquireHubMcpLockContextRespectsCancellation(t *testing.T) {
	hubMcpStateTestHelper(t)
	dir, err := DaemonStateDir()
	if err != nil {
		t.Fatalf("DaemonStateDir: %v", err)
	}

	// Holder: a separate *flock.Flock keeps hub-mcp.lock held for the
	// duration of the test. Released via t.Cleanup so failed
	// assertions don't leak the lock to other tests.
	holder := flock.New(filepath.Join(dir, hubMcpLockFileLeaf))
	if err := holder.Lock(); err != nil {
		t.Fatalf("holder.Lock: %v", err)
	}
	t.Cleanup(func() { _ = holder.Unlock() })

	// Probe the deadline-exceeded path: ctx with a tight timeout MUST
	// return within a small grace window. The 10 ms TryLockContext
	// retry cadence means observed cancellation latency is < ~20 ms
	// for a 100 ms budget.
	t.Run("deadline-exceeded", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
		defer cancel()
		start := time.Now()
		_, err := acquireHubMcpLockContext(ctx)
		elapsed := time.Since(start)
		if err == nil {
			t.Fatalf("acquireHubMcpLockContext succeeded under contention; want ctx error")
		}
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Errorf("err chain missing context.DeadlineExceeded: %v", err)
		}
		// Allow generous slack for noisy CI: budget + 5x retry cadence.
		if elapsed > 250*time.Millisecond {
			t.Errorf("acquireHubMcpLockContext returned after %s; want < 250ms", elapsed)
		}
	})

	// Probe the explicit-cancel path: a canceled ctx MUST short-circuit
	// at the head of acquireHubMcpLockContext before TryLockContext
	// even runs.
	t.Run("canceled-before-call", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		_, err := acquireHubMcpLockContext(ctx)
		if err == nil {
			t.Fatalf("acquireHubMcpLockContext succeeded with pre-canceled ctx")
		}
		if !errors.Is(err, context.Canceled) {
			t.Errorf("err chain missing context.Canceled: %v", err)
		}
	})
}

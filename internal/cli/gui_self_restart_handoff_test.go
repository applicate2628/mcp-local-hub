package cli

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"mcp-local-hub/internal/gui"
)

// TestSelfRestartHandoffEnvMatchesGUI pins the CLI-local handoff env const
// equal to the gui package's canonical name so the parent (gui handler)
// and the child (this CLI startup path) never drift on the signal.
func TestSelfRestartHandoffEnvMatchesGUI(t *testing.T) {
	if selfRestartHandoffEnv != gui.SelfRestartHandoffEnv {
		t.Fatalf("env const drift: cli=%q gui=%q", selfRestartHandoffEnv, gui.SelfRestartHandoffEnv)
	}
}

// TestAcquireHandoff_NoEnvSingleShot: without the handoff env, a busy lock
// returns ErrSingleInstanceBusy IMMEDIATELY (no retry) — identical to the
// prior direct AcquireSingleInstanceAt call. We hold the lock for the
// whole test, so a retrying acquire would block; a single-shot returns at
// once.
func TestAcquireHandoff_NoEnvSingleShot(t *testing.T) {
	t.Setenv(selfRestartHandoffEnv, "") // ensure not set
	pidport := filepath.Join(t.TempDir(), "gui.pidport")

	held, err := gui.AcquireSingleInstanceAt(pidport, 1)
	if err != nil {
		t.Fatalf("seed lock: %v", err)
	}
	defer held.Release()

	start := time.Now()
	lock, err := acquireSingleInstanceWithHandoff(context.Background(), pidport, 1)
	elapsed := time.Since(start)
	if lock != nil {
		lock.Release()
		t.Fatalf("expected busy, got a lock")
	}
	if !errors.Is(err, gui.ErrSingleInstanceBusy) {
		t.Fatalf("err = %v, want ErrSingleInstanceBusy", err)
	}
	// Single-shot must be near-instant; a generous ceiling rules out an
	// accidental retry loop.
	if elapsed > 2*time.Second {
		t.Fatalf("single-shot acquire took %v, want near-instant (retry loop leaked?)", elapsed)
	}
}

// TestAcquireHandoff_EnvRetriesUntilRelease: with the handoff env set, a
// busy lock is RETRIED; once the holder releases (mid-flight, simulating
// the outgoing parent's exit) the child acquires it. Uses a short test
// deadline so the test is fast.
func TestAcquireHandoff_EnvRetriesUntilRelease(t *testing.T) {
	t.Setenv(selfRestartHandoffEnv, "1")

	// Shrink the handoff window for the test, restore after.
	origDeadline := selfRestartHandoffAcquireDeadline
	origBackoff := selfRestartHandoffAcquireBackoff
	selfRestartHandoffAcquireDeadline = 3 * time.Second
	selfRestartHandoffAcquireBackoff = 20 * time.Millisecond
	t.Cleanup(func() {
		selfRestartHandoffAcquireDeadline = origDeadline
		selfRestartHandoffAcquireBackoff = origBackoff
	})

	pidport := filepath.Join(t.TempDir(), "gui.pidport")
	held, err := gui.AcquireSingleInstanceAt(pidport, 1)
	if err != nil {
		t.Fatalf("seed lock: %v", err)
	}

	// Release the held lock shortly after the acquire starts polling,
	// simulating the parent exiting and the OS releasing the flock.
	go func() {
		time.Sleep(150 * time.Millisecond)
		held.Release()
	}()

	lock, err := acquireSingleInstanceWithHandoff(context.Background(), pidport, 1)
	if err != nil {
		t.Fatalf("handoff acquire = %v, want success after release", err)
	}
	if lock == nil {
		t.Fatalf("nil lock on success")
	}
	lock.Release()
}

// TestAcquireHandoff_EnvDeadlineExpires: with the handoff env set but the
// holder NEVER releasing, the retry loop gives up at the deadline and
// returns the busy error so the normal handshake/--force flow still runs
// for a genuinely-occupied lock.
func TestAcquireHandoff_EnvDeadlineExpires(t *testing.T) {
	t.Setenv(selfRestartHandoffEnv, "1")

	origDeadline := selfRestartHandoffAcquireDeadline
	origBackoff := selfRestartHandoffAcquireBackoff
	selfRestartHandoffAcquireDeadline = 200 * time.Millisecond
	selfRestartHandoffAcquireBackoff = 20 * time.Millisecond
	t.Cleanup(func() {
		selfRestartHandoffAcquireDeadline = origDeadline
		selfRestartHandoffAcquireBackoff = origBackoff
	})

	pidport := filepath.Join(t.TempDir(), "gui.pidport")
	held, err := gui.AcquireSingleInstanceAt(pidport, 1)
	if err != nil {
		t.Fatalf("seed lock: %v", err)
	}
	defer held.Release()

	lock, err := acquireSingleInstanceWithHandoff(context.Background(), pidport, 1)
	if lock != nil {
		lock.Release()
		t.Fatalf("expected deadline-expiry busy, got a lock")
	}
	if !errors.Is(err, gui.ErrSingleInstanceBusy) {
		t.Fatalf("err = %v, want ErrSingleInstanceBusy", err)
	}
}

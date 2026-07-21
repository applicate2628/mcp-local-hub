package api

import (
	"context"
	"errors"
	"os/exec"
	"runtime"
	"testing"
	"time"
)

// blockingProbeCommand returns a command that provably blocks for ~60s while
// starting almost instantly.
//
// The 60s block against a sub-second deadline is a ~100x gap that is
// ENGINEERED, not hoped for: the test must never depend on a real probe
// happening to be slow on the day it runs. `ping -n 60` is preferred over
// `powershell -Command "Start-Sleep"` because PowerShell's own startup cost was
// measured at 10.5s on a loaded reference host, which would make the test both
// slow and timing-sensitive.
func blockingProbeCommand(t *testing.T) (string, []string) {
	t.Helper()
	name, args := "sleep", []string{"60"}
	if runtime.GOOS == "windows" {
		name, args = "ping", []string{"-n", "60", "127.0.0.1"}
	}
	if _, err := exec.LookPath(name); err != nil {
		t.Skipf("blocking helper %q unavailable on this host: %v", name, err)
	}
	return name, args
}

// TestRunProbeCommand_BoundsChildThatNeverReturns is the regression guard for
// the readiness hang: before the bounded runner, every OS-fact probe ran through
// a bare exec.Command + cmd.Output(), which on Windows is
// WaitForSingleObject(handle, INFINITE). A child that never exits blocked the
// caller forever — captured in a goroutine dump with the GUI readiness handler
// sitting underneath it.
func TestRunProbeCommand_BoundsChildThatNeverReturns(t *testing.T) {
	name, args := blockingProbeCommand(t)

	const timeout = 500 * time.Millisecond
	start := time.Now()
	out, err := runProbeCommand(timeout, name, args...)
	elapsed := time.Since(start)

	if err == nil {
		t.Fatalf("a 60s-blocking child returned success (%d bytes of output): the deadline never fired", len(out))
	}
	if !errors.Is(err, ErrProbeTimeout) {
		t.Fatalf("err = %v, want it to wrap ErrProbeTimeout so callers can tell "+
			"'did not answer in time' from 'answered with a failure'", err)
	}
	// Generous ceiling: the point is bounded-vs-unbounded, not precision. If the
	// call is NOT bounded this sits at ~60s (or forever) and blows past it.
	if ceiling := timeout + probeWaitDelay + 10*time.Second; elapsed > ceiling {
		t.Fatalf("probe returned after %s, want < %s — the call is not bounded", elapsed, ceiling)
	}
}

// TestRunProbeCommandCtx_ExhaustedChainBudgetDoesNotSpawn pins the fallback-chain
// contract: a wmic→PowerShell chain shares ONE budget, so once it is spent the
// fallback must not start a fresh full-price attempt. Without this, a slow wmic
// buys a second slow PowerShell run and the chain's worst case doubles.
func TestRunProbeCommandCtx_ExhaustedChainBudgetDoesNotSpawn(t *testing.T) {
	name, args := blockingProbeCommand(t)

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // deterministically spent before the call — no sleep, no race window

	start := time.Now()
	_, err := runProbeCommandCtx(ctx, name, args...)
	elapsed := time.Since(start)

	if !errors.Is(err, ErrProbeTimeout) {
		t.Fatalf("err = %v, want ErrProbeTimeout for an already-spent chain budget", err)
	}
	// A spawned-and-killed child would still cost process creation plus
	// WaitDelay; returning this fast proves no child was started at all.
	if elapsed > 2*time.Second {
		t.Fatalf("took %s on an already-spent budget — the fallback spawned a child it could not finish", elapsed)
	}
}

// TestRunProbeCommand_RealCommandStillSucceeds guards against the bound being so
// aggressive (or the plumbing so broken) that healthy probes stop answering.
func TestRunProbeCommand_RealCommandStillSucceeds(t *testing.T) {
	name, args := "echo", []string{"probe-ok"}
	if runtime.GOOS == "windows" {
		name, args = "cmd", []string{"/c", "echo probe-ok"}
	}
	if _, err := exec.LookPath(name); err != nil {
		t.Skipf("helper %q unavailable: %v", name, err)
	}
	out, err := runProbeCommand(probeCommandTimeout, name, args...)
	if err != nil {
		t.Fatalf("healthy probe failed: %v", err)
	}
	if len(out) == 0 {
		t.Fatal("healthy probe returned no output")
	}
}

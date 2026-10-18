//go:build windows

package cli

import (
	"context"
	"errors"
	"io"
	"os"
	"reflect"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"mcp-local-hub/internal/api"

	"golang.org/x/sys/windows"
)

// Root must supply a qualified controlled child and canonical state namespace.
// Keeping operands explicit prevents ordinary unit suites from starting an
// installed supervisor or guessing an isolated Windows state-root override.
func runGUICreatorFailedCaptureReceivingFixture(t *testing.T, parent context.Context, controlledBinary string, waitFor time.Duration) {
	t.Helper()
	if parent == nil || controlledBinary == "" {
		t.Fatal("qualified creator fixture context/binary required")
	}
	ctx, cancel := context.WithCancel(parent)
	entered, release := make(chan struct{}), make(chan struct{})
	var releaseOnce sync.Once
	unblock := func() { releaseOnce.Do(func() { close(release) }) }
	var mu sync.Mutex
	var observed supervisorCreationSnapshot
	var original *supervisorOwner
	var generation api.SupervisorLockOwner
	var realPeerCaptured bool
	var settlementCalls atomic.Int32
	want := errors.New("fixture forced peer capture failure after real native capture")
	type creatorResult struct {
		owner *supervisorOwner
		err   error
	}
	finished := make(chan creatorResult, 1)
	var result creatorResult
	received := false
	join := func() {
		if !received {
			result = <-finished
			received = true
		}
	}
	// Register original gate/creator receiving before admitting its Start.
	t.Cleanup(func() { unblock(); cancel(); join() })
	go func() {
		owner, err := ensureSupervisorRunningWithOperations(ctx, controlledBinary, false, waitFor, func(snapshot supervisorCreationSnapshot) error {
			mu.Lock()
			observed = snapshot
			mu.Unlock()
			return nil
		}, supervisorCreatorOperations{
			capturePeer: func(proc *os.Process, actual api.SupervisorLockOwner) (api.SupervisorProcessIdentityV1, error) {
				peer, err := captureCreatedSupervisorPeer(proc, actual)
				if err != nil {
					return peer, err
				}
				generation, realPeerCaptured = actual, true
				return peer, want
			},
			settleUnbound: func(owner *supervisorOwner) error {
				settlementCalls.Add(1)
				original = owner
				close(entered)
				<-release // Original cleanup-entry gate, not a fabricated held Wait.
				return owner.settleUnboundCreator()
			},
		})
		finished <- creatorResult{owner, err}
	}()
	select {
	case <-entered:
	case result = <-finished:
		received = true
		t.Fatalf("real Start/capture/cleanup gate not reached: owner=%v err=%v", result.owner, result.err)
	}
	mu.Lock()
	before := observed
	mu.Unlock()
	if !realPeerCaptured || !before.Created || before.PID <= 0 || !before.GenerationAvailable || before.Generation != generation || !errors.Is(before.Cause, want) || before.Finished {
		t.Errorf("actual pre-settlement creation/capture facts missing: realCapture=%v snapshot=%#v", realPeerCaptured, before)
	}
	select {
	case result = <-finished:
		received = true
		t.Error("creator returned across held original cleanup")
	default:
	}
	unblock()
	join()
	if original != nil {
		<-original.exitDone
	} // Same original monitor; no competing Wait or Kill.
	mu.Lock()
	final := observed
	mu.Unlock()
	if result.owner != nil || !errors.Is(result.err, want) || !final.Created || !final.WaitReceived || !final.Finished || settlementCalls.Load() != 1 {
		t.Errorf("original failed creator/Wait not received: owner=%v err=%v snapshot=%#v settlements=%d", result.owner, result.err, final, settlementCalls.Load())
	}
}

func assertWindowsBackgroundCommand(t *testing.T, cmdArgs []string, flags uint32, hide bool) {
	t.Helper()
	if !hide || flags&windows.CREATE_NO_WINDOW == 0 {
		t.Fatalf("background child flags=%#x HideWindow=%v, want CREATE_NO_WINDOW and HideWindow", flags, hide)
	}
	for _, arg := range cmdArgs {
		if arg == WindowsDebugConsolePrefix {
			t.Fatalf("background child argv propagated %q: %q", WindowsDebugConsolePrefix, cmdArgs)
		}
	}
}

func TestWindowsSupervisorSpawnNoConsole(t *testing.T) {
	cmd := newGUISupervisorCmd("mcphub", []string{"supervise"}, io.Discard)
	if cmd.SysProcAttr == nil {
		t.Fatal("supervisor SysProcAttr is nil")
	}
	assertWindowsBackgroundCommand(t, cmd.Args, cmd.SysProcAttr.CreationFlags, cmd.SysProcAttr.HideWindow)
	if len(cmd.Env) != 0 {
		t.Fatalf("supervisor command unexpectedly rewrote environment: %q", cmd.Env)
	}
}

func TestWindowsSupervisorRetryNoConsole(t *testing.T) {
	if got, want := reflect.ValueOf(spawnSupervisorFn).Pointer(), reflect.ValueOf(ensureSupervisorRunningObserved).Pointer(); got != want {
		t.Fatalf("respawn seam moved: got %v want %v", got, want)
	}
	for _, args := range [][]string{{"supervise"}, {"supervise", "--strict-mode"}} {
		for attempt := 0; attempt < 3; attempt++ {
			cmd := newGUISupervisorCmd("mcphub", args, io.Discard)
			assertWindowsBackgroundCommand(t, cmd.Args, cmd.SysProcAttr.CreationFlags, cmd.SysProcAttr.HideWindow)
			if len(cmd.Env) != 0 {
				t.Fatalf("retry %d unexpectedly rewrote environment: %q", attempt, cmd.Env)
			}
		}
	}
}

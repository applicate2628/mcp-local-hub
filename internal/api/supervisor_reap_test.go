package api

import (
	"context"
	"errors"
	"testing"
	"time"
)

type reapDepsFake struct {
	quiesce  IPCResponse
	qerr     error
	exitErr  error
	forceErr error
	killed   int
}

func (f *reapDepsFake) QuiesceTimers(context.Context, string, int) (IPCResponse, error) {
	return f.quiesce, f.qerr
}
func (f *reapDepsFake) ExitGraceful(context.Context, string, int) (IPCResponse, error) {
	return IPCResponse{Final: true}, f.exitErr
}
func (f *reapDepsFake) ForceKillSupervisor(string) error { f.killed++; return f.forceErr }

func TestReapSupervisor_UnknownLegacyQuiesceRoutesThroughExactFallback(t *testing.T) {
	f := &reapDepsFake{qerr: errors.New("UNKNOWN_COMMAND")}
	if err := ReapSupervisor(context.Background(), SupervisorReapOpts{PipePath: "pipe", Deps: f}); err != nil {
		t.Fatal(err)
	}
	if f.killed != 1 {
		t.Fatalf("force kill=%d want 1 after unproven legacy quiesce", f.killed)
	}
}

func TestReapSupervisor_CleanCurrentPathDoesNotForceKill(t *testing.T) {
	f := &reapDepsFake{quiesce: IPCResponse{Result: map[string]any{"still_running": []any{}}, Final: true}}
	if err := ReapSupervisor(context.Background(), SupervisorReapOpts{PipePath: "pipe", Deps: f}); err != nil {
		t.Fatal(err)
	}
	if f.killed != 0 {
		t.Fatalf("force kill=%d want 0 for clean current path", f.killed)
	}
}

func TestReapSupervisor_ForceKillFailureRetainsTypedCause(t *testing.T) {
	cause := errors.New("ACCESS_DENIED")
	f := &reapDepsFake{exitErr: errors.New("timeout"), forceErr: cause}
	err := ReapSupervisor(context.Background(), SupervisorReapOpts{
		PipePath: "pipe",
		Deps:     f,
	})
	if !errors.Is(err, ErrSupervisorReapForceKill) || !errors.Is(err, cause) {
		t.Fatalf("error=%v want typed force-kill classification and original cause", err)
	}
}

func TestReapSupervisor_DefaultPortReleaseTimeoutIsAPIOwned(t *testing.T) {
	f := &reapDepsFake{quiesce: IPCResponse{Result: map[string]any{"still_running": []any{}}, Final: true}}
	var got time.Duration
	err := ReapSupervisor(context.Background(), SupervisorReapOpts{
		PipePath:      "pipe",
		ExpectedPorts: []int{9304},
		VerifyPortsUnbound: func(_ []int, timeout time.Duration) error {
			got = timeout
			return nil
		},
		Deps: f,
	})
	if err != nil {
		t.Fatal(err)
	}
	if got != DefaultSupervisorReapPortReleaseTimeout {
		t.Fatalf("port release timeout=%s want API default %s", got, DefaultSupervisorReapPortReleaseTimeout)
	}
}

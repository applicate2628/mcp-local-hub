package api

import (
	"context"
	"errors"
	"fmt"
	"time"
)

const (
	// DefaultSupervisorReapQuiesceTimeoutMs and
	// DefaultSupervisorReapExitTimeoutMs are the shared lifecycle budgets for
	// every supervisor reap transaction. Higher-level upgrade callers may add
	// their own post-reap settlement budget but do not redefine these phases.
	DefaultSupervisorReapQuiesceTimeoutMs = 30000
	DefaultSupervisorReapExitTimeoutMs    = 5000
	// DefaultSupervisorReapPortReleaseTimeout is the shared per-port listener
	// release budget after a supervisor reap. Callers may provide a tighter or
	// wider explicit timeout, but must not copy the product default locally.
	DefaultSupervisorReapPortReleaseTimeout = 10 * time.Second
)

// ErrSupervisorReapForceKill identifies a failed exact-owner force fallback.
// Higher-level transactions may retain their established error contract while
// still preserving this lower lifecycle classification and its original cause.
var ErrSupervisorReapForceKill = errors.New("supervisor reap force-kill failed")

// SupervisorReapDeps is the lower, capability-neutral owner of an existing
// supervisor handoff. Implementations authenticate IPC and identity-gate their
// own force-kill; this transaction never chooses a PID or port itself.
type SupervisorReapDeps interface {
	QuiesceTimers(context.Context, string, int) (IPCResponse, error)
	ExitGraceful(context.Context, string, int) (IPCResponse, error)
	ForceKillSupervisor(string) error
}

// SupervisorReapOpts binds the one reusable quiesce -> graceful -> exact-tree
// fallback -> listener-release transaction. IsAlreadyExited is injected because
// only the platform adapter owns its process-error vocabulary.
type SupervisorReapOpts struct {
	PipePath           string
	QuiesceTimeoutMs   int
	ExitTimeoutMs      int
	ExpectedPorts      []int
	VerifyPortsUnbound func([]int, time.Duration) error
	PortReleaseTimeout time.Duration
	IsAlreadyExited    func(error) bool
	Deps               SupervisorReapDeps
}

func ReapSupervisor(ctx context.Context, opts SupervisorReapOpts) error {
	if opts.Deps == nil {
		return fmt.Errorf("supervisor reap dependencies are required")
	}
	if opts.QuiesceTimeoutMs == 0 {
		opts.QuiesceTimeoutMs = DefaultSupervisorReapQuiesceTimeoutMs
	}
	if opts.ExitTimeoutMs == 0 {
		opts.ExitTimeoutMs = DefaultSupervisorReapExitTimeoutMs
	}
	if opts.PortReleaseTimeout == 0 {
		opts.PortReleaseTimeout = DefaultSupervisorReapPortReleaseTimeout
	}
	quiesce, qerr := opts.Deps.QuiesceTimers(ctx, opts.PipePath, opts.QuiesceTimeoutMs)
	unclean := qerr != nil
	if !unclean {
		if body, ok := quiesce.Result.(map[string]any); ok {
			if rows, ok := body["still_running"].([]any); ok && len(rows) != 0 {
				unclean = true
			}
		}
	}
	_, exitErr := opts.Deps.ExitGraceful(ctx, opts.PipePath, opts.ExitTimeoutMs)
	if unclean || exitErr != nil {
		if err := opts.Deps.ForceKillSupervisor(opts.PipePath); err != nil && (opts.IsAlreadyExited == nil || !opts.IsAlreadyExited(err)) {
			return fmt.Errorf("%w: force-kill supervisor during reap: %w", ErrSupervisorReapForceKill, err)
		}
	}
	if len(opts.ExpectedPorts) != 0 && opts.VerifyPortsUnbound != nil {
		if err := opts.VerifyPortsUnbound(opts.ExpectedPorts, opts.PortReleaseTimeout); err != nil {
			return fmt.Errorf("port-unbound verification after supervisor reap: %w", err)
		}
	}
	return nil
}

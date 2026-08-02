package cli

import (
	"context"
	"errors"
	"fmt"
	"time"

	"mcp-local-hub/internal/api"
)

type targetSettlementLivenessProbeFunc func(
	d api.SupervisorDaemon,
	entry DaemonRuntimeEntry,
	now time.Time,
) supervisorLivenessVerdict

type targetSettlementWaitFunc func(context.Context) error

// settleReconcileTarget is the single runtime-readiness owner for a targeted
// reconcile. It first establishes a FIFO controller-processing barrier, then
// observes the exact persisted generation, processed intent snapshot, tracker
// generation, and canonical PID/port liveness under the caller's bounded
// context. It never starts a background lifecycle.
func (c *supervisorController) settleReconcileTarget(ctx context.Context, target api.ReconcileTarget) api.ReconcileTargetSettlement {
	result := api.ReconcileTargetSettlement{
		State:  api.ReconcileTargetSettlementIncomplete,
		Reason: api.ReconcileTargetReasonLivenessUnverified,
		Target: target,
	}
	if c == nil {
		result.Reason = api.ReconcileTargetReasonControllerUnavailable
		return result
	}
	if c.eventLoop == nil {
		result.Reason = api.ReconcileTargetReasonEventLoopUnavailable
		return result
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if err := c.waitForControllerBarrier(ctx); err != nil {
		return targetSettlementContextResult(result, err)
	}

	registryPathFn := c.targetRegistryPath
	if registryPathFn == nil {
		registryPathFn = api.DefaultRegistryPath
	}
	registryPath, err := registryPathFn()
	if err != nil {
		result.Reason = api.ReconcileTargetReasonRegistryUnavailable
		result.Error = fmt.Sprintf("resolve workspace registry: %v", err)
		return result
	}

	waitFn := c.targetSettlementWait
	if waitFn == nil {
		waitFn = waitForNextTargetSettlementProbe
	}
	last := result
	for {
		descriptor, terminal := c.observeReconcileTargetIdentity(registryPath, target)
		if terminal != nil {
			return *terminal
		}
		if c.tracker == nil {
			last.Reason = api.ReconcileTargetReasonControllerUnavailable
			last.Error = "runtime tracker unavailable"
			return last
		}

		entry, ok := c.tracker.Get(target.TaskName)
		if !ok {
			last = targetSettlementWithRuntime(result, api.ReconcileTargetSettlementIncomplete, api.ReconcileTargetReasonLivenessUnverified, entry, "runtime generation not yet observed")
		} else {
			switch entry.State {
			case daemonRuntimeStateQuarantine:
				return targetSettlementWithRuntime(result, api.ReconcileTargetSettlementFailed, api.ReconcileTargetReasonQuarantined, entry, entry.LastError)
			case daemonRuntimeStateBackoff:
				return targetSettlementWithRuntime(result, api.ReconcileTargetSettlementFailed, api.ReconcileTargetReasonBackoff, entry, entry.LastError)
			case daemonRuntimeStateRunning:
				verdict := c.probeReconcileTargetLiveness(*descriptor, entry)
				if verdict.TargetReady() {
					// Close both replacement windows opened by the liveness probe:
					// persisted intent/registry replacement and tracker PID generation
					// replacement. Ready is returned only if both identities remain exact.
					if _, changed := c.observeReconcileTargetIdentity(registryPath, target); changed != nil {
						return *changed
					}
					current, present := c.tracker.Get(target.TaskName)
					if !present || current.CurrentPID != entry.CurrentPID || current.PIDGeneration != entry.PIDGeneration {
						last = targetSettlementWithRuntime(result, api.ReconcileTargetSettlementIncomplete, api.ReconcileTargetReasonLivenessUnverified, current, "runtime generation changed during readiness probe")
					} else {
						return targetSettlementWithRuntime(result, api.ReconcileTargetSettlementReady, api.ReconcileTargetReasonReady, current, "")
					}
				} else if verdict.Live && verdict.PortBound {
					// The canonical liveness owner may retain TCP-only operational
					// liveness on platforms without socket-owner support. That weaker
					// proof is terminally unverified for this target attempt: a listener
					// exists, but it is not attributable to the tracked PID.
					return targetSettlementWithRuntime(result, api.ReconcileTargetSettlementIncomplete, api.ReconcileTargetReasonLivenessUnverified, entry, "TCP listener observed without current-PID ownership proof")
				} else {
					switch verdict.Reason {
					case supervisorLivenessReasonPortOwnerMismatch, supervisorLivenessReasonPortOwnerSelf:
						return targetSettlementWithRuntime(result, api.ReconcileTargetSettlementFailed, api.ReconcileTargetReasonPortOwnerMismatch, entry, verdict.IdentityDetail)
					case supervisorLivenessReasonPortUnbound:
						return targetSettlementWithRuntime(result, api.ReconcileTargetSettlementIncomplete, api.ReconcileTargetReasonPortUnbound, entry, verdict.IdentityDetail)
					default:
						detail := verdict.IdentityDetail
						if detail == "" {
							detail = verdict.Reason
						}
						last = targetSettlementWithRuntime(result, api.ReconcileTargetSettlementIncomplete, api.ReconcileTargetReasonLivenessUnverified, entry, detail)
					}
				}
			default:
				last = targetSettlementWithRuntime(result, api.ReconcileTargetSettlementIncomplete, api.ReconcileTargetReasonLivenessUnverified, entry, entry.LastError)
			}
		}

		if err := waitFn(ctx); err != nil {
			return targetSettlementContextResult(last, err)
		}
	}
}

func (c *supervisorController) waitForControllerBarrier(ctx context.Context) error {
	done := make(chan struct{})
	if err := c.eventLoop.PostCtx(ctx, api.LoopEvent{
		Kind: evControllerBarrier,
		Body: map[string]any{controllerBarrierResultBodyKey: done},
	}); err != nil {
		return err
	}
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func waitForNextTargetSettlementProbe(ctx context.Context) error {
	timer := time.NewTimer(supervisorPortProbeTimeout)
	defer timer.Stop()
	select {
	case <-timer.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (c *supervisorController) observeReconcileTargetIdentity(registryPath string, target api.ReconcileTarget) (*api.SupervisorDaemon, *api.ReconcileTargetSettlement) {
	base := api.ReconcileTargetSettlement{Target: target}
	registry := api.NewRegistry(registryPath)
	if err := registry.Load(); err != nil {
		out := base
		out.State = api.ReconcileTargetSettlementIncomplete
		out.Reason = api.ReconcileTargetReasonRegistryUnavailable
		out.Error = err.Error()
		return nil, &out
	}
	row, ok := registry.GetSerena(target.WorkspaceKey)
	registeredAt, parseErr := time.Parse(time.RFC3339Nano, target.RegisteredAt)
	if !ok || parseErr != nil || row.PendingSerenaRemoval ||
		row.WorkspacePath != target.WorkspacePath ||
		canonicalSupervisorTaskName(row.TaskName) != canonicalSupervisorTaskName(target.TaskName) ||
		row.Port != target.ExpectedPort ||
		!row.RegisteredAt.Equal(registeredAt) {
		out := base
		out.State = api.ReconcileTargetSettlementFailed
		out.Reason = api.ReconcileTargetReasonTargetGenerationReplaced
		if parseErr != nil {
			out.Error = fmt.Sprintf("parse target registered_at: %v", parseErr)
		}
		return nil, &out
	}
	if c.intentCache == nil {
		out := base
		out.State = api.ReconcileTargetSettlementFailed
		out.Reason = api.ReconcileTargetReasonIntentMissing
		return nil, &out
	}
	descriptor, ok := c.intentCache.LookupCanonical(target.TaskName)
	if !ok || descriptor == nil {
		out := base
		out.State = api.ReconcileTargetSettlementFailed
		out.Reason = api.ReconcileTargetReasonIntentMissing
		return nil, &out
	}
	effectivePort, portOK := api.EffectiveDaemonPort(*descriptor)
	if canonicalSupervisorTaskName(descriptor.TaskName) != canonicalSupervisorTaskName(target.TaskName) ||
		descriptor.Workspace != target.WorkspacePath || !portOK || effectivePort != target.ExpectedPort {
		out := base
		out.State = api.ReconcileTargetSettlementFailed
		out.Reason = api.ReconcileTargetReasonTargetGenerationReplaced
		return nil, &out
	}
	copy := *descriptor
	copy.Port = effectivePort
	return &copy, nil
}

func (c *supervisorController) probeReconcileTargetLiveness(d api.SupervisorDaemon, entry DaemonRuntimeEntry) supervisorLivenessVerdict {
	if c.targetLivenessProbe != nil {
		return c.targetLivenessProbe(d, entry, time.Now().UTC())
	}
	return supervisorDaemonLivenessVerdictWithProbe(
		d,
		entry,
		time.Now().UTC(),
		supervisorLivenessProbeFns,
		supervisorStartupBindDeadline(d),
	)
}

func targetSettlementWithRuntime(base api.ReconcileTargetSettlement, state api.ReconcileTargetSettlementState, reason string, entry DaemonRuntimeEntry, detail string) api.ReconcileTargetSettlement {
	base.State = state
	base.Reason = reason
	base.CurrentPID = entry.CurrentPID
	base.PIDGeneration = entry.PIDGeneration
	base.Error = detail
	return base
}

func targetSettlementContextResult(base api.ReconcileTargetSettlement, err error) api.ReconcileTargetSettlement {
	base.State = api.ReconcileTargetSettlementIncomplete
	base.Error = err.Error()
	if errors.Is(err, context.Canceled) {
		base.Reason = api.ReconcileTargetReasonSettlementCancelled
	} else {
		base.Reason = api.ReconcileTargetReasonSettlementTimeout
	}
	return base
}

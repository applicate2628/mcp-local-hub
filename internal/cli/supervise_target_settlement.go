package cli

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"mcp-local-hub/internal/api"
)

type targetSettlementLivenessProbeFunc func(
	ctx context.Context,
	d api.SupervisorDaemon,
	entry DaemonRuntimeEntry,
	now time.Time,
) supervisorLivenessVerdict

type targetSettlementWaitFunc func(context.Context) error

const stopSettlementRecoveryRetryDelay = time.Second

type stopSettlementRetryDecision struct {
	retry         bool
	operatorBlock bool
}

// stopSettlementRetryPolicy is the sole controller retry policy for durable
// stop failures. It is intentionally keyed by the closed enum; FailureDetail
// is diagnostics only and must never decide retry behavior.
var stopSettlementRetryPolicy = map[api.StopSettlementFailureClass]stopSettlementRetryDecision{
	api.StopSettlementFailureProcessAlive:       {retry: true},
	api.StopSettlementFailureListenerAlive:      {retry: true},
	api.StopSettlementFailureSettlementTimeout:  {retry: true},
	api.StopSettlementFailureSettlementCanceled: {retry: true},
	api.StopSettlementFailureIdentityUnverified: {operatorBlock: true},
	api.StopSettlementFailureRuntimeReplaced:    {operatorBlock: true},
	api.StopSettlementFailureTerminationFailed:  {retry: true},
	api.StopSettlementFailurePersistence:        {operatorBlock: true},
}

// enqueueStopSettlementRecovery is the only trigger path. It never changes a
// receipt: it reserves one token and waits to enqueue the FIFO command.
func (c *supervisorController) enqueueStopSettlementRecovery(ctx context.Context, taskName string) error {
	if c == nil || c.eventLoop == nil || c.tracker == nil {
		return fmt.Errorf("stop settlement controller unavailable")
	}
	key := canonicalSupervisorTaskName(taskName)
	if _, loaded := c.stopSettlementRecoveryArmed.LoadOrStore(key, struct{}{}); loaded {
		return nil
	}
	if err := c.eventLoop.PostCtx(ctx, api.LoopEvent{Kind: evStopSettlementRecovery, TaskName: key}); err != nil {
		c.stopSettlementRecoveryArmed.Delete(key)
		return err
	}
	return nil
}

func (c *supervisorController) enqueuePendingStopSettlementRecovery(ctx context.Context) {
	if c == nil || c.tracker == nil {
		return
	}
	for _, receipt := range c.tracker.PendingStopSettlements() {
		_ = c.enqueueStopSettlementRecovery(ctx, receipt.TaskName)
	}
}

func (c *supervisorController) armStopSettlementRecovery(taskName string) {
	if c == nil || c.ctx == nil || c.eventLoop == nil || c.tracker == nil {
		return
	}
	key := canonicalSupervisorTaskName(taskName)
	if _, loaded := c.stopSettlementRecoveryArmed.LoadOrStore(key, struct{}{}); loaded {
		return
	}
	time.AfterFunc(stopSettlementRecoveryRetryDelay, func() {
		if err := c.eventLoop.PostCtx(c.ctx, api.LoopEvent{Kind: evStopSettlementRecovery, TaskName: key}); err != nil {
			c.stopSettlementRecoveryArmed.Delete(key)
		}
	})
}

// recoverStopReceiptOnLoop owns a single retry admission. It validates the
// current intent and exact runtime identity before any tracked re-terminate;
// port fences never terminate or spawn and only return an observation target.
func (c *supervisorController) recoverStopReceiptOnLoop(taskName string) (api.StopBatchTargetV1, bool) {
	if c == nil || c.tracker == nil || c.intentCache == nil || strings.TrimSpace(c.statePath) == "" {
		return api.StopBatchTargetV1{}, false
	}
	receipt, ok := c.tracker.StopSettlementReceipt(taskName)
	if !ok {
		return api.StopBatchTargetV1{}, false
	}
	desc, ok := c.intentCache.LookupCanonical(taskName)
	if !ok || desc == nil {
		return api.StopBatchTargetV1{}, false
	}
	port, ok := api.EffectiveDaemonPort(*desc)
	if !ok || port != receipt.Port {
		return api.StopBatchTargetV1{}, false
	}
	intent := c.daemonIntent.Lookup(taskName)
	active, _ := intent.IsActiveStop(time.Now().UTC())
	if intent.Desired != api.IntentDesiredStopped || !active {
		return api.StopBatchTargetV1{}, false
	}
	entry, present := c.tracker.Get(taskName)
	effectivePhase := receipt.Phase
	if receipt.Phase == api.StopSettlementPhaseFailed {
		effectivePhase = receipt.ResumePhase
	}
	if !present || !stopReceiptRecoveryIdentityMatches(receipt, effectivePhase, entry) {
		return api.StopBatchTargetV1{}, false
	}
	if receipt.Phase == api.StopSettlementPhaseFailed {
		decision, known := stopSettlementRetryPolicy[receipt.FailureClass]
		if !known || !decision.retry || decision.operatorBlock {
			return api.StopBatchTargetV1{}, false
		}
		resumed, err := c.tracker.AdvanceStopSettlement(c.statePath, receipt, receipt.ResumePhase, "", "")
		if err != nil {
			c.armStopSettlementRecovery(taskName)
			return api.StopBatchTargetV1{}, false
		}
		receipt = resumed
	}
	target := api.StopBatchTargetV1{TaskName: receipt.TaskName, ExpectedPort: receipt.Port}
	if receipt.Mode == "port_fence" {
		return target, true
	}
	if receipt.Phase != api.StopSettlementPhaseStopRequested {
		return target, true
	}
	if err := c.applyAuthoritativeStopTransition(taskName, desc); err != nil {
		if current, exists := c.tracker.StopSettlementReceipt(taskName); exists && current.Phase != api.StopSettlementPhaseFailed {
			_, _ = c.tracker.AdvanceStopSettlement(c.statePath, current, api.StopSettlementPhaseFailed, api.StopSettlementFailureTerminationFailed, err.Error())
		}
		c.armStopSettlementRecovery(taskName)
		return api.StopBatchTargetV1{}, false
	}
	return target, true
}

// stopReceiptRecoveryIdentityMatches evaluates the identity needed for the
// phase being recovered. A receipt already at exit_observed has deliberately
// lost its live PID, while a stop_requested receipt still requires that exact
// live child before controller re-termination is permitted.
func stopReceiptRecoveryIdentityMatches(receipt api.StopSettlementReceiptV1, phase api.StopSettlementPhase, entry DaemonRuntimeEntry) bool {
	switch receipt.Mode {
	case "stop":
		switch phase {
		case api.StopSettlementPhaseStopRequested:
			return entry.CurrentPID == receipt.PID && entry.PIDGeneration == receipt.PIDGeneration && !entry.StartedAt.IsZero() && entry.StartedAt.UTC().Format(time.RFC3339Nano) == receipt.StartedAt
		case api.StopSettlementPhaseExitObserved, api.StopSettlementPhasePortReleased:
			return entry.State == daemonRuntimeStateIdle && entry.CurrentPID == 0 && entry.PIDGeneration == receipt.PIDGeneration
		}
	case "port_fence":
		switch phase {
		case api.StopSettlementPhaseExitObserved, api.StopSettlementPhasePortReleased:
			return entry.State == daemonRuntimeStateIdle && entry.CurrentPID == 0 && entry.PIDGeneration == receipt.PIDGeneration
		}
	}
	return false
}

// settleStopBatchTargets settles one ordered batch through a single FIFO barrier
// and one port-owner snapshot per sweep. It deliberately does not spawn a
// goroutine or timer per target: all selected tasks make equal progress under
// the caller's bounded context.
func (c *supervisorController) settleStopBatchTargets(ctx context.Context, targets []api.StopBatchTargetV1) (results []api.StoppedSettlement) {
	results = make([]api.StoppedSettlement, len(targets))
	// A terminal stopped observation commits a staged receipt only after both
	// evidence legs are present: the exact child exit was recorded by the
	// controller and this sweep observed its expected listener free.  Failed or
	// incomplete rows deliberately retain their receipt for restart recovery.
	defer func() {
		for i, target := range targets {
			results[i] = c.finalizeStoppedSettlementReceipt(target, results[i])
		}
	}()
	for i, target := range targets {
		results[i] = api.StoppedSettlement{TaskName: target.TaskName, State: api.StoppedSettlementIncomplete, Reason: api.StoppedSettlementReasonIdentityUnverified}
	}
	if len(targets) == 0 {
		return results
	}
	if c == nil || c.eventLoop == nil || c.tracker == nil {
		for i := range results {
			results[i].Error = "supervisor controller runtime unavailable"
		}
		return results
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if err := c.waitForControllerBarrier(ctx); err != nil {
		for i := range results {
			results[i] = stoppedSettlementContextResult(results[i], err)
		}
		return results
	}

	descriptors := make([]*api.SupervisorDaemon, len(targets))
	pending := make([]bool, len(targets))
	for i, target := range targets {
		d, terminal := c.observeStopBatchTarget(target)
		if terminal != nil {
			results[i] = *terminal
			continue
		}
		descriptors[i] = d
		pending[i] = true
	}
	snapshotFn := c.stoppedSettlementPortOwnersSnapshot
	if snapshotFn == nil {
		snapshotFn = api.LoopbackPortOwnersSnapshotContext
	}
	waitFn := c.stoppedSettlementWait
	if waitFn == nil {
		waitFn = waitForNextTargetSettlementProbe
	}
	for {
		anyPending := false
		for _, p := range pending {
			anyPending = anyPending || p
		}
		if !anyPending {
			return results
		}
		owners, snapshotErr := snapshotFn(ctx)
		for i, target := range targets {
			if !pending[i] {
				continue
			}
			before, present := c.tracker.Get(target.TaskName)
			if !present {
				results[i].Error = "runtime generation not observed"
				continue
			}
			after, stillPresent := c.tracker.Get(target.TaskName)
			if !stillPresent || after.PIDGeneration != before.PIDGeneration {
				results[i].Error = "runtime generation replaced during stop settlement"
				continue
			}
			if after.State != daemonRuntimeStateIdle || after.CurrentPID != 0 {
				results[i] = api.StoppedSettlement{TaskName: target.TaskName, State: api.StoppedSettlementFailed, Reason: api.StoppedSettlementReasonProcessAlive, CurrentPID: after.CurrentPID, PIDGeneration: after.PIDGeneration, Error: "controller runtime has not reached idle"}
				continue
			}
			if snapshotErr != nil {
				results[i] = api.StoppedSettlement{TaskName: target.TaskName, State: api.StoppedSettlementIncomplete, Reason: api.StoppedSettlementReasonIdentityUnverified, PIDGeneration: after.PIDGeneration, Error: "probe stopped listener: " + snapshotErr.Error()}
				continue
			}
			if ownerPID, occupied := owners[descriptors[i].Port]; occupied && ownerPID > 0 {
				results[i] = api.StoppedSettlement{TaskName: target.TaskName, State: api.StoppedSettlementFailed, Reason: api.StoppedSettlementReasonListenerAlive, CurrentPID: ownerPID, PIDGeneration: after.PIDGeneration, Error: "expected listener remains bound"}
				continue
			}
			results[i] = api.StoppedSettlement{TaskName: target.TaskName, State: api.StoppedSettlementStopped, Reason: api.StoppedSettlementReasonStopped, PIDGeneration: after.PIDGeneration}
			pending[i] = false
		}
		anyPending = false
		for _, p := range pending {
			anyPending = anyPending || p
		}
		if !anyPending {
			return results
		}
		if err := waitFn(ctx); err != nil {
			for i := range results {
				if pending[i] && results[i].State != api.StoppedSettlementFailed {
					results[i] = stoppedSettlementContextResult(results[i], err)
				}
			}
			return results
		}
	}
}

func (c *supervisorController) finalizeStoppedSettlementReceipt(target api.StopBatchTargetV1, result api.StoppedSettlement) api.StoppedSettlement {
	if c == nil || c.tracker == nil || strings.TrimSpace(c.statePath) == "" {
		return result
	}
	receipt, pending := c.tracker.StopSettlementReceipt(canonicalSupervisorTaskName(target.TaskName))
	if !pending {
		// Idle-respawn uses this same observer as a safety preflight.  It has no
		// stop transaction to commit, so an absent receipt there is not an
		// invented success.
		return result
	}
	if result.State == api.StoppedSettlementFailed {
		if receipt.Phase != api.StopSettlementPhaseFailed {
			if _, err := c.tracker.AdvanceStopSettlement(c.statePath, receipt, api.StopSettlementPhaseFailed, stopSettlementFailureForResult(result), result.Error); err != nil {
				c.armStopSettlementRecovery(target.TaskName)
				return api.StoppedSettlement{TaskName: target.TaskName, State: api.StoppedSettlementIncomplete, Reason: api.StoppedSettlementReasonIdentityUnverified, Error: "persist failed stop settlement: " + err.Error()}
			}
		}
		c.armStopSettlementRecovery(target.TaskName)
		return result
	}
	if result.State != api.StoppedSettlementStopped || result.Reason != api.StoppedSettlementReasonStopped {
		return result
	}
	if receipt.Phase == api.StopSettlementPhasePortReleased {
		// Crash recovery after the durable port-fence: re-probing free reached
		// this branch, so commit the already-recorded release. A rebound port
		// never reaches it and retains the receipt for a later retry.
		if err := c.tracker.RemoveStopSettlement(c.statePath, receipt); err != nil {
			if _, pending := c.tracker.StopSettlementReceipt(target.TaskName); !pending {
				// A concurrent compatible stop already performed the identical
				// commit-last removal after both callers observed the same exact
				// exit and free listener. Its terminal state is this caller's
				// terminal state too.
				return result
			}
			c.armStopSettlementRecovery(target.TaskName)
			return api.StoppedSettlement{TaskName: target.TaskName, State: api.StoppedSettlementIncomplete, Reason: api.StoppedSettlementReasonIdentityUnverified, PIDGeneration: result.PIDGeneration, Error: "commit recovered stop settlement: " + err.Error()}
		}
		return result
	}
	if receipt.Phase != api.StopSettlementPhaseExitObserved {
		return api.StoppedSettlement{TaskName: target.TaskName, State: api.StoppedSettlementIncomplete, Reason: api.StoppedSettlementReasonIdentityUnverified, PIDGeneration: result.PIDGeneration, Error: "stop receipt has no exact child-exit observation"}
	}
	released, err := c.tracker.AdvanceStopSettlement(c.statePath, receipt, api.StopSettlementPhasePortReleased, "", "")
	if err != nil {
		if current, pending := c.tracker.StopSettlementReceipt(target.TaskName); pending && current.Phase == api.StopSettlementPhasePortReleased {
			if removeErr := c.tracker.RemoveStopSettlement(c.statePath, current); removeErr == nil {
				return result
			}
			if _, stillPending := c.tracker.StopSettlementReceipt(target.TaskName); !stillPending {
				return result
			}
		} else if !pending {
			return result
		}
		c.armStopSettlementRecovery(target.TaskName)
		return api.StoppedSettlement{TaskName: target.TaskName, State: api.StoppedSettlementIncomplete, Reason: api.StoppedSettlementReasonIdentityUnverified, PIDGeneration: result.PIDGeneration, Error: "persist released stop listener: " + err.Error()}
	}
	if err := c.tracker.RemoveStopSettlement(c.statePath, released); err != nil {
		if _, pending := c.tracker.StopSettlementReceipt(target.TaskName); !pending {
			return result
		}
		c.armStopSettlementRecovery(target.TaskName)
		return api.StoppedSettlement{TaskName: target.TaskName, State: api.StoppedSettlementIncomplete, Reason: api.StoppedSettlementReasonIdentityUnverified, PIDGeneration: result.PIDGeneration, Error: "commit stop settlement: " + err.Error()}
	}
	return result
}

// stopSettlementFailureForResult is the single controller mapping from an
// observed terminal settlement outcome to a durable retry class. The retry
// path consumes this enum, never Error text.
func stopSettlementFailureForResult(result api.StoppedSettlement) api.StopSettlementFailureClass {
	switch result.Reason {
	case api.StoppedSettlementReasonProcessAlive:
		return api.StopSettlementFailureProcessAlive
	case api.StoppedSettlementReasonListenerAlive:
		return api.StopSettlementFailureListenerAlive
	case api.StoppedSettlementReasonSettlementTimeout:
		return api.StopSettlementFailureSettlementTimeout
	case api.StoppedSettlementReasonSettlementCancelled:
		return api.StopSettlementFailureSettlementCanceled
	default:
		return api.StopSettlementFailureIdentityUnverified
	}
}

func (c *supervisorController) observeStopBatchTarget(target api.StopBatchTargetV1) (*api.SupervisorDaemon, *api.StoppedSettlement) {
	base := api.StoppedSettlement{TaskName: target.TaskName, State: api.StoppedSettlementFailed, Reason: api.StoppedSettlementReasonIdentityUnverified}
	if target.TaskName == "" || target.ExpectedPort <= 0 || target.ExpectedPort > 65535 {
		base.Error = "invalid stopped settlement target"
		return nil, &base
	}
	if c.intentCache == nil {
		base.Error = "supervisor intent cache unavailable"
		return nil, &base
	}
	descriptor, ok := c.intentCache.LookupCanonical(target.TaskName)
	if !ok || descriptor == nil {
		base.Error = "task descriptor absent from supervisor intent"
		return nil, &base
	}
	port, ok := api.EffectiveDaemonPort(*descriptor)
	if !ok || port != target.ExpectedPort || canonicalSupervisorTaskName(descriptor.TaskName) != canonicalSupervisorTaskName(target.TaskName) {
		base.Error = "task descriptor does not match requested stop generation"
		return nil, &base
	}
	copy := *descriptor
	copy.Port = port
	return &copy, nil
}

func stoppedSettlementContextResult(base api.StoppedSettlement, err error) api.StoppedSettlement {
	base.State = api.StoppedSettlementIncomplete
	base.Error = err.Error()
	if errors.Is(err, context.Canceled) {
		base.Reason = api.StoppedSettlementReasonSettlementCancelled
	} else {
		base.Reason = api.StoppedSettlementReasonSettlementTimeout
	}
	return base
}

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
				verdict := c.probeReconcileTargetLiveness(ctx, *descriptor, entry)
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
					// A TCP listener without either a strong proof or the canonical
					// capability-unavailable verdict remains unverified. TargetReady owns
					// the narrow platform fallback so injected nil probes and transient
					// owner-probe failures cannot enter it here.
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

func (c *supervisorController) probeReconcileTargetLiveness(ctx context.Context, d api.SupervisorDaemon, entry DaemonRuntimeEntry) supervisorLivenessVerdict {
	if c.targetLivenessProbe != nil {
		return c.targetLivenessProbe(ctx, d, entry, time.Now().UTC())
	}
	return targetSettlementLivenessWithContext(ctx, d, entry, time.Now().UTC(), supervisorLivenessProbeFns, api.LoopbackPortOwnerPIDContext)
}

// targetSettlementLivenessWithContext derives the target request's liveness
// probe from the shared production probe without mutating it. Only the known
// production per-port owner probe is rebound to the target's bounded context;
// custom test probes and unsupported-platform TCP fallback remain unchanged.
func targetSettlementLivenessWithContext(
	ctx context.Context,
	d api.SupervisorDaemon,
	entry DaemonRuntimeEntry,
	now time.Time,
	probe supervisorLivenessProbe,
	portOwnerPIDContext func(context.Context, int) (int, bool, error),
) supervisorLivenessVerdict {
	targetProbe := probe
	if supervisorLivenessUsesProductionPortOwnerProbe(targetProbe.PortOwnerPID) {
		targetProbe.PortOwnerPID = func(port int) (int, bool, error) {
			return portOwnerPIDContext(ctx, port)
		}
	}
	return supervisorDaemonLivenessVerdictWithProbe(
		d,
		entry,
		now,
		targetProbe,
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

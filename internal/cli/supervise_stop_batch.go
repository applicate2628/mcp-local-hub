package cli

import (
	"context"
	"fmt"
	"reflect"
	"strings"
	"time"

	"mcp-local-hub/internal/api"
)

const stopBatchRequestBodyKey = "stop_batch_request"

type stopBatchLoopRequest struct {
	command api.StopBatchCommandV1
	reply   chan stopBatchLoopResult
}

type stopBatchLoopResult struct {
	err               error
	applicationErrors map[string]string
}

// postStopBatchAndSettle serializes one complete transaction through the
// controller FIFO, then waits outside the loop for terminal per-target
// evidence.  A buffered reply guarantees the loop never blocks if the IPC
// caller times out after admission.
func (c *supervisorController) postStopBatchAndSettle(ctx context.Context, command api.StopBatchCommandV1) (api.StopBatchResultV1, error) {
	result := api.StopBatchResultV1{ProtocolVersion: command.ProtocolVersion, BatchID: command.BatchID, Targets: append([]api.StopBatchTargetV1(nil), command.Targets...), IntentGeneration: command.IntentGeneration, SupervisorIntent: command.SupervisorIntent, UnifiedStops: command.UnifiedStops}
	if c == nil || c.eventLoop == nil || c.tracker == nil {
		return result, fmt.Errorf("supervisor controller runtime unavailable")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	reply := make(chan stopBatchLoopResult, 1)
	if err := c.eventLoop.PostCtx(ctx, api.LoopEvent{Kind: evStopBatch, Body: map[string]any{stopBatchRequestBodyKey: stopBatchLoopRequest{command: command, reply: reply}}}); err != nil {
		return result, err
	}
	select {
	case admitted := <-reply:
		if admitted.err != nil {
			return result, admitted.err
		}
		result.Settlements = c.settleStopBatchTargets(ctx, command.Targets)
		for i := range result.Settlements {
			if cause := admitted.applicationErrors[result.Settlements[i].TaskName]; cause != "" && result.Settlements[i].State != api.StoppedSettlementStopped {
				result.Settlements[i].State = api.StoppedSettlementFailed
				result.Settlements[i].Reason = api.StoppedSettlementReasonIdentityUnverified
				result.Settlements[i].Error = cause
			}
		}
		return result, nil
	case <-ctx.Done():
		return result, ctx.Err()
	}
}

// handleStopBatchOnLoop validates all targets and makes exactly one durable
// receipt mutation before recursively applying the stop intent transitions on
// this same FIFO goroutine.  Returning an error means no transition/terminate
// was issued for any target.
func (c *supervisorController) handleStopBatchOnLoop(request stopBatchLoopRequest) {
	respond := func(err error) {
		request.reply <- stopBatchLoopResult{err: err}
	}
	command := request.command
	if command.ProtocolVersion != 1 || command.BatchID == "" || len(command.Targets) == 0 || command.IntentGeneration == 0 || command.SupervisorIntent == nil || command.UnifiedStops == nil || c.intentCache == nil || c.tracker == nil || c.statePath == "" {
		respond(fmt.Errorf("invalid or unavailable stop_batch v1"))
		return
	}
	// Admission is deliberately split in two.  This first pass is pure: it
	// validates every target against the command snapshot and current controller
	// state without changing caches, receipts, state-machine state or processes.
	// An invalid Nth row therefore cannot partially select a new intent or issue
	// work for any preceding target.
	if err := c.preflightStopBatch(command); err != nil {
		respond(err)
		return
	}
	if err := c.selectStopBatchIntent(command); err != nil {
		respond(err)
		return
	}
	ports := stopBatchPorts(command)
	if _, err := c.tracker.BeginStopSettlementBatch(c.statePath, command, ports); err != nil {
		respond(err)
		return
	}
	applicationErrors := make(map[string]string)
	for _, target := range command.Targets {
		descriptor := command.SupervisorIntent.FindSupervisorDaemonByTaskName(target.TaskName)
		if receipt, ok := c.tracker.StopSettlementReceipt(target.TaskName); ok && receipt.Mode == "port_fence" {
			continue
		}
		if err := c.applyAuthoritativeStopTransition(target.TaskName, descriptor); err != nil {
			applicationErrors[target.TaskName] = err.Error()
			if receipt, ok := c.tracker.StopSettlementReceipt(target.TaskName); ok && receipt.Phase != api.StopSettlementPhaseFailed {
				if _, persistErr := c.tracker.AdvanceStopSettlement(c.statePath, receipt, api.StopSettlementPhaseFailed, api.StopSettlementFailureTerminationFailed, err.Error()); persistErr != nil {
					applicationErrors[target.TaskName] += "; persist failure: " + persistErr.Error()
				}
			}
			c.armStopSettlementRecovery(target.TaskName)
		}
	}
	request.reply <- stopBatchLoopResult{applicationErrors: applicationErrors}
}

// preflightStopBatch validates every operation prerequisite without side
// effects. It is intentionally kept ahead of selectStopBatchIntent and
// BeginStopSettlementBatch: the latter two persist/select state respectively.
func (c *supervisorController) preflightStopBatch(command api.StopBatchCommandV1) error {
	if c == nil || c.intentCache == nil || c.tracker == nil || strings.TrimSpace(c.statePath) == "" {
		return fmt.Errorf("stop_batch controller prerequisites unavailable")
	}
	if err := c.tracker.StopSettlementIntegrityError(); err != nil {
		return err
	}
	if command.SupervisorIntent.IntentGeneration != command.IntentGeneration {
		return fmt.Errorf("stop_batch intent generation does not match snapshot")
	}
	wantStops := command.SupervisorIntent.StopsAsDaemonIntentFile()
	if !reflect.DeepEqual(wantStops.Tasks, command.UnifiedStops.Tasks) {
		return fmt.Errorf("stop_batch unified stops do not match supervisor intent snapshot")
	}
	if err := c.validateStopBatchIntentSelection(command); err != nil {
		return err
	}
	seen := make(map[string]struct{}, len(command.Targets))
	for i, target := range command.Targets {
		canonical := canonicalSupervisorTaskName(target.TaskName)
		if target.TaskName != canonical || target.ExpectedPort <= 0 || target.ExpectedPort > 65535 {
			return fmt.Errorf("invalid stop_batch target at index %d", i)
		}
		if _, duplicate := seen[canonical]; duplicate {
			return fmt.Errorf("duplicate stop_batch target %s", target.TaskName)
		}
		seen[canonical] = struct{}{}
		descriptor := command.SupervisorIntent.FindSupervisorDaemonByTaskName(target.TaskName)
		if descriptor == nil {
			return fmt.Errorf("stop_batch descriptor absent for %s", target.TaskName)
		}
		port, ok := api.EffectiveDaemonPort(*descriptor)
		if !ok || port != target.ExpectedPort {
			return fmt.Errorf("stop_batch descriptor port mismatch for %s", target.TaskName)
		}
		stop, ok := command.UnifiedStops.Tasks[target.TaskName]
		active, _ := stop.IsActiveStop(time.Now().UTC())
		if !ok || stop.Desired != api.IntentDesiredStopped || !active {
			return fmt.Errorf("stop_batch snapshot has no active stopped intent for %s", target.TaskName)
		}
		entry, present := c.tracker.Get(target.TaskName)
		if !present || (entry.CurrentPID > 0 && (entry.PIDGeneration <= 0 || entry.StartedAt.IsZero())) || (entry.CurrentPID == 0 && entry.State != daemonRuntimeStateIdle) {
			return fmt.Errorf("stop settlement requires running generation or idle port fence for %s", target.TaskName)
		}
		if _, pending := c.tracker.StopSettlementReceipt(target.TaskName); pending {
			return fmt.Errorf("stop settlement already pending for %s", target.TaskName)
		}
		state := c.stopBatchSMState(target.TaskName)
		failures := c.tracker.CrashCountInWindow(target.TaskName, time.Now().UTC(), c.failureWindow)
		queuedAction, _ := c.queuedActions.Load(target.TaskName)
		queued, _ := queuedAction.(string)
		_, side, _, matched := api.Transition(state, api.EvIntentUpdate, api.SMContext{
			IntentDesired: api.IntentDesiredStopped, IntentIsActiveStop: true, Failures: failures, QueuedAction: queued,
		})
		if !matched {
			return fmt.Errorf("stop_batch has no state transition for %s from %s", target.TaskName, state)
		}
		if strings.Contains(side, "terminate") && c.terminateOutcome == nil && c.terminate == nil {
			return fmt.Errorf("stop_batch terminate implementation unavailable for %s", target.TaskName)
		}
	}
	return nil
}

func stopBatchPorts(command api.StopBatchCommandV1) map[string]int {
	ports := make(map[string]int, len(command.Targets))
	for _, target := range command.Targets {
		ports[canonicalSupervisorTaskName(target.TaskName)] = target.ExpectedPort
	}
	return ports
}

func (c *supervisorController) stopBatchSMState(taskName string) api.SMState {
	if c != nil {
		if raw, ok := c.smStates.Load(taskName); ok {
			if state, ok := raw.(api.SMState); ok {
				return state
			}
		}
	}
	return api.StIdle
}

// selectStopBatchIntent makes the command's durable snapshot the controller
// source before any receipt or termination mutation. A newer command refreshes
// the caches synchronously on this FIFO. An older command is rejected only
// when the newer selected intent would keep one of its targets running; this
// preserves an already-stopped operation without letting it revive a target.
func (c *supervisorController) selectStopBatchIntent(command api.StopBatchCommandV1) error {
	if err := c.validateStopBatchIntentSelection(command); err != nil {
		return err
	}
	current := c.intentCache.Snapshot()
	if current != nil && current.IntentGeneration > command.IntentGeneration {
		for _, target := range command.Targets {
			stop, active := current.Stops[target.TaskName]
			if !active {
				return fmt.Errorf("stop_batch command generation %d is older than current running intent %d for %s", command.IntentGeneration, current.IntentGeneration, target.TaskName)
			}
			isStop, _ := stop.IsActiveStop(time.Now().UTC())
			if !isStop || stop.Desired != api.IntentDesiredStopped {
				return fmt.Errorf("stop_batch command generation %d is older than current running intent %d for %s", command.IntentGeneration, current.IntentGeneration, target.TaskName)
			}
		}
		return nil
	}
	if current == nil || current.IntentGeneration <= command.IntentGeneration {
		c.intentCache.Refresh(command.SupervisorIntent)
		if c.daemonIntent != nil {
			c.daemonIntent.Refresh(command.UnifiedStops)
		}
	}
	return nil
}

// validateStopBatchIntentSelection is the read-only counterpart to
// selectStopBatchIntent. Keeping this check separate prevents a later target
// validation error from changing the selected cache generation.
func (c *supervisorController) validateStopBatchIntentSelection(command api.StopBatchCommandV1) error {
	if c == nil || c.intentCache == nil {
		return fmt.Errorf("stop_batch intent cache unavailable")
	}
	current := c.intentCache.Snapshot()
	if current != nil && current.IntentGeneration > command.IntentGeneration {
		for _, target := range command.Targets {
			stop, active := current.Stops[target.TaskName]
			if !active {
				return fmt.Errorf("stop_batch command generation %d is older than current running intent %d for %s", command.IntentGeneration, current.IntentGeneration, target.TaskName)
			}
			isStop, _ := stop.IsActiveStop(time.Now().UTC())
			if !isStop || stop.Desired != api.IntentDesiredStopped {
				return fmt.Errorf("stop_batch command generation %d is older than current running intent %d for %s", command.IntentGeneration, current.IntentGeneration, target.TaskName)
			}
		}
	}
	return nil
}

// applyAuthoritativeStopTransition is the controller-owned stop transition for
// one admitted batch target. It intentionally does not recurse through the
// generic event dispatcher: the batch snapshot is already selected and the
// transition/terminate happens on this FIFO turn, without waiting for an
// intent watcher or an unrelated EvIntentUpdate.
func (c *supervisorController) applyAuthoritativeStopTransition(taskName string, descriptor *api.SupervisorDaemon) error {
	if c == nil || descriptor == nil {
		return fmt.Errorf("stop_batch descriptor unavailable for %s", taskName)
	}
	currentState := api.StIdle
	if raw, ok := c.smStates.Load(taskName); ok {
		if state, ok := raw.(api.SMState); ok {
			currentState = state
		}
	}
	failures := 0
	if c.tracker != nil {
		failures = c.tracker.CrashCountInWindow(taskName, time.Now().UTC(), c.failureWindow)
	}
	queuedAction := ""
	if raw, ok := c.queuedActions.Load(taskName); ok {
		queuedAction, _ = raw.(string)
	}
	newState, side, persistBefore, matched := api.Transition(currentState, api.EvIntentUpdate, api.SMContext{
		IntentDesired: api.IntentDesiredStopped, IntentIsActiveStop: true, Failures: failures, QueuedAction: queuedAction,
	})
	if !matched {
		return fmt.Errorf("stop_batch has no state transition for %s from %s", taskName, currentState)
	}
	if strings.Contains(side, "queued_action=stop") {
		c.queuedActions.Store(taskName, "stop")
	}
	c.smStates.Store(taskName, newState)
	if persistBefore && c.tracker != nil && c.statePath != "" {
		if err := persistDaemonRuntimeTracker(c.events, c.tracker, c.statePath, taskName); err != nil {
			return err
		}
	}
	return c.executeSideEffect(side, newState, descriptor, api.LoopEvent{Kind: api.EvIntentUpdate, TaskName: taskName})
}

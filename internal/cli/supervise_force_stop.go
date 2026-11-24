package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"runtime"
	"time"

	"mcp-local-hub/internal/api"
	"mcp-local-hub/internal/process"
)

const forceStopRequestBodyKey = "force_stop_request"

type forceStopLoopRequest struct {
	ctx     context.Context
	command api.ForceStopCommandV1
	reply   chan api.ForceStopResultV1
}

func handleForceStop(conn net.Conn, req api.IPCRequest, deps ipcDispatchDeps) error {
	var command api.ForceStopCommandV1
	raw, err := json.Marshal(req.Args["command"])
	if err == nil {
		err = json.Unmarshal(raw, &command)
	}
	if err == nil {
		err = api.ValidateForceStopCommandV1(command)
	}
	if err != nil {
		return writeIPCFrame(conn, api.IPCResponse{ID: req.ID, Error: &api.IPCErr{Code: api.ForceStopInvalidRequest, Message: err.Error()}, Final: true})
	}
	var ctrl *supervisorController
	if deps.controllerProvider != nil {
		ctrl = deps.controllerProvider()
	}
	// The existing connection handler owns this bounded request. Disconnecting
	// does not issue a second action or transfer custody to the client.
	ctx, cancel := context.WithTimeout(baseReconcileContext(deps), daemonCreatorTerminalObservationBudget)
	defer cancel()
	result := ctrl.postForceStop(ctx, command)
	return writeIPCFrame(conn, api.IPCResponse{ID: req.ID, OK: true, Result: result, Final: true})
}

func forceStopRefusal(command api.ForceStopCommandV1, reason string, cause error) api.ForceStopResultV1 {
	return api.ForceStopResultV1{ProtocolVersion: command.ProtocolVersion, Target: command.Target,
		State: api.ForceStopRefused, Reason: reason, Action: "not-requested", PortState: "not_observed", Error: cause.Error()}
}

func (c *supervisorController) postForceStop(ctx context.Context, command api.ForceStopCommandV1) api.ForceStopResultV1 {
	if c == nil || c.eventLoop == nil || c.tracker == nil {
		return forceStopRefusal(command, api.ForceStopOwnerUnavailable, errors.New("Supervisor controller is unavailable"))
	}
	reply := make(chan api.ForceStopResultV1, 1)
	request := forceStopLoopRequest{ctx: ctx, command: command, reply: reply}
	if err := c.eventLoop.PostCtx(ctx, api.LoopEvent{Kind: evForceStop, Body: map[string]any{forceStopRequestBodyKey: request}}); err != nil {
		return forceStopRefusal(command, api.ForceStopControllerBusy, err)
	}
	select {
	case result := <-reply:
		return result
	case <-ctx.Done():
		result := forceStopRefusal(command, api.ForceStopClosureIncomplete, ctx.Err())
		result.State, result.Action = api.ForceStopIncomplete, "requested-unknown"
		return result
	}
}

// The FIFO admits and completes at most one bounded terminal operation. It
// never posts a synthetic exit or restart: the original waiter remains the
// only publisher. Concurrent lifecycle requests cannot retarget this action.
func (c *supervisorController) forceStopOnLoop(ctx context.Context, command api.ForceStopCommandV1) api.ForceStopResultV1 {
	if err := api.ValidateForceStopCommandV1(command); err != nil {
		return forceStopRefusal(command, api.ForceStopInvalidRequest, err)
	}
	if err := ctx.Err(); err != nil {
		return forceStopRefusal(command, api.ForceStopControllerBusy, err)
	}
	if runtime.GOOS != "windows" {
		return forceStopRefusal(command, api.ForceStopUnsupported, errors.New("original creator tree termination is unavailable on this platform"))
	}
	if c.daemonCreators == nil {
		return forceStopRefusal(command, api.ForceStopOwnerUnavailable, errors.New("this Supervisor has no original daemon creators"))
	}
	// Exact duplicates join the retained result even after the tracker advances
	// to a successor. They never select the successor or repeat a native action.
	operation, repeated := c.daemonCreators.forceStopOperation(command.Target)
	if !repeated {
		if c.tracker == nil || c.intentCache == nil || (c.graceful != nil && c.graceful.InProgress()) {
			return forceStopRefusal(command, api.ForceStopControllerBusy, errors.New("Supervisor is unavailable or draining"))
		}
		if err := c.tracker.StopSettlementIntegrityError(); err != nil {
			return forceStopRefusal(command, api.ForceStopControllerBusy, err)
		}
		if _, pending := c.tracker.StopSettlementReceipt(command.Target.TaskName); pending {
			return forceStopRefusal(command, api.ForceStopControllerBusy, errors.New("terminal stop settlement is already in progress"))
		}
		state, present := c.GetSMState(command.Target.TaskName)
		if !present || state != api.StRunning {
			return forceStopRefusal(command, api.ForceStopNotRunning, fmt.Errorf("task is not a running creator generation (state %s)", state))
		}
		descriptor, found := c.intentCache.LookupCanonical(command.Target.TaskName)
		if !found || descriptor == nil {
			return forceStopRefusal(command, api.ForceStopGenerationChanged, errors.New("task is absent from current intent"))
		}
		port, _ := api.EffectiveDaemonPort(*descriptor)
		if port != command.Target.ExpectedPort {
			return forceStopRefusal(command, api.ForceStopGenerationChanged, errors.New("task descriptor port changed"))
		}
		if c.daemonIntent != nil {
			stop := c.daemonIntent.Lookup(command.Target.TaskName)
			if active, _ := stop.IsActiveStop(time.Now().UTC()); active {
				return forceStopRefusal(command, api.ForceStopIntentStopped, errors.New("task has an active stopped intent"))
			}
		}
		entry, found := c.tracker.Get(command.Target.TaskName)
		if !found || !forceStopTargetMatchesEntry(command.Target, entry) {
			return forceStopRefusal(command, api.ForceStopGenerationChanged, errors.New("runtime generation changed since status"))
		}
		c.daemonCreators.mu.Lock()
		unsupported := operation != nil && operation.unsupported
		available := operation != nil && operation.owner != nil && !operation.unsupported && !operation.starting
		otherTermination := available && operation.otherTerminationSelected
		if available && !otherTermination {
			operation.forceStopRequested = true
		}
		c.daemonCreators.mu.Unlock()
		if unsupported {
			return forceStopRefusal(command, api.ForceStopUnsupported, errors.New("this generation has no native original creator capability"))
		}
		if !available {
			return forceStopRefusal(command, api.ForceStopOwnerUnavailable, errors.New("original creator was not retained for this generation"))
		}
		if otherTermination {
			return forceStopRefusal(command, api.ForceStopControllerBusy, errors.New("another lifecycle path already selected termination for this generation"))
		}
	}
	terminal, err := c.daemonCreators.completeTerminal(ctx, operation, operation.owner.TerminateCreatorAndWait)
	result := api.ForceStopResultV1{ProtocolVersion: command.ProtocolVersion, Target: command.Target,
		State: api.ForceStopIncomplete, Reason: api.ForceStopClosureIncomplete, Action: "not-requested", PortState: "not_observed"}
	if terminal != nil {
		snapshot := terminal.Snapshot()
		result.RootExited, result.TreeClosed, result.HandlesClosed = snapshot.Root.Known, snapshot.CurrentMembersExited, snapshot.HandlesClosed
		result.Action = snapshot.Action
		if snapshot.Root.OriginalPID != command.Target.PID || snapshot.Root.OriginalStartedAt != command.Target.StartedAt {
			err = errors.Join(err, errors.New("original creator terminal receipt does not match the selected generation"))
		}
		if terminal.Closure() != nil && err == nil {
			result.State, result.Reason = api.ForceStopClosed, api.ForceStopClosed
		}
	}
	if result.State != api.ForceStopClosed {
		if err == nil {
			err = errors.New("original creator terminal receipt is incomplete")
		}
		result.Error = err.Error()
		return result
	}
	if command.Target.ExpectedPort == 0 {
		result.PortState = "unspecified"
		return result
	}
	pid, occupied, probeErr := api.LoopbackPortOwnerPIDContext(ctx, command.Target.ExpectedPort)
	switch {
	case probeErr != nil:
		result.PortState, result.PortError = "unknown", probeErr.Error()
	case occupied:
		result.PortState, result.PortOwnerPID = "occupied", pid
	default:
		result.PortState = "free"
	}
	return result
}

func forceStopTargetMatchesEntry(target api.ForceStopTargetV1, entry DaemonRuntimeEntry) bool {
	return target.PID == entry.CurrentPID && target.PIDGeneration == entry.PIDGeneration && target.StartedAt == entry.StartedAt.UTC().Format(time.RFC3339Nano)
}

// This is selection over already retained creators, never OpenProcess by PID.
func (r *daemonCreatorRecords) forceStopOperation(target api.ForceStopTargetV1) (*daemonCreatorOperation, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, operation := range r.operations {
		if canonicalSupervisorTaskName(operation.task) == target.TaskName && operation.generation == target.PIDGeneration &&
			operation.pid == target.PID && operation.startedAt.UTC().Format(time.RFC3339Nano) == target.StartedAt {
			return operation, operation.forceStopRequested
		}
	}
	return nil, false
}

// completeTerminal joins, rather than retries, the one native terminal call.
// A canceled join does not overwrite the original result or discard its owner.
// The record mutex is never held across a native call, wait, or publication.
func (r *daemonCreatorRecords) completeTerminal(ctx context.Context, operation *daemonCreatorOperation, finish func(context.Context) (*process.CreatorTerminalResult, error)) (*process.CreatorTerminalResult, error) {
	r.mu.Lock()
	if done := operation.terminalDone; done != nil {
		r.mu.Unlock()
		select {
		case <-done:
			r.mu.Lock()
			result, err := operation.terminal, operation.terminalErr
			r.mu.Unlock()
			return result, err
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	operation.terminalDone = make(chan struct{})
	r.mu.Unlock()
	result, err := finish(ctx)
	r.mu.Lock()
	operation.terminal, operation.terminalErr = result, err
	close(operation.terminalDone)
	r.mu.Unlock()
	return result, err
}

// Other lifecycle requests may arrive after force-stop and before its real
// child-exit event is consumed. They join that generation's outcome and must
// not issue a second numerical termination, even after an incomplete action.
func guardForceStopTermination(records *daemonCreatorRecords, next terminationOutcomeFunc) terminationOutcomeFunc {
	return func(descriptor api.SupervisorDaemon, expected terminationExpectedTuple) terminationOutcome {
		if records != nil {
			records.mu.Lock()
			var operation *daemonCreatorOperation
			for _, candidate := range records.operations {
				if canonicalSupervisorTaskName(candidate.task) != expected.CanonicalTaskName {
					continue
				}
				matches := expected.Valid && candidate.pid == expected.PID && candidate.generation == expected.PIDGeneration && candidate.startedAt.UTC().Format(time.RFC3339Nano) == expected.StartedAt
				if candidate.forceStopRequested && (!expected.Valid || matches) {
					operation = candidate
				} else if matches {
					// Startup reconciliation also calls this closure off the FIFO.
					// Reserve that selection before releasing the record lock so
					// force-stop cannot race an already entered legacy action.
					candidate.otherTerminationSelected = true
				}
			}
			var terminal *process.CreatorTerminalResult
			var err error
			if operation != nil {
				terminal, err = operation.terminal, operation.terminalErr
			}
			records.mu.Unlock()
			if operation != nil {
				if expected.Valid && terminal != nil && terminal.Closure() != nil && err == nil {
					return terminationOutcome{Kind: terminationOutcomeAlreadyExited, Expected: expected}
				}
				return terminationOutcome{Kind: terminationOutcomeFailed, Expected: expected, Cause: errors.Join(err, errors.New("original force-stop result is pending or incomplete; another termination is forbidden"))}
			}
		}
		return next(descriptor, expected)
	}
}

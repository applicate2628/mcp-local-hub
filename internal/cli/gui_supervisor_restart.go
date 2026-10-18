package cli

import (
	"context"
	"errors"
	"sync"
	"time"

	"mcp-local-hub/internal/api"
	"mcp-local-hub/internal/gui"
	"mcp-local-hub/internal/process"
)

// This is the existing manager's one admitted operation, not a second owner.
// done closes only after the original creators and closers have been received.
// A caller may stop observing before that point; GUI shutdown still joins done.
type supervisorManagerOperation struct {
	kind                      string
	requestContext            context.Context
	done                      chan struct{}
	original                  *supervisorOwner
	target                    api.SupervisorLockOwner
	expected                  SupervisorControlExpectation
	selected                  api.SupervisorLockOwner
	externalOriginalQualified bool
	transferCommitted         bool
	launch                    SupervisorLaunch
	createdOwner              *supervisorOwner
	creation                  supervisorCreationSnapshot
	admissionExpected         SupervisorControlExpectation
	admissionTarget           api.SupervisorLockOwner
	admissionOwned            bool
	admissionExternal         bool
	held                      process.HeldPIDGeneration
	fence                     *api.UpgradeFenceLease
	resourcesPending          bool
	result                    gui.SupervisorRestartResult
	err                       error
	observers                 int
	nativeExit                []supervisorRestartNativeExitObservation
	completeOnce              sync.Once
}

func (m *supervisorManager) receiveCreationAttempt(operation *supervisorManagerOperation) supervisorAttemptReceiver {
	return func(snapshot supervisorCreationSnapshot) error {
		m.mu.Lock()
		defer m.mu.Unlock()
		if operation == nil {
			m.startupAttempt = snapshot
		} else {
			operation.creation = snapshot
			operation.publishResultLocked(operation.result)
		}
		return nil
	}
}

// Called under the manager mutex. Old-target settlement remains in its own
// fields; this projection describes the operation-created actor and closers.
func (operation *supervisorManagerOperation) mergeCreationResult(result gui.SupervisorRestartResult) gui.SupervisorRestartResult {
	if !operation.creation.Created {
		return result
	}
	result.Spawned, result.SpawnedPID = true, operation.creation.PID
	switch {
	case result.Outcome == gui.SupervisorRestartPending:
		result.OriginalChildDisposition = "pending"
	case operation.creation.WaitReceived:
		result.OriginalChildDisposition = "settled"
	case operation.result.OriginalChildDisposition == "retained" || result.OriginalChildDisposition == "retained":
		result.OriginalChildDisposition = "retained"
	default:
		result.OriginalChildDisposition = "pending"
	}
	return result
}

func (m *supervisorManager) creationAttemptFields(operation *supervisorManagerOperation) map[string]any {
	m.mu.Lock()
	snapshot := m.startupAttempt
	if operation != nil {
		snapshot = operation.creation
	}
	m.mu.Unlock()
	return snapshot.diagnosticFields()
}

type supervisorRestartNativeExitObservation struct {
	Phase        string                    `json:"phase"`
	ContextOwner string                    `json:"context_owner"`
	ContextError string                    `json:"context_error,omitempty"`
	Deadline     *time.Time                `json:"deadline,omitempty"`
	Result       process.HeldPIDExitResult `json:"result"`
	Error        string                    `json:"error,omitempty"`
}

func (m *supervisorManager) receiveRestartNativeExit(operation *supervisorManagerOperation, phase, contextOwner string, ctx context.Context, waiter process.HeldPIDExitWaiter) (process.HeldPIDExitResult, error) {
	result, err := waiter.WaitExit(ctx)
	if result.Exited && !result.ExitCodeAvailable && err == nil {
		err = errors.New("original held exit status is unavailable")
	}
	if !result.Exited && result.ExitCodeAvailable {
		err = errors.Join(err, errors.New("original held exit observation is inconsistent"))
	}
	observation := supervisorRestartNativeExitObservation{Phase: phase, ContextOwner: contextOwner, Result: result}
	if ctx == nil {
		observation.ContextError = "context unavailable"
	} else {
		if cause := ctx.Err(); cause != nil {
			observation.ContextError = cause.Error()
		}
		if deadline, exists := ctx.Deadline(); exists {
			observation.Deadline = &deadline
		}
	}
	if err != nil {
		observation.Error = err.Error()
	}
	m.mu.Lock()
	operation.nativeExit = append(operation.nativeExit, observation)
	operation.err = errors.Join(operation.err, err)
	m.mu.Unlock()
	return result, err
}

// Called only after original settlement, positive namespace clearance and
// checked Mode.Valid admission. An existing actor preserves its actual topology;
// only an absent actor uses the durable owner's recognised default.
func (operation *supervisorManagerOperation) retainsGUIReplacement(mode api.OwnerMode) bool {
	if operation.original != nil {
		return operation.original.Spawned()
	}
	return !operation.externalOriginalQualified && mode == api.OwnerModeGUI
}

func restartRefusal(code string, cause error) (gui.SupervisorRestartResult, error) {
	return gui.SupervisorRestartResult{
		Outcome: gui.SupervisorRestartRefused, Phase: "admission", Code: code,
		OriginalChildDisposition: "none", NamespaceDisposition: "unknown",
	}, cause
}

func cloneRestartResult(result gui.SupervisorRestartResult) gui.SupervisorRestartResult {
	if result.PerStepError != nil {
		copied := make(map[string]string, len(result.PerStepError))
		for step, cause := range result.PerStepError {
			copied[step] = cause
		}
		result.PerStepError = copied
	}
	if result.OriginalGeneration != nil {
		value := *result.OriginalGeneration
		result.OriginalGeneration = &value
	}
	if result.SelectedGeneration != nil {
		value := *result.SelectedGeneration
		result.SelectedGeneration = &value
	}
	return result
}

// Both helpers require manager.mu. A stored value and every outward copy
// incorporate the same already-received facts at this publication cut.
func (operation *supervisorManagerOperation) snapshotResultLocked(candidate gui.SupervisorRestartResult) gui.SupervisorRestartResult {
	return cloneRestartResult(operation.mergeCreationResult(candidate))
}

func (operation *supervisorManagerOperation) publishResultLocked(candidate gui.SupervisorRestartResult) {
	operation.result = operation.snapshotResultLocked(candidate)
}

func (operation *supervisorManagerOperation) receiveDiagnosticResultLocked(deliveryErr error, detail string) {
	// Resume from authoritative state, never a pre-I/O execution copy.
	result := operation.snapshotResultLocked(operation.result)
	if deliveryErr != nil {
		operation.err = errors.Join(operation.err, deliveryErr)
		if result.Outcome == gui.SupervisorRestartRestarted {
			result.Outcome = gui.SupervisorRestartFailed
		}
		result.Code = "RESTART_DIAGNOSTIC_UNCONFIRMED"
		if result.PerStepError == nil {
			result.PerStepError = make(map[string]string)
		}
		result.PerStepError["diagnostic"] = detail
	}
	operation.publishResultLocked(result)
}

func (m *supervisorManager) observeRestart(operation *supervisorManagerOperation, result gui.SupervisorRestartResult) gui.SupervisorRestartResult {
	m.mu.Lock()
	operation.publishResultLocked(result)
	result = operation.snapshotResultLocked(operation.result)
	m.mu.Unlock()
	return result
}

func (m *supervisorManager) completeManagerOperation(operation *supervisorManagerOperation, result gui.SupervisorRestartResult, err error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	result = operation.snapshotResultLocked(result)
	err = errors.Join(operation.err, err)
	if operation.kind == "restart" {
		requestErr := errors.New("original restart request context is unavailable")
		if operation.requestContext != nil {
			requestErr = operation.requestContext.Err()
		}
		for _, cause := range []error{requestErr, m.ctx.Err()} {
			if cause != nil && !errors.Is(err, cause) {
				err = errors.Join(err, cause)
			}
		}
		if err != nil && result.Outcome == gui.SupervisorRestartRestarted {
			result.Outcome = gui.SupervisorRestartFailed
			if result.Code == "" {
				result.Code = "RESTART_FAILED"
			}
		}
	}
	operation.err = err
	operation.publishResultLocked(result)
	if operation.kind == "restart" && len(operation.nativeExit) != 0 {
		// These acquired native values are absent from the public DTO. Receive
		// them through the existing checked audit AFTER physical cleanup ran;
		// this is not a duplicate HTTP result event or a new outcome policy.
		creation := operation.creation
		fields := map[string]any{"original_generation": operation.target, "receiving": "native-exit-observations", "observations": append([]supervisorRestartNativeExitObservation(nil), operation.nativeExit...)}
		m.mu.Unlock()
		fields["creation_attempt"] = creation.diagnosticFields()
		deliveryErr := logGUIDiagnosticEventRequired("info", "supervisor-restart-native-exit", fields)
		m.mu.Lock()
		operation.receiveDiagnosticResultLocked(deliveryErr, "native exit diagnostic delivery incomplete")
	}
	if operation.kind == "restart" && operation.observers == 0 {
		// All HTTP observers returned pending before this genuinely later
		// receiving. Native settlement has already run; logging never prevents
		// required physical cleanup and never retries its effects.
		result, err := operation.snapshotResultLocked(operation.result), operation.err
		creation := operation.creation
		m.mu.Unlock()
		fields := restartFinalizationFields(result, err)
		fields["creation_attempt"] = creation.diagnosticFields()
		deliveryErr := logGUIDiagnosticEventRequired("info", "supervisor-restart-via-gui", fields)
		m.mu.Lock()
		operation.receiveDiagnosticResultLocked(deliveryErr, "restart diagnostic delivery incomplete")
		// The same manager's existing aggregate remains a receiving owner even
		// when checked logging itself fails and no HTTP observer remains.
		m.loopErr = errors.Join(m.loopErr, operation.err)
	}
	if operation.resourcesPending {
		// Retain the same actual capability and operation; no further admission
		// or GUI lifetime release can label an uncertain closer settled.
		return
	}
	operation.completeOnce.Do(func() {
		if m.operation == operation {
			m.operation = nil
		}
		if operation.kind == "restart" {
			m.armWorkerLocked()
		}
		close(operation.done)
	})
}

func restartFinalizationFields(result gui.SupervisorRestartResult, cause error) map[string]any {
	fields := map[string]any{
		"receiving": "manager-finalization", "outcome": result.Outcome,
		"ready": result.Ready, "phase": result.Phase, "code": result.Code,
		"killed_pid": result.KilledPID, "killed": result.Killed,
		"spawned_pid": result.SpawnedPID, "spawned": result.Spawned,
		"original_generation": result.OriginalGeneration, "selected_generation": result.SelectedGeneration,
		"original_settled": result.OriginalSettled, "transfer_committed": result.TransferCommitted,
		"transferred": result.Transferred, "original_child_disposition": result.OriginalChildDisposition,
		"namespace_disposition": result.NamespaceDisposition,
	}
	// Only this existing checked/redacted diagnostic facility receives raw
	// causal evidence. Public HTTP fields retain their existing safe projection.
	if cause != nil {
		fields["owner_error"] = cause.Error()
	}
	return fields
}

func (m *supervisorManager) restart(ctx context.Context) (gui.SupervisorRestartResult, error) {
	if m == nil || ctx == nil {
		return restartRefusal("RESTART_UNAVAILABLE", errors.New("supervisor restart context/coordinator absent"))
	}
	var admitted *supervisorOwner
	var expected SupervisorControlExpectation
	var target api.SupervisorLockOwner
	var selected, owned bool
	var external *supervisorManagerOperation
	for {
		m.mu.Lock()
		if !m.initialized {
			m.mu.Unlock()
			return restartRefusal("RESTART_NOT_READY", errors.New("supervisor startup decision is pending"))
		}
		if m.shuttingDown || m.ctx.Err() != nil || ctx.Err() != nil {
			m.mu.Unlock()
			return restartRefusal("RESTART_CANCELED", errors.Join(context.Canceled, ctx.Err(), m.ctx.Err()))
		}
		if !selected {
			admitted, selected = m.current, true
			if admitted != nil && admitted.Spawned() {
				if admitted.control.proof == nil {
					m.mu.Unlock()
					return restartRefusal("RESTART_IDENTITY_UNCONFIRMED", errSupervisorControlBinding)
				}
				owned = true
				expected, target = admitted.control.proof.expected, admitted.control.proof.owner
			}
		} else if m.current != admitted || (owned && (admitted.control.proof == nil || admitted.control.proof.expected != expected || admitted.control.proof.owner != target)) {
			m.mu.Unlock()
			return restartRefusal("RESTART_GENERATION_CONFLICT", errors.New("supervisor selection changed after request admission"))
		}
		operation := m.operation
		if operation != nil && operation.kind == "restart" && (operation.original != admitted ||
			(operation.admissionOwned && (operation.admissionExpected != expected || operation.admissionTarget != target)) ||
			(external != nil && (!operation.admissionExternal || operation.admissionExpected != external.expected || operation.admissionTarget != external.target))) {
			m.mu.Unlock()
			return restartRefusal("RESTART_GENERATION_CONFLICT", errors.New("active explicit restart has a different admitted selection"))
		}
		if operation != nil && operation.kind == "respawn" {
			m.mu.Unlock()
			if !owned && (admitted == nil || !admitted.Spawned()) && external == nil {
				var err error
				external, err = m.pinRestartExternalAdmission(admitted)
				if err != nil {
					return restartRefusal("RESTART_IDENTITY_UNCONFIRMED", err)
				}
			}
			select {
			case <-operation.done:
				continue
			case <-ctx.Done():
				return restartRefusal("RESTART_CANCELED", ctx.Err())
			case <-m.ctx.Done():
				return restartRefusal("RESTART_CANCELED", m.ctx.Err())
			}
		}
		if operation == nil {
			operation = &supervisorManagerOperation{
				kind: "restart", requestContext: ctx, done: make(chan struct{}), original: admitted,
				admissionExpected: expected, admissionTarget: target, admissionOwned: owned,
				result: gui.SupervisorRestartResult{
					Outcome: gui.SupervisorRestartPending, Phase: "admission",
					OriginalChildDisposition: "none", NamespaceDisposition: "unknown",
				},
			}
			if external != nil {
				operation.admissionExternal = true
				operation.admissionExpected, operation.admissionTarget = external.expected, external.target
			}
			m.operation = operation
			workerCancel, workerDone := m.workerCancel, m.loopDone
			run := m.restartFn
			if run == nil {
				run = m.runSupervisorRestart
			}
			// The existing automatic worker is quiesced, without latching GUI
			// shutdown. An already admitted creator stays joined by that worker.
			if workerCancel != nil {
				workerCancel()
			}
			go func() {
				operationCtx, cancel := context.WithCancel(ctx)
				stopCancellation := context.AfterFunc(m.ctx, cancel)
				defer cancel()
				defer stopCancellation()
				if workerDone != nil {
					<-workerDone
				}
				result, err := run(operationCtx, operation)
				m.completeManagerOperation(operation, result, err)
			}()
		}
		operation.observers++
		m.mu.Unlock()
		select {
		case <-operation.done:
			m.mu.Lock()
			operation.observers--
			result, err := operation.snapshotResultLocked(operation.result), operation.err
			m.mu.Unlock()
			return result, err
		case <-ctx.Done():
			return m.restartObserverCanceled(operation, ctx.Err())
		case <-m.ctx.Done():
			return m.restartObserverCanceled(operation, m.ctx.Err())
		}
	}
}

func (m *supervisorManager) restartObserverCanceled(operation *supervisorManagerOperation, cause error) (gui.SupervisorRestartResult, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	operation.observers--
	result, err := operation.result, operation.err
	select {
	case <-operation.done:
		// Completion and cancellation share this mutex decision. A completed
		// synchronous result is still received by its existing HTTP audit owner.
		return operation.snapshotResultLocked(result), err
	default:
		result.Outcome = gui.SupervisorRestartPending
		return operation.snapshotResultLocked(result), errors.Join(err, cause)
	}
}

// Lock order is original GUI lifecycle, then manager.mu. No native call,
// launch mutex, IPC, settings access or Wait occurs inside this transition.
func (m *supervisorManager) commitSupervisorTransfer(ctx context.Context, operation *supervisorManagerOperation, selected api.SupervisorLockOwner) error {
	if m.lifecycle == nil || m.generation == "" {
		return gui.ErrGUIQuitGeneration
	}
	return m.lifecycle.WithActiveGeneration(m.generation, func(gui.GUIOwnerRecord) error {
		m.mu.Lock()
		defer m.mu.Unlock()
		if m.operation != operation || operation.kind != "restart" || m.current != operation.original || operation.selected != selected ||
			operation.expected.StateDir == "" || operation.expected.ProductPath == "" ||
			m.shuttingDown || ctx == nil || ctx.Err() != nil || m.ctx.Err() != nil {
			return errors.New("supervisor standalone commitment refused by original lifecycle/operation")
		}
		operation.transferCommitted = true
		return nil
	})
}

// GUI startup and both actual normal/reused Server projections enter here.
// The standby constructor and the route daemon never compose this callback.
func composeSupervisorRestart(s *gui.Server, manager *supervisorManager, lifecycle *gui.GUIOwnerLifecycle) {
	manager.lifecycle, manager.generation = lifecycle, lifecycle.Generation()
	s.SetSupervisorRestartHandler(manager.restart)
}

func supervisorRestartCleanupContext() (context.Context, context.CancelFunc) {
	// The existing GUI cleanup budget bounds control, never proof of exit.
	return context.WithTimeout(context.Background(), 30*time.Second)
}

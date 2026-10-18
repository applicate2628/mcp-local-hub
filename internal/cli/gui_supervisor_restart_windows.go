//go:build windows

package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"mcp-local-hub/internal/api"
	"mcp-local-hub/internal/gui"
	"mcp-local-hub/internal/process"
)

func restartStepFailure(result *gui.SupervisorRestartResult, phase, code string, err error) error {
	result.Outcome, result.Phase, result.Code = gui.SupervisorRestartFailed, phase, code
	if phase == "read_lock" {
		result.Phase = "admission"
	}
	if result.PerStepError == nil {
		result.PerStepError = make(map[string]string)
	}
	result.PerStepError[phase] = "restart step incomplete"
	return err
}

func (m *supervisorManager) checkRestartAdmission(ctx context.Context, operation *supervisorManagerOperation) error {
	if m.lifecycle == nil {
		return gui.ErrGUIQuitGeneration
	}
	return m.lifecycle.WithActiveGeneration(m.generation, func(gui.GUIOwnerRecord) error {
		if err := api.NewAPI().WithGUIOwnerControl(func(control api.GUIOwnerControl) error { return control.ValidateAutomaticStart() }); err != nil {
			return err
		}
		m.mu.Lock()
		defer m.mu.Unlock()
		if m.operation != operation || m.current != operation.original || m.shuttingDown || ctx.Err() != nil || m.ctx.Err() != nil {
			return errors.Join(errors.New("original GUI restart admission changed"), ctx.Err(), m.ctx.Err())
		}
		return nil
	})
}

var errRestartBinaryUnavailable = errors.New("GUI supervisor binary is unavailable")

func (m *supervisorManager) restartExpectation() (SupervisorControlExpectation, error) {
	m.mu.Lock()
	bin := m.bin
	m.mu.Unlock()
	if bin == "" {
		// A settled startup resolution failure is not permanent admission
		// failure. A fresh explicit request queries the SAME image authority.
		var resolveErr error
		bin, resolveErr = resolveMCPHubBinary()
		if resolveErr != nil || bin == "" {
			return SupervisorControlExpectation{}, errors.Join(errRestartBinaryUnavailable, resolveErr)
		}
	}
	stateDir, err := api.DaemonStateDir()
	if err != nil {
		return SupervisorControlExpectation{}, err
	}
	productPath, err := filepath.Abs(bin)
	if err == nil {
		productPath, err = filepath.EvalSymlinks(productPath)
	}
	if err != nil {
		return SupervisorControlExpectation{}, err
	}
	digest, err := startedSupervisorDigest(productPath)
	if err != nil {
		return SupervisorControlExpectation{}, err
	}
	return supervisorExpectation(stateDir, productPath, digest), nil
}

// This preliminary read pins data only. The same selector must confirm it
// again under the existing namespace fence before any control is acquired.
func (m *supervisorManager) pinRestartExternalAdmission(original *supervisorOwner) (*supervisorManagerOperation, error) {
	expected, err := m.restartExpectation()
	if err != nil {
		return nil, err
	}
	selection := &supervisorManagerOperation{original: original, expected: expected}
	var result gui.SupervisorRestartResult
	if err = m.selectRestartOriginal(selection, &result); err != nil {
		return nil, err
	}
	return selection, nil
}

func (m *supervisorManager) runSupervisorRestart(ctx context.Context, operation *supervisorManagerOperation) (result gui.SupervisorRestartResult, retErr error) {
	m.mu.Lock()
	result = operation.snapshotResultLocked(operation.result)
	m.mu.Unlock()
	expected, err := m.restartExpectation()
	if err != nil {
		if errors.Is(err, errRestartBinaryUnavailable) {
			return restartRefusal("RESTART_UNAVAILABLE", err)
		}
		return restartRefusal("RESTART_IDENTITY_UNCONFIRMED", err)
	}
	operation.expected = expected
	stateDir := expected.StateDir
	if (operation.admissionOwned || operation.admissionExternal) && operation.expected != operation.admissionExpected {
		return restartRefusal("RESTART_GENERATION_CONFLICT", errors.New("admitted supervisor expectation changed"))
	}
	if err = m.checkRestartAdmission(ctx, operation); err != nil {
		return restartRefusal("RESTART_STOPPED", err)
	}
	fence, acquired, err := api.TryAcquireUpgradeFence(ctx, stateDir)
	if err != nil || !acquired {
		return restartRefusal("RESTART_UNAVAILABLE", errors.Join(errors.New("original upgrade fence is unavailable"), err))
	}
	operation.fence = fence
	defer func() {
		if operation.held != nil && !operation.resourcesPending {
			closeErr := operation.held.Close()
			retErr = errors.Join(retErr, closeErr)
			if closeErr != nil {
				operation.resourcesPending = true
				restartStepFailure(&result, "settlement", "RESTART_PENDING", closeErr)
				result.Outcome = gui.SupervisorRestartPending
			} else {
				operation.held = nil
			}
		}
		// A pending original resource keeps the same fence/GUI custody. Never
		// release exclusion while original creator or closer is unreceived.
		if !operation.resourcesPending {
			closeErr := fence.Release()
			retErr = errors.Join(retErr, closeErr)
			if closeErr != nil {
				operation.resourcesPending = true
				restartStepFailure(&result, "settlement", "RESTART_PENDING", closeErr)
				result.Outcome = gui.SupervisorRestartPending
			} else {
				operation.fence = nil
			}
		}
		if retErr != nil && result.Outcome == gui.SupervisorRestartRestarted {
			restartStepFailure(&result, "settlement", "RESTART_FAILED", retErr)
		}
		result = m.observeRestart(operation, result)
	}()
	if pending, readErr := inspectWindowsProductRecoveryLeaves(stateDir); readErr != nil || pending {
		result, retErr = restartRefusal("RESTART_UNAVAILABLE", errors.Join(errors.New("Product recovery admission is pending or unconfirmed"), readErr))
		return
	}
	if suppressed, readErr := ensureAliveHeadlessFleetLiveHandoffSuppressed(stateDir, time.Now()); readErr != nil || suppressed {
		result, retErr = restartRefusal("RESTART_UNAVAILABLE", errors.Join(errors.New("GUI handoff admission is pending or unconfirmed"), readErr))
		return
	}
	// Checked intent selects the existing cohort/mode only. The GUI's actual
	// captured runtime strictMode remains the launch operand in BOTH topologies.
	intent, err := api.ReadSupervisorIntent(filepath.Join(stateDir, "supervisor-intent.json"))
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		result, retErr = restartRefusal("RESTART_UNAVAILABLE", err)
		return
	}
	if !intent.EffectiveOwnerMode().Valid() {
		result, retErr = restartRefusal("RESTART_UNAVAILABLE", errors.New("supervisor intent owner mode is unsupported"))
		return
	}
	ports, err := supervisorCompatExpectedPorts(stateDir)
	if err != nil {
		result, retErr = restartRefusal("RESTART_UNAVAILABLE", err)
		return
	}
	// Dependencies are frozen by value before any original target is stopped.
	launchDeps := supervisorLaunchDependencies{
		exePath: expected.ProductPath, supervisorLockPath: filepath.Join(stateDir, "supervisor.lock"), pipePath: api.SupervisorIPCAddress(stateDir),
		readOwner: api.ReadSupervisorLockOwner, sendIPCPeer: sendIPCWithResponseForPeer,
		waitLockReleased: func(c context.Context, budget time.Duration) error {
			return waitGUIRestartNamespaceReleased(c, budget, stateDir)
		},
	}
	if err = launchDeps.validate(); err != nil {
		result, retErr = restartRefusal("RESTART_UNAVAILABLE", err)
		return
	}
	if err = m.selectRestartOriginal(operation, &result); err != nil {
		code := "RESTART_IDENTITY_UNCONFIRMED"
		if errors.Is(err, errSupervisorControlGenerationChanged) {
			code = "RESTART_GENERATION_CONFLICT"
		}
		retErr = restartStepFailure(&result, "read_lock", code, err)
		result.Outcome = gui.SupervisorRestartRefused
		return
	}
	if err = m.checkRestartAdmission(ctx, operation); err != nil {
		retErr = restartStepFailure(&result, "admission", "RESTART_STOPPED", err)
		result.Outcome = gui.SupervisorRestartRefused
		return
	}
	result.Phase = "stop"
	m.observeRestart(operation, result)
	if operation.target.PID > 0 {
		if operation.original != nil && operation.original.Spawned() {
			cleanup, cancel := supervisorRestartCleanupContext()
			result.OriginalChildDisposition = "pending"
			m.observeRestart(operation, result)
			err = m.stopOwnerAndJoin(cleanup, operation.original, defaultExitTimeoutMs)
			cancel()
			result.Killed = operation.original.killCommitted
			if result.Killed {
				result.KilledPID = operation.target.PID
			}
			select {
			case <-operation.original.exitDone:
				result.OriginalSettled = true
			default:
			}
			// The original manager's unique Wait/sender join is the receiver.
			if err != nil {
				retErr = restartStepFailure(&result, "settlement", "RESTART_FAILED", err)
			}
		} else {
			var settled, committed bool
			settled, committed, err = m.stopRestartExternalOriginal(ctx, operation)
			result.OriginalSettled, result.Killed = settled, committed
			if committed {
				result.KilledPID = operation.target.PID
			}
			if err != nil {
				code := "RESTART_IDENTITY_UNCONFIRMED"
				if settled {
					code = "RESTART_FAILED"
				}
				retErr = restartStepFailure(&result, "stop", code, err)
			}
			if settled && operation.held != nil {
				closeErr := operation.held.Close()
				if closeErr != nil {
					operation.resourcesPending = true
					retErr = errors.Join(retErr, restartStepFailure(&result, "settlement", "RESTART_PENDING", closeErr))
					result.Outcome = gui.SupervisorRestartPending
					result.OriginalChildDisposition = "pending"
					return
				}
				operation.held = nil
			}
		}
	} else {
		result.OriginalSettled = true
	}
	if !result.OriginalSettled {
		if operation.resourcesPending {
			result.Outcome = gui.SupervisorRestartPending
			result.Code = "RESTART_PENDING"
			result.OriginalChildDisposition = "pending"
		} else {
			result.Outcome = gui.SupervisorRestartRefused
		}
		if retErr == nil {
			retErr = errors.New("original supervisor settlement is unconfirmed")
		}
		return
	}
	result.OriginalChildDisposition = "none"
	result.Phase = "namespace"
	m.observeRestart(operation, result)
	if err = m.receiveRestartNamespace(ctx, operation, &result); err != nil {
		retErr = errors.Join(retErr, err)
		return
	}
	if err = m.checkRestartAdmission(ctx, operation); err != nil {
		retErr = errors.Join(retErr, restartStepFailure(&result, "admission", "RESTART_CANCELED", err))
		return
	}
	if operation.retainsGUIReplacement(intent.EffectiveOwnerMode()) {
		return m.restartGUIOwned(ctx, operation, result, retErr)
	}
	return m.restartStandalone(ctx, operation, launchDeps, ports, result, retErr)
}

func (m *supervisorManager) selectRestartOriginal(operation *supervisorManagerOperation, result *gui.SupervisorRestartResult) error {
	if operation.original != nil && operation.original.Spawned() {
		proof := operation.original.control.proof
		if proof == nil || proof.expected != operation.expected || operation.original.proc == nil || proof.creator != operation.original.proc {
			return errSupervisorControlBinding
		}
		operation.target = proof.owner
		if operation.admissionOwned && (proof.expected != operation.admissionExpected || proof.owner != operation.admissionTarget) {
			return errSupervisorControlGenerationChanged
		}
	} else {
		owner, err := api.ReadSupervisorLockOwner(filepath.Join(operation.expected.StateDir, "supervisor.lock"))
		if err != nil {
			if !errors.Is(err, os.ErrNotExist) {
				return err
			}
			running, _, probeErr := api.SupervisorRunningUnderStateDir(operation.expected.StateDir)
			if probeErr != nil || running {
				return errors.Join(errors.New("held supervisor namespace has no qualified original generation"), probeErr)
			}
			if operation.admissionExternal && operation.admissionTarget.PID != 0 {
				return errSupervisorControlGenerationChanged
			}
			return nil
		}
		operation.target = owner
		if operation.admissionExternal && owner != operation.admissionTarget {
			return errSupervisorControlGenerationChanged
		}
	}
	started, err := time.Parse(time.RFC3339Nano, operation.target.StartedAt)
	if operation.target.PID <= 0 || err != nil || started.IsZero() {
		return errors.New("original supervisor generation is incomplete")
	}
	value := operation.target
	result.OriginalGeneration = &value
	return nil
}

// Global namespace state is independent from an original child Wait outcome.
// A different winner is only observed, never selected/adopted/terminated.
func (m *supervisorManager) receiveRestartNamespace(ctx context.Context, operation *supervisorManagerOperation, result *gui.SupervisorRestartResult) error {
	running, _, err := api.SupervisorRunningUnderStateDir(operation.expected.StateDir)
	if err != nil {
		return restartStepFailure(result, "namespace", "RESTART_NAMESPACE_UNCONFIRMED", err)
	}
	if !running {
		result.NamespaceDisposition = "cleared"
		return nil
	}
	winner, err := api.ReadSupervisorLockOwner(filepath.Join(operation.expected.StateDir, "supervisor.lock"))
	if err == nil && winner != operation.target && winner.PID > 0 {
		var peer api.SupervisorProcessIdentityV1
		peer, err = api.ObserveSupervisorProcessIdentityV1(winner.PID)
		if err == nil {
			_, err = observeCreatedSupervisorControl(ctx, operation.expected, winner, peer, func(owner api.SupervisorLockOwner) error {
				return validateCreatedSupervisorNativeControl(operation.expected, owner)
			}, api.ReadSupervisorLockOwner)
		}
		if err == nil {
			result.NamespaceDisposition = "occupied"
			return restartStepFailure(result, "namespace", "RESTART_NAMESPACE_CONFLICT", errors.New("a different qualified generation owns the namespace"))
		}
	}
	result.NamespaceDisposition = "unknown"
	return restartStepFailure(result, "namespace", "RESTART_NAMESPACE_UNCONFIRMED", errors.Join(errors.New("namespace clearance is unconfirmed"), err))
}

func waitGUIRestartNamespaceReleased(ctx context.Context, budget time.Duration, stateDir string) error {
	limit, cancel := context.WithTimeout(ctx, budget)
	defer cancel()
	for {
		running, _, err := api.SupervisorRunningUnderStateDir(stateDir)
		if err != nil {
			return err
		}
		if !running {
			return nil
		}
		select {
		case <-limit.Done():
			return limit.Err()
		case <-time.After(supervisorReadyPollInterval):
		}
	}
}

func (m *supervisorManager) waitRestartReady(ctx context.Context, operation *supervisorManagerOperation, binding SupervisorControlBinding, result *gui.SupervisorRestartResult) error {
	proof := binding.proof
	if proof == nil || proof.expected != operation.expected || proof.owner.PID <= 0 {
		return errSupervisorControlBinding
	}
	limit, cancel := context.WithTimeout(ctx, m.waitFor)
	defer cancel()
	for {
		ready, err := api.ReadSupervisorControlReadinessForPeerFromStateDir(limit, proof.expected.StateDir, proof.owner, proof.peer)
		if ready.ReconcileReadyPresent && ready.ReconcileReadyValid && ready.ReconcileReady {
			result.Ready = true
		}
		if !ready.ReconcileReadyPresent || !ready.ReconcileReadyValid {
			return errors.Join(err, api.ErrSupervisorControlReadinessUnavailable)
		}
		if err != nil {
			return err
		}
		if result.Ready {
			return nil
		}
		select {
		case <-limit.Done():
			return limit.Err()
		case <-time.After(supervisorReadyPollInterval):
		}
	}
}

func (m *supervisorManager) stopRestartExternalOriginal(ctx context.Context, operation *supervisorManagerOperation) (settled, committed bool, retErr error) {
	held, err := process.HoldPIDForTermination(operation.target.PID)
	if errors.Is(err, process.ErrProcessAlreadyExited) {
		return true, false, nil
	}
	if err != nil {
		return false, false, err
	}
	if held == nil {
		return false, false, errors.New("held original generation is absent")
	}
	operation.held = held
	return m.stopHeldRestartOriginal(ctx, operation, held)
}

func (m *supervisorManager) stopHeldRestartOriginal(ctx context.Context, operation *supervisorManagerOperation, held process.HeldPIDGeneration) (settled, committed bool, retErr error) {
	waiter, ok := held.(process.HeldPIDExitWaiter)
	if !ok {
		return false, false, process.ErrProcessIdentityUnsupported
	}
	identityPort, ok := held.(process.HeldPIDIdentity)
	if !ok {
		return false, false, process.ErrProcessIdentityUnsupported
	}
	identity, err := identityPort.Identity()
	if err != nil {
		return false, false, err
	}
	originalStart, err := time.Parse(time.RFC3339Nano, operation.target.StartedAt)
	if err != nil || identity.PID != operation.target.PID || identity.StartedAt.IsZero() {
		return false, false, errors.Join(err, process.ErrProcessIdentityMismatch)
	}
	if !identity.StartedAt.Equal(originalStart) {
		// Positive native generation mismatch proves only the original exited;
		// this held replacement receives no control. Namespace proof is separate.
		return true, false, nil
	}
	proof := process.PIDIdentityProof{PID: operation.target.PID, StartedAt: operation.target.StartedAt, ExecutablePath: operation.expected.ProductPath}
	if err = held.VerifyIdentity(proof); err != nil {
		return false, false, err
	}
	peer, err := api.ObserveSupervisorProcessIdentityV1(operation.target.PID)
	if err != nil {
		return false, false, err
	}
	if err = api.ValidateSupervisorStatusServerIdentityV1(operation.target, peer, peer.UserSID, peer.SessionID, operation.expected.ProductPath); err != nil {
		return false, false, err
	}
	running, _, namespaceErr := api.SupervisorRunningUnderStateDir(operation.expected.StateDir)
	if namespaceErr != nil || !running {
		return false, false, errors.Join(errors.New("original live generation does not have a qualified held namespace"), namespaceErr)
	}
	if _, err = observeCreatedSupervisorControl(ctx, operation.expected, operation.target, peer, func(owner api.SupervisorLockOwner) error {
		return validateCreatedSupervisorNativeControl(operation.expected, owner)
	}, api.ReadSupervisorLockOwner); err != nil {
		return false, false, err
	}
	if err = held.VerifyIdentity(proof); err != nil {
		return false, false, err
	}
	if err = ctx.Err(); err != nil {
		return false, false, err
	}
	operation.externalOriginalQualified = true
	exitCtx, cancel := context.WithTimeout(ctx, time.Duration(defaultExitTimeoutMs)*time.Millisecond)
	response, exitErr := sendIPCWithResponseForPeer(exitCtx, api.SupervisorIPCAddress(operation.expected.StateDir), operation.target, peer, "exit", map[string]any{"graceful": true, "timeout_ms": defaultExitTimeoutMs}, time.Duration(defaultExitTimeoutMs)*time.Millisecond)
	// Only this immediate, direct canonical invocation supplies dispatch
	// evidence. Callback errors or joined history cannot earn this authority.
	if dispatch, ok := exitErr.(*supervisorIPCDispatchError); ok && dispatch != nil &&
		!dispatch.requestWriteEntered && dispatch.owner == operation.target && dispatch.peer == peer && dispatch.command == "exit" {
		cancel()
		// No independent-process exit obligation was acquired. The original
		// operation still receives its held Close and fence Release below.
		return false, false, exitErr
	}
	exitAccepted := response.OK && response.Final && response.Error == nil
	if exitErr == nil && !exitAccepted {
		exitErr = fmt.Errorf("original supervisor exit response is not final success: %v", response.Error)
	}
	retErr = exitErr
	if errors.Is(exitErr, api.ErrSupervisorControlPeerIdentity) || errors.Is(exitErr, api.ErrSupervisorControlPeerUnsupported) {
		cancel()
		return false, false, retErr
	}
	phaseWaitReceived := false
	if exitAccepted {
		observed, waitErr := m.receiveRestartNativeExit(operation, "exit", "request-grace", exitCtx, waiter)
		phaseWaitReceived = true
		retErr = errors.Join(retErr, waitErr)
		if observed.Exited {
			cancel()
			return true, false, retErr
		}
		if !errors.Is(waitErr, context.DeadlineExceeded) || ctx.Err() != nil {
			cancel()
			return m.joinRestartHeldOriginal(operation, waiter, false, errors.Join(retErr, errors.New("original cooperative exit observation is pending")), ctx, phaseWaitReceived)
		}
	}
	cancel()
	if ctx.Err() != nil {
		// The original operation retains custody below; cancellation grants no
		// destructive fallback against a process which has not positively exited.
		// First receive the actual phase wait/callback under its own canceled
		// context. Only then may the same executor passively join its custody.
		return m.joinRestartHeldOriginal(operation, waiter, false, errors.Join(retErr, ctx.Err()), ctx, phaseWaitReceived)
	}
	if response.Error != nil {
		return m.joinRestartHeldOriginal(operation, waiter, false, retErr, ctx, phaseWaitReceived)
	}
	if err = held.VerifyIdentity(proof); err != nil {
		if exitAccepted {
			return m.joinRestartHeldOriginal(operation, waiter, false, errors.Join(retErr, err), ctx, phaseWaitReceived)
		}
		return false, false, errors.Join(retErr, err)
	}
	if err = ctx.Err(); err != nil {
		return m.joinRestartHeldOriginal(operation, waiter, false, errors.Join(retErr, err), ctx, phaseWaitReceived)
	}
	committed, err = held.Terminate()
	retErr = errors.Join(retErr, err)
	if committed {
		m.mu.Lock()
		operation.result.Killed = true
		operation.result.KilledPID = operation.target.PID
		operation.err = errors.Join(operation.err, retErr)
		operation.publishResultLocked(operation.result)
		m.mu.Unlock()
	}
	if !committed && !errors.Is(err, process.ErrProcessAlreadyExited) {
		if exitAccepted {
			return m.joinRestartHeldOriginal(operation, waiter, false, retErr, ctx, phaseWaitReceived)
		}
		return false, false, retErr
	}
	cleanup, cleanupCancel := supervisorRestartCleanupContext()
	defer cleanupCancel()
	observed, waitErr := m.receiveRestartNativeExit(operation, "post-termination", "GUI-cleanup", cleanup, waiter)
	retErr = errors.Join(retErr, waitErr)
	if observed.Exited {
		return true, committed, retErr
	}
	return m.joinRestartHeldOriginal(operation, waiter, committed, errors.Join(retErr, errors.New("original held generation exit remains pending")), cleanup, true)
}

func (m *supervisorManager) joinRestartHeldOriginal(operation *supervisorManagerOperation, waiter process.HeldPIDExitWaiter, committed bool, firstErr error, phaseContext context.Context, phaseWaitReceived bool) (settled, terminated bool, retErr error) {
	// This SAME registered operation executor owns this passive continuation.
	// Receive a missing original phase before passive continuation. No new goroutine, finite
	// allowance, PID reopening, proof, control, termination or retry is admitted.
	if !phaseWaitReceived {
		observed, phaseErr := m.receiveRestartNativeExit(operation, "exit", "request", phaseContext, waiter)
		firstErr = errors.Join(firstErr, phaseErr)
		if observed.Exited {
			return true, committed, firstErr
		}
	}
	m.mu.Lock()
	operation.result.Outcome = gui.SupervisorRestartPending
	operation.result.Phase = "settlement"
	operation.result.Code = "RESTART_PENDING"
	operation.result.OriginalChildDisposition = "pending"
	operation.err = errors.Join(operation.err, firstErr)
	operation.publishResultLocked(operation.result)
	m.mu.Unlock()
	observed, joinErr := m.receiveRestartNativeExit(operation, "passive-join", "retained-GUI-custody", context.Background(), waiter)
	if !observed.Exited {
		operation.resourcesPending = true
	}
	return observed.Exited, committed, errors.Join(firstErr, joinErr)
}

func (m *supervisorManager) restartGUIOwned(ctx context.Context, operation *supervisorManagerOperation, result gui.SupervisorRestartResult, retErr error) (final gui.SupervisorRestartResult, finalErr error) {
	defer func() {
		m.mu.Lock()
		final = operation.snapshotResultLocked(final)
		m.mu.Unlock()
	}()
	result.Phase = "spawn"
	m.observeRestart(operation, result)
	if err := ctx.Err(); err != nil {
		return result, errors.Join(retErr, restartStepFailure(&result, "admission", "RESTART_CANCELED", err))
	}
	created, err := m.spawnFn(ctx, operation.expected.ProductPath, m.strictMode, m.waitFor, m.receiveCreationAttempt(operation))
	operation.createdOwner = created
	m.mu.Lock()
	if !operation.creation.Entered {
		result.OriginalChildDisposition = "unknown"
	}
	result = operation.snapshotResultLocked(result)
	m.mu.Unlock()
	if err != nil || created == nil || !created.Spawned() {
		retErr = errors.Join(retErr, restartStepFailure(&result, "spawn", "RESTART_NAMESPACE_CONFLICT", errors.Join(errors.New("created GUI replacement was not retained by its original creator"), err)))
		if created != nil && created.Spawned() {
			return m.settleGUIRestartCreator(operation, result, retErr)
		}
		if created != nil && created.createdPID > 0 {
			select {
			case <-created.exitDone:
				retErr = errors.Join(retErr, created.exitResult.exitErr)
			default:
				operation.resourcesPending = true
				result.Outcome = gui.SupervisorRestartPending
			}
		}
		if !operation.resourcesPending {
			retErr = errors.Join(retErr, m.receiveRestartNamespace(ctx, operation, &result))
		}
		return result, retErr
	}
	proof := created.control.proof
	if proof == nil {
		return m.settleGUIRestartCreator(operation, result, errors.Join(retErr, restartStepFailure(&result, "readiness", "RESTART_IDENTITY_UNCONFIRMED", errSupervisorControlBinding)))
	}
	operation.selected = proof.owner
	selected := proof.owner
	result.SelectedGeneration = &selected
	result.Phase = "readiness"
	m.observeRestart(operation, result)
	if err = m.waitRestartReady(ctx, operation, created.control, &result); err != nil {
		return m.settleGUIRestartCreator(operation, result, errors.Join(retErr, restartStepFailure(&result, "readiness", "RESTART_NOT_READY", err)))
	}
	m.mu.Lock()
	if m.operation != operation || m.current != operation.original || m.shuttingDown || ctx.Err() != nil || m.ctx.Err() != nil {
		m.mu.Unlock()
		return m.settleGUIRestartCreator(operation, result, errors.Join(retErr, restartStepFailure(&result, "commit", "RESTART_CANCELED", errors.New("GUI-owned replacement publication canceled"))))
	}
	m.current = created
	m.bin = operation.expected.ProductPath
	result.OriginalChildDisposition = "retained"
	result.NamespaceDisposition = "occupied"
	result.Phase = "complete"
	if retErr == nil {
		result.Outcome = gui.SupervisorRestartRestarted
		result.Code = ""
	} else {
		result.Outcome = gui.SupervisorRestartFailed
	}
	operation.publishResultLocked(result)
	m.mu.Unlock()
	return result, retErr
}

func (m *supervisorManager) settleGUIRestartCreator(operation *supervisorManagerOperation, result gui.SupervisorRestartResult, retErr error) (gui.SupervisorRestartResult, error) {
	m.observeRestart(operation, result)
	cleanup, cancel := supervisorRestartCleanupContext()
	retErr = errors.Join(retErr, m.stopOwnerAndJoin(cleanup, operation.createdOwner, defaultExitTimeoutMs))
	cancel()
	if operation.createdOwner == nil || operation.createdOwner.exitDone == nil {
		operation.resourcesPending = true
		result.Outcome = gui.SupervisorRestartPending
		result.Code = "RESTART_PENDING"
		return result, errors.Join(retErr, errors.New("original GUI creator completion is unavailable"))
	}
	select {
	case <-operation.createdOwner.exitDone:
	default:
		operation.resourcesPending = true
		result.Outcome = gui.SupervisorRestartPending
		result.Code = "RESTART_PENDING"
		return result, errors.Join(retErr, errors.New("original GUI creator completion is pending"))
	}
	// Child settlement cannot replace global namespace/port clearance.
	retErr = errors.Join(retErr, m.receiveRestartNamespace(m.ctx, operation, &result))
	return result, retErr
}

func (m *supervisorManager) restartStandalone(ctx context.Context, operation *supervisorManagerOperation, deps supervisorLaunchDependencies, ports []int, result gui.SupervisorRestartResult, retErr error) (gui.SupervisorRestartResult, error) {
	result.Phase = "spawn"
	m.observeRestart(operation, result)
	build := installSupervisorCmdBuilder(operation.expected.ProductPath, m.strictMode)
	if err := ctx.Err(); err != nil {
		return result, errors.Join(retErr, restartStepFailure(&result, "admission", "RESTART_CANCELED", err))
	}
	started, err := startSupervisorDetachedBreakaway(build(), build, nil, process.BreakawayRequired)
	if err != nil {
		return result, errors.Join(retErr, restartStepFailure(&result, "spawn", "RESTART_FAILED", err))
	}
	result.Spawned = true
	result.SpawnedPID = started.Process.Pid
	result.OriginalChildDisposition = "pending"
	m.observeRestart(operation, result)
	launch, err := newOwnedSupervisorLaunch(started, deps, ports)
	operation.launch = launch
	if launch == nil {
		// A real started command must never be discarded by a projection.
		operation.resourcesPending = true
		return result, errors.Join(retErr, restartStepFailure(&result, "settlement", "RESTART_PENDING", errors.Join(errors.New("actual original creator capability missing"), err)))
	}
	if err != nil {
		return m.settleStandaloneRestart(ctx, operation, launch, result, errors.Join(retErr, restartStepFailure(&result, "spawn", "RESTART_FAILED", err)))
	}
	result.Phase = "readiness"
	m.observeRestart(operation, result)
	binding, err := launch.BindControl(ctx, operation.expected)
	if err != nil {
		return m.settleStandaloneRestart(ctx, operation, launch, result, errors.Join(retErr, restartStepFailure(&result, "readiness", "RESTART_NOT_READY", err)))
	}
	operation.selected = binding.proof.owner
	selected := operation.selected
	result.SelectedGeneration = &selected
	if err = m.waitRestartReady(ctx, operation, binding, &result); err != nil {
		return m.settleStandaloneRestart(ctx, operation, launch, result, errors.Join(retErr, restartStepFailure(&result, "readiness", "RESTART_NOT_READY", err)))
	}
	result.Phase = "commit"
	m.observeRestart(operation, result)
	if err = m.commitSupervisorTransfer(ctx, operation, selected); err != nil {
		return m.settleStandaloneRestart(ctx, operation, launch, result, errors.Join(retErr, restartStepFailure(&result, "commit", "RESTART_CANCELED", err)))
	}
	result.TransferCommitted = true
	result.Phase = "transfer"
	m.observeRestart(operation, result)
	// Both manager and lifecycle locks have been released. This is the original
	// Product capability, with unchanged diagnostic cancellation semantics.
	handoffErr := launch.Handoff(ctx, binding)
	transferred, releaseErr := launch.handoffDisposition()
	result.Transferred = transferred
	retErr = errors.Join(retErr, handoffErr, releaseErr, ctx.Err(), m.ctx.Err())
	if !transferred {
		return m.settleStandaloneRestart(ctx, operation, launch, result, errors.Join(retErr, restartStepFailure(&result, "transfer", "RESTART_FAILED", errors.New("original handoff did not transfer custody"))))
	}
	// Actual transfer is irreversible by caller cancellation or Release error.
	// Ordinary Quit sees adopted DATA and never invokes Abort on this receiver.
	m.mu.Lock()
	m.current = &supervisorOwner{stateDir: operation.expected.StateDir}
	m.mu.Unlock()
	result.OriginalChildDisposition = "transferred"
	result.NamespaceDisposition = "occupied"
	result.Phase = "complete"
	if retErr == nil {
		result.Outcome = gui.SupervisorRestartRestarted
		result.Code = ""
	} else {
		restartStepFailure(&result, "transfer", "RESTART_FAILED", retErr)
	}
	return result, retErr
}

func (m *supervisorManager) settleStandaloneRestart(ctx context.Context, operation *supervisorManagerOperation, launch *ownedSupervisorLaunch, result gui.SupervisorRestartResult, retErr error) (gui.SupervisorRestartResult, error) {
	result.OriginalChildDisposition = "pending"
	m.observeRestart(operation, result)
	cleanup, cancel := supervisorRestartCleanupContext()
	abortErr := launch.Abort(cleanup)
	cancel()
	retErr = errors.Join(retErr, abortErr)
	received, waitErr := launch.originalChildOutcome()
	if !received {
		// Abort already started the unique original Wait. Join that original
		// completion without another Kill, Abort, Wait, spawn or control budget.
		result.Outcome = gui.SupervisorRestartPending
		result.Code = "RESTART_PENDING"
		m.observeRestart(operation, result)
		<-launch.waitDone
		received, waitErr = launch.originalChildOutcome()
	}
	retErr = errors.Join(retErr, waitErr)
	if !received {
		operation.resourcesPending = true
		return result, retErr
	}
	result.OriginalChildDisposition = "settled"
	result.Outcome = gui.SupervisorRestartFailed
	if err := m.receiveRestartNamespace(ctx, operation, &result); err != nil {
		retErr = errors.Join(retErr, err)
	}
	if result.Code == "RESTART_PENDING" {
		result.Code = "RESTART_FAILED"
	}
	return result, retErr
}

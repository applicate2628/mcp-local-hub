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
)

const supervisorCompatHandoffTimeout = time.Duration(defaultQuiesceTimeoutMs+defaultExitTimeoutMs) * time.Millisecond

func init() {
	// API owns the shared gate; this Windows composition root supplies the one
	// platform-specific legacy takeover used by CLI, GUI, tray and direct IPC
	// respawn callers in the product process.
	api.RegisterSupervisorControlAdmission(ensureSupervisorControlCompatibility)
}

var (
	// snapshot preserves the authenticated lock generation across the legacy
	// decision and the reaper transaction.
	supervisorControlCapabilitySnapshotFn = api.ProbeSupervisorControlCapabilitiesSnapshot
	supervisorControlGenerationCheckFn    = api.VerifySupervisorControlGeneration
	supervisorControlStateDirFn           = api.DaemonStateDir
	supervisorControlTargetPathFn         = setupTargetPath
	supervisorControlAdmitCurrentFn       = admitV5UpgradeCandidate
	supervisorControlBuildDepsFn          = buildV5UpgradeDeps
	supervisorControlReapFn               = ReapSupervisorForRestart
	supervisorControlReplaceLegacyFn      = replaceLegacySupervisorForControl
	supervisorControlExpectedPortsFn      = supervisorCompatExpectedPorts
	supervisorControlVerifyPortsFn        = verifyPortsUnboundForUpgrade
	supervisorControlForceKillOwnerFn     = func(deps *v5UpgradeDeps, owner api.SupervisorLockOwner) error {
		return deps.forceKillSupervisorForOwner(owner)
	}
	supervisorControlWaitLockReleasedFn = func(ctx context.Context, deps *v5UpgradeDeps, timeout time.Duration) error {
		return deps.WaitSupervisorLockReleased(ctx, timeout)
	}
	supervisorControlStartFn = func(deps *v5UpgradeDeps, target string) error {
		return deps.StartSupervisor(target)
	}
	supervisorControlWaitReadyFn = func(ctx context.Context, deps *v5UpgradeDeps, timeout time.Duration, target string, candidate UpgradeCandidateV1) error {
		return deps.WaitSupervisorReady(ctx, timeout, target, candidate)
	}
)

// ensureSupervisorControlCompatibility performs the authenticated, read-only
// capability decision before Stop/Restart reaches its first mutation. A current
// supervisor advertises both strict transactions and is left untouched. Only a
// proven legacy UNKNOWN_COMMAND response is replaced with the current canonical
// successor; transport/authentication failures never trigger a destructive
// fallback.
func ensureSupervisorControlCompatibility(ctx context.Context) error {
	probe, err := supervisorControlCapabilitySnapshotFn(ctx)
	if err == nil {
		return nil
	}
	if errors.Is(err, api.ErrSupervisorIPCUnavailable) {
		// No live supervisor is a normal legacy-scheduler condition. The API
		// operation owns its existing no-supervisor handling.
		return nil
	}
	if !errors.Is(err, api.ErrSupervisorCapabilityLegacy) {
		return fmt.Errorf("probe authenticated supervisor control capabilities before mutation: %w", err)
	}
	return supervisorControlReplaceLegacyFn(ctx, probe.Owner)
}

func replaceLegacySupervisorForControl(ctx context.Context, expectedOwner api.SupervisorLockOwner) (retErr error) {
	stateDir, err := supervisorControlStateDirFn()
	if err != nil {
		return fmt.Errorf("prepare legacy supervisor replacement: resolve state dir: %w", err)
	}
	fence, err := api.AcquireUpgradeFence(ctx, stateDir)
	if err != nil {
		return fmt.Errorf("prepare legacy supervisor replacement: acquire upgrade fence: %w", err)
	}
	defer func() {
		if releaseErr := fence.Release(); releaseErr != nil {
			retErr = errors.Join(retErr, fmt.Errorf("release legacy supervisor replacement fence: %w", releaseErr))
		}
	}()

	// Once this transaction owns the fence, completion cannot be abandoned by a
	// cancelled HTTP/CLI caller after it has reaped the old supervisor. A later
	// concurrent caller observes the current capabilities under the same fence
	// and performs no second reap.
	handoffCtx, cancel := supervisorCompatPhaseContext()
	defer cancel()
	probe, err := supervisorControlCapabilitySnapshotFn(handoffCtx)
	if err == nil || errors.Is(err, api.ErrSupervisorIPCUnavailable) {
		return nil
	}
	if !errors.Is(err, api.ErrSupervisorCapabilityLegacy) {
		return fmt.Errorf("prepare legacy supervisor replacement: re-probe current generation: %w", err)
	}
	if expectedOwner != (api.SupervisorLockOwner{}) && probe.Owner != expectedOwner {
		return fmt.Errorf("prepare legacy supervisor replacement: %w: legacy generation changed before fence admission", api.ErrSupervisorControlGenerationChanged)
	}
	if err := supervisorControlGenerationCheckFn(handoffCtx, probe.Owner); err != nil {
		return fmt.Errorf("verify legacy supervisor generation before replacement: %w: %v", api.ErrSupervisorControlGenerationChanged, err)
	}
	target, err := supervisorControlTargetPathFn()
	if err != nil {
		return fmt.Errorf("prepare legacy supervisor replacement: resolve canonical target: %w", err)
	}
	// Admit and hash-bind the successor before reaping the old generation: a
	// bad current executable must not turn a compatibility probe into outage.
	candidate, err := supervisorControlAdmitCurrentFn(target)
	if err != nil {
		return fmt.Errorf("prepare legacy supervisor replacement: admit current successor: %w", err)
	}
	expectedPorts, err := supervisorControlExpectedPortsFn(stateDir)
	if err != nil {
		return err
	}
	deps := supervisorControlBuildDepsFn(target, stateDir)
	// Bind the observed legacy generation to every control frame and force-kill
	// decision. If it exits and a current supervisor takes the pipe, hello or
	// sidecar equality fails loudly instead of stopping the successor.
	reapDeps := supervisorCompatReapDeps{base: deps, expectedOwner: expectedOwner}
	reapCtx, cancelReap := supervisorCompatPhaseContext()
	defer cancelReap()
	if err := supervisorControlReapFn(reapCtx, ReapOpts{
		PipePath:           deps.pipePath,
		ExpectedPorts:      expectedPorts,
		VerifyPortsUnbound: supervisorControlVerifyPortsFn,
		Deps:               reapDeps,
	}); err != nil {
		return fmt.Errorf("replace legacy supervisor before control mutation: %w", err)
	}
	lockCtx, cancelLock := supervisorCompatPhaseContext()
	defer cancelLock()
	if err := supervisorControlWaitLockReleasedFn(lockCtx, deps, supervisorCompatHandoffTimeout); err != nil {
		return settleLegacySupervisorLockReleaseFailure(deps, target, candidate, expectedOwner, expectedPorts,
			fmt.Errorf("replace legacy supervisor before control mutation: prior lock release: %w", err))
	}
	if err := supervisorControlStartFn(deps, target); err != nil {
		return recoverCurrentSuccessorAfterLegacyReap(deps, target, candidate, fmt.Errorf("start current successor: %w", err))
	}
	readyCtx, cancelReady := supervisorCompatPhaseContext()
	defer cancelReady()
	if err := supervisorControlWaitReadyFn(readyCtx, deps, supervisorCompatHandoffTimeout, target, candidate); err != nil {
		return recoverCurrentSuccessorAfterLegacyReap(deps, target, candidate, fmt.Errorf("current successor readiness: %w", err))
	}
	return nil
}

// settleLegacySupervisorLockReleaseFailure reclassifies an old lock that
// remained held after the initial exact-generation reap. The upgrade fence is
// still held by the caller. A current supervisor is accepted only after its
// canonical identity and readiness are proven. The same authenticated legacy
// owner receives one identity-gated force fallback, followed by fresh lock and
// port release proof. Any changed or otherwise unverifiable owner fails loud.
func settleLegacySupervisorLockReleaseFailure(deps *v5UpgradeDeps, target string, candidate UpgradeCandidateV1, expectedOwner api.SupervisorLockOwner, expectedPorts []int, trigger error) error {
	probeCtx, cancelProbe := supervisorCompatPhaseContext()
	defer cancelProbe()
	probe, probeErr := supervisorControlCapabilitySnapshotFn(probeCtx)
	if probeErr == nil {
		readyCtx, cancelReady := supervisorCompatPhaseContext()
		defer cancelReady()
		if err := supervisorControlWaitReadyFn(readyCtx, deps, supervisorCompatHandoffTimeout, target, candidate); err != nil {
			return fmt.Errorf("%w; current supervisor appeared while waiting for legacy lock release but is not admitted and ready: %w", trigger, err)
		}
		return nil
	}
	if !errors.Is(probeErr, api.ErrSupervisorCapabilityLegacy) {
		return fmt.Errorf("%w; supervisor ownership after legacy lock-release failure is unverifiable: %w", trigger, probeErr)
	}
	if probe.Owner != expectedOwner {
		return fmt.Errorf("%w; %w: legacy owner changed after reap: got pid=%d started_at=%s want pid=%d started_at=%s", trigger, api.ErrSupervisorControlGenerationChanged, probe.Owner.PID, probe.Owner.StartedAt, expectedOwner.PID, expectedOwner.StartedAt)
	}
	if err := supervisorControlGenerationCheckFn(probeCtx, expectedOwner); err != nil {
		return fmt.Errorf("%w; %w: cannot re-prove legacy owner before force fallback: %v", trigger, api.ErrSupervisorControlGenerationChanged, err)
	}
	if err := supervisorControlForceKillOwnerFn(deps, expectedOwner); err != nil && !isAlreadyExitedError(err) {
		return fmt.Errorf("%w; exact legacy force fallback failed: %w", trigger, err)
	}
	lockCtx, cancelLock := supervisorCompatPhaseContext()
	defer cancelLock()
	if err := supervisorControlWaitLockReleasedFn(lockCtx, deps, supervisorCompatHandoffTimeout); err != nil {
		return fmt.Errorf("%w; exact legacy force fallback completed but supervisor.lock remained held: %w", trigger, err)
	}
	if len(expectedPorts) != 0 && supervisorControlVerifyPortsFn != nil {
		if err := supervisorControlVerifyPortsFn(expectedPorts, api.DefaultSupervisorReapPortReleaseTimeout); err != nil {
			return fmt.Errorf("%w; exact legacy force fallback completed but expected ports remained bound: %w", trigger, err)
		}
	}
	if err := supervisorControlStartFn(deps, target); err != nil {
		return recoverCurrentSuccessorAfterLegacyReap(deps, target, candidate, fmt.Errorf("start current successor after exact legacy force fallback: %w", err))
	}
	readyCtx, cancelReady := supervisorCompatPhaseContext()
	defer cancelReady()
	if err := supervisorControlWaitReadyFn(readyCtx, deps, supervisorCompatHandoffTimeout, target, candidate); err != nil {
		return recoverCurrentSuccessorAfterLegacyReap(deps, target, candidate, fmt.Errorf("current successor readiness after exact legacy force fallback: %w", err))
	}
	return nil
}

func supervisorCompatPhaseContext() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), supervisorCompatHandoffTimeout)
}

// recoverCurrentSuccessorAfterLegacyReap makes one independently-budgeted
// recovery attempt after the irreversible old-supervisor reap. Its outcome is
// joined to the original failure: the caller receives both the failed primary
// phase and whether a usable current successor was recovered.
func recoverCurrentSuccessorAfterLegacyReap(deps *v5UpgradeDeps, target string, candidate UpgradeCandidateV1, primary error) error {
	if err := supervisorControlStartFn(deps, target); err != nil {
		return errors.Join(primary, fmt.Errorf("bounded successor recovery start: %w", err))
	}
	recoveryCtx, cancel := supervisorCompatPhaseContext()
	defer cancel()
	if err := supervisorControlWaitReadyFn(recoveryCtx, deps, supervisorCompatHandoffTimeout, target, candidate); err != nil {
		return errors.Join(primary, fmt.Errorf("bounded successor recovery readiness: %w", err))
	}
	return fmt.Errorf("%w; current successor recovered after legacy reap", primary)
}

type supervisorCompatReapDeps struct {
	base          *v5UpgradeDeps
	expectedOwner api.SupervisorLockOwner
}

func (d supervisorCompatReapDeps) QuiesceTimers(ctx context.Context, pipePath string, timeoutMs int) (api.IPCResponse, error) {
	return sendIPCWithResponse(ctx, pipePath, d.expectedOwner, "quiesce-timers", map[string]any{"timeout_ms": timeoutMs}, time.Duration(timeoutMs+5000)*time.Millisecond)
}

func (d supervisorCompatReapDeps) ExitGraceful(ctx context.Context, pipePath string, timeoutMs int) (api.IPCResponse, error) {
	return sendIPCWithResponse(ctx, pipePath, d.expectedOwner, "exit", map[string]any{"graceful": true, "timeout_ms": timeoutMs}, time.Duration(timeoutMs+5000)*time.Millisecond)
}

func (d supervisorCompatReapDeps) ForceKillSupervisor(string) error {
	return supervisorControlForceKillOwnerFn(d.base, d.expectedOwner)
}

func supervisorCompatExpectedPorts(stateDir string) ([]int, error) {
	intentPath := filepath.Join(stateDir, "supervisor-intent.json")
	intent, err := api.ReadSupervisorIntent(intentPath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("prepare legacy supervisor replacement: read supervisor intent: %w", err)
	}
	if intent == nil {
		return nil, nil
	}
	ports := make([]int, 0, len(intent.Daemons))
	seen := make(map[int]struct{}, len(intent.Daemons))
	for _, daemon := range intent.Daemons {
		port, ok := api.EffectiveDaemonPort(daemon)
		if !ok || port <= 0 {
			continue
		}
		if _, duplicate := seen[port]; duplicate {
			continue
		}
		seen[port] = struct{}{}
		ports = append(ports, port)
	}
	return ports, nil
}

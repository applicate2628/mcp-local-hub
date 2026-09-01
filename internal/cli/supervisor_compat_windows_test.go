//go:build windows

package cli

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"mcp-local-hub/internal/api"
)

func TestEnsureSupervisorControlCompatibility_HistoricalV0432RoutesOnceToReaper(t *testing.T) {
	oldProbe := supervisorControlCapabilitySnapshotFn
	oldGenerationCheck := supervisorControlGenerationCheckFn
	oldReplace := supervisorControlReplaceLegacyFn
	t.Cleanup(func() {
		supervisorControlCapabilitySnapshotFn = oldProbe
		supervisorControlGenerationCheckFn = oldGenerationCheck
		supervisorControlReplaceLegacyFn = oldReplace
	})
	wantOwner := api.SupervisorLockOwner{PID: 42, StartedAt: "2026-09-01T00:00:00Z"}
	supervisorControlCapabilitySnapshotFn = func(context.Context) (api.SupervisorControlProbe, error) {
		return api.SupervisorControlProbe{Owner: wantOwner}, fmt.Errorf("0.4.32 UNKNOWN_COMMAND: %w", api.ErrSupervisorCapabilityLegacy)
	}
	supervisorControlGenerationCheckFn = func(_ context.Context, got api.SupervisorLockOwner) error {
		if got != wantOwner {
			t.Fatalf("generation owner=%+v want %+v", got, wantOwner)
		}
		return nil
	}
	replacements := 0
	supervisorControlReplaceLegacyFn = func(_ context.Context, got api.SupervisorLockOwner) error {
		if got != wantOwner {
			t.Fatalf("reaper owner=%+v want %+v", got, wantOwner)
		}
		replacements++
		return nil
	}
	if err := ensureSupervisorControlCompatibility(context.Background()); err != nil {
		t.Fatal(err)
	}
	if replacements != 1 {
		t.Fatalf("legacy supervisor replacements=%d, want exactly one reaper handoff", replacements)
	}
}

func TestEnsureSupervisorControlCompatibility_AuthenticationFailureNeverReaps(t *testing.T) {
	oldProbe := supervisorControlCapabilitySnapshotFn
	oldReplace := supervisorControlReplaceLegacyFn
	t.Cleanup(func() {
		supervisorControlCapabilitySnapshotFn = oldProbe
		supervisorControlReplaceLegacyFn = oldReplace
	})
	supervisorControlCapabilitySnapshotFn = func(context.Context) (api.SupervisorControlProbe, error) {
		return api.SupervisorControlProbe{}, errors.New("hello mismatch")
	}
	supervisorControlReplaceLegacyFn = func(context.Context, api.SupervisorLockOwner) error {
		t.Fatal("unauthenticated probe failure reached legacy reaper")
		return nil
	}
	if err := ensureSupervisorControlCompatibility(context.Background()); err == nil {
		t.Fatal("unauthenticated probe failure was accepted")
	}
}

func TestEnsureSupervisorControlCompatibility_CurrentCapabilitiesStayStrict(t *testing.T) {
	oldProbe := supervisorControlCapabilitySnapshotFn
	oldReplace := supervisorControlReplaceLegacyFn
	t.Cleanup(func() {
		supervisorControlCapabilitySnapshotFn = oldProbe
		supervisorControlReplaceLegacyFn = oldReplace
	})
	supervisorControlCapabilitySnapshotFn = func(context.Context) (api.SupervisorControlProbe, error) {
		return api.SupervisorControlProbe{Capabilities: api.SupervisorControlCapabilities{StopBatch: true, Respawn: true}}, nil
	}
	supervisorControlReplaceLegacyFn = func(context.Context, api.SupervisorLockOwner) error {
		t.Fatal("current capable supervisor reached legacy reaper")
		return nil
	}
	if err := ensureSupervisorControlCompatibility(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestEnsureSupervisorControlCompatibility_IncompleteCapabilitiesNeverReap(t *testing.T) {
	oldProbe := supervisorControlCapabilitySnapshotFn
	oldReplace := supervisorControlReplaceLegacyFn
	t.Cleanup(func() {
		supervisorControlCapabilitySnapshotFn = oldProbe
		supervisorControlReplaceLegacyFn = oldReplace
	})
	supervisorControlCapabilitySnapshotFn = func(context.Context) (api.SupervisorControlProbe, error) {
		return api.SupervisorControlProbe{Capabilities: api.SupervisorControlCapabilities{StopBatch: true}}, api.ErrSupervisorCapabilityIncomplete
	}
	supervisorControlReplaceLegacyFn = func(context.Context, api.SupervisorLockOwner) error {
		t.Fatal("incomplete current capabilities reached legacy reaper")
		return nil
	}
	if err := ensureSupervisorControlCompatibility(context.Background()); !errors.Is(err, api.ErrSupervisorCapabilityIncomplete) {
		t.Fatalf("error=%v want incomplete capability refusal", err)
	}
}

func TestEnsureSupervisorControlCompatibility_GenerationChangeUnderFenceNeverReapsOrStarts(t *testing.T) {
	oldProbe := supervisorControlCapabilitySnapshotFn
	oldGenerationCheck := supervisorControlGenerationCheckFn
	oldReplace := supervisorControlReplaceLegacyFn
	oldStateDir := supervisorControlStateDirFn
	oldTarget := supervisorControlTargetPathFn
	oldAdmit := supervisorControlAdmitCurrentFn
	oldBuild := supervisorControlBuildDepsFn
	oldReap := supervisorControlReapFn
	oldExpectedPorts := supervisorControlExpectedPortsFn
	oldVerifyPorts := supervisorControlVerifyPortsFn
	oldForceKill := supervisorControlForceKillOwnerFn
	oldStart := supervisorControlStartFn
	t.Cleanup(func() {
		supervisorControlCapabilitySnapshotFn = oldProbe
		supervisorControlGenerationCheckFn = oldGenerationCheck
		supervisorControlReplaceLegacyFn = oldReplace
		supervisorControlStateDirFn = oldStateDir
		supervisorControlTargetPathFn = oldTarget
		supervisorControlAdmitCurrentFn = oldAdmit
		supervisorControlBuildDepsFn = oldBuild
		supervisorControlReapFn = oldReap
		supervisorControlExpectedPortsFn = oldExpectedPorts
		supervisorControlVerifyPortsFn = oldVerifyPorts
		supervisorControlForceKillOwnerFn = oldForceKill
		supervisorControlStartFn = oldStart
	})
	initial := api.SupervisorLockOwner{PID: 42, StartedAt: "old"}
	changed := api.SupervisorLockOwner{PID: 43, StartedAt: "new"}
	probes := 0
	supervisorControlCapabilitySnapshotFn = func(context.Context) (api.SupervisorControlProbe, error) {
		probes++
		owner := initial
		if probes == 2 {
			owner = changed
		}
		return api.SupervisorControlProbe{Owner: owner}, api.ErrSupervisorCapabilityLegacy
	}
	supervisorControlStateDirFn = func() (string, error) { return t.TempDir(), nil }
	supervisorControlTargetPathFn = func() (string, error) { return `C:\fixture\mcphub.exe`, nil }
	supervisorControlAdmitCurrentFn = func(string) (UpgradeCandidateV1, error) { return UpgradeCandidateV1{}, nil }
	supervisorControlBuildDepsFn = func(string, string) *v5UpgradeDeps { return &v5UpgradeDeps{} }
	reaps, kills, starts := 0, 0, 0
	supervisorControlReapFn = func(context.Context, ReapOpts) error { reaps++; return nil }
	supervisorControlExpectedPortsFn = func(string) ([]int, error) { return nil, nil }
	supervisorControlVerifyPortsFn = func([]int, time.Duration) error { return nil }
	supervisorControlForceKillOwnerFn = func(*v5UpgradeDeps, api.SupervisorLockOwner) error { kills++; return nil }
	supervisorControlStartFn = func(*v5UpgradeDeps, string) error { starts++; return nil }
	supervisorControlGenerationCheckFn = func(context.Context, api.SupervisorLockOwner) error {
		t.Fatal("changed under-fence owner reached generation check")
		return nil
	}
	supervisorControlReplaceLegacyFn = replaceLegacySupervisorForControl
	err := ensureSupervisorControlCompatibility(context.Background())
	if !errors.Is(err, api.ErrSupervisorControlGenerationChanged) {
		t.Fatalf("error=%v want typed changed-generation refusal", err)
	}
	if probes != 2 || reaps != 0 || kills != 0 || starts != 0 {
		t.Fatalf("probes=%d reaps=%d kills=%d starts=%d; changed under-fence generation must have no destructive action", probes, reaps, kills, starts)
	}
}

func TestReplaceLegacySupervisorForControl_StuckLegacyLockUsesOneExactForceFallback(t *testing.T) {
	oldSnapshot := supervisorControlCapabilitySnapshotFn
	oldStateDir := supervisorControlStateDirFn
	oldTarget := supervisorControlTargetPathFn
	oldAdmit := supervisorControlAdmitCurrentFn
	oldBuild := supervisorControlBuildDepsFn
	oldReap := supervisorControlReapFn
	oldGeneration := supervisorControlGenerationCheckFn
	oldExpectedPorts := supervisorControlExpectedPortsFn
	oldVerifyPorts := supervisorControlVerifyPortsFn
	oldForceKill := supervisorControlForceKillOwnerFn
	oldWaitLock := supervisorControlWaitLockReleasedFn
	oldStart := supervisorControlStartFn
	oldReady := supervisorControlWaitReadyFn
	t.Cleanup(func() {
		supervisorControlCapabilitySnapshotFn = oldSnapshot
		supervisorControlStateDirFn = oldStateDir
		supervisorControlTargetPathFn = oldTarget
		supervisorControlAdmitCurrentFn = oldAdmit
		supervisorControlBuildDepsFn = oldBuild
		supervisorControlReapFn = oldReap
		supervisorControlGenerationCheckFn = oldGeneration
		supervisorControlExpectedPortsFn = oldExpectedPorts
		supervisorControlVerifyPortsFn = oldVerifyPorts
		supervisorControlForceKillOwnerFn = oldForceKill
		supervisorControlWaitLockReleasedFn = oldWaitLock
		supervisorControlStartFn = oldStart
		supervisorControlWaitReadyFn = oldReady
	})

	owner := api.SupervisorLockOwner{PID: 42, StartedAt: "legacy"}
	stateDir := t.TempDir()
	probes, reaps, generationChecks, forceKills := 0, 0, 0, 0
	lockWaits, portChecks, starts, readies := 0, 0, 0, 0
	supervisorControlCapabilitySnapshotFn = func(context.Context) (api.SupervisorControlProbe, error) {
		probes++
		return api.SupervisorControlProbe{Owner: owner}, api.ErrSupervisorCapabilityLegacy
	}
	supervisorControlStateDirFn = func() (string, error) { return stateDir, nil }
	supervisorControlTargetPathFn = func() (string, error) { return `C:\fixture\mcphub.exe`, nil }
	candidate := UpgradeCandidateV1{Admission: UpgradeAdmissionLocalProduct, SHA256: "candidate"}
	supervisorControlAdmitCurrentFn = func(string) (UpgradeCandidateV1, error) { return candidate, nil }
	supervisorControlBuildDepsFn = func(string, string) *v5UpgradeDeps { return &v5UpgradeDeps{pipePath: "fixture-pipe"} }
	supervisorControlReapFn = func(context.Context, ReapOpts) error { reaps++; return nil }
	supervisorControlGenerationCheckFn = func(context.Context, api.SupervisorLockOwner) error { generationChecks++; return nil }
	supervisorControlExpectedPortsFn = func(string) ([]int, error) { return []int{9304}, nil }
	supervisorControlVerifyPortsFn = func(ports []int, timeout time.Duration) error {
		portChecks++
		if len(ports) != 1 || ports[0] != 9304 || timeout != api.DefaultSupervisorReapPortReleaseTimeout {
			t.Fatalf("port re-proof ports=%v timeout=%s", ports, timeout)
		}
		return nil
	}
	supervisorControlForceKillOwnerFn = func(_ *v5UpgradeDeps, got api.SupervisorLockOwner) error {
		forceKills++
		if got != owner {
			t.Fatalf("force owner=%+v want %+v", got, owner)
		}
		return nil
	}
	supervisorControlWaitLockReleasedFn = func(context.Context, *v5UpgradeDeps, time.Duration) error {
		lockWaits++
		if lockWaits == 1 {
			return errors.New("legacy lock still held")
		}
		return nil
	}
	supervisorControlStartFn = func(*v5UpgradeDeps, string) error { starts++; return nil }
	supervisorControlWaitReadyFn = func(context.Context, *v5UpgradeDeps, time.Duration, string, UpgradeCandidateV1) error {
		readies++
		return nil
	}

	if err := replaceLegacySupervisorForControl(context.Background(), owner); err != nil {
		t.Fatal(err)
	}
	if probes != 2 || reaps != 1 || generationChecks != 2 || forceKills != 1 || lockWaits != 2 || portChecks != 1 || starts != 1 || readies != 1 {
		t.Fatalf("probes=%d reaps=%d generation=%d force=%d lock=%d ports=%d starts=%d ready=%d", probes, reaps, generationChecks, forceKills, lockWaits, portChecks, starts, readies)
	}
}

func TestReplaceLegacySupervisorForControl_LockFailureCurrentSuccessorReadyAcceptsWithoutReplacement(t *testing.T) {
	oldSnapshot := supervisorControlCapabilitySnapshotFn
	oldStateDir := supervisorControlStateDirFn
	oldTarget := supervisorControlTargetPathFn
	oldAdmit := supervisorControlAdmitCurrentFn
	oldBuild := supervisorControlBuildDepsFn
	oldReap := supervisorControlReapFn
	oldGeneration := supervisorControlGenerationCheckFn
	oldExpectedPorts := supervisorControlExpectedPortsFn
	oldVerifyPorts := supervisorControlVerifyPortsFn
	oldForceKill := supervisorControlForceKillOwnerFn
	oldWaitLock := supervisorControlWaitLockReleasedFn
	oldStart := supervisorControlStartFn
	oldReady := supervisorControlWaitReadyFn
	t.Cleanup(func() {
		supervisorControlCapabilitySnapshotFn = oldSnapshot
		supervisorControlStateDirFn = oldStateDir
		supervisorControlTargetPathFn = oldTarget
		supervisorControlAdmitCurrentFn = oldAdmit
		supervisorControlBuildDepsFn = oldBuild
		supervisorControlReapFn = oldReap
		supervisorControlGenerationCheckFn = oldGeneration
		supervisorControlExpectedPortsFn = oldExpectedPorts
		supervisorControlVerifyPortsFn = oldVerifyPorts
		supervisorControlForceKillOwnerFn = oldForceKill
		supervisorControlWaitLockReleasedFn = oldWaitLock
		supervisorControlStartFn = oldStart
		supervisorControlWaitReadyFn = oldReady
	})

	owner := api.SupervisorLockOwner{PID: 42, StartedAt: "legacy"}
	stateDir := t.TempDir()
	probes, reaps, forceKills, starts, readies := 0, 0, 0, 0, 0
	supervisorControlCapabilitySnapshotFn = func(context.Context) (api.SupervisorControlProbe, error) {
		probes++
		if probes == 2 {
			return api.SupervisorControlProbe{Capabilities: api.SupervisorControlCapabilities{StopBatch: true, Respawn: true}}, nil
		}
		return api.SupervisorControlProbe{Owner: owner}, api.ErrSupervisorCapabilityLegacy
	}
	supervisorControlStateDirFn = func() (string, error) { return stateDir, nil }
	supervisorControlTargetPathFn = func() (string, error) { return `C:\fixture\mcphub.exe`, nil }
	candidate := UpgradeCandidateV1{Admission: UpgradeAdmissionLocalProduct, SHA256: "candidate"}
	supervisorControlAdmitCurrentFn = func(string) (UpgradeCandidateV1, error) { return candidate, nil }
	supervisorControlBuildDepsFn = func(string, string) *v5UpgradeDeps { return &v5UpgradeDeps{} }
	supervisorControlGenerationCheckFn = func(context.Context, api.SupervisorLockOwner) error { return nil }
	supervisorControlExpectedPortsFn = func(string) ([]int, error) { return nil, nil }
	supervisorControlVerifyPortsFn = func([]int, time.Duration) error { return nil }
	supervisorControlReapFn = func(context.Context, ReapOpts) error { reaps++; return nil }
	supervisorControlForceKillOwnerFn = func(*v5UpgradeDeps, api.SupervisorLockOwner) error { forceKills++; return nil }
	supervisorControlWaitLockReleasedFn = func(context.Context, *v5UpgradeDeps, time.Duration) error {
		return errors.New("legacy lock still held")
	}
	supervisorControlStartFn = func(*v5UpgradeDeps, string) error { starts++; return nil }
	supervisorControlWaitReadyFn = func(context.Context, *v5UpgradeDeps, time.Duration, string, UpgradeCandidateV1) error {
		readies++
		return nil
	}

	if err := replaceLegacySupervisorForControl(context.Background(), owner); err != nil {
		t.Fatal(err)
	}
	if probes != 2 || reaps != 1 || forceKills != 0 || starts != 0 || readies != 1 {
		t.Fatalf("probes=%d reaps=%d force=%d starts=%d ready=%d; current ready successor must be accepted without a second replacement", probes, reaps, forceKills, starts, readies)
	}
}

func TestReplaceLegacySupervisorForControl_FenceSerializesAndCompletesAfterCallerCancel(t *testing.T) {
	oldSnapshot := supervisorControlCapabilitySnapshotFn
	oldStateDir := supervisorControlStateDirFn
	oldTarget := supervisorControlTargetPathFn
	oldAdmit := supervisorControlAdmitCurrentFn
	oldBuild := supervisorControlBuildDepsFn
	oldReap := supervisorControlReapFn
	oldGeneration := supervisorControlGenerationCheckFn
	oldWaitLock := supervisorControlWaitLockReleasedFn
	oldStart := supervisorControlStartFn
	oldReady := supervisorControlWaitReadyFn
	t.Cleanup(func() {
		supervisorControlCapabilitySnapshotFn = oldSnapshot
		supervisorControlStateDirFn = oldStateDir
		supervisorControlTargetPathFn = oldTarget
		supervisorControlAdmitCurrentFn = oldAdmit
		supervisorControlBuildDepsFn = oldBuild
		supervisorControlReapFn = oldReap
		supervisorControlGenerationCheckFn = oldGeneration
		supervisorControlWaitLockReleasedFn = oldWaitLock
		supervisorControlStartFn = oldStart
		supervisorControlWaitReadyFn = oldReady
	})

	owner := api.SupervisorLockOwner{PID: 42, StartedAt: "legacy"}
	stateDir := t.TempDir()
	var mu sync.Mutex
	probes := 0
	supervisorControlCapabilitySnapshotFn = func(context.Context) (api.SupervisorControlProbe, error) {
		mu.Lock()
		defer mu.Unlock()
		probes++
		if probes == 1 {
			return api.SupervisorControlProbe{Owner: owner}, fmt.Errorf("%w: %w", api.ErrSupervisorCapabilityUnsupported, api.ErrSupervisorCapabilityLegacy)
		}
		return api.SupervisorControlProbe{Capabilities: api.SupervisorControlCapabilities{StopBatch: true, Respawn: true}}, nil
	}
	supervisorControlStateDirFn = func() (string, error) { return stateDir, nil }
	supervisorControlTargetPathFn = func() (string, error) { return `C:\fixture\mcphub.exe`, nil }
	supervisorControlAdmitCurrentFn = func(string) (UpgradeCandidateV1, error) { return UpgradeCandidateV1{}, nil }
	supervisorControlGenerationCheckFn = func(context.Context, api.SupervisorLockOwner) error { return nil }

	reapEntered := make(chan struct{})
	reapRelease := make(chan struct{})
	reaps := 0
	supervisorControlReapFn = func(context.Context, ReapOpts) error {
		mu.Lock()
		reaps++
		mu.Unlock()
		close(reapEntered)
		<-reapRelease
		return nil
	}
	settled := 0
	supervisorControlWaitLockReleasedFn = func(ctx context.Context, _ *v5UpgradeDeps, _ time.Duration) error {
		if ctx.Err() != nil {
			t.Fatalf("handoff lock wait inherited cancelled caller context: %v", ctx.Err())
		}
		settled++
		return nil
	}
	supervisorControlStartFn = func(_ *v5UpgradeDeps, _ string) error { settled++; return nil }
	supervisorControlWaitReadyFn = func(ctx context.Context, _ *v5UpgradeDeps, _ time.Duration, _ string, _ UpgradeCandidateV1) error {
		if ctx.Err() != nil {
			t.Fatalf("successor readiness inherited cancelled caller context: %v", ctx.Err())
		}
		settled++
		return nil
	}

	callerCtx, cancel := context.WithCancel(context.Background())
	firstDone := make(chan error, 1)
	go func() { firstDone <- replaceLegacySupervisorForControl(callerCtx, owner) }()
	select {
	case <-reapEntered:
	case <-time.After(time.Second):
		t.Fatal("first replacement did not reach reap")
	}
	secondDone := make(chan error, 1)
	go func() { secondDone <- replaceLegacySupervisorForControl(context.Background(), owner) }()
	cancel()
	close(reapRelease)
	if err := <-firstDone; err != nil {
		t.Fatalf("first replacement: %v", err)
	}
	if err := <-secondDone; err != nil {
		t.Fatalf("second replacement: %v", err)
	}
	mu.Lock()
	gotReaps, gotProbes := reaps, probes
	mu.Unlock()
	if gotReaps != 1 || gotProbes != 2 {
		t.Fatalf("reaps=%d probes=%d want exactly one reap then one current re-probe", gotReaps, gotProbes)
	}
	if settled != 3 {
		t.Fatalf("settlement calls=%d want lock/start/readiness after caller cancellation", settled)
	}
}

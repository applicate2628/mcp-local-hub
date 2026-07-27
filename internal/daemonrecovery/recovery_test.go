package daemonrecovery

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gofrs/flock"

	"mcp-local-hub/internal/api"
	"mcp-local-hub/internal/process"
)

type recoveryCallLog struct {
	probe     int
	terminate []process.PIDIdentityProof
	respawn   []respawnCall
	order     []string
}

type respawnCall struct {
	task  string
	force bool
}

type fakeHeldPIDGeneration struct {
	pid        int
	lastProof  process.PIDIdentityProof
	verify     func(process.PIDIdentityProof) error
	terminate  func() (bool, error)
	closeCalls int
}

func (f *fakeHeldPIDGeneration) PID() int { return f.pid }

func (f *fakeHeldPIDGeneration) VerifyIdentity(proof process.PIDIdentityProof) error {
	f.lastProof = proof
	if f.verify != nil {
		return f.verify(proof)
	}
	if proof.PID != f.pid {
		return process.ErrProcessIdentityMismatch
	}
	return nil
}

func (f *fakeHeldPIDGeneration) Terminate() (bool, error) {
	if f.terminate != nil {
		return f.terminate()
	}
	return true, nil
}

func (f *fakeHeldPIDGeneration) Close() error {
	f.closeCalls++
	return nil
}

func recoveryDescriptor() api.SupervisorDaemon {
	return api.SupervisorDaemon{
		TaskName: `\mcp-local-hub-memory-default`,
		Server:   "memory",
		Daemon:   "default",
		Command:  `C:\mcphub.exe`,
		Args:     []string{"daemon", "--server", "memory", "--daemon", "default"},
		Port:     9123,
	}
}

func recoveryIdentity(pid int, d api.SupervisorDaemon) process.ProcessIdentity {
	return process.ProcessIdentity{
		PID:              pid,
		Basename:         "mcphub.exe",
		ExecutablePath:   d.Command,
		CommandLine:      `C:\mcphub.exe daemon --server memory --daemon default`,
		CreationDateUnix: time.Now().Add(-time.Minute).Unix(),
	}
}

func recoveryDependencies(t *testing.T, calls *recoveryCallLog, d api.SupervisorDaemon) Dependencies {
	t.Helper()
	stateDir := t.TempDir()
	return Dependencies{
		StateDir: func() (string, error) { return stateDir, nil },
		ReadIntent: func(path string) (*api.SupervisorIntentFile, error) {
			if filepath.Base(path) != "supervisor-intent.json" {
				t.Fatalf("intent path = %q", path)
			}
			return &api.SupervisorIntentFile{Version: 1, Daemons: []api.SupervisorDaemon{d}}, nil
		},
		ReadState: func(string) (*api.SupervisorStateFile, error) {
			return &api.SupervisorStateFile{Daemons: map[string]api.SupervisorDaemonState{
				d.TaskName: {CurrentPID: 22036},
			}}, nil
		},
		PortOwner: func(context.Context, int) (int, bool, error) {
			calls.probe++
			return 0, false, nil
		},
		SelfPID: func() int { return 1 },
		LookupIdentity: func(_ context.Context, pid int) (process.ProcessIdentity, error) {
			return recoveryIdentity(pid, d), nil
		},
		ExecutableMatches: func(int, string) bool { return true },
		HoldProcess: func(pid int) (process.HeldPIDGeneration, error) {
			generation := &fakeHeldPIDGeneration{pid: pid}
			generation.terminate = func() (bool, error) {
				calls.order = append(calls.order, "terminate")
				calls.terminate = append(calls.terminate, generation.lastProof)
				return true, nil
			}
			return generation, nil
		},
		ProbeSupervisor: func(context.Context) error { return nil },
		Respawn: func(_ context.Context, task string, force bool) (api.RespawnResult, error) {
			calls.order = append(calls.order, "respawn")
			calls.respawn = append(calls.respawn, respawnCall{task: task, force: force})
			return api.RespawnResult{Success: true}, nil
		},
		Now:              time.Now,
		Sleep:            func(context.Context, time.Duration) error { return nil },
		PortPollInterval: time.Millisecond,
		PortWaitTimeout:  10 * time.Millisecond,
		PostKillTimeout:  100 * time.Millisecond,
		RespawnReserve:   20 * time.Millisecond,
	}
}

func requireFailureKind(t *testing.T, err error, want FailureKind) *OperationError {
	t.Helper()
	var opErr *OperationError
	if !errors.As(err, &opErr) {
		t.Fatalf("error = %v, want *OperationError", err)
	}
	if opErr.Kind != want {
		t.Fatalf("failure kind = %q, want %q", opErr.Kind, want)
	}
	return opErr
}

func recoveryPendingCarrierCount(t *testing.T, stateDir string) int {
	t.Helper()
	matches, err := filepath.Glob(filepath.Join(
		stateDir,
		api.SupervisorEventLogFileLeaf+".pending",
		"*.jsonl",
	))
	if err != nil {
		t.Fatalf("glob committed-audit handoffs: %v", err)
	}
	return len(matches)
}

func replayRecoveryPending(t *testing.T, stateDir string) {
	t.Helper()
	logger, err := api.OpenSupervisorEventLog(filepath.Join(stateDir, api.SupervisorEventLogFileLeaf))
	if err != nil {
		t.Fatalf("open supervisor event log for replay: %v", err)
	}
	defer func() { _ = logger.Close() }()
	if err := logger.TryReplayPending(); err != nil {
		t.Fatalf("replay committed-audit handoff: %v", err)
	}
}

func TestExecuteUnknownTaskDoesNotProbeTerminateOrRespawn(t *testing.T) {
	d := recoveryDescriptor()
	calls := &recoveryCallLog{}
	deps := recoveryDependencies(t, calls, d)

	_, err := ExecuteWithDependencies(context.Background(), `\mcp-local-hub-missing`, Options{Confirmed: true}, deps)
	requireFailureKind(t, err, FailureUnknownTask)
	if calls.probe != 0 || len(calls.terminate) != 0 || len(calls.respawn) != 0 {
		t.Fatalf("unknown task side effects: probes=%d terminate=%d respawn=%d", calls.probe, len(calls.terminate), len(calls.respawn))
	}
}

func TestExecuteForeignAndUnverifiedOwnerRefuseWithoutTerminateOrRespawn(t *testing.T) {
	d := recoveryDescriptor()
	const ownerPID = 44000
	tests := []struct {
		name string
		edit func(*Dependencies)
	}{
		{
			name: "foreign executable",
			edit: func(deps *Dependencies) {
				deps.HoldProcess = func(pid int) (process.HeldPIDGeneration, error) {
					return &fakeHeldPIDGeneration{
						pid: pid,
						verify: func(process.PIDIdentityProof) error {
							return process.ErrProcessIdentityMismatch
						},
					}, nil
				}
			},
		},
		{
			name: "unverifiable identity",
			edit: func(deps *Dependencies) {
				deps.LookupIdentity = func(context.Context, int) (process.ProcessIdentity, error) {
					return process.ProcessIdentity{}, errors.New("identity unavailable")
				}
			},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			calls := &recoveryCallLog{}
			deps := recoveryDependencies(t, calls, d)
			deps.PortOwner = func(context.Context, int) (int, bool, error) {
				calls.probe++
				return ownerPID, true, nil
			}
			tc.edit(&deps)
			stateDir, err := deps.StateDir()
			if err != nil {
				t.Fatalf("StateDir: %v", err)
			}

			_, err = ExecuteWithDependencies(context.Background(), d.TaskName, Options{Confirmed: true}, deps)
			requireFailureKind(t, err, FailureRefusedPortOwner)
			if len(calls.terminate) != 0 || len(calls.respawn) != 0 {
				t.Fatalf("refused owner side effects: terminate=%d respawn=%d", len(calls.terminate), len(calls.respawn))
			}
			audit, readErr := os.ReadFile(filepath.Join(stateDir, api.SupervisorEventLogFileLeaf))
			if readErr != nil {
				t.Fatalf("read refusal audit: %v", readErr)
			}
			if !strings.Contains(string(audit), `"source":"recover"`) ||
				!strings.Contains(string(audit), `daemon-port-squatter-`) {
				t.Fatalf("refusal audit missing recover attribution: %s", audit)
			}
		})
	}
}

func TestExecuteVerifiedOwnerTerminatesWithIdentityBeforeSingleForceRespawn(t *testing.T) {
	d := recoveryDescriptor()
	const ownerPID = 44000
	calls := &recoveryCallLog{}
	deps := recoveryDependencies(t, calls, d)
	probeCount := 0
	deps.PortOwner = func(context.Context, int) (int, bool, error) {
		calls.probe++
		probeCount++
		if probeCount <= 2 {
			return ownerPID, true, nil
		}
		return 0, false, nil
	}

	result, err := ExecuteWithDependencies(context.Background(), d.TaskName, Options{Confirmed: true}, deps)
	if err != nil {
		t.Fatalf("ExecuteWithDependencies: %v", err)
	}
	if !result.Reaped || result.PortOwnerCheck != PortOwnerReaped {
		t.Fatalf("result = %+v, want reaped", result)
	}
	if len(calls.terminate) != 1 {
		t.Fatalf("terminate calls = %d, want 1", len(calls.terminate))
	}
	proof := calls.terminate[0]
	if proof.PID != ownerPID || proof.ExecutablePath != d.Command || proof.CommandLine == "" || proof.StartedAt == "" {
		t.Fatalf("identity proof = %+v", proof)
	}
	if !reflect.DeepEqual(calls.order, []string{"terminate", "respawn"}) {
		t.Fatalf("call order = %v, want terminate then respawn", calls.order)
	}
	if calls.probe < 3 {
		t.Fatalf("port probes = %d, want initial owner, kill-boundary, and bounded port-free probes", calls.probe)
	}
	if len(calls.respawn) != 1 || calls.respawn[0] != (respawnCall{task: d.TaskName, force: true}) {
		t.Fatalf("respawn calls = %+v, want one force=true", calls.respawn)
	}
}

func TestExecuteUnboundPortSkipsTerminateAndForcesRespawn(t *testing.T) {
	d := recoveryDescriptor()
	calls := &recoveryCallLog{}
	deps := recoveryDependencies(t, calls, d)
	readState := deps.ReadState
	stateReads := 0
	deps.ReadState = func(path string) (*api.SupervisorStateFile, error) {
		stateReads++
		return readState(path)
	}

	result, err := ExecuteWithDependencies(context.Background(), d.TaskName, Options{Confirmed: true}, deps)
	if err != nil {
		t.Fatalf("ExecuteWithDependencies: %v", err)
	}
	if result.Reaped || result.PortOwnerCheck != PortOwnerUnbound {
		t.Fatalf("result = %+v, want unbound without reap", result)
	}
	if len(calls.terminate) != 0 || len(calls.respawn) != 1 || !calls.respawn[0].force {
		t.Fatalf("side effects: terminate=%d respawn=%+v", len(calls.terminate), calls.respawn)
	}
	if stateReads != 1 {
		t.Fatalf("state reads=%d want=1 before the unbound force-respawn", stateReads)
	}
}

func TestExecuteUnboundPortStateReadFailureFailsClosedBeforeTerminateOrRespawn(t *testing.T) {
	d := recoveryDescriptor()
	calls := &recoveryCallLog{}
	deps := recoveryDependencies(t, calls, d)
	deps.ReadState = func(string) (*api.SupervisorStateFile, error) {
		return nil, errors.New("state unreadable")
	}

	_, err := ExecuteWithDependencies(context.Background(), d.TaskName, Options{Confirmed: true}, deps)
	requireFailureKind(t, err, FailureStateRead)
	if calls.probe != 0 || len(calls.terminate) != 0 || len(calls.respawn) != 0 {
		t.Fatalf("state-read failure side effects: probes=%d terminate=%d respawn=%d", calls.probe, len(calls.terminate), len(calls.respawn))
	}
}

func TestExecuteUnboundPortMissingTargetStateRowFailsClosedBeforeTerminateOrRespawn(t *testing.T) {
	d := recoveryDescriptor()
	calls := &recoveryCallLog{}
	deps := recoveryDependencies(t, calls, d)
	deps.ReadState = func(string) (*api.SupervisorStateFile, error) {
		return &api.SupervisorStateFile{Daemons: map[string]api.SupervisorDaemonState{
			`\mcp-local-hub-other-default`: {CurrentPID: 1234},
		}}, nil
	}

	_, err := ExecuteWithDependencies(context.Background(), d.TaskName, Options{Confirmed: true}, deps)
	requireFailureKind(t, err, FailureStateRead)
	if calls.probe != 0 || len(calls.terminate) != 0 || len(calls.respawn) != 0 {
		t.Fatalf("missing-target-row side effects: probes=%d terminate=%d respawn=%d", calls.probe, len(calls.terminate), len(calls.respawn))
	}
}

func TestExecuteUnresolvablePortStateReadFailureFailsClosedBeforeRespawn(t *testing.T) {
	d := recoveryDescriptor()
	d.Port = 0
	d.Server = ""
	d.Daemon = ""
	d.Args = nil
	if port, ok := api.EffectiveDaemonPort(d); ok || port != 0 {
		t.Fatalf("test descriptor port = (%d,%v), want unresolvable", port, ok)
	}
	calls := &recoveryCallLog{}
	deps := recoveryDependencies(t, calls, d)
	deps.ReadState = func(string) (*api.SupervisorStateFile, error) {
		return nil, errors.New("state unreadable")
	}

	_, err := ExecuteWithDependencies(context.Background(), d.TaskName, Options{Confirmed: true}, deps)
	requireFailureKind(t, err, FailureStateRead)
	if calls.probe != 0 || len(calls.terminate) != 0 || len(calls.respawn) != 0 {
		t.Fatalf("unresolvable-port state-read failure side effects: probes=%d terminate=%d respawn=%d", calls.probe, len(calls.terminate), len(calls.respawn))
	}
}

func TestExecutePortProbeErrorStateReadFailureFailsClosedBeforeRespawn(t *testing.T) {
	d := recoveryDescriptor()
	calls := &recoveryCallLog{}
	deps := recoveryDependencies(t, calls, d)
	deps.ReadState = func(string) (*api.SupervisorStateFile, error) {
		return nil, errors.New("state unreadable")
	}
	deps.PortOwner = func(context.Context, int) (int, bool, error) {
		calls.probe++
		return 0, false, errors.New("port probe unavailable")
	}

	_, err := ExecuteWithDependencies(context.Background(), d.TaskName, Options{Confirmed: true}, deps)
	requireFailureKind(t, err, FailureStateRead)
	if calls.probe != 0 || len(calls.terminate) != 0 || len(calls.respawn) != 0 {
		t.Fatalf("probe-error state-read failure side effects: probes=%d terminate=%d respawn=%d", calls.probe, len(calls.terminate), len(calls.respawn))
	}
}

func TestExecuteAlreadyExitedStillRequestsForceRespawn(t *testing.T) {
	d := recoveryDescriptor()
	const ownerPID = 44000
	calls := &recoveryCallLog{}
	deps := recoveryDependencies(t, calls, d)
	stateDir, err := deps.StateDir()
	if err != nil {
		t.Fatalf("StateDir: %v", err)
	}
	probeCount := 0
	deps.PortOwner = func(context.Context, int) (int, bool, error) {
		probeCount++
		if probeCount <= 2 {
			return ownerPID, true, nil
		}
		return 0, false, nil
	}
	deps.HoldProcess = func(pid int) (process.HeldPIDGeneration, error) {
		generation := &fakeHeldPIDGeneration{pid: pid}
		generation.terminate = func() (bool, error) {
			calls.order = append(calls.order, "terminate")
			calls.terminate = append(calls.terminate, generation.lastProof)
			return false, process.ErrProcessAlreadyExited
		}
		return generation, nil
	}

	var notifications []Notification
	result, err := ExecuteWithDependencies(context.Background(), d.TaskName, Options{
		Confirmed: true,
		Notify: func(notification Notification) {
			notifications = append(notifications, notification)
		},
	}, deps)
	if err != nil {
		t.Fatalf("ExecuteWithDependencies: %v", err)
	}
	if result.Reaped || result.PortOwnerCheck != PortOwnerAlreadyExited || result.PortWaitOutcome != PortWaitNotRequired {
		t.Fatalf("result=%+v, want distinct already-exited outcome without reap", result)
	}
	if len(calls.respawn) != 1 || !calls.respawn[0].force {
		t.Fatalf("result=%+v respawn=%+v", result, calls.respawn)
	}
	alreadyExitedNotifications := 0
	for _, notification := range notifications {
		if notification.Kind == NotificationReaped {
			t.Fatalf("already-exited outcome emitted a reaped notification: %+v", notifications)
		}
		if notification.Kind == NotificationAlreadyExited {
			alreadyExitedNotifications++
		}
	}
	if alreadyExitedNotifications != 1 {
		t.Fatalf("already-exited notifications=%d want=1; all=%+v", alreadyExitedNotifications, notifications)
	}
	audit, readErr := os.ReadFile(filepath.Join(stateDir, api.SupervisorEventLogFileLeaf))
	if readErr != nil {
		t.Fatalf("read already-exited audit: %v", readErr)
	}
	auditText := string(audit)
	if got := strings.Count(auditText, `"event":"daemon-port-squatter-reaped"`); got != 0 {
		t.Fatalf("reaped audit count=%d want=0; audit=%s", got, audit)
	}
	if got := strings.Count(auditText, `"event":"daemon-port-squatter-already-exited"`); got != 1 {
		t.Fatalf("already-exited audit count=%d want=1; audit=%s", got, audit)
	}
	if !strings.Contains(auditText, `"source":"recover"`) || !strings.Contains(auditText, `"actor":`) {
		t.Fatalf("already-exited audit missing bounded attribution: %s", audit)
	}
}

func TestExecuteCommittedTerminationWaitErrorIsUnconfirmedAndStillRespawns(t *testing.T) {
	d := recoveryDescriptor()
	const ownerPID = 44000
	calls := &recoveryCallLog{}
	deps := recoveryDependencies(t, calls, d)
	stateDir, err := deps.StateDir()
	if err != nil {
		t.Fatalf("StateDir: %v", err)
	}
	probeCount := 0
	deps.PortOwner = func(context.Context, int) (int, bool, error) {
		probeCount++
		if probeCount <= 2 {
			return ownerPID, true, nil
		}
		return 0, false, nil
	}
	waitErr := errors.New("simulated WAIT_TIMEOUT after committed terminate: " + strings.Repeat("x", 4096))
	deps.HoldProcess = func(pid int) (process.HeldPIDGeneration, error) {
		generation := &fakeHeldPIDGeneration{pid: pid}
		generation.terminate = func() (bool, error) {
			calls.order = append(calls.order, "terminate")
			calls.terminate = append(calls.terminate, generation.lastProof)
			return true, waitErr
		}
		return generation, nil
	}

	var notifications []Notification
	result, err := ExecuteWithDependencies(context.Background(), d.TaskName, Options{
		Confirmed: true,
		Notify: func(notification Notification) {
			notifications = append(notifications, notification)
		},
	}, deps)
	if err != nil {
		t.Fatalf("ExecuteWithDependencies: %v", err)
	}
	if result.Reaped || result.PortOwnerCheck != PortOwnerTerminationUnconfirmed {
		t.Fatalf("result=%+v, want committed-but-unconfirmed without a confirmed reap", result)
	}
	if len(calls.terminate) != 1 || len(calls.respawn) != 1 {
		t.Fatalf("committed-unconfirmed calls: terminate=%d respawn=%d", len(calls.terminate), len(calls.respawn))
	}
	unconfirmedNotifications := 0
	for _, notification := range notifications {
		if notification.Kind == NotificationReaped {
			t.Fatalf("committed-unconfirmed outcome emitted a confirmed reap notification: %+v", notifications)
		}
		if notification.Kind == NotificationTerminationUnconfirmed {
			unconfirmedNotifications++
			if !errors.Is(notification.Cause, waitErr) {
				t.Fatalf("unconfirmed notification cause=%v want %v", notification.Cause, waitErr)
			}
		}
	}
	if unconfirmedNotifications != 1 {
		t.Fatalf("unconfirmed notifications=%d want=1; all=%+v", unconfirmedNotifications, notifications)
	}
	audit, readErr := os.ReadFile(filepath.Join(stateDir, api.SupervisorEventLogFileLeaf))
	if readErr != nil {
		t.Fatalf("read committed-unconfirmed audit: %v", readErr)
	}
	auditText := string(audit)
	if got := strings.Count(auditText, `"event":"daemon-port-squatter-reaped"`); got != 0 {
		t.Fatalf("confirmed-reap audit count=%d want=0; audit=%s", got, audit)
	}
	if got := strings.Count(auditText, `"event":"daemon-port-squatter-termination-unconfirmed"`); got != 1 {
		t.Fatalf("committed-unconfirmed audit count=%d want=1; audit=%s", got, audit)
	}
	boundedWaitErr := BoundEventField(waitErr.Error())
	if !strings.Contains(auditText, boundedWaitErr) {
		t.Fatalf("committed-unconfirmed audit omitted bounded wait error %q: %s", boundedWaitErr, audit)
	}
	if strings.Contains(auditText, waitErr.Error()) {
		t.Fatalf("committed-unconfirmed audit retained the unbounded wait error: %s", audit)
	}
}

func TestExecutePIDReuseSameExecutableDifferentArgvDoesNotTerminate(t *testing.T) {
	d := recoveryDescriptor()
	const ownerPID = 44000
	calls := &recoveryCallLog{}
	deps := recoveryDependencies(t, calls, d)
	deps.PortOwner = func(context.Context, int) (int, bool, error) { return ownerPID, true, nil }
	deps.LookupIdentity = func(context.Context, int) (process.ProcessIdentity, error) {
		identity := recoveryIdentity(ownerPID, d)
		identity.CommandLine = `C:\mcphub.exe daemon --server memory --daemon attacker`
		return identity, nil
	}

	_, err := ExecuteWithDependencies(context.Background(), d.TaskName, Options{Confirmed: true}, deps)
	requireFailureKind(t, err, FailureRefusedPortOwner)
	if len(calls.terminate) != 0 || len(calls.respawn) != 0 {
		t.Fatalf("PID-reuse different-argv side effects: terminate=%d respawn=%d", len(calls.terminate), len(calls.respawn))
	}
}

func TestExecuteRejectsDuplicateOrConflictingTaskDiscriminatorsWithoutTerminate(t *testing.T) {
	global := recoveryDescriptor()
	globalOtherServer := global
	globalOtherServer.TaskName = `\mcp-local-hub-time-default`
	globalOtherServer.Server = "time"
	globalOtherServer.Args = []string{"daemon", "--server", "time", "--daemon", "default"}
	globalOtherDaemon := global
	globalOtherDaemon.TaskName = `\mcp-local-hub-memory-secondary`
	globalOtherDaemon.Daemon = "secondary"
	globalOtherDaemon.Args = []string{"daemon", "--server", "memory", "--daemon", "secondary"}

	workspace := api.SupervisorDaemon{
		TaskName: `\mcp-local-hub-mcp-language-server-go-a`,
		Server:   "mcp-language-server",
		Daemon:   "go-a",
		Command:  `C:\mcphub.exe`,
		Args:     []string{"daemon", "workspace-proxy", "--workspace", `C:\ws-a`, "--language", "go"},
		Port:     9401,
	}
	workspaceOtherPath := workspace
	workspaceOtherPath.TaskName = `\mcp-local-hub-mcp-language-server-go-b`
	workspaceOtherPath.Daemon = "go-b"
	workspaceOtherPath.Args = []string{"daemon", "workspace-proxy", "--workspace", `C:\ws-b`, "--language", "go"}
	workspaceOtherLanguage := workspace
	workspaceOtherLanguage.TaskName = `\mcp-local-hub-mcp-language-server-python-a`
	workspaceOtherLanguage.Daemon = "python-a"
	workspaceOtherLanguage.Args = []string{"daemon", "workspace-proxy", "--workspace", `C:\ws-a`, "--language", "python"}

	serena := api.SupervisorDaemon{
		TaskName: `\mcp-local-hub-serena-a`,
		Server:   "serena",
		Daemon:   "a",
		Command:  `C:\mcphub.exe`,
		Args:     []string{"daemon", "serena-proxy", "--task-name", `\mcp-local-hub-serena-a`},
		Port:     9151,
	}
	serenaOther := serena
	serenaOther.TaskName = `\mcp-local-hub-serena-b`
	serenaOther.Daemon = "b"
	serenaOther.Args = []string{"daemon", "serena-proxy", "--task-name", `\mcp-local-hub-serena-b`}

	tests := []struct {
		name        string
		descriptors []api.SupervisorDaemon
		commandLine string
	}{
		{name: "duplicate same task-name", descriptors: []api.SupervisorDaemon{serena}, commandLine: `C:\mcphub.exe daemon serena-proxy --task-name \mcp-local-hub-serena-a --task-name \mcp-local-hub-serena-a`},
		{name: "conflicting task-name", descriptors: []api.SupervisorDaemon{serena, serenaOther}, commandLine: `C:\mcphub.exe daemon serena-proxy --task-name \mcp-local-hub-serena-a --task-name \mcp-local-hub-serena-b`},
		{name: "duplicate same workspace", descriptors: []api.SupervisorDaemon{workspace}, commandLine: `C:\mcphub.exe daemon workspace-proxy --workspace C:\ws-a --workspace C:\ws-a --language go`},
		{name: "conflicting workspace", descriptors: []api.SupervisorDaemon{workspace, workspaceOtherPath}, commandLine: `C:\mcphub.exe daemon workspace-proxy --workspace C:\ws-a --workspace C:\ws-b --language go`},
		{name: "duplicate same language", descriptors: []api.SupervisorDaemon{workspace}, commandLine: `C:\mcphub.exe daemon workspace-proxy --workspace C:\ws-a --language go --language go`},
		{name: "conflicting language", descriptors: []api.SupervisorDaemon{workspace, workspaceOtherLanguage}, commandLine: `C:\mcphub.exe daemon workspace-proxy --workspace C:\ws-a --language go --language python`},
		{name: "duplicate same server", descriptors: []api.SupervisorDaemon{global}, commandLine: `C:\mcphub.exe daemon --server memory --server memory --daemon default`},
		{name: "conflicting server", descriptors: []api.SupervisorDaemon{global, globalOtherServer}, commandLine: `C:\mcphub.exe daemon --server memory --server time --daemon default`},
		{name: "duplicate same daemon", descriptors: []api.SupervisorDaemon{global}, commandLine: `C:\mcphub.exe daemon --server memory --daemon default --daemon default`},
		{name: "conflicting daemon", descriptors: []api.SupervisorDaemon{global, globalOtherDaemon}, commandLine: `C:\mcphub.exe daemon --server memory --daemon default --daemon secondary`},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			for _, descriptor := range tc.descriptors {
				descriptor := descriptor
				t.Run(descriptor.TaskName, func(t *testing.T) {
					const ownerPID = 44000
					calls := &recoveryCallLog{}
					deps := recoveryDependencies(t, calls, descriptor)
					deps.PortOwner = func(context.Context, int) (int, bool, error) { return ownerPID, true, nil }
					deps.LookupIdentity = func(context.Context, int) (process.ProcessIdentity, error) {
						identity := recoveryIdentity(ownerPID, descriptor)
						identity.CommandLine = tc.commandLine
						return identity, nil
					}

					_, err := ExecuteWithDependencies(context.Background(), descriptor.TaskName, Options{Confirmed: true}, deps)
					requireFailureKind(t, err, FailureRefusedPortOwner)
					if len(calls.terminate) != 0 || len(calls.respawn) != 0 {
						t.Fatalf("ambiguous discriminator side effects: terminate=%d respawn=%d", len(calls.terminate), len(calls.respawn))
					}
				})
			}
		})
	}
}

func TestExecuteSameExecutableAndArgvOutsideHeldStartProofDoesNotTerminate(t *testing.T) {
	d := recoveryDescriptor()
	const ownerPID = 44000
	calls := &recoveryCallLog{}
	deps := recoveryDependencies(t, calls, d)
	deps.PortOwner = func(context.Context, int) (int, bool, error) { return ownerPID, true, nil }
	heldStartedAt := time.Now().Add(-10 * time.Minute).UTC().Format(time.RFC3339Nano)
	deps.HoldProcess = func(pid int) (process.HeldPIDGeneration, error) {
		return &fakeHeldPIDGeneration{
			pid: pid,
			verify: func(proof process.PIDIdentityProof) error {
				if proof.StartedAt != heldStartedAt {
					return process.ErrProcessIdentityMismatch
				}
				return nil
			},
			terminate: func() (bool, error) {
				t.Fatal("terminate called for a generation outside the start-time proof")
				return false, nil
			},
		}, nil
	}

	_, err := ExecuteWithDependencies(context.Background(), d.TaskName, Options{Confirmed: true}, deps)
	requireFailureKind(t, err, FailureRefusedPortOwner)
	if len(calls.terminate) != 0 || len(calls.respawn) != 0 {
		t.Fatalf("start-mismatch side effects: terminate=%d respawn=%d", len(calls.terminate), len(calls.respawn))
	}
}

func TestExecuteIdentityMismatchAtKillBoundarySkipsOwnershipProbeAndTerminate(t *testing.T) {
	d := recoveryDescriptor()
	const ownerPID = 44000
	calls := &recoveryCallLog{}
	deps := recoveryDependencies(t, calls, d)
	deps.PortOwner = func(context.Context, int) (int, bool, error) {
		calls.probe++
		return ownerPID, true, nil
	}
	verifyCalls := 0
	held := &fakeHeldPIDGeneration{pid: ownerPID}
	held.verify = func(process.PIDIdentityProof) error {
		verifyCalls++
		if verifyCalls == 1 {
			return nil
		}
		return process.ErrProcessIdentityMismatch
	}
	held.terminate = func() (bool, error) {
		t.Fatal("terminate called after kill-boundary identity mismatch")
		return false, nil
	}
	deps.HoldProcess = func(int) (process.HeldPIDGeneration, error) {
		return held, nil
	}

	_, err := ExecuteWithDependencies(context.Background(), d.TaskName, Options{Confirmed: true}, deps)
	requireFailureKind(t, err, FailureRefusedPortOwner)
	if verifyCalls != 2 {
		t.Fatalf("identity verification calls = %d, want classification plus kill-boundary checks", verifyCalls)
	}
	if calls.probe != 1 {
		t.Fatalf("port-owner probes = %d, want only initial classification probe", calls.probe)
	}
	if held.closeCalls != 1 {
		t.Fatalf("held generation close calls = %d, want 1", held.closeCalls)
	}
	if len(calls.terminate) != 0 || len(calls.respawn) != 0 {
		t.Fatalf("identity-mismatch side effects: terminate=%d respawn=%d", len(calls.terminate), len(calls.respawn))
	}
}

func TestExecutePortOwnerChangedAtKillBoundaryDoesNotTerminate(t *testing.T) {
	d := recoveryDescriptor()
	const ownerPID = 44000
	calls := &recoveryCallLog{}
	deps := recoveryDependencies(t, calls, d)
	probeCount := 0
	deps.PortOwner = func(context.Context, int) (int, bool, error) {
		probeCount++
		if probeCount == 1 {
			return ownerPID, true, nil
		}
		return ownerPID + 1, true, nil
	}

	_, err := ExecuteWithDependencies(context.Background(), d.TaskName, Options{Confirmed: true}, deps)
	requireFailureKind(t, err, FailureRefusedPortOwner)
	if len(calls.terminate) != 0 || len(calls.respawn) != 0 {
		t.Fatalf("changed-owner side effects: terminate=%d respawn=%d", len(calls.terminate), len(calls.respawn))
	}
}

func TestExecuteBoundaryStateRereadRefusesNewSupervisorTrackedChild(t *testing.T) {
	d := recoveryDescriptor()
	const ownerPID = 44000
	calls := &recoveryCallLog{}
	deps := recoveryDependencies(t, calls, d)
	deps.PortOwner = func(context.Context, int) (int, bool, error) {
		calls.probe++
		return ownerPID, true, nil
	}
	stateReads := 0
	deps.ReadState = func(string) (*api.SupervisorStateFile, error) {
		stateReads++
		currentPID := 22036
		if stateReads == 2 {
			currentPID = ownerPID
		}
		return &api.SupervisorStateFile{Daemons: map[string]api.SupervisorDaemonState{
			d.TaskName: {CurrentPID: currentPID},
		}}, nil
	}

	_, err := ExecuteWithDependencies(context.Background(), d.TaskName, Options{Confirmed: true}, deps)
	opErr := requireFailureKind(t, err, FailureRefusedPortOwner)
	if !errors.Is(opErr, ErrSupervisorTrackedChild) || !strings.Contains(opErr.Error(), "supervisor-tracked child") {
		t.Fatalf("refusal = %v, want supervisor-tracked child reason", opErr)
	}
	if stateReads != 2 {
		t.Fatalf("state reads=%d want early classification plus destructive-boundary reread", stateReads)
	}
	if len(calls.terminate) != 0 || len(calls.respawn) != 0 {
		t.Fatalf("new tracked-child side effects: terminate=%d respawn=%d", len(calls.terminate), len(calls.respawn))
	}
}

func TestExecuteStateReadFailureFailsClosedBeforeTerminateOrRespawn(t *testing.T) {
	d := recoveryDescriptor()
	const ownerPID = 44000
	calls := &recoveryCallLog{}
	deps := recoveryDependencies(t, calls, d)
	deps.PortOwner = func(context.Context, int) (int, bool, error) { return ownerPID, true, nil }
	deps.ReadState = func(string) (*api.SupervisorStateFile, error) {
		return nil, errors.New("state unreadable")
	}

	_, err := ExecuteWithDependencies(context.Background(), d.TaskName, Options{Confirmed: true}, deps)
	requireFailureKind(t, err, FailureStateRead)
	if len(calls.terminate) != 0 || len(calls.respawn) != 0 {
		t.Fatalf("state-read failure side effects: terminate=%d respawn=%d", len(calls.terminate), len(calls.respawn))
	}
}

func TestExecuteMissingTargetStateRowFailsClosedBeforeTerminateOrRespawn(t *testing.T) {
	d := recoveryDescriptor()
	const ownerPID = 44000
	for _, tc := range []struct {
		name    string
		daemons map[string]api.SupervisorDaemonState
	}{
		{name: "empty state map", daemons: map[string]api.SupervisorDaemonState{}},
		{name: "target row missing", daemons: map[string]api.SupervisorDaemonState{`\mcp-local-hub-other-default`: {CurrentPID: 1234}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := &recoveryCallLog{}
			deps := recoveryDependencies(t, calls, d)
			deps.PortOwner = func(context.Context, int) (int, bool, error) { return ownerPID, true, nil }
			deps.ReadState = func(string) (*api.SupervisorStateFile, error) {
				return &api.SupervisorStateFile{Daemons: tc.daemons}, nil
			}

			_, err := ExecuteWithDependencies(context.Background(), d.TaskName, Options{Confirmed: true}, deps)
			requireFailureKind(t, err, FailureStateRead)
			if len(calls.terminate) != 0 || len(calls.respawn) != 0 {
				t.Fatalf("missing-target-row side effects: terminate=%d respawn=%d", len(calls.terminate), len(calls.respawn))
			}
		})
	}
}

func TestExecuteBoundPortWithoutReadableOwnerPIDRefusesWithoutTerminateOrRespawn(t *testing.T) {
	d := recoveryDescriptor()
	for _, ownerPID := range []int{0, -1} {
		t.Run(fmt.Sprintf("pid_%d", ownerPID), func(t *testing.T) {
			calls := &recoveryCallLog{}
			deps := recoveryDependencies(t, calls, d)
			deps.PortOwner = func(context.Context, int) (int, bool, error) { return ownerPID, true, nil }

			_, err := ExecuteWithDependencies(context.Background(), d.TaskName, Options{Confirmed: true}, deps)
			requireFailureKind(t, err, FailureRefusedPortOwner)
			if len(calls.terminate) != 0 || len(calls.respawn) != 0 {
				t.Fatalf("unreadable-owner side effects: terminate=%d respawn=%d", len(calls.terminate), len(calls.respawn))
			}
		})
	}
}

func TestExecuteCanceledBeforeKillDoesNotTerminate(t *testing.T) {
	d := recoveryDescriptor()
	const ownerPID = 44000
	calls := &recoveryCallLog{}
	deps := recoveryDependencies(t, calls, d)
	ctx, cancel := context.WithCancel(context.Background())
	deps.PortOwner = func(context.Context, int) (int, bool, error) { return ownerPID, true, nil }
	verifyCalls := 0
	deps.HoldProcess = func(pid int) (process.HeldPIDGeneration, error) {
		generation := &fakeHeldPIDGeneration{pid: pid}
		generation.verify = func(process.PIDIdentityProof) error {
			verifyCalls++
			if verifyCalls == 2 {
				cancel()
			}
			return nil
		}
		generation.terminate = func() (bool, error) {
			t.Fatal("terminate called after the request context was canceled")
			return false, nil
		}
		return generation, nil
	}

	_, err := ExecuteWithDependencies(ctx, d.TaskName, Options{Confirmed: true}, deps)
	requireFailureKind(t, err, FailureRequestCanceled)
	if len(calls.terminate) != 0 || len(calls.respawn) != 0 {
		t.Fatalf("pre-kill cancellation side effects: terminate=%d respawn=%d", len(calls.terminate), len(calls.respawn))
	}
}

func TestExecuteInitialPortProbeHonorsContextDeadlineWithoutOpeningGeneration(t *testing.T) {
	d := recoveryDescriptor()
	calls := &recoveryCallLog{}
	deps := recoveryDependencies(t, calls, d)
	probeStarted := make(chan struct{})
	holdCalls := 0
	deps.PortOwner = func(probeCtx context.Context, _ int) (int, bool, error) {
		close(probeStarted)
		<-probeCtx.Done()
		return 0, false, probeCtx.Err()
	}
	deps.HoldProcess = func(int) (process.HeldPIDGeneration, error) {
		holdCalls++
		return nil, errors.New("unexpected generation hold")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	done := make(chan error, 1)
	go func() {
		_, err := ExecuteWithDependencies(ctx, d.TaskName, Options{Confirmed: true}, deps)
		done <- err
	}()
	<-probeStarted

	select {
	case err := <-done:
		requireFailureKind(t, err, FailureRequestCanceled)
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("error = %v, want context deadline", err)
		}
		if holdCalls != 0 || len(calls.terminate) != 0 || len(calls.respawn) != 0 {
			t.Fatalf("initial-probe deadline side effects: hold=%d terminate=%d respawn=%d", holdCalls, len(calls.terminate), len(calls.respawn))
		}
	case <-time.After(100 * time.Millisecond):
		t.Fatal("initial port-owner probe ignored the governing context deadline")
	}
}

func TestExecuteCancellationDuringSuccessfulBoundaryProbeAuditsOnceAndDoesNotTerminate(t *testing.T) {
	d := recoveryDescriptor()
	const ownerPID = 44000
	calls := &recoveryCallLog{}
	deps := recoveryDependencies(t, calls, d)
	stateDir, err := deps.StateDir()
	if err != nil {
		t.Fatalf("StateDir: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	stateReads := 0
	readState := deps.ReadState
	deps.ReadState = func(path string) (*api.SupervisorStateFile, error) {
		stateReads++
		return readState(path)
	}
	probeCount := 0
	deps.PortOwner = func(context.Context, int) (int, bool, error) {
		probeCount++
		if probeCount == 2 {
			cancel()
		}
		return ownerPID, true, nil
	}
	held := &fakeHeldPIDGeneration{pid: ownerPID}
	held.terminate = func() (bool, error) {
		t.Fatal("terminate called after cancellation during boundary probe")
		return false, nil
	}
	deps.HoldProcess = func(int) (process.HeldPIDGeneration, error) { return held, nil }

	_, err = ExecuteWithDependencies(ctx, d.TaskName, Options{Confirmed: true}, deps)
	requireFailureKind(t, err, FailureRequestCanceled)
	if held.closeCalls != 1 {
		t.Fatalf("held generation close calls = %d, want 1", held.closeCalls)
	}
	if len(calls.terminate) != 0 || len(calls.respawn) != 0 {
		t.Fatalf("boundary-probe cancellation side effects: terminate=%d respawn=%d", len(calls.terminate), len(calls.respawn))
	}
	if stateReads != 1 {
		t.Fatalf("state reads=%d want=1 (cancellation must stop before the post-probe boundary state reread)", stateReads)
	}
	audit, readErr := os.ReadFile(filepath.Join(stateDir, api.SupervisorEventLogFileLeaf))
	if readErr != nil {
		t.Fatalf("read cancellation audit: %v", readErr)
	}
	if got := strings.Count(string(audit), `"event":"daemon-recovery-canceled"`); got != 1 {
		t.Fatalf("cancellation audit count=%d want=1; audit=%s", got, audit)
	}
	if !strings.Contains(string(audit), `"stage":"boundary_port_probe"`) {
		t.Fatalf("cancellation audit did not pin boundary_port_probe stage: %s", audit)
	}
	if !strings.Contains(string(audit), `"source":"recover"`) || !strings.Contains(string(audit), `"actor":`) {
		t.Fatalf("cancellation audit missing bounded attribution: %s", audit)
	}
	if strings.Contains(string(audit), `"command_line"`) || strings.Contains(string(audit), `"executable_path"`) {
		t.Fatalf("cancellation audit leaked identity text: %s", audit)
	}
}

func TestExecuteBlockingPortProbeReturnsOnContextDeadlineAndClosesHeldGeneration(t *testing.T) {
	d := recoveryDescriptor()
	const ownerPID = 44000
	calls := &recoveryCallLog{}
	deps := recoveryDependencies(t, calls, d)
	probeStarted := make(chan struct{})
	releaseProbe := make(chan struct{})
	probeCount := 0
	deps.PortOwner = func(probeCtx context.Context, _ int) (int, bool, error) {
		probeCount++
		if probeCount == 1 {
			return ownerPID, true, nil
		}
		close(probeStarted)
		select {
		case <-probeCtx.Done():
			return 0, false, probeCtx.Err()
		case <-releaseProbe:
			return ownerPID, true, nil
		}
	}
	held := &fakeHeldPIDGeneration{pid: ownerPID}
	held.terminate = func() (bool, error) {
		t.Fatal("terminate called after the governing context expired")
		return false, nil
	}
	deps.HoldProcess = func(int) (process.HeldPIDGeneration, error) {
		return held, nil
	}

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	done := make(chan error, 1)
	go func() {
		_, err := ExecuteWithDependencies(ctx, d.TaskName, Options{Confirmed: true}, deps)
		done <- err
	}()
	<-probeStarted
	<-ctx.Done()

	select {
	case err := <-done:
		requireFailureKind(t, err, FailureBoundaryProbeTimeout)
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("error = %v, want context deadline", err)
		}
		if held.closeCalls != 1 {
			t.Fatalf("held generation close calls = %d, want 1", held.closeCalls)
		}
		if len(calls.terminate) != 0 || len(calls.respawn) != 0 {
			t.Fatalf("deadline side effects: terminate=%d respawn=%d", len(calls.terminate), len(calls.respawn))
		}
	case <-time.After(100 * time.Millisecond):
		close(releaseProbe)
		<-done
		t.Fatal("blocking port-owner probe ignored the governing context deadline")
	}
}

func TestExecuteCancellationAfterPointOfNoReturnStillAttemptsDetachedRespawn(t *testing.T) {
	d := recoveryDescriptor()
	const ownerPID = 44000
	calls := &recoveryCallLog{}
	deps := recoveryDependencies(t, calls, d)
	ctx, cancel := context.WithCancel(context.Background())
	probeCount := 0
	deps.PortOwner = func(context.Context, int) (int, bool, error) {
		probeCount++
		if probeCount <= 2 {
			return ownerPID, true, nil
		}
		return 0, false, nil
	}
	deps.HoldProcess = func(pid int) (process.HeldPIDGeneration, error) {
		generation := &fakeHeldPIDGeneration{pid: pid}
		generation.terminate = func() (bool, error) {
			calls.order = append(calls.order, "terminate")
			calls.terminate = append(calls.terminate, generation.lastProof)
			cancel()
			return true, nil
		}
		return generation, nil
	}
	deps.Respawn = func(respawnCtx context.Context, task string, force bool) (api.RespawnResult, error) {
		calls.respawn = append(calls.respawn, respawnCall{task: task, force: force})
		if err := respawnCtx.Err(); err != nil {
			t.Fatalf("post-commit respawn inherited canceled request context: %v", err)
		}
		return api.RespawnResult{Code: "RESPAWN_FAILED", Message: "refused"}, nil
	}

	_, err := ExecuteWithDependencies(ctx, d.TaskName, Options{Confirmed: true}, deps)
	requireFailureKind(t, err, FailureRespawnFailed)
	if len(calls.terminate) != 1 || len(calls.respawn) != 1 {
		t.Fatalf("post-commit calls: terminate=%d respawn=%d", len(calls.terminate), len(calls.respawn))
	}
}

func TestExecuteSupervisorUnreachableAtPreKillProbeRefusesWithoutTerminateOrRespawn(t *testing.T) {
	d := recoveryDescriptor()
	const ownerPID = 44000
	calls := &recoveryCallLog{}
	deps := recoveryDependencies(t, calls, d)
	deps.PortOwner = func(context.Context, int) (int, bool, error) { return ownerPID, true, nil }
	probeCalls := 0
	probeErr := fmt.Errorf("status probe: %w", api.ErrSupervisorIPCUnavailable)
	deps.ProbeSupervisor = func(context.Context) error {
		probeCalls++
		return probeErr
	}
	deps.HoldProcess = func(pid int) (process.HeldPIDGeneration, error) {
		generation := &fakeHeldPIDGeneration{pid: pid}
		generation.terminate = func() (bool, error) {
			t.Fatal("terminate called after supervisor reachability probe failed")
			return false, nil
		}
		return generation, nil
	}

	_, err := ExecuteWithDependencies(context.Background(), d.TaskName, Options{Confirmed: true}, deps)
	opErr := requireFailureKind(t, err, FailureSupervisorUnavailable)
	if !errors.Is(opErr, api.ErrSupervisorIPCUnavailable) {
		t.Fatalf("supervisor-unavailable error=%v want status-probe cause", opErr)
	}
	if probeCalls != 1 || len(calls.terminate) != 0 || len(calls.respawn) != 0 {
		t.Fatalf("pre-kill supervisor probe calls=%d terminate=%d respawn=%d", probeCalls, len(calls.terminate), len(calls.respawn))
	}
}

func TestExecuteBlockingPostCommitAuditCannotPreemptRespawn(t *testing.T) {
	d := recoveryDescriptor()
	const ownerPID = 44000
	calls := &recoveryCallLog{}
	deps := recoveryDependencies(t, calls, d)
	const auditTimeout = 80 * time.Millisecond
	deps.AuditEmitTimeout = auditTimeout
	probeCount := 0
	deps.PortOwner = func(context.Context, int) (int, bool, error) {
		probeCount++
		if probeCount <= 2 {
			return ownerPID, true, nil
		}
		return 0, false, nil
	}

	stateDir, err := deps.StateDir()
	if err != nil {
		t.Fatalf("state dir: %v", err)
	}
	auditLock := flock.New(filepath.Join(stateDir, api.SupervisorEventLogFileLeaf) + ".lock")
	if err := auditLock.Lock(); err != nil {
		t.Fatalf("hold audit flock: %v", err)
	}
	locked := true
	defer func() {
		if locked {
			_ = auditLock.Unlock()
		}
	}()

	respawnDispatched := make(chan struct{})
	attemptStarted := time.Now()
	var respawnElapsed, respawnRemaining time.Duration
	deps.Respawn = func(respawnCtx context.Context, task string, force bool) (api.RespawnResult, error) {
		respawnElapsed = time.Since(attemptStarted)
		deadline, ok := respawnCtx.Deadline()
		if !ok {
			t.Fatal("post-commit respawn context has no reserved deadline")
		}
		respawnRemaining = time.Until(deadline)
		calls.order = append(calls.order, "respawn")
		calls.respawn = append(calls.respawn, respawnCall{task: task, force: force})
		close(respawnDispatched)
		return api.RespawnResult{Success: true}, nil
	}
	done := make(chan error, 1)
	go func() {
		_, executeErr := ExecuteWithDependencies(context.Background(), d.TaskName, Options{Confirmed: true}, deps)
		done <- executeErr
	}()

	select {
	case <-respawnDispatched:
	case <-time.After(time.Second):
		t.Fatal("respawn was not dispatched while the post-commit audit flock was held")
	}
	select {
	case executeErr := <-done:
		if executeErr != nil {
			t.Fatalf("ExecuteWithDependencies: %v", executeErr)
		}
	case <-time.After(time.Second):
		t.Fatal("recovery waited on replay after the durable handoff was established")
	}
	if got := recoveryPendingCarrierCount(t, stateDir); got != 1 {
		t.Fatalf("pending committed-audit carriers=%d want=1 while the event-log flock is held", got)
	}
	if err := auditLock.Unlock(); err != nil {
		t.Fatalf("release audit flock: %v", err)
	}
	locked = false
	replayRecoveryPending(t, stateDir)
	if got := recoveryPendingCarrierCount(t, stateDir); got != 0 {
		t.Fatalf("pending committed-audit carriers after replay=%d want=0", got)
	}
	if len(calls.terminate) != 1 || len(calls.respawn) != 1 {
		t.Fatalf("post-commit calls: terminate=%d respawn=%d want one each", len(calls.terminate), len(calls.respawn))
	}
	if respawnElapsed < auditTimeout/2 {
		t.Fatalf("respawn dispatched after %s, want evidence of the bounded %s pre-respawn audit attempt", respawnElapsed, auditTimeout)
	}
	if respawnRemaining < deps.RespawnReserve-5*time.Millisecond {
		t.Fatalf("respawn reservation remaining=%s want the configured %s minus scheduling tolerance", respawnRemaining, deps.RespawnReserve)
	}
}

func TestExecuteAlreadyExitedBlockingAuditCannotPreemptRespawn(t *testing.T) {
	d := recoveryDescriptor()
	const ownerPID = 44000
	calls := &recoveryCallLog{}
	deps := recoveryDependencies(t, calls, d)
	deps.PortOwner = func(context.Context, int) (int, bool, error) {
		return ownerPID, true, nil
	}
	deps.HoldProcess = func(pid int) (process.HeldPIDGeneration, error) {
		generation := &fakeHeldPIDGeneration{pid: pid}
		generation.terminate = func() (bool, error) {
			calls.order = append(calls.order, "terminate")
			calls.terminate = append(calls.terminate, generation.lastProof)
			return false, process.ErrProcessAlreadyExited
		}
		return generation, nil
	}

	stateDir, err := deps.StateDir()
	if err != nil {
		t.Fatalf("state dir: %v", err)
	}
	auditLock := flock.New(filepath.Join(stateDir, api.SupervisorEventLogFileLeaf) + ".lock")
	if err := auditLock.Lock(); err != nil {
		t.Fatalf("hold audit flock: %v", err)
	}
	locked := true
	defer func() {
		if locked {
			_ = auditLock.Unlock()
		}
	}()

	respawnDispatched := make(chan struct{})
	deps.Respawn = func(_ context.Context, task string, force bool) (api.RespawnResult, error) {
		calls.order = append(calls.order, "respawn")
		calls.respawn = append(calls.respawn, respawnCall{task: task, force: force})
		close(respawnDispatched)
		return api.RespawnResult{Success: true}, nil
	}
	done := make(chan error, 1)
	go func() {
		_, executeErr := ExecuteWithDependencies(context.Background(), d.TaskName, Options{Confirmed: true}, deps)
		done <- executeErr
	}()

	select {
	case <-respawnDispatched:
	case <-time.After(time.Second):
		t.Fatal("already-exited respawn was preempted by the held audit flock")
	}
	select {
	case executeErr := <-done:
		t.Fatalf("recovery returned before the queued already-exited audit flock was released: %v", executeErr)
	case <-time.After(50 * time.Millisecond):
	}
	if err := auditLock.Unlock(); err != nil {
		t.Fatalf("release audit flock: %v", err)
	}
	locked = false
	select {
	case executeErr := <-done:
		if executeErr != nil {
			t.Fatalf("ExecuteWithDependencies: %v", executeErr)
		}
	case <-time.After(time.Second):
		t.Fatal("already-exited recovery did not finish after the audit flock was released")
	}
	if len(calls.terminate) != 1 || len(calls.respawn) != 1 {
		t.Fatalf("already-exited calls: terminate=%d respawn=%d want one each", len(calls.terminate), len(calls.respawn))
	}
}

// recoveryStallAuditWrite installs a supervisor-event write function that
// blocks every append until the returned release func is called. The worker
// genuinely owns both locks when the tracked call times out, so the committed
// audit finalizer must establish a process-exit-safe carrier without waiting
// for or racing that worker.
func recoveryStallAuditWrite(t *testing.T, stateDir string) (release func()) {
	t.Helper()
	block := make(chan struct{})
	var once sync.Once
	release = func() { once.Do(func() { close(block) }) }
	restore := api.SetSupervisorEventWriteFnForTest(func(l *api.SupervisorEventLog, raw []byte) error {
		<-block
		f, ferr := os.OpenFile(filepath.Join(stateDir, api.SupervisorEventLogFileLeaf), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
		if ferr != nil {
			return ferr
		}
		defer f.Close()
		_, werr := f.Write(raw)
		return werr
	})
	t.Cleanup(func() {
		release()
		restore()
	})
	return release
}

// TestExecuteDoesNotHangWhenAuditWorkerNeverSettles proves the finalizer
// acknowledges the durable carrier and returns while the original write
// remains stalled. A blocking wait or a second blocking emit fails the bound.
func TestExecuteDoesNotHangWhenAuditWorkerNeverSettles(t *testing.T) {
	d := recoveryDescriptor()
	const ownerPID = 44000
	calls := &recoveryCallLog{}
	deps := recoveryDependencies(t, calls, d)
	deps.AuditEmitTimeout = 60 * time.Millisecond
	probeCount := 0
	deps.PortOwner = func(context.Context, int) (int, bool, error) {
		probeCount++
		if probeCount <= 2 {
			return ownerPID, true, nil
		}
		return 0, false, nil
	}

	stateDir, err := deps.StateDir()
	if err != nil {
		t.Fatalf("state dir: %v", err)
	}
	// Deliberately never released within this test: recovery must still
	// return, proving the fold removed the unbounded second wait/write.
	recoveryStallAuditWrite(t, stateDir)

	respawnDispatched := make(chan struct{})
	deps.Respawn = func(_ context.Context, task string, force bool) (api.RespawnResult, error) {
		calls.order = append(calls.order, "respawn")
		calls.respawn = append(calls.respawn, respawnCall{task: task, force: force})
		close(respawnDispatched)
		return api.RespawnResult{Success: true}, nil
	}

	done := make(chan error, 1)
	go func() {
		_, executeErr := ExecuteWithDependencies(context.Background(), d.TaskName, Options{Confirmed: true}, deps)
		done <- executeErr
	}()

	select {
	case <-respawnDispatched:
	case <-time.After(time.Second):
		t.Fatal("respawn was not dispatched while the audit write was stalled")
	}
	select {
	case executeErr := <-done:
		if executeErr != nil {
			t.Fatalf("ExecuteWithDependencies: %v", executeErr)
		}
	case <-time.After(4 * time.Second):
		t.Fatal("recovery did not return even though the stalled audit write was never released -- the fallback is hung")
	}
	if got := recoveryPendingCarrierCount(t, stateDir); got != 1 {
		t.Fatalf("pending committed-audit carriers=%d want=1 before recovery returns", got)
	}
}

// TestExecuteLateAuditWorkerSettlingAfterReturnProducesExactlyOneRow proves a
// late original completion and the retained carrier converge through exact-row
// replay to one active-plus-.1 row.
func TestExecuteLateAuditWorkerSettlingAfterReturnProducesExactlyOneRow(t *testing.T) {
	d := recoveryDescriptor()
	const ownerPID = 44000
	calls := &recoveryCallLog{}
	deps := recoveryDependencies(t, calls, d)
	deps.AuditEmitTimeout = 60 * time.Millisecond
	probeCount := 0
	deps.PortOwner = func(context.Context, int) (int, bool, error) {
		probeCount++
		if probeCount <= 2 {
			return ownerPID, true, nil
		}
		return 0, false, nil
	}

	stateDir, err := deps.StateDir()
	if err != nil {
		t.Fatalf("state dir: %v", err)
	}
	release := recoveryStallAuditWrite(t, stateDir)

	respawnDispatched := make(chan struct{})
	deps.Respawn = func(_ context.Context, task string, force bool) (api.RespawnResult, error) {
		calls.order = append(calls.order, "respawn")
		calls.respawn = append(calls.respawn, respawnCall{task: task, force: force})
		close(respawnDispatched)
		return api.RespawnResult{Success: true}, nil
	}

	done := make(chan error, 1)
	go func() {
		_, executeErr := ExecuteWithDependencies(context.Background(), d.TaskName, Options{Confirmed: true}, deps)
		done <- executeErr
	}()

	select {
	case <-respawnDispatched:
	case <-time.After(time.Second):
		t.Fatal("respawn was not dispatched while the audit write was stalled")
	}
	// Recovery must ALREADY have returned without waiting for the stalled
	// write at all -- the fold removed the synchronous wait, so this is not
	// a race: it must settle well before any old fixed grace window would
	// have expired.
	select {
	case executeErr := <-done:
		if executeErr != nil {
			t.Fatalf("ExecuteWithDependencies: %v", executeErr)
		}
	case <-time.After(500 * time.Millisecond):
		t.Fatal("recovery did not return promptly even though the fold removed the synchronous wait for the stalled write")
	}

	// Only now -- well after recovery has returned -- let the original
	// worker's write land, simulating (and exceeding) the round-2 repro's
	// "released after the grace window expired" timing.
	release()

	var raw []byte
	deadline := time.Now().Add(2 * time.Second)
	for {
		raw, err = os.ReadFile(filepath.Join(stateDir, api.SupervisorEventLogFileLeaf))
		if err == nil && strings.Count(string(raw), `"event":"daemon-port-squatter-reaped"`) >= 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("committed-kill audit row never landed after release; last read err=%v raw=%s", err, raw)
		}
		time.Sleep(10 * time.Millisecond)
	}
	// Give any (incorrect) second writer a further window to land before
	// the final count -- otherwise a reintroduced duplicate could race
	// past this check and the test would falsely report success.
	time.Sleep(200 * time.Millisecond)
	raw, err = os.ReadFile(filepath.Join(stateDir, api.SupervisorEventLogFileLeaf))
	if err != nil {
		t.Fatalf("read event log: %v", err)
	}
	replayRecoveryPending(t, stateDir)
	raw, err = os.ReadFile(filepath.Join(stateDir, api.SupervisorEventLogFileLeaf))
	if err != nil {
		t.Fatalf("read replayed event log: %v", err)
	}
	if got := strings.Count(string(raw), `"event":"daemon-port-squatter-reaped"`); got != 1 {
		t.Fatalf("committed-kill audit row count=%d, want exactly 1 (duplicate audit row); log=%s", got, raw)
	}
	if got := recoveryPendingCarrierCount(t, stateDir); got != 0 {
		t.Fatalf("pending committed-audit carriers after exact-row replay=%d want=0", got)
	}
}

func TestExecutePendingCommittedAuditSurvivesProcessExitAndReplaysOnce(t *testing.T) {
	const helperFlag = "MCPHUB_TEST_PENDING_COMMITTED_AUDIT_HELPER"
	const helperStateDir = "MCPHUB_TEST_PENDING_COMMITTED_AUDIT_STATE_DIR"
	if os.Getenv(helperFlag) == "1" {
		stateDir := os.Getenv(helperStateDir)
		if stateDir == "" {
			t.Fatal("helper state directory is empty")
		}
		d := recoveryDescriptor()
		const ownerPID = 44000
		calls := &recoveryCallLog{}
		deps := recoveryDependencies(t, calls, d)
		deps.StateDir = func() (string, error) { return stateDir, nil }
		deps.AuditEmitTimeout = 40 * time.Millisecond
		probeCount := 0
		deps.PortOwner = func(context.Context, int) (int, bool, error) {
			probeCount++
			if probeCount <= 2 {
				return ownerPID, true, nil
			}
			return 0, false, nil
		}

		block := make(chan struct{})
		restore := api.SetSupervisorEventWriteFnForTest(func(*api.SupervisorEventLog, []byte) error {
			<-block
			return nil
		})
		defer restore()

		if _, err := ExecuteWithDependencies(context.Background(), d.TaskName, Options{Confirmed: true}, deps); err != nil {
			t.Fatalf("helper ExecuteWithDependencies: %v", err)
		}
		if got := recoveryPendingCarrierCount(t, stateDir); got != 1 {
			t.Fatalf("helper pending committed-audit carriers=%d want=1", got)
		}
		return
	}

	stateDir := t.TempDir()
	cmd := exec.Command(os.Args[0], "-test.run=^TestExecutePendingCommittedAuditSurvivesProcessExitAndReplaysOnce$")
	cmd.Env = append(os.Environ(),
		helperFlag+"=1",
		helperStateDir+"="+stateDir,
	)
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("committed-audit helper failed: %v\n%s", err, output)
	}
	if got := recoveryPendingCarrierCount(t, stateDir); got != 1 {
		t.Fatalf("pending carriers after helper process exit=%d want=1; helper output=%s", got, output)
	}

	replayRecoveryPending(t, stateDir)
	raw, err := os.ReadFile(filepath.Join(stateDir, api.SupervisorEventLogFileLeaf))
	if err != nil {
		t.Fatalf("read replayed committed audit: %v", err)
	}
	if got := strings.Count(string(raw), `"event":"daemon-port-squatter-reaped"`); got != 1 {
		t.Fatalf("replayed committed-audit rows=%d want=1; log=%s", got, raw)
	}
	if got := recoveryPendingCarrierCount(t, stateDir); got != 0 {
		t.Fatalf("pending carriers after replay=%d want=0", got)
	}
	replayRecoveryPending(t, stateDir)
	raw, err = os.ReadFile(filepath.Join(stateDir, api.SupervisorEventLogFileLeaf))
	if err != nil {
		t.Fatalf("read committed audit after idempotent replay: %v", err)
	}
	if got := strings.Count(string(raw), `"event":"daemon-port-squatter-reaped"`); got != 1 {
		t.Fatalf("committed-audit rows after second replay=%d want=1; log=%s", got, raw)
	}
}

func TestExecuteFastCommittedAuditIsDurableBeforeRespawn(t *testing.T) {
	d := recoveryDescriptor()
	const ownerPID = 44000
	calls := &recoveryCallLog{}
	deps := recoveryDependencies(t, calls, d)
	probeCount := 0
	deps.PortOwner = func(context.Context, int) (int, bool, error) {
		probeCount++
		if probeCount <= 2 {
			return ownerPID, true, nil
		}
		return 0, false, nil
	}
	stateDir, err := deps.StateDir()
	if err != nil {
		t.Fatalf("state dir: %v", err)
	}
	deps.Respawn = func(_ context.Context, task string, force bool) (api.RespawnResult, error) {
		raw, readErr := os.ReadFile(filepath.Join(stateDir, api.SupervisorEventLogFileLeaf))
		if readErr != nil {
			t.Fatalf("read pre-respawn audit: %v", readErr)
		}
		if got := strings.Count(string(raw), `"event":"daemon-port-squatter-reaped"`); got != 1 {
			t.Fatalf("committed-kill audit count before respawn=%d want=1; audit=%s", got, raw)
		}
		calls.order = append(calls.order, "respawn")
		calls.respawn = append(calls.respawn, respawnCall{task: task, force: force})
		return api.RespawnResult{Success: true}, nil
	}

	_, err = ExecuteWithDependencies(context.Background(), d.TaskName, Options{Confirmed: true}, deps)
	if err != nil {
		t.Fatalf("ExecuteWithDependencies: %v", err)
	}
	raw, err := os.ReadFile(filepath.Join(stateDir, api.SupervisorEventLogFileLeaf))
	if err != nil {
		t.Fatalf("read final audit: %v", err)
	}
	if got := strings.Count(string(raw), `"event":"daemon-port-squatter-reaped"`); got != 1 {
		t.Fatalf("committed-kill audit count after recovery=%d want=1; audit=%s", got, raw)
	}
	if got := recoveryPendingCarrierCount(t, stateDir); got != 0 {
		t.Fatalf("fast committed audit created %d pending carriers, want 0", got)
	}
}

func auditDurabilityFailureDependencies(
	t *testing.T,
	respawn api.RespawnResult,
	respawnErr error,
) (Dependencies, *recoveryCallLog) {
	t.Helper()
	d := recoveryDescriptor()
	const ownerPID = 44000
	calls := &recoveryCallLog{}
	deps := recoveryDependencies(t, calls, d)
	blockedStateDir := filepath.Join(t.TempDir(), "state-dir-is-a-file")
	if err := os.WriteFile(blockedStateDir, []byte("not a directory"), 0o600); err != nil {
		t.Fatalf("create blocked state path: %v", err)
	}
	deps.StateDir = func() (string, error) { return blockedStateDir, nil }
	probeCount := 0
	deps.PortOwner = func(context.Context, int) (int, bool, error) {
		probeCount++
		if probeCount <= 2 {
			return ownerPID, true, nil
		}
		return 0, false, nil
	}
	deps.Respawn = func(_ context.Context, task string, force bool) (api.RespawnResult, error) {
		calls.order = append(calls.order, "respawn")
		calls.respawn = append(calls.respawn, respawnCall{task: task, force: force})
		return respawn, respawnErr
	}
	return deps, calls
}

func TestExecuteAuditHandoffPersistenceFailureReturnsFailureAuditDurabilityAfterRespawn(t *testing.T) {
	respawn := api.RespawnResult{Success: true}
	deps, calls := auditDurabilityFailureDependencies(t, respawn, nil)

	result, err := ExecuteWithDependencies(
		context.Background(),
		recoveryDescriptor().TaskName,
		Options{Confirmed: true},
		deps,
	)
	opErr := requireFailureKind(t, err, FailureAuditDurability)
	if len(calls.terminate) != 1 || len(calls.respawn) != 1 {
		t.Fatalf("calls: terminate=%d respawn=%d want one each", len(calls.terminate), len(calls.respawn))
	}
	if !opErr.Respawn.Success {
		t.Fatalf("audit durability error lost accepted respawn result: %+v", opErr.Respawn)
	}
	if opErr.Cause == nil || !strings.Contains(opErr.Cause.Error(), "persist committed recovery audit handoff") {
		t.Fatalf("audit durability cause=%v want handoff persistence failure", opErr.Cause)
	}
	if !result.Reaped || result.PortOwnerCheck != PortOwnerReaped {
		t.Fatalf("partial result=%+v want committed reap fact", result)
	}
}

func TestExecuteAuditDurabilityFailurePreservesRespawnFailureFact(t *testing.T) {
	respawnCause := errors.New("respawn transport failed")
	respawn := api.RespawnResult{
		Success: false,
		Code:    "RESPAWN_REJECTED",
		Message: "restart policy refused the request",
	}
	deps, calls := auditDurabilityFailureDependencies(t, respawn, respawnCause)

	result, err := ExecuteWithDependencies(
		context.Background(),
		recoveryDescriptor().TaskName,
		Options{Confirmed: true},
		deps,
	)
	opErr := requireFailureKind(t, err, FailureAuditDurability)
	if len(calls.terminate) != 1 || len(calls.respawn) != 1 {
		t.Fatalf("calls: terminate=%d respawn=%d want one each", len(calls.terminate), len(calls.respawn))
	}
	if !errors.Is(opErr.Cause, respawnCause) {
		t.Fatalf("audit durability cause=%v want joined respawn cause", opErr.Cause)
	}
	if opErr.Respawn != respawn {
		t.Fatalf("respawn result=%+v want=%+v", opErr.Respawn, respawn)
	}
	if !result.Reaped || result.PortOwnerCheck != PortOwnerReaped {
		t.Fatalf("partial result=%+v want committed reap fact", result)
	}
}

func TestExecuteRespawnFailureReturnsCommittedReapFact(t *testing.T) {
	d := recoveryDescriptor()
	const ownerPID = 44000
	calls := &recoveryCallLog{}
	deps := recoveryDependencies(t, calls, d)
	probeCount := 0
	deps.PortOwner = func(context.Context, int) (int, bool, error) {
		probeCount++
		if probeCount <= 2 {
			return ownerPID, true, nil
		}
		return 0, false, nil
	}
	respawnErr := fmt.Errorf("respawn transport: %w", api.ErrSupervisorIPCUnavailable)
	deps.Respawn = func(context.Context, string, bool) (api.RespawnResult, error) {
		return api.RespawnResult{}, respawnErr
	}

	result, err := ExecuteWithDependencies(context.Background(), d.TaskName, Options{Confirmed: true}, deps)
	opErr := requireFailureKind(t, err, FailureSupervisorUnavailable)
	if !errors.Is(opErr, api.ErrSupervisorIPCUnavailable) {
		t.Fatalf("respawn failure=%v want supervisor-unavailable cause", opErr)
	}
	if !result.Reaped || result.PortOwnerCheck != PortOwnerReaped {
		t.Fatalf("partial result=%+v want committed reap fact", result)
	}
}

func TestExecutePanickingPostCommitNotifyCannotPreemptRespawn(t *testing.T) {
	d := recoveryDescriptor()
	const ownerPID = 44000
	calls := &recoveryCallLog{}
	deps := recoveryDependencies(t, calls, d)
	probeCount := 0
	deps.PortOwner = func(context.Context, int) (int, bool, error) {
		probeCount++
		if probeCount <= 2 {
			return ownerPID, true, nil
		}
		return 0, false, nil
	}

	var panicValue any
	func() {
		defer func() { panicValue = recover() }()
		_, _ = ExecuteWithDependencies(context.Background(), d.TaskName, Options{
			Confirmed: true,
			Notify: func(notification Notification) {
				if notification.Kind == NotificationReaped {
					panic("adapter notify panic")
				}
			},
		}, deps)
	}()
	if panicValue == nil {
		t.Fatal("test notification did not panic")
	}
	if len(calls.terminate) != 1 || len(calls.respawn) != 1 {
		t.Fatalf("post-commit panic calls: terminate=%d respawn=%d want one each", len(calls.terminate), len(calls.respawn))
	}
}

func TestExecuteTerminateWaitConsumesSharedPostCommitBudgetAndStillRespawns(t *testing.T) {
	d := recoveryDescriptor()
	const ownerPID = 44000
	calls := &recoveryCallLog{}
	deps := recoveryDependencies(t, calls, d)
	probeCount := 0
	deps.PortOwner = func(context.Context, int) (int, bool, error) {
		calls.probe++
		probeCount++
		return ownerPID, true, nil
	}
	const totalBudget = 90 * time.Millisecond
	const terminateElapsed = 70 * time.Millisecond
	deps.PostKillTimeout = totalBudget
	deps.PortWaitTimeout = totalBudget
	deps.PortPollInterval = 10 * time.Millisecond
	clock := time.Now()
	attemptBoundary := clock
	deps.Now = func() time.Time { return clock }
	deps.Sleep = func(context.Context, time.Duration) error {
		clock = clock.Add(deps.PortPollInterval)
		return nil
	}
	deps.HoldProcess = func(pid int) (process.HeldPIDGeneration, error) {
		generation := &fakeHeldPIDGeneration{pid: pid}
		generation.terminate = func() (bool, error) {
			calls.order = append(calls.order, "terminate")
			calls.terminate = append(calls.terminate, generation.lastProof)
			attemptBoundary = clock
			clock = clock.Add(terminateElapsed)
			return true, nil
		}
		return generation, nil
	}
	var respawnRemaining time.Duration
	deps.Respawn = func(respawnCtx context.Context, task string, force bool) (api.RespawnResult, error) {
		calls.order = append(calls.order, "respawn")
		calls.respawn = append(calls.respawn, respawnCall{task: task, force: force})
		deadline, ok := respawnCtx.Deadline()
		if !ok {
			t.Fatal("respawn context has no shared deadline")
		}
		respawnRemaining = time.Until(deadline)
		return api.RespawnResult{Success: true}, nil
	}

	result, err := ExecuteWithDependencies(context.Background(), d.TaskName, Options{Confirmed: true}, deps)
	if err != nil {
		t.Fatalf("ExecuteWithDependencies: %v", err)
	}
	if len(calls.terminate) != 1 || len(calls.respawn) != 1 {
		t.Fatalf("calls: terminate=%d respawn=%d", len(calls.terminate), len(calls.respawn))
	}
	if elapsed := clock.Sub(attemptBoundary); elapsed > totalBudget {
		t.Fatalf("post-commit span=%s exceeds shared budget=%s", elapsed, totalBudget)
	}
	if respawnRemaining <= 0 || respawnRemaining >= totalBudget-terminateElapsed+5*time.Millisecond {
		t.Fatalf("respawn remaining budget=%s, want positive remainder after %s terminate wait", respawnRemaining, terminateElapsed)
	}
	if !result.Reaped || result.PortOwnerCheck != PortOwnerReaped {
		t.Fatalf("result=%+v, want committed reap with respawn", result)
	}
}

func TestExecuteTerminateConsumesAttemptBudgetStillRespawnsWithReservedDeadline(t *testing.T) {
	d := recoveryDescriptor()
	const ownerPID = 44000
	calls := &recoveryCallLog{}
	deps := recoveryDependencies(t, calls, d)
	deps.PortOwner = func(context.Context, int) (int, bool, error) {
		return ownerPID, true, nil
	}
	const totalBudget = 90 * time.Millisecond
	const respawnReserve = 20 * time.Millisecond
	deps.PostKillTimeout = totalBudget
	deps.RespawnReserve = respawnReserve
	deps.PortWaitTimeout = totalBudget
	clock := time.Now()
	deps.Now = func() time.Time { return clock }
	deps.HoldProcess = func(pid int) (process.HeldPIDGeneration, error) {
		generation := &fakeHeldPIDGeneration{pid: pid}
		generation.terminate = func() (bool, error) {
			calls.order = append(calls.order, "terminate")
			calls.terminate = append(calls.terminate, generation.lastProof)
			clock = clock.Add(totalBudget)
			return true, nil
		}
		return generation, nil
	}
	var respawnRemaining time.Duration
	deps.Respawn = func(respawnCtx context.Context, task string, force bool) (api.RespawnResult, error) {
		calls.order = append(calls.order, "respawn")
		calls.respawn = append(calls.respawn, respawnCall{task: task, force: force})
		deadline, ok := respawnCtx.Deadline()
		if !ok {
			t.Fatal("reserved respawn context has no deadline")
		}
		respawnRemaining = time.Until(deadline)
		if err := respawnCtx.Err(); err != nil {
			t.Fatalf("reserved respawn context already expired: %v", err)
		}
		return api.RespawnResult{Success: true}, nil
	}

	var notifications []Notification
	result, err := ExecuteWithDependencies(context.Background(), d.TaskName, Options{
		Confirmed: true,
		Notify: func(notification Notification) {
			notifications = append(notifications, notification)
		},
	}, deps)
	if err != nil {
		t.Fatalf("ExecuteWithDependencies: %v", err)
	}
	if len(calls.terminate) != 1 || len(calls.respawn) != 1 {
		t.Fatalf("calls: terminate=%d respawn=%d", len(calls.terminate), len(calls.respawn))
	}
	if respawnRemaining <= 0 || respawnRemaining > respawnReserve {
		t.Fatalf("reserved respawn deadline remaining=%s want (0,%s]", respawnRemaining, respawnReserve)
	}
	if !result.Reaped || result.PortOwnerCheck != PortOwnerReaped {
		t.Fatalf("result=%+v, want confirmed reap with reserved respawn", result)
	}
	if result.PortWaitOutcome != PortWaitProbeUnavailable {
		t.Fatalf("port wait outcome=%q want %q when the wait budget is zero", result.PortWaitOutcome, PortWaitProbeUnavailable)
	}
	foundSkipped := false
	for _, notification := range notifications {
		if notification.Kind == NotificationPortWaitTimeout && notification.Duration == 0 {
			foundSkipped = strings.Contains(notification.Cause.Error(), "release wait skipped")
		}
	}
	if !foundSkipped {
		t.Fatalf("notifications=%+v want an honest zero-budget wait-skipped observation", notifications)
	}
}

func TestExecuteRuntimeRespawnReservationDepletionRefusesBeforeTerminate(t *testing.T) {
	d := recoveryDescriptor()
	const ownerPID = 44000
	calls := &recoveryCallLog{}
	deps := recoveryDependencies(t, calls, d)
	deps.PortOwner = func(context.Context, int) (int, bool, error) { return ownerPID, true, nil }
	deps.PostKillTimeout = 100 * time.Millisecond
	deps.RespawnReserve = 20 * time.Millisecond
	clock := time.Now()
	deps.Now = func() time.Time { return clock }
	probeCalls := 0
	deps.ProbeSupervisor = func(context.Context) error {
		probeCalls++
		clock = clock.Add(81 * time.Millisecond)
		return nil
	}
	deps.HoldProcess = func(pid int) (process.HeldPIDGeneration, error) {
		generation := &fakeHeldPIDGeneration{pid: pid}
		generation.terminate = func() (bool, error) {
			t.Fatal("terminate called without a guaranteed respawn reservation")
			return false, nil
		}
		return generation, nil
	}

	_, err := ExecuteWithDependencies(context.Background(), d.TaskName, Options{Confirmed: true}, deps)
	opErr := requireFailureKind(t, err, FailureRespawnBudgetInsufficient)
	if !errors.Is(opErr, ErrInsufficientRespawnBudget) {
		t.Fatalf("insufficient-reservation error=%v want ErrInsufficientRespawnBudget", opErr)
	}
	if probeCalls != 1 || len(calls.terminate) != 0 || len(calls.respawn) != 0 {
		t.Fatalf("runtime reservation side effects: probe=%d terminate=%d respawn=%d", probeCalls, len(calls.terminate), len(calls.respawn))
	}
}

func TestExecuteStaticRespawnReservationRefusalHasDedicatedKind(t *testing.T) {
	d := recoveryDescriptor()
	calls := &recoveryCallLog{}
	deps := recoveryDependencies(t, calls, d)
	deps.PostKillTimeout = 19 * time.Millisecond
	deps.RespawnReserve = 20 * time.Millisecond

	_, err := ExecuteWithDependencies(context.Background(), d.TaskName, Options{Confirmed: true}, deps)
	opErr := requireFailureKind(t, err, FailureRespawnBudgetInsufficient)
	if !errors.Is(opErr, ErrInsufficientRespawnBudget) {
		t.Fatalf("static insufficient-reservation error=%v want ErrInsufficientRespawnBudget", opErr)
	}
	if len(calls.terminate) != 0 || len(calls.respawn) != 0 {
		t.Fatalf("static reservation side effects: terminate=%d respawn=%d", len(calls.terminate), len(calls.respawn))
	}
}

func TestExecutePermittedGUIIdentityAndProbeLatencyPreservesFullDetachedReserve(t *testing.T) {
	d := recoveryDescriptor()
	const ownerPID = 44000
	calls := &recoveryCallLog{}
	deps := recoveryDependencies(t, calls, d)
	// Scaled 10x faster than the production 30s request / 12s identity / 5s
	// probe / 20s reserve while retaining the same budget arithmetic.
	const requestBudget = 300 * time.Millisecond
	const identityLatency = 120 * time.Millisecond
	const probeLatency = 50 * time.Millisecond
	const respawnReserve = 200 * time.Millisecond
	deps.PostKillTimeout = requestBudget
	deps.RespawnReserve = respawnReserve
	probeCount := 0
	deps.PortOwner = func(context.Context, int) (int, bool, error) {
		probeCount++
		if probeCount <= 2 {
			return ownerPID, true, nil
		}
		return 0, false, nil
	}
	deps.LookupIdentity = func(ctx context.Context, pid int) (process.ProcessIdentity, error) {
		select {
		case <-ctx.Done():
			return process.ProcessIdentity{}, ctx.Err()
		case <-time.After(identityLatency):
			return recoveryIdentity(pid, d), nil
		}
	}
	deps.ProbeSupervisor = func(ctx context.Context) error {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(probeLatency):
			return nil
		}
	}
	var respawnRemaining time.Duration
	deps.Respawn = func(respawnCtx context.Context, task string, force bool) (api.RespawnResult, error) {
		calls.respawn = append(calls.respawn, respawnCall{task: task, force: force})
		deadline, ok := respawnCtx.Deadline()
		if !ok {
			t.Fatal("detached respawn context has no deadline")
		}
		respawnRemaining = time.Until(deadline)
		return api.RespawnResult{Success: true}, nil
	}

	ctx, cancel := context.WithTimeout(context.Background(), requestBudget)
	defer cancel()
	result, err := ExecuteWithDependencies(ctx, d.TaskName, Options{Confirmed: true}, deps)
	if err != nil {
		t.Fatalf("ExecuteWithDependencies: %v", err)
	}
	if len(calls.terminate) != 1 || len(calls.respawn) != 1 {
		t.Fatalf("permitted-latency calls: terminate=%d respawn=%d", len(calls.terminate), len(calls.respawn))
	}
	if respawnRemaining < 150*time.Millisecond || respawnRemaining > respawnReserve {
		t.Fatalf("detached respawn reserve=%s want near full configured %s", respawnRemaining, respawnReserve)
	}
	if !result.Reaped || result.PortOwnerCheck != PortOwnerReaped {
		t.Fatalf("result=%+v want confirmed reap and respawn", result)
	}
}

func TestExecuteCommittedKillPortNeverFreesStillRespawnsOnceAndReportsPortWaitOutcome(t *testing.T) {
	d := recoveryDescriptor()
	const ownerPID = 44000
	calls := &recoveryCallLog{}
	deps := recoveryDependencies(t, calls, d)
	deps.PortOwner = func(context.Context, int) (int, bool, error) {
		calls.probe++
		return ownerPID, true, nil
	}
	deps.PortWaitTimeout = 90 * time.Millisecond
	deps.PostKillTimeout = 60 * time.Millisecond
	deps.PortPollInterval = time.Hour
	deps.Sleep = sleepContext
	started := time.Now()
	var respawnDeadline time.Time
	deps.Respawn = func(respawnCtx context.Context, task string, force bool) (api.RespawnResult, error) {
		calls.order = append(calls.order, "respawn")
		calls.respawn = append(calls.respawn, respawnCall{task: task, force: force})
		if err := respawnCtx.Err(); err != nil {
			t.Fatalf("respawn received an expired context after the port-wait phase: %v", err)
		}
		deadline, ok := respawnCtx.Deadline()
		if !ok {
			t.Fatal("post-commit respawn context has no deadline")
		}
		respawnDeadline = deadline
		return api.RespawnResult{Success: true}, nil
	}
	var notifications []Notification

	result, err := ExecuteWithDependencies(context.Background(), d.TaskName, Options{
		Confirmed: true,
		Notify: func(notification Notification) {
			notifications = append(notifications, notification)
		},
	}, deps)
	if err != nil {
		t.Fatalf("ExecuteWithDependencies: %v", err)
	}
	if len(calls.terminate) != 1 || len(calls.respawn) != 1 {
		t.Fatalf("post-commit calls: terminate=%d respawn=%d", len(calls.terminate), len(calls.respawn))
	}
	if respawnDeadline.IsZero() {
		t.Fatal("respawn did not observe the shared post-kill deadline")
	}
	if got := respawnDeadline.Sub(started); got > deps.PostKillTimeout+20*time.Millisecond {
		t.Fatalf("post-kill deadline extends %s from operation start, want no more than %s plus scheduling tolerance", got, deps.PostKillTimeout)
	}
	if remaining := time.Until(respawnDeadline); remaining < deps.RespawnReserve-5*time.Millisecond {
		t.Fatalf("respawn reserve = %s, want the configured %s reservation (minus scheduling tolerance)", remaining, deps.RespawnReserve)
	}
	if result.PortWaitOutcome != PortWaitStillBound {
		t.Fatalf("port wait outcome = %q, want %q", result.PortWaitOutcome, PortWaitStillBound)
	}
	waitTimeouts := 0
	for _, notification := range notifications {
		if notification.Kind == NotificationPortWaitTimeout {
			waitTimeouts++
		}
	}
	if waitTimeouts != 1 {
		t.Fatalf("port-wait timeout notifications = %d, want 1; all=%+v", waitTimeouts, notifications)
	}
}

func TestBoundedPortWaitBudgetReservesRespawnRemainder(t *testing.T) {
	if got := boundedPortWaitBudget(10*time.Second, 30*time.Second, 20*time.Second, 0); got != 10*time.Second {
		t.Fatalf("default port-wait budget = %s, want 10s", got)
	}
	if got := boundedPortWaitBudget(20*time.Second, 30*time.Second, 20*time.Second, 0); got != 10*time.Second {
		t.Fatalf("capped port-wait budget = %s, want 10s", got)
	}
	if got := boundedPortWaitBudget(10*time.Second, 30*time.Second, 20*time.Second, 15*time.Second); got != 0 {
		t.Fatalf("exhausted port-wait budget = %s, want zero so respawn reservation is preserved", got)
	}
}

func TestBoundEventFieldUsesShared2048ByteLimit(t *testing.T) {
	if got := BoundEventField(strings.Repeat("x", 2048)); got != strings.Repeat("x", 2048) {
		t.Fatalf("2048-byte field changed: len=%d", len(got))
	}
	got := BoundEventField(strings.Repeat("x", 2049))
	if !strings.HasPrefix(got, strings.Repeat("x", 2048)) || !strings.HasSuffix(got, "…[truncated]") {
		t.Fatalf("2049-byte field was not capped at the shared boundary: len=%d", len(got))
	}
}

func TestExecuteDeclinedReapEmitsExactlyOneConfirmationDeclinedAudit(t *testing.T) {
	d := recoveryDescriptor()
	const ownerPID = 44000
	calls := &recoveryCallLog{}
	deps := recoveryDependencies(t, calls, d)
	deps.PortOwner = func(context.Context, int) (int, bool, error) { return ownerPID, true, nil }
	stateDir, err := deps.StateDir()
	if err != nil {
		t.Fatalf("StateDir: %v", err)
	}

	_, err = ExecuteWithDependencies(context.Background(), d.TaskName, Options{
		ConfirmReap: func(ReapCandidate) bool { return false },
	}, deps)
	requireFailureKind(t, err, FailureRefusedPortOwner)
	audit, readErr := os.ReadFile(filepath.Join(stateDir, api.SupervisorEventLogFileLeaf))
	if readErr != nil {
		t.Fatalf("read decline audit: %v", readErr)
	}
	if got := strings.Count(string(audit), `"event":"daemon-port-squatter-confirmation-declined"`); got != 1 {
		t.Fatalf("confirmation-declined audit count=%d want=1; audit=%s", got, audit)
	}
	if strings.Contains(string(audit), `"command_line"`) || strings.Contains(string(audit), `"executable_path"`) {
		t.Fatalf("confirmation-declined audit leaked process-controlled text: %s", audit)
	}
	if len(calls.terminate) != 0 || len(calls.respawn) != 0 {
		t.Fatalf("declined side effects: terminate=%d respawn=%d", len(calls.terminate), len(calls.respawn))
	}
}

func TestExecuteMapsRespawnSetupFailureToStateReadAndDialFailureToUnavailable(t *testing.T) {
	d := recoveryDescriptor()
	tests := []struct {
		name string
		err  error
		want FailureKind
	}{
		{name: "local setup failure", err: fmt.Errorf("owner sidecar corrupt: %w", api.ErrRespawnSetupFailure), want: FailureStateRead},
		{name: "transport dial failure", err: errors.New("dial failed"), want: FailureSupervisorUnavailable},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			calls := &recoveryCallLog{}
			deps := recoveryDependencies(t, calls, d)
			deps.Respawn = func(context.Context, string, bool) (api.RespawnResult, error) {
				return api.RespawnResult{}, tc.err
			}
			_, err := ExecuteWithDependencies(context.Background(), d.TaskName, Options{Confirmed: true}, deps)
			requireFailureKind(t, err, tc.want)
			if len(calls.terminate) != 0 {
				t.Fatalf("unexpected terminate calls=%d", len(calls.terminate))
			}
		})
	}
}

// TestQueueIdempotentAuditFallbackOutcomeMatrix pins every tracked-write
// outcome to one finalizer CARRIER/REPLAY action: a release failure (confirmed,
// or still unresolved in an abandoned worker) establishes a carrier and does NOT
// reacquire the event-log flock; every other definitely-absent row gets a
// carrier and an opportunistic replay that drains it.
//
// This matrix deliberately no longer varies the expected AuditHandoff. Every row
// here injects a SYNTHETIC outcome into the finalizer struct, so NO real flock is
// ever taken — which means "durable" is the truthful verdict for all of them, and
// asserting it uniformly is itself the regression guard: it proves the verdict is
// READ from the process-scoped lock owner rather than fabricated from the injected
// error. The previous version pinned wantHandoff per row from the injected value,
// which is how the "PendingStillUnsettled" row came to assert
// AuditHandoffDurable — the right answer for a fake handle, but recorded as
// though it were the answer for a real in-flight worker holding the lock.
// Real-lock handoff coverage lives in
// TestCommittedAuditFinalizerHandoffReadsProcessLockOwner.
func TestQueueIdempotentAuditFallbackOutcomeMatrix(t *testing.T) {
	cases := []struct {
		name        string
		attempted   bool
		pending     bool
		outcome     error
		wantRows    int
		wantPending int
	}{
		{
			name:      "FastSuccess",
			attempted: true,
		},
		{
			name:        "NoAttempt",
			wantRows:    1,
			wantPending: 0,
		},
		{
			name:        "LockAcquisitionTimeout",
			attempted:   true,
			outcome:     api.ErrSupervisorEventEmitTimeout,
			wantRows:    1,
			wantPending: 0,
		},
		{
			name:        "DirectDefiniteFailure",
			attempted:   true,
			outcome:     errors.New("write supervisor event log: simulated disk failure"),
			wantRows:    1,
			wantPending: 0,
		},
		{
			name:        "DirectReleaseFailure",
			attempted:   true,
			outcome:     fmt.Errorf("%w: UnlockFileEx: simulated persistent failure", api.ErrSupervisorEventReleaseFailed),
			wantPending: 1,
		},
		{
			name:      "PendingSuccess",
			attempted: true,
			pending:   true,
		},
		{
			// The seventh occurrence of the discarded-release class lived here.
			// Wait(0) reports ErrSupervisorEventEmitTimeout while the abandoned
			// worker is still inside its write holding BOTH locks, so this row
			// must take the no-replay path, exactly like a confirmed release
			// failure — it previously took the replay path and reported durable.
			name:        "PendingStillUnsettled",
			attempted:   true,
			pending:     true,
			outcome:     api.ErrSupervisorEventEmitTimeout,
			wantPending: 1,
		},
		{
			name:        "PendingReleaseFailure",
			attempted:   true,
			pending:     true,
			outcome:     fmt.Errorf("%w: UnlockFileEx: simulated persistent failure", api.ErrSupervisorEventReleaseFailed),
			wantPending: 1,
		},
		{
			name:        "PendingDefiniteFailure",
			attempted:   true,
			pending:     true,
			outcome:     errors.New("write supervisor event log: simulated disk failure"),
			wantRows:    1,
			wantPending: 0,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			stateDir := t.TempDir()
			prepared, err := api.PrepareSupervisorEvent(api.SupervisorEvent{
				Severity: api.SupervisorEventSeverityInfo,
				Source:   api.SupervisorEventSourceLifecycle,
				Event:    "daemon-recovery-fallback-outcome-matrix",
				TS:       time.Unix(1_700_000_000, 0).UTC().Format(time.RFC3339Nano),
			})
			if err != nil {
				t.Fatalf("prepare audit: %v", err)
			}
			finalizer := &committedAuditFinalizer{
				stateDir:  stateDir,
				prepared:  prepared,
				attempted: tc.attempted,
				emitErr:   tc.outcome,
			}
			if tc.pending {
				finalizer.pending = api.NewPendingSupervisorEventEmitForTest(tc.outcome)
				finalizer.emitErr = api.ErrSupervisorEventEmitTimeout
			}
			handoff, err := finalizer.finalize()
			if err != nil {
				t.Fatalf("finalize: %v", err)
			}
			// Uniformly durable BY CONSTRUCTION: an injected outcome takes no
			// real flock, so the process-scoped owner truthfully reports
			// "released". A row that reported release_unconfirmed here would
			// mean the verdict was derived from the injected error again.
			if handoff != AuditHandoffDurable {
				t.Fatalf("audit handoff=%q want=%q (no real flock is taken by an injected outcome)", handoff, AuditHandoffDurable)
			}
			if !handoff.Valid() {
				t.Fatalf("audit handoff %q is outside the safe response enum", handoff)
			}

			logPath := filepath.Join(stateDir, api.SupervisorEventLogFileLeaf)
			raw, err := os.ReadFile(logPath)
			if err != nil && !os.IsNotExist(err) {
				t.Fatalf("read %s: %v", logPath, err)
			}
			if rows := strings.Count(string(raw), "daemon-recovery-fallback-outcome-matrix"); rows != tc.wantRows {
				t.Fatalf("audit rows=%d want=%d; log=%q", rows, tc.wantRows, string(raw))
			}
			if got := recoveryPendingCarrierCount(t, stateDir); got != tc.wantPending {
				t.Fatalf("pending carriers=%d want=%d", got, tc.wantPending)
			}
		})
	}
}

// TestCommittedAuditFinalizerHandoffReadsProcessLockOwner is the real-lock
// counterpart to the injected-outcome matrix above, and the regression guard for
// the seventh occurrence of the discarded-flock-release class.
//
// Each subtest strands or occupies the REAL cross-process flock through the
// production emit path and then asserts finalize() refuses to claim "durable".
// The matrix above cannot do this: a synthetic error takes no lock.
//
// MUTATION (any subtest): restore the pre-fix derivation in finalize by making
// handoff() return AuditHandoffDurable unconditionally, i.e.
//
//	func (f *committedAuditFinalizer) handoff() AuditHandoff { return AuditHandoffDurable }
//
// Every subtest then fails with
// `audit handoff="durable" want="release_unconfirmed" ...`.
func TestCommittedAuditFinalizerHandoffReadsProcessLockOwner(t *testing.T) {
	newFinalizer := func(t *testing.T, stateDir string) *committedAuditFinalizer {
		t.Helper()
		prepared, err := api.PrepareSupervisorEvent(api.SupervisorEvent{
			Severity: api.SupervisorEventSeverityInfo,
			Source:   api.SupervisorEventSourceLifecycle,
			Event:    "daemon-recovery-handoff-owner",
			TS:       time.Unix(1_700_000_000, 0).UTC().Format(time.RFC3339Nano),
		})
		if err != nil {
			t.Fatalf("prepare audit: %v", err)
		}
		return &committedAuditFinalizer{stateDir: stateDir, prepared: prepared}
	}

	// F6: the 15 daemon-recover audit call sites all funnel through
	// emitRecoverAuditEvent's `_ = logger.Emit(event)`. The discarded error is
	// the ONLY thing that used to carry a failed release, so a stranded flock
	// left no trace anywhere. The owner now carries it instead, and the
	// finalizer reports it without the call site being touched.
	t.Run("DiscardedFireAndForgetAuditStrandsTheLock", func(t *testing.T) {
		stateDir := t.TempDir()
		logPath := filepath.Join(stateDir, api.SupervisorEventLogFileLeaf)
		defer api.ResetSupervisorEventLockStateForPathForTest(logPath)

		restoreUnlock := api.SetSupervisorEventUnlockFnForTest(func(l *api.SupervisorEventLog) error {
			// Free the real handle, then report the failure the caller would see.
			_ = api.ReleaseSupervisorEventFlockForTest(l)
			return errors.New("UnlockFileEx: simulated persistent failure")
		})
		defer restoreUnlock()

		// Exactly what production does on 15 recover paths — verdict discarded.
		emitRecoverAuditEvent(stateDir, api.SupervisorEvent{
			Severity: api.SupervisorEventSeverityWarn,
			Source:   "recover",
			Event:    "daemon-port-squatter-reaped",
		})

		handoff, err := newFinalizer(t, stateDir).finalize()
		if err != nil {
			t.Fatalf("finalize: %v", err)
		}
		if handoff != AuditHandoffReleaseUnconfirmed {
			t.Fatalf("audit handoff=%q want=%q after a discarded fire-and-forget audit stranded the flock", handoff, AuditHandoffReleaseUnconfirmed)
		}
	})

	// F1: the abandoned bounded-emit worker. finalize()'s own Wait(0) reports
	// ErrSupervisorEventEmitTimeout while that worker still owns BOTH locks.
	// The stall window is engineered large so a fast machine cannot flake it.
	t.Run("AbandonedWorkerStillHoldsTheLock", func(t *testing.T) {
		stateDir := t.TempDir()
		logPath := filepath.Join(stateDir, api.SupervisorEventLogFileLeaf)
		defer api.ResetSupervisorEventLockStateForPathForTest(logPath)

		release := make(chan struct{})
		var releaseOnce sync.Once
		safeRelease := func() { releaseOnce.Do(func() { close(release) }) }
		// The stall is what matters; the row itself is irrelevant to this
		// assertion, so the seam blocks and then reports a clean write without
		// touching the temp dir (nothing to race with t.TempDir cleanup).
		restoreWrite := api.SetSupervisorEventWriteFnForTest(func(*api.SupervisorEventLog, []byte) error {
			<-release // the filesystem/AV stall the emit timeout exists for
			return nil
		})
		var pending *api.PendingSupervisorEventEmit
		defer func() {
			safeRelease()
			// Join on the abandoned worker before restoring the seam. finalize's
			// own Wait(0) took the non-blocking default and drained nothing, so
			// this receive is the first one to consume the worker's result.
			if pending != nil {
				_ = pending.Wait(10 * time.Second)
			}
			restoreWrite()
		}()

		prepared, err := api.PrepareSupervisorEvent(api.SupervisorEvent{
			Severity: api.SupervisorEventSeverityInfo,
			Source:   api.SupervisorEventSourceLifecycle,
			Event:    "daemon-recovery-committed-stalled",
		})
		if err != nil {
			t.Fatalf("prepare stalled audit: %v", err)
		}
		trackedPending, emitErr := emitRecoverPreparedAuditWithTimeoutTracked(stateDir, prepared, 100*time.Millisecond)
		pending = trackedPending
		if !errors.Is(emitErr, api.ErrSupervisorEventEmitTimeout) {
			t.Fatalf("tracked emit with a stalled write = %v, want ErrSupervisorEventEmitTimeout", emitErr)
		}
		if pending == nil {
			t.Fatal("pending handle is nil after a genuine timeout")
		}

		finalizer := newFinalizer(t, stateDir)
		finalizer.attempted = true
		finalizer.pending = pending
		finalizer.emitErr = emitErr

		handoff, err := finalizer.finalize()
		if err != nil {
			t.Fatalf("finalize: %v", err)
		}
		if handoff != AuditHandoffReleaseUnconfirmed {
			t.Fatalf("audit handoff=%q want=%q while an abandoned worker still holds the flock", handoff, AuditHandoffReleaseUnconfirmed)
		}
	})

	// F5's concern, restated: the third release-failure branch used to be a
	// bespoke `errors.Is(logger.TryReplayPending(), ...)` check that no test
	// covered. That branch is gone — the replay path reports its own release
	// outcome to the same owner — so this pins the behaviour, not the branch.
	t.Run("ReplayPathReleaseFailureStrandsTheLock", func(t *testing.T) {
		stateDir := t.TempDir()
		logPath := filepath.Join(stateDir, api.SupervisorEventLogFileLeaf)
		defer api.ResetSupervisorEventLockStateForPathForTest(logPath)

		restoreUnlock := api.SetSupervisorEventUnlockFnForTest(func(l *api.SupervisorEventLog) error {
			// Free the real handle, then report the failure the caller would see.
			_ = api.ReleaseSupervisorEventFlockForTest(l)
			return errors.New("UnlockFileEx: simulated persistent failure")
		})
		defer restoreUnlock()

		// attempted=false reaches PersistPending + the opportunistic replay,
		// which is the only flock this finalizer takes on this path.
		handoff, err := newFinalizer(t, stateDir).finalize()
		if err != nil {
			t.Fatalf("finalize: %v", err)
		}
		if handoff != AuditHandoffReleaseUnconfirmed {
			t.Fatalf("audit handoff=%q want=%q after the replay path failed to release the flock", handoff, AuditHandoffReleaseUnconfirmed)
		}
	})
}

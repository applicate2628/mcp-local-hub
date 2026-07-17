package daemonrecovery

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
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
		t.Fatalf("recovery returned before the held audit flock was released: %v", executeErr)
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
		t.Fatal("recovery did not finish after the audit flock was released")
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

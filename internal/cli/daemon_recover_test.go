package cli

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"

	"mcp-local-hub/internal/api"
	"mcp-local-hub/internal/process"
)

// recoverTestEnv wires every daemon-recover seam to in-memory fakes so the flow
// runs without a live supervisor, a real state dir, or a real kill.
type recoverTestEnv struct {
	intent       *api.SupervisorIntentFile
	state        *api.SupervisorStateFile
	portOwner    func(int) (int, bool, error)
	respawnCalls []respawnCall
	respawnRes   api.RespawnResult
	respawnErr   error
	killCalls    []process.PIDIdentityProof
	killErr      error
	order        []string
}

type respawnCall struct {
	task  string
	force bool
}

func newRecoverTestEnv(t *testing.T, env *recoverTestEnv) {
	t.Helper()
	prevStateDir := recoverStateDirFn
	prevReadIntent := recoverReadIntentFn
	prevReadState := recoverReadStateFn
	prevPortOwner := recoverPortOwnerFn
	prevRespawn := recoverRespawnFn
	prevSelf := recoverSelfPIDFn
	prevKill := squatterTerminatePIDFn
	prevPollInterval := recoverPortFreePollInterval
	prevTimeout := recoverPortFreeTimeout

	recoverStateDirFn = func() (string, error) { return t.TempDir(), nil }
	recoverReadIntentFn = func(string) (*api.SupervisorIntentFile, error) { return env.intent, nil }
	recoverReadStateFn = func(string) (*api.SupervisorStateFile, error) { return env.state, nil }
	recoverPortOwnerFn = func(port int) (int, bool, error) {
		if env.portOwner != nil {
			return env.portOwner(port)
		}
		return 0, false, nil
	}
	recoverRespawnFn = func(_ context.Context, task string, force bool) (api.RespawnResult, error) {
		env.order = append(env.order, "respawn")
		env.respawnCalls = append(env.respawnCalls, respawnCall{task: task, force: force})
		return env.respawnRes, env.respawnErr
	}
	recoverSelfPIDFn = func() int { return 1 }
	squatterTerminatePIDFn = func(proof process.PIDIdentityProof) error {
		env.order = append(env.order, "kill")
		env.killCalls = append(env.killCalls, proof)
		return env.killErr
	}
	recoverPortFreePollInterval = time.Millisecond
	recoverPortFreeTimeout = 10 * time.Millisecond

	t.Cleanup(func() {
		recoverStateDirFn = prevStateDir
		recoverReadIntentFn = prevReadIntent
		recoverReadStateFn = prevReadState
		recoverPortOwnerFn = prevPortOwner
		recoverRespawnFn = prevRespawn
		recoverSelfPIDFn = prevSelf
		squatterTerminatePIDFn = prevKill
		recoverPortFreePollInterval = prevPollInterval
		recoverPortFreeTimeout = prevTimeout
	})
}

func recoverCmd(stdin string) (*cobra.Command, *bytes.Buffer, *bytes.Buffer) {
	cmd := &cobra.Command{}
	outBuf, errBuf := &bytes.Buffer{}, &bytes.Buffer{}
	cmd.SetOut(outBuf)
	cmd.SetErr(errBuf)
	cmd.SetIn(strings.NewReader(stdin))
	return cmd, outBuf, errBuf
}

func exitCodeOf(t *testing.T, err error) int {
	t.Helper()
	if err == nil {
		return 0
	}
	var fe interface {
		ExitCode() int
		IsMcphubForceExit() bool
	}
	if !errors.As(err, &fe) {
		t.Fatalf("error %v is not a forceExit", err)
	}
	return fe.ExitCode()
}

func TestDaemonRecover_UnknownTask(t *testing.T) {
	env := &recoverTestEnv{
		intent: &api.SupervisorIntentFile{Version: 1, Daemons: []api.SupervisorDaemon{globalDaemonDescriptor()}},
	}
	newRecoverTestEnv(t, env)
	cmd, _, errBuf := recoverCmd("")

	err := runDaemonRecover(cmd, `\mcp-local-hub-does-not-exist`, true)
	if got := exitCodeOf(t, err); got != daemonRecoverExitUnknownTask {
		t.Fatalf("exit = %d, want %d (unknown task)", got, daemonRecoverExitUnknownTask)
	}
	if len(env.respawnCalls) != 0 {
		t.Fatalf("respawn should not be attempted for an unknown task")
	}
	if !strings.Contains(errBuf.String(), "known tasks") {
		t.Fatalf("stderr should list known tasks; got:\n%s", errBuf.String())
	}
}

func TestDaemonRecover_NoSquatterGoesStraightToForceRespawn(t *testing.T) {
	d := globalDaemonDescriptor()
	env := &recoverTestEnv{
		intent:     &api.SupervisorIntentFile{Version: 1, Daemons: []api.SupervisorDaemon{d}},
		portOwner:  func(int) (int, bool, error) { return 0, false, nil }, // port unbound
		respawnRes: api.RespawnResult{Success: true},
	}
	newRecoverTestEnv(t, env)
	cmd, outBuf, _ := recoverCmd("")

	if err := runDaemonRecover(cmd, d.TaskName, true); err != nil {
		t.Fatalf("recover errored: %v", err)
	}
	if len(env.killCalls) != 0 {
		t.Fatalf("no kill expected when the port is unbound; got %d", len(env.killCalls))
	}
	if len(env.respawnCalls) != 1 || !env.respawnCalls[0].force {
		t.Fatalf("expected exactly one force=true respawn; got %+v", env.respawnCalls)
	}
	if !strings.Contains(outBuf.String(), "recovered") {
		t.Fatalf("success message missing; got:\n%s", outBuf.String())
	}
}

func TestDaemonRecover_OwnSquatterReapThenRespawn(t *testing.T) {
	d := globalDaemonDescriptor()
	const owner = 44000
	portCalls := 0
	env := &recoverTestEnv{
		intent: &api.SupervisorIntentFile{Version: 1, Daemons: []api.SupervisorDaemon{d}},
		state: &api.SupervisorStateFile{Daemons: map[string]api.SupervisorDaemonState{
			canonicalSupervisorTaskName(d.TaskName): {CurrentPID: 22036},
		}},
		portOwner: func(int) (int, bool, error) {
			portCalls++
			if portCalls == 1 {
				return owner, true, nil // first probe: squatter owns the port
			}
			return 0, false, nil // after the kill: port free
		},
		respawnRes: api.RespawnResult{Success: true},
	}
	newRecoverTestEnv(t, env)
	setSquatterLookupForTest(t, func(int) (process.ProcessIdentity, error) {
		return squatterIdentityFor(owner, d), nil
	}, alwaysExeMatch)

	cmd, _, _ := recoverCmd("")
	if err := runDaemonRecover(cmd, d.TaskName, true); err != nil {
		t.Fatalf("recover errored: %v", err)
	}

	if len(env.killCalls) != 1 || env.killCalls[0].PID != owner {
		t.Fatalf("expected one kill of pid %d; got %+v", owner, env.killCalls)
	}
	if len(env.respawnCalls) != 1 || !env.respawnCalls[0].force {
		t.Fatalf("expected one force=true respawn; got %+v", env.respawnCalls)
	}
	// Ordering: reap BEFORE respawn (the port must be freed first).
	if len(env.order) < 2 || env.order[0] != "kill" || env.order[len(env.order)-1] != "respawn" {
		t.Fatalf("order = %v, want kill before respawn", env.order)
	}
}

func TestDaemonRecover_ForeignSquatterRefusesExit3(t *testing.T) {
	d := globalDaemonDescriptor()
	const owner = 44000
	env := &recoverTestEnv{
		intent:    &api.SupervisorIntentFile{Version: 1, Daemons: []api.SupervisorDaemon{d}},
		portOwner: func(int) (int, bool, error) { return owner, true, nil },
	}
	newRecoverTestEnv(t, env)
	// Exe gate says foreign.
	setSquatterLookupForTest(t, func(int) (process.ProcessIdentity, error) {
		return squatterIdentityFor(owner, d), nil
	}, func(int, string) bool { return false })

	cmd, _, errBuf := recoverCmd("")
	err := runDaemonRecover(cmd, d.TaskName, true)
	if got := exitCodeOf(t, err); got != daemonRecoverExitRefused {
		t.Fatalf("exit = %d, want %d (foreign refusal)", got, daemonRecoverExitRefused)
	}
	if len(env.killCalls) != 0 {
		t.Fatalf("a foreign owner must NEVER be killed; got %d kills", len(env.killCalls))
	}
	if len(env.respawnCalls) != 0 {
		t.Fatalf("no respawn after a foreign refusal; got %+v", env.respawnCalls)
	}
	if !strings.Contains(errBuf.String(), "refused") {
		t.Fatalf("stderr should explain the refusal; got:\n%s", errBuf.String())
	}
}

func TestDaemonRecover_SupervisorUnreachableExit5(t *testing.T) {
	d := globalDaemonDescriptor()
	env := &recoverTestEnv{
		intent:     &api.SupervisorIntentFile{Version: 1, Daemons: []api.SupervisorDaemon{d}},
		portOwner:  func(int) (int, bool, error) { return 0, false, nil },
		respawnRes: api.RespawnResult{Code: "SUPERVISOR_UNAVAILABLE", Message: "no lock owner"},
	}
	newRecoverTestEnv(t, env)
	cmd, _, errBuf := recoverCmd("")

	err := runDaemonRecover(cmd, d.TaskName, true)
	if got := exitCodeOf(t, err); got != daemonRecoverExitUnreachable {
		t.Fatalf("exit = %d, want %d (supervisor unreachable)", got, daemonRecoverExitUnreachable)
	}
	if !strings.Contains(errBuf.String(), "mcphub supervise") {
		t.Fatalf("stderr should point at `mcphub supervise`; got:\n%s", errBuf.String())
	}
}

func TestDaemonRecover_DeclinedPromptAbortsNoKillNoRespawn(t *testing.T) {
	d := globalDaemonDescriptor()
	const owner = 44000
	env := &recoverTestEnv{
		intent:    &api.SupervisorIntentFile{Version: 1, Daemons: []api.SupervisorDaemon{d}},
		portOwner: func(int) (int, bool, error) { return owner, true, nil },
	}
	newRecoverTestEnv(t, env)
	setSquatterLookupForTest(t, func(int) (process.ProcessIdentity, error) {
		return squatterIdentityFor(owner, d), nil
	}, alwaysExeMatch)

	cmd, _, _ := recoverCmd("n\n") // operator declines
	err := runDaemonRecover(cmd, d.TaskName, false /* yes=false → prompt */)
	if got := exitCodeOf(t, err); got != daemonRecoverExitRefused {
		t.Fatalf("exit = %d, want %d (declined)", got, daemonRecoverExitRefused)
	}
	if len(env.killCalls) != 0 || len(env.respawnCalls) != 0 {
		t.Fatalf("decline must kill nothing and respawn nothing; kills=%d respawns=%d", len(env.killCalls), len(env.respawnCalls))
	}
}

// TestQuarantineReasonNamesRecoverVerb pins the parametrized quarantine reason:
// it names `mcphub daemon recover <task>`, un-hardcodes the threshold/window
// from the controller config, and no longer implies a supervisor restart is the
// only recovery.
func TestQuarantineReasonNamesRecoverVerb(t *testing.T) {
	c := &supervisorController{
		quarantineThreshold: respawnQuarantineThreshold,
		failureWindow:       respawnFailureWindow,
	}
	task := `\mcp-local-hub-serena-b133f336`
	reason := c.quarantineReasonMessage(task)

	if !strings.Contains(reason, "mcphub daemon recover "+task) {
		t.Fatalf("reason must name the recover verb with the task; got:\n%s", reason)
	}
	if !strings.Contains(reason, "10+ failures") {
		t.Fatalf("reason must carry the threshold from config; got:\n%s", reason)
	}
	if !strings.Contains(reason, "30-min") {
		t.Fatalf("reason must carry the window from config; got:\n%s", reason)
	}
	if strings.Contains(reason, "suspended until supervisor restart") {
		t.Fatalf("reason must not imply a supervisor restart is the ONLY recovery; got:\n%s", reason)
	}
}

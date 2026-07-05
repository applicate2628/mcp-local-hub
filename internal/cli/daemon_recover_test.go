package cli

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
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
	// stateDir is where the audit log lands (also fed to the injected
	// intent/state readers, which ignore it). stateDirOverride forces a
	// specific (possibly unwritable) dir to exercise the best-effort audit path.
	stateDir         string
	stateDirOverride string
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

	env.stateDir = t.TempDir()
	if env.stateDirOverride != "" {
		env.stateDir = env.stateDirOverride
	}
	recoverStateDirFn = func() (string, error) { return env.stateDir, nil }
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

// TestDaemonRecover_Port0ResolvesEffectivePortOrWarns is the P3c guard: recover
// no longer skips a Port=0 descriptor + prints a 3-way F5-modelling hint. It
// resolves the effective port through the owner: a legacy Port=0 row whose
// manifest declares a port PROCEEDS into the squatter/respawn path (no
// no-resolvable-port warning); a genuine resolve-miss (renamed/removed manifest,
// or a non-manifest-daemon / portless row) warns and returns. The force respawn
// runs in every case.
func TestDaemonRecover_Port0ResolvesEffectivePortOrWarns(t *testing.T) {
	const noPortWarn = "no resolvable port"
	cases := []struct {
		name     string
		desc     api.SupervisorDaemon
		wantWarn bool // true → unresolvable → warn; false → resolved → proceeds
	}{
		{
			name: "resolvable memory Port=0 proceeds (manifest 9123)",
			desc: api.SupervisorDaemon{TaskName: `\mcp-local-hub-memory-default`, Server: "memory", Daemon: "default",
				Args: []string{"daemon", "--server", "memory", "--daemon", "default"}, Port: 0},
			wantWarn: false,
		},
		{
			name: "resolvable blank-field serena Port=0 proceeds (args-recovered, manifest 9121)",
			desc: api.SupervisorDaemon{TaskName: `\mcp-local-hub-serena-unified`,
				Args: []string{"daemon", "--server", "serena", "--daemon", "unified"}, Port: 0},
			wantWarn: false,
		},
		{
			name: "unresolvable renamed server warns",
			desc: api.SupervisorDaemon{TaskName: `\mcp-local-hub-ghost-default`, Server: "ghost-server-x", Daemon: "default",
				Args: []string{"daemon", "--server", "ghost-server-x", "--daemon", "default"}, Port: 0},
			wantWarn: true,
		},
		{
			name: "unresolvable non-daemon timer row warns",
			desc: api.SupervisorDaemon{TaskName: `\mcp-local-hub-workspace-weekly-refresh`,
				Args: []string{"workspace-weekly-refresh"}, Port: 0},
			wantWarn: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			env := &recoverTestEnv{
				intent:     &api.SupervisorIntentFile{Version: 1, Daemons: []api.SupervisorDaemon{tc.desc}},
				respawnRes: api.RespawnResult{Success: true},
			}
			newRecoverTestEnv(t, env)
			cmd, _, errBuf := recoverCmd("")

			if err := runDaemonRecover(cmd, tc.desc.TaskName, true); err != nil {
				t.Fatalf("recover errored: %v", err)
			}
			got := errBuf.String()
			if tc.wantWarn && !strings.Contains(got, noPortWarn) {
				t.Errorf("want no-resolvable-port warning; got:\n%s", got)
			}
			if !tc.wantWarn && strings.Contains(got, noPortWarn) {
				t.Errorf("resolvable row wrongly warned no-port; got:\n%s", got)
			}
			// The force respawn runs in every case (recoverReapPortSquatter → nil).
			if len(env.respawnCalls) != 1 || !env.respawnCalls[0].force {
				t.Fatalf("expected one force=true respawn; got %+v", env.respawnCalls)
			}
		})
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
	// F1: the operator kill is audited to supervisor-events.log with bounded
	// identity fields + actor + source=recover (D-A clause 6).
	log := readRecoverAuditLog(t, env.stateDir)
	if !strings.Contains(log, "daemon-port-squatter-reaped") {
		t.Fatalf("audit log missing daemon-port-squatter-reaped; log:\n%s", log)
	}
	if !strings.Contains(log, `"source":"recover"`) {
		t.Fatalf("audit log missing source=recover; log:\n%s", log)
	}
	if !strings.Contains(log, `"actor":`) {
		t.Fatalf("audit log missing actor; log:\n%s", log)
	}
	if !strings.Contains(log, `"squatter_pid":44000`) {
		t.Fatalf("audit log missing squatter_pid; log:\n%s", log)
	}
	if !strings.Contains(log, `"executable_path":`) {
		t.Fatalf("audit log missing executable_path; log:\n%s", log)
	}
}

// readRecoverAuditLog returns the contents of supervisor-events.log in stateDir
// (empty string if absent).
func readRecoverAuditLog(t *testing.T, stateDir string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(stateDir, api.SupervisorEventLogFileLeaf))
	if err != nil {
		return ""
	}
	return string(data)
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
	// F1: the refusal is audited too (attributable no-kill decision).
	log := readRecoverAuditLog(t, env.stateDir)
	if !strings.Contains(log, "daemon-port-squatter-foreign") {
		t.Fatalf("audit log missing daemon-port-squatter-foreign; log:\n%s", log)
	}
	if !strings.Contains(log, `"source":"recover"`) {
		t.Fatalf("audit log missing source=recover; log:\n%s", log)
	}
}

// TestDaemonRecover_ForeignSquatterTerminalControlsStripped (deep-audit P2
// STRONG): the attacker-controlled command line / exe path of a FOREIGN
// squatter is printed to the operator's TTY, so terminal-control bytes
// (ESC/OSC/BEL) must be stripped before printing — otherwise a crafted command
// line could erase/forge the "refused / NOT a verified child" warning or inject
// an OSC-8 phishing hyperlink, subverting the trust decision. The strip is
// TERMINAL-only: the audit log keeps the raw bytes (JSON-escaped) for forensics.
func TestDaemonRecover_ForeignSquatterTerminalControlsStripped(t *testing.T) {
	d := globalDaemonDescriptor()
	const owner = 44000
	env := &recoverTestEnv{
		intent:    &api.SupervisorIntentFile{Version: 1, Daemons: []api.SupervisorDaemon{d}},
		portOwner: func(int) (int, bool, error) { return owner, true, nil },
	}
	newRecoverTestEnv(t, env)
	const esc = "\x1b"
	const bel = "\x07"
	malicious := `"C:\evil.exe" ` + esc + `[2K` + esc + `]8;;http://phish.example` + bel + `click-here`
	setSquatterLookupForTest(t, func(int) (process.ProcessIdentity, error) {
		return process.ProcessIdentity{
			PID:              owner,
			Basename:         "evil.exe",
			CommandLine:      malicious,
			ExecutablePath:   `C:\evil.exe` + esc + `[31m`,
			CreationDateUnix: time.Now().Add(-time.Minute).Unix(),
		}, nil
	}, func(int, string) bool { return false })

	cmd, _, errBuf := recoverCmd("")
	err := runDaemonRecover(cmd, d.TaskName, true)
	if got := exitCodeOf(t, err); got != daemonRecoverExitRefused {
		t.Fatalf("exit = %d, want %d (foreign refusal)", got, daemonRecoverExitRefused)
	}
	out := errBuf.String()
	if strings.ContainsRune(out, 0x1b) {
		t.Fatalf("stderr leaked a raw ESC (0x1b) — a crafted command line could erase/forge the refusal warning; out=%q", out)
	}
	if strings.ContainsRune(out, 0x07) {
		t.Fatalf("stderr leaked a raw BEL (0x07); out=%q", out)
	}
	if !strings.Contains(out, "refused") || !strings.Contains(out, "command line:") {
		t.Fatalf("sanitized output must still show the refusal + the (stripped) fields; out=%q", out)
	}
	// Forensics: the audit event keeps the raw bytes, JSON-escaped (strip is
	// terminal-only, not inside boundSquatterField).
	log := readRecoverAuditLog(t, env.stateDir)
	if !strings.Contains(log, "\\u001b") {
		t.Fatalf("audit log must preserve the raw ESC as \\u001b for forensics; log:\n%s", log)
	}
}

// TestDaemonRecover_AuditEmitFailureDoesNotFailCommand is F1's best-effort
// requirement: an unwritable audit log (here, a non-existent state dir) must NOT
// fail the command — the kill + respawn still complete and exit 0.
func TestDaemonRecover_AuditEmitFailureDoesNotFailCommand(t *testing.T) {
	d := globalDaemonDescriptor()
	const owner = 44000
	portCalls := 0
	env := &recoverTestEnv{
		intent:           &api.SupervisorIntentFile{Version: 1, Daemons: []api.SupervisorDaemon{d}},
		stateDirOverride: filepath.Join(t.TempDir(), "does", "not", "exist"), // OpenSupervisorEventLog will fail
		portOwner: func(int) (int, bool, error) {
			portCalls++
			if portCalls == 1 {
				return owner, true, nil
			}
			return 0, false, nil
		},
		respawnRes: api.RespawnResult{Success: true},
	}
	newRecoverTestEnv(t, env)
	setSquatterLookupForTest(t, func(int) (process.ProcessIdentity, error) {
		return squatterIdentityFor(owner, d), nil
	}, alwaysExeMatch)

	cmd, _, _ := recoverCmd("")
	if err := runDaemonRecover(cmd, d.TaskName, true); err != nil {
		t.Fatalf("recover must succeed despite an unwritable audit log; got: %v", err)
	}
	if len(env.killCalls) != 1 {
		t.Fatalf("kill should still happen; got %d", len(env.killCalls))
	}
	if len(env.respawnCalls) != 1 {
		t.Fatalf("respawn should still happen; got %d", len(env.respawnCalls))
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

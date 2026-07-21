package cli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/spf13/cobra"

	"mcp-local-hub/internal/api"
	"mcp-local-hub/internal/api/apitest"
)

// Pre-spawn existence gate (P1.1) — regression suite.
//
// The production incident this closes: `supervisor-intent.json` recorded
// `"command": "C:\\Users\\<user>\\.local\\bin\\mcphub.exe"` for all 12 daemons on
// a host where that file did NOT exist. Every daemon IS mcphub.exe (the
// supervisor spawns itself with `daemon --server X --daemon Y`), so ALL 12
// failed identically with `CreateProcess: The system cannot find the file
// specified.` — 541 daemon-spawn-failed events, 10 failures inside ~4 minutes
// per daemon, 48 daemon-quarantined events. Quarantine is in-memory, so a
// supervisor restart cleared it, the supervisor honestly retried, hit the same
// absent file, and re-quarantined ~4 minutes later. Forever.
//
// These tests are written against the OBSERVABLE contract (SM state + spawn
// call count + emitted events), not against the gate's internals, so the
// budget-burn test below compiles and RUNS RED on the pre-fix tree.

// prespawnGateController builds a controller wired for the real
// StBackoffWaiting -> EvTimerDue -> StSpawning create-process chain with a
// spawn closure that always fails the way CreateProcess fails on a missing
// image. It returns the controller, the loop, and the spawn-call counter.
func prespawnGateController(t *testing.T, command string) (*supervisorController, *api.EventLoop, *atomic.Int32, string) {
	t.Helper()
	ctrl, loop, _ := lostChildParoleController(t, respawnQuarantineThreshold)
	task := `\mcp-local-hub-memory-default`
	d := api.SupervisorDaemon{
		TaskName: task,
		Server:   "memory",
		Daemon:   "default",
		Command:  command,
		Args:     []string{"daemon", "--server", "memory", "--daemon", "default"},
	}
	ctrl.intentCache.Refresh(&api.SupervisorIntentFile{Daemons: []api.SupervisorDaemon{d}})

	var spawnCalls atomic.Int32
	ctrl.spawn = func(api.SupervisorDaemon) error {
		spawnCalls.Add(1)
		// Verbatim shape of the 541 laptop failures (StartWithJob path).
		return fmt.Errorf("%w: CreateProcess: The system cannot find the file specified.", errSpawnPreChild)
	}
	ctrl.smStates.Store(task, api.StBackoffWaiting)
	loop.RegisterHandler(ctrl.handleLoopEvent)

	// The loop must be CANCELLED AND JOINED before the test's temp dir is
	// removed, not merely cancelled.
	//
	// Several tests here return the moment they observe what they came for
	// (a spawn attempt, a state transition) while the loop is still running.
	// The loop writes supervisor-state.json through the hardened state-file
	// pipeline, so a write can be in flight when t.TempDir()'s RemoveAll runs
	// — and Windows refuses to delete a directory holding a live handle. It
	// surfaced as an intermittent "TempDir RemoveAll cleanup: ...
	// hardened-parent: The directory is not empty" on whichever test happened
	// to exit first, which is why it looked like a flake that moved around.
	//
	// The pre-existing t.Cleanup(cancel) in lostChildParoleController only
	// SIGNALS; nothing waited for Run to return. A private child context makes
	// the cancel+join a single cleanup owned here, and because it is
	// registered AFTER t.TempDir() it runs BEFORE the directory removal
	// (t.Cleanup is LIFO).
	loopCtx, loopCancel := context.WithCancel(ctrl.ctx)
	loopDone := make(chan struct{})
	go func() {
		defer close(loopDone)
		loop.Run(loopCtx)
	}()
	t.Cleanup(func() {
		loopCancel()
		<-loopDone
	})

	return ctrl, loop, &spawnCalls, task
}

// driveRespawnAttempts posts n EvTimerDue events (the StBackoffWaiting ->
// StSpawning create-process transition) and waits for the loop to settle.
func driveRespawnAttempts(t *testing.T, ctrl *supervisorController, loop *api.EventLoop, task string, n int) {
	t.Helper()
	for i := 0; i < n; i++ {
		loop.Post(api.LoopEvent{Kind: api.EvTimerDue, TaskName: task})
		// Give the synthetic EvChildExit -> StBackoffWaiting round trip a chance
		// to land before the next attempt, so each posted timer maps to at most
		// one respawn attempt rather than collapsing in the queue.
		time.Sleep(5 * time.Millisecond)
		if st, _ := ctrl.GetSMState(task); st == api.StQuarantined {
			return
		}
	}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if st, _ := ctrl.GetSMState(task); st == api.StQuarantined {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// TestPreSpawnBinaryGate_MissingBinaryNeverBurnsBudget is the laptop-incident
// reproduction. A descriptor whose Command does not exist on disk must NOT
// consume the crash budget and must NEVER reach quarantine, no matter how many
// respawn attempts the backoff ladder drives.
//
// RED on the pre-fix tree: without the gate the controller calls spawn, spawn
// fails, handleBackoffWaiting records a crash, and the 10th failure quarantines.
func TestPreSpawnBinaryGate_MissingBinaryNeverBurnsBudget(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "mcphub-does-not-exist.exe")
	ctrl, loop, spawnCalls, task := prespawnGateController(t, missing)

	driveRespawnAttempts(t, ctrl, loop, task, respawnQuarantineThreshold*2)

	if st, _ := ctrl.GetSMState(task); st == api.StQuarantined {
		t.Fatalf("daemon reached StQuarantined with a missing binary at %q; the pre-spawn existence gate must hold it WITHOUT consuming the crash budget (this is the 12-daemon laptop incident)", missing)
	}
	if n := spawnCalls.Load(); n != 0 {
		t.Fatalf("spawn was invoked %d times for a binary that does not exist at %q; the gate must refuse BEFORE create-process so no failure is recorded", n, missing)
	}
	if st, _ := ctrl.GetSMState(task); st != api.StBackoffWaiting {
		t.Fatalf("held daemon SM state = %s, want StBackoffWaiting (held with an armed re-probe timer, not parked)", st)
	}
}

// TestPreSpawnBinaryGate_AutoRecoversWhenBinaryAppears proves the operator's
// actual fix — reinstalling mcphub — recovers the daemon with no operator
// action and no manual quarantine clearing. The gate holds while the file is
// absent and spawns on the very next attempt once it appears.
func TestPreSpawnBinaryGate_AutoRecoversWhenBinaryAppears(t *testing.T) {
	binPath := filepath.Join(t.TempDir(), "mcphub.exe")
	ctrl, loop, spawnCalls, task := prespawnGateController(t, binPath)

	driveRespawnAttempts(t, ctrl, loop, task, 3)
	if n := spawnCalls.Load(); n != 0 {
		t.Fatalf("spawn invoked %d times while the binary was absent, want 0", n)
	}

	// The operator reinstalls mcphub: the file appears.
	if err := os.WriteFile(binPath, []byte("MZ"), 0o600); err != nil {
		t.Fatalf("materialize binary: %v", err)
	}

	loop.Post(api.LoopEvent{Kind: api.EvTimerDue, TaskName: task})
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if spawnCalls.Load() > 0 {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("spawn was never attempted after the binary appeared at %q; the hold must auto-recover on the next tick with no operator action", binPath)
}

// TestPreSpawnBinaryGate_PresentBinarySpawns is the control: an existing binary
// must spawn exactly as today. Guards against the gate holding a healthy daemon.
func TestPreSpawnBinaryGate_PresentBinarySpawns(t *testing.T) {
	binPath := filepath.Join(t.TempDir(), "mcphub.exe")
	if err := os.WriteFile(binPath, []byte("MZ"), 0o600); err != nil {
		t.Fatalf("write binary: %v", err)
	}
	ctrl, loop, spawnCalls, task := prespawnGateController(t, binPath)

	loop.Post(api.LoopEvent{Kind: api.EvTimerDue, TaskName: task})
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if spawnCalls.Load() > 0 {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	_ = ctrl
	t.Fatal("spawn was not attempted for a binary that EXISTS; the gate must be transparent on the healthy path")
}

// TestPreSpawnBinaryGate_EmptyCommandSpawns proves a descriptor with no Command
// (the shape most existing controller tests and some legacy intent rows use) is
// never gated — there is nothing to stat, so the gate must be inert.
func TestPreSpawnBinaryGate_EmptyCommandSpawns(t *testing.T) {
	_, loop, spawnCalls, task := prespawnGateController(t, "")

	loop.Post(api.LoopEvent{Kind: api.EvTimerDue, TaskName: task})
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if spawnCalls.Load() > 0 {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("spawn was not attempted for an empty Command; the gate must skip descriptors with nothing to probe")
}

// TestPreSpawnBinaryGate_EmitsActionableOperatorMessage proves the event log
// row names the missing PATH and the REMEDY, not just a status code. This is
// the forensic record; the GUI surface is the primary delivery.
func TestPreSpawnBinaryGate_EmitsActionableOperatorMessage(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "mcphub-does-not-exist.exe")
	ctrl, loop, _, task := prespawnGateController(t, missing)
	driveRespawnAttempts(t, ctrl, loop, task, 2)

	logPath := filepath.Join(filepath.Dir(ctrl.statePath), "supervisor-events.log")
	deadline := time.Now().Add(2 * time.Second)
	var body string
	for time.Now().Before(deadline) {
		raw, err := os.ReadFile(logPath)
		if err == nil && strings.Contains(string(raw), "daemon-spawn-held-missing-path") {
			body = string(raw)
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if body == "" {
		t.Fatalf("no daemon-spawn-held-missing-path event in %s", logPath)
	}
	// Literal wire values on purpose: these are the contract the GUI badge and
	// `mcphub status --json` consumers key on, so a rename must break this test.
	for _, want := range []string{"missing-binary", "reinstall"} {
		if !strings.Contains(body, want) {
			t.Fatalf("event body missing %q — the operator message must name the cause and the remedy.\nGot:\n%s", want, body)
		}
	}
	// The missing path must be present (JSON-escaped on Windows).
	if !strings.Contains(body, strings.ReplaceAll(missing, `\`, `\\`)) && !strings.Contains(body, missing) {
		t.Fatalf("event body does not name the missing path %q.\nGot:\n%s", missing, body)
	}
}

// --- Fail-open arms ----------------------------------------------------------
//
// The standing rule is FAIL OPEN: only a definite "not found" on an absolute
// local path may hold. Everything ambiguous proceeds to spawn exactly as today,
// because a transient condition mis-read as absent would hold a recoverable
// daemon, whereas a missed hold only costs today's behavior.

func TestClassifySpawnPath_FailOpenArms(t *testing.T) {
	present := filepath.Join(t.TempDir(), "here.exe")
	if err := os.WriteFile(present, []byte("MZ"), 0o600); err != nil {
		t.Fatalf("seed: %v", err)
	}
	absMissing := filepath.Join(t.TempDir(), "gone.exe")

	denied := func(string) (os.FileInfo, error) { return nil, fs.ErrPermission }
	ioErr := func(string) (os.FileInfo, error) { return nil, errors.New("device not ready") }
	notExist := func(string) (os.FileInfo, error) { return nil, fs.ErrNotExist }

	cases := []struct {
		name string
		stat func(string) (os.FileInfo, error)
		path string
		want spawnPathVerdict
		why  string
	}{
		{"absolute missing holds", nil, absMissing, spawnPathAbsent,
			"the ONE verdict that may hold: a definite not-found on an absolute local path"},
		{"absolute present proceeds", nil, present, spawnPathPresent, "healthy path"},
		{"empty proceeds", nil, "", spawnPathIndeterminate, "nothing to probe"},
		{"bare command name proceeds", notExist, "mcphub", spawnPathIndeterminate,
			"PATH-resolved by the OS; os.Stat against the cwd cannot answer it, and holding would park every uvx/npx/node daemon"},
		{"relative path proceeds", notExist, filepath.Join("sub", "mcphub.exe"), spawnPathIndeterminate,
			"resolved relative to the child cwd, not the supervisor cwd"},
		{"UNC proceeds", notExist, `\\fileserver\share\mcphub.exe`, spawnPathIndeterminate,
			"a momentarily unreachable share is transient"},
		{"extended-length UNC proceeds", notExist, `\\?\UNC\fileserver\share\mcphub.exe`, spawnPathIndeterminate,
			"same as plain UNC"},
		{"access denied proceeds", denied, absMissing, spawnPathIndeterminate,
			"probed: an ordinary deny-ACE directory still spawns fine because bypass-traverse-checking is granted to Everyone by default"},
		{"io error proceeds", ioErr, absMissing, spawnPathIndeterminate,
			"an unreadable device is not a missing file"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := classifySpawnPath(tc.stat, tc.path); got != tc.want {
				t.Fatalf("classifySpawnPath(%q) = %v, want %v — %s", tc.path, got, tc.want, tc.why)
			}
		})
	}
}

// TestClassifySpawnPath_UnavailableVolumeIsNotAMissingFile covers FIX-5.
//
// Go maps ERROR_BAD_NETPATH to fs.ErrNotExist, so a disconnected mapped drive
// (`Z:\...`) is indistinguishable from a deleted file by the stat error alone.
// Holding is correct either way, but the REMEDY differs: telling that operator
// to reinstall mcphub is wrong — their installation is fine and reinstalling
// cannot fix an offline share. The volume-root probe separates the two.
// It is PLATFORM-SPLIT rather than Windows-only-with-a-skip, because the POSIX
// behaviour is a documented degradation that deserves its own assertion:
// filepathlite.volumeNameLen returns 0 on unix, so VolumeName is always empty,
// spawnPathVolumeRoot returns "", and the unavailable verdict is UNREACHABLE
// there by construction. Those hosts keep the generic missing-* remedy rather
// than asserting a drive state they cannot observe.
//
// The Windows-literal form of this test previously ran on every platform and
// would have FAILED the Ubuntu CI leg: unix IsAbs is HasPrefix(path, "/")
// (verified in $GOROOT/src/internal/filepathlite/path_unix.go), so
// `Z:\tools\mcphub.exe` is not absolute there and classifySpawnPath returns
// Indeterminate before the injected stat is ever called. Note `GOOS=linux go
// vet` cannot catch this class: it type-checks, it does not RUN assertions.
func TestClassifySpawnPath_UnavailableVolumeIsNotAMissingFile(t *testing.T) {
	// A stat that fails with not-exist for EVERYTHING, including the volume
	// root: the whole drive is gone.
	wholeVolumeGone := func(string) (os.FileInfo, error) { return nil, fs.ErrNotExist }

	if runtime.GOOS != "windows" {
		// POSIX: no volume concept, so a missing absolute path is always the
		// plain "deleted" verdict and NEVER the unavailable one.
		if got := classifySpawnPath(wholeVolumeGone, "/mnt/share/tools/mcphub"); got != spawnPathAbsent {
			t.Fatalf("POSIX missing path classified %v, want spawnPathAbsent (the unavailable verdict is unreachable without a volume name)", got)
		}
		return
	}

	if got := classifySpawnPath(wholeVolumeGone, `Z:\tools\mcphub.exe`); got != spawnPathUnavailable {
		t.Fatalf("disconnected volume classified %v, want spawnPathUnavailable — the operator would be told to reinstall, which cannot fix an offline drive", got)
	}

	// A stat where the FILE is missing but its volume root is fine: a genuinely
	// deleted file, which is the incident's own shape.
	volumeRootOK := func(p string) (os.FileInfo, error) {
		if p == `C:\` {
			return nil, nil
		}
		return nil, fs.ErrNotExist
	}
	if got := classifySpawnPath(volumeRootOK, `C:\Users\dev\.local\bin\mcphub.exe`); got != spawnPathAbsent {
		t.Fatalf("deleted file on a healthy volume classified %v, want spawnPathAbsent", got)
	}
}

// TestSpawnHoldOperatorMessage_UnavailableRemedyDiffers pins the FIX-5 copy
// split: an offline drive must NOT be met with "reinstall mcphub".
func TestSpawnHoldOperatorMessage_UnavailableRemedyDiffers(t *testing.T) {
	missing := spawnHoldOperatorMessage(missingBinaryReasonID, `C:\bin\mcphub.exe`)
	if !strings.Contains(strings.ToLower(missing), "reinstall") {
		t.Fatalf("a genuinely deleted binary must still say reinstall; got %q", missing)
	}
	unavailable := spawnHoldOperatorMessage(unavailableBinaryReasonID, `Z:\bin\mcphub.exe`)
	low := strings.ToLower(unavailable)
	if !strings.Contains(low, "not available") && !strings.Contains(low, "reconnect") {
		t.Fatalf("an offline drive must be described as unavailable/reconnectable; got %q", unavailable)
	}
	if !strings.Contains(low, "reinstalling will not help") {
		t.Fatalf("the unavailable copy must actively steer the operator AWAY from the wrong remedy; got %q", unavailable)
	}
	for _, id := range []string{unavailableBinaryReasonID, unavailableWorkspaceReasonID} {
		if !strings.Contains(strings.ToLower(spawnHoldOperatorMessage(id, "p")), "automatically") {
			t.Fatalf("%s copy must still promise automatic recovery", id)
		}
	}
}

// TestPreSpawnBinaryGate_BareCommandNameSpawns is the end-to-end guard for the
// fail-open arm above. Caught for real: an early revision of this gate stat'd
// bare names against the cwd and held six unrelated controller suites' daemons.
func TestPreSpawnBinaryGate_BareCommandNameSpawns(t *testing.T) {
	_, loop, spawnCalls, task := prespawnGateController(t, "mcphub")

	loop.Post(api.LoopEvent{Kind: api.EvTimerDue, TaskName: task})
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if spawnCalls.Load() > 0 {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("spawn was not attempted for a bare PATH-resolved command name; the gate must fail open because os.Stat cannot resolve PATH")
}

// TestPreSpawnBinaryGate_MissingWorkspaceHolds proves the workspace (cmd.Dir,
// supervise.go:3295) is gated too, under its own reason id.
func TestPreSpawnBinaryGate_MissingWorkspaceHolds(t *testing.T) {
	binPath := filepath.Join(t.TempDir(), "mcphub.exe")
	if err := os.WriteFile(binPath, []byte("MZ"), 0o600); err != nil {
		t.Fatalf("seed: %v", err)
	}
	ctrl, loop, spawnCalls, task := prespawnGateController(t, binPath)

	missingWS := filepath.Join(t.TempDir(), "deleted-project")
	ctrl.intentCache.Refresh(&api.SupervisorIntentFile{Daemons: []api.SupervisorDaemon{{
		TaskName: task, Server: "memory", Daemon: "default",
		Command: binPath, Workspace: missingWS,
	}}})

	driveRespawnAttempts(t, ctrl, loop, task, 3)
	if n := spawnCalls.Load(); n != 0 {
		t.Fatalf("spawn invoked %d times with a missing workspace %q, want 0", n, missingWS)
	}
	if st, _ := ctrl.GetSMState(task); st == api.StQuarantined {
		t.Fatal("a missing workspace must HOLD, never quarantine")
	}
}

// --- Fleet-wide collapse -----------------------------------------------------

// TestDeriveFleetWideSpawnHold covers the incident shape: all 12 daemons spawn
// from ONE mcphub.exe, so twelve identical red cards must collapse into one
// headline. A single held daemon, a mixed set, and a healthy fleet must NOT.
func TestDeriveFleetWideSpawnHold(t *testing.T) {
	held := func(path string) api.DaemonStatus {
		return api.DaemonStatus{SpawnHoldReason: missingBinaryReasonID, SpawnHoldPath: path}
	}
	const p = `C:\Users\dev\.local\bin\mcphub.exe`

	t.Run("all held on one path collapses", func(t *testing.T) {
		got := DeriveFleetWideSpawnHold([]api.DaemonStatus{held(p), held(p), held(p)})
		if got == nil {
			t.Fatal("want a fleet-wide headline when every held daemon shares one path")
		}
		if got.Count != 3 || got.Path != p {
			t.Fatalf("got count=%d path=%q, want 3 / %q", got.Count, got.Path, p)
		}
		if !strings.Contains(got.Message, "reinstall") {
			t.Fatalf("headline must carry the remedy; got %q", got.Message)
		}
	})
	t.Run("single held daemon does not collapse", func(t *testing.T) {
		if got := DeriveFleetWideSpawnHold([]api.DaemonStatus{held(p), {}}); got != nil {
			t.Fatalf("one held daemon needs no headline; got %+v", got)
		}
	})
	t.Run("mixed paths do not collapse", func(t *testing.T) {
		if got := DeriveFleetWideSpawnHold([]api.DaemonStatus{held(p), held(`D:\other.exe`)}); got != nil {
			t.Fatalf("mixed causes have no single headline; got %+v", got)
		}
	})
	t.Run("healthy fleet has no headline", func(t *testing.T) {
		if got := DeriveFleetWideSpawnHold([]api.DaemonStatus{{}, {}}); got != nil {
			t.Fatalf("healthy fleet must produce no headline; got %+v", got)
		}
	})
}

// --- CLI surface -------------------------------------------------------------

// TestPrintSpawnHoldNotice proves `mcphub status` explains a held daemon in
// plain words rather than leaving a bare "Stopped" row with no reason — the
// exact experience that cost the incident's operator a day.
func TestPrintSpawnHoldNotice(t *testing.T) {
	const bin = `C:\Users\dev\.local\bin\mcphub.exe`
	held := func(server, path string) api.DaemonStatus {
		return api.DaemonStatus{Server: server, Daemon: "default", State: "Stopped",
			SpawnHoldReason: missingBinaryReasonID, SpawnHoldPath: path}
	}

	t.Run("fleet-wide cause is stated once", func(t *testing.T) {
		var buf bytes.Buffer
		cmd := &cobra.Command{}
		cmd.SetOut(&buf)
		printSpawnHoldNotice(cmd, []api.DaemonStatus{held("memory", bin), held("fetch", bin), held("time", bin)})
		got := buf.String()
		if !strings.Contains(got, "3 servers cannot start") {
			t.Fatalf("want a single fleet-wide headline naming the count; got:\n%s", got)
		}
		if !strings.Contains(got, "reinstall") || !strings.Contains(got, bin) {
			t.Fatalf("headline must name the remedy and the path; got:\n%s", got)
		}
		if n := strings.Count(got, bin); n != 1 {
			t.Fatalf("the shared path is repeated %d times; the whole point is to say it ONCE, not once per daemon", n)
		}
	})

	t.Run("a lone held daemon is named individually", func(t *testing.T) {
		var buf bytes.Buffer
		cmd := &cobra.Command{}
		cmd.SetOut(&buf)
		printSpawnHoldNotice(cmd, []api.DaemonStatus{held("memory", bin), {Server: "fetch", State: "Running"}})
		got := buf.String()
		if !strings.Contains(got, "memory") || !strings.Contains(got, "reinstall") {
			t.Fatalf("want the held daemon named with its remedy; got:\n%s", got)
		}
	})

	// The helper being correct is worthless if the table never calls it. This
	// asserts the WIRING: `mcphub status` output itself must carry the reason.
	t.Run("the status table actually prints the notice", func(t *testing.T) {
		var buf bytes.Buffer
		cmd := &cobra.Command{}
		cmd.SetOut(&buf)
		if err := printDefaultStatusTable(cmd, []api.DaemonStatus{held("memory", bin), held("fetch", bin)}, false); err != nil {
			t.Fatalf("printDefaultStatusTable: %v", err)
		}
		got := buf.String()
		if !strings.Contains(got, "cannot start") || !strings.Contains(got, "reinstall") {
			t.Fatalf("the status table must explain held daemons, not just print Stopped rows; got:\n%s", got)
		}
	})

	// FIX-4: the message interpolates a filesystem path, which may carry ESC /
	// OSC / BEL bytes from a POSIX basename. Unstripped, a deleted workspace
	// named with control sequences would spoof rows in the operator's terminal
	// — and this notice fires exactly when such a directory has gone missing.
	t.Run("control bytes in the path are stripped", func(t *testing.T) {
		var buf bytes.Buffer
		cmd := &cobra.Command{}
		cmd.SetOut(&buf)
		hostile := "/home/dev/proj\x1b[2K\x07-evil"
		printSpawnHoldNotice(cmd, []api.DaemonStatus{{
			Server: "serena", Daemon: "default", State: "Stopped",
			SpawnHoldReason: missingWorkspaceReasonID, SpawnHoldPath: hostile,
		}})
		got := buf.String()
		if strings.ContainsAny(got, "\x1b\x07") {
			t.Fatalf("notice emitted raw terminal control bytes: %q", got)
		}
		if !strings.Contains(got, "evil") {
			t.Fatalf("stripping must remove control bytes but KEEP the readable path; got:\n%s", got)
		}
	})

	// The workspace-scoped table is the view MOST likely to be showing a daemon
	// held for a missing workspace, so it is the last table that should stay
	// silent about it. Its omission was an asymmetry with printDefaultStatusTable,
	// not a decision.
	t.Run("the workspace-scoped table prints the notice too", func(t *testing.T) {
		var buf bytes.Buffer
		cmd := &cobra.Command{}
		cmd.SetOut(&buf)
		row := api.DaemonStatus{
			Server: "serena", Daemon: "default", State: "Stopped",
			IsWorkspaceScoped: true,
			SpawnHoldReason:   missingWorkspaceReasonID,
			SpawnHoldPath:     `C:\projects\deleted`,
		}
		if err := printWorkspaceScopedTable(cmd, []api.DaemonStatus{row}, false); err != nil {
			t.Fatalf("printWorkspaceScopedTable: %v", err)
		}
		got := buf.String()
		if !strings.Contains(got, "cannot start") || !strings.Contains(got, `C:\projects\deleted`) {
			t.Fatalf("--workspace-scoped must explain a held daemon, not just print a Stopped row; got:\n%s", got)
		}
	})

	t.Run("a healthy fleet prints nothing", func(t *testing.T) {
		var buf bytes.Buffer
		cmd := &cobra.Command{}
		cmd.SetOut(&buf)
		printSpawnHoldNotice(cmd, []api.DaemonStatus{{Server: "memory", State: "Running"}})
		if got := buf.String(); got != "" {
			t.Fatalf("healthy fleet must print no notice; got:\n%s", got)
		}
	})
}

// --- A hold must not outlive the attempt to start ----------------------------

// TestSpawnHold_ClearedWhenDaemonStops reproduces the reported chain: an
// operator stops a daemon WHILE the pre-spawn gate is holding it. The
// StBackoffWaiting + EvIntentUpdate(stopped) transition lands in StIdle, where
// the controller calls MarkExited.
//
// A stopped daemon gets NO later create-process pass, so the gate's own
// ClearSpawnHold can never run for it. Before the fix the hold was persisted
// and kept telling the CLI and the GUI that a path was missing and the daemon
// would auto-start — after the operator had already fixed the path and stopped
// the daemon deliberately. A diagnostic that keeps asserting a condition once
// it is gone teaches people to ignore it, which is the exact failure this gate
// exists to end.
func TestSpawnHold_ClearedWhenDaemonStops(t *testing.T) {
	tracker := NewDaemonRuntimeTracker()
	const task = `\mcp-local-hub-memory-default`

	assertHeld := func(t *testing.T, want bool, whenever string) {
		t.Helper()
		snap := tracker.Snapshot()[canonicalSupervisorTaskName(task)]
		got := snap.SpawnHoldReason != ""
		if got != want {
			t.Fatalf("%s: hold present = %v, want %v (reason=%q path=%q)",
				whenever, got, want, snap.SpawnHoldReason, snap.SpawnHoldPath)
		}
	}

	// The gate marks the hold, then holdSpawnInBackoff -> MarkBackoff. The hold
	// MUST survive that pair, or the feature never reaches an operator at all.
	tracker.MarkSpawnHold(task, missingBinaryReasonID, `C:\gone\mcphub.exe`)
	tracker.MarkBackoff(task)
	assertHeld(t, true, "after MarkSpawnHold + MarkBackoff")

	// Operator stops the daemon: StBackoffWaiting + EvIntentUpdate(stopped) ->
	// StIdle -> MarkExited. The statement "cannot start, will auto-start" is no
	// longer true and must not persist.
	tracker.MarkExited(task)
	assertHeld(t, false, "after the operator stopped the daemon (MarkExited)")

	// Every other lifecycle mark that ends a start attempt clears it too.
	for _, tc := range []struct {
		name string
		end  func()
	}{
		{"MarkTerminated", func() { tracker.MarkTerminated(task) }},
		{"MarkQuarantined", func() { tracker.MarkQuarantined(task) }},
		{"MarkSpawnFailed", func() { tracker.MarkSpawnFailed(task, errors.New("boom")) }},
		{"MarkSpawnFailedPreservePID", func() { tracker.MarkSpawnFailedPreservePID(task, errors.New("boom"), 4242) }},
		{"MarkExitedIfCurrent", func() { tracker.MarkExitedIfCurrent(task, 999) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tracker.MarkSpawnHold(task, missingBinaryReasonID, `C:\gone\mcphub.exe`)
			assertHeld(t, true, "re-armed before "+tc.name)
			tc.end()
			assertHeld(t, false, "after "+tc.name)
		})
	}
}

// --- Delivery chain: producer half -------------------------------------------

// TestSupervisorStatusDaemons_EmitsSpawnHoldWireFields is the PRODUCER half of
// the delivery chain (the consumer half is
// TestDecodeSupervisorIPCStatusResult_SpawnHoldRoundTrip in internal/api).
//
// A review mutation disabled the row["spawn_hold_reason"] / ["spawn_hold_path"]
// writes here and the whole suite stayed green. Producer and consumer are bound
// by nothing but two independent sets of string literals, so BOTH ends need a
// test that spells the wire keys out; together they are the drift gate.
//
// The keys below are written as literals ON PURPOSE — asserting through a
// helper or a constant shared with the production code would make the test
// blind to the very rename it exists to catch.
func TestSupervisorStatusDaemons_EmitsSpawnHoldWireFields(t *testing.T) {
	stateDir := apitest.HardenedTempDir(t)
	const taskName = `\mcp-local-hub-memory-default`
	const heldPath = `C:\Users\dev\.local\bin\mcphub.exe`

	intent := &api.SupervisorIntentFile{
		Version: 1,
		Daemons: []api.SupervisorDaemon{{
			TaskName: taskName, Server: "memory", Daemon: "default", Port: 9123,
		}},
	}
	if err := api.WriteSupervisorIntent(filepath.Join(stateDir, "supervisor-intent.json"), intent); err != nil {
		t.Fatalf("seed supervisor-intent.json: %v", err)
	}

	tracker := NewDaemonRuntimeTracker()
	tracker.MarkSpawnHold(taskName, missingBinaryReasonID, heldPath)

	rows, err := supervisorStatusDaemons(stateDir, tracker, nil)
	if err != nil {
		t.Fatalf("supervisorStatusDaemons: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("rows len = %d, want 1", len(rows))
	}
	if got := rows[0]["spawn_hold_reason"]; got != missingBinaryReasonID {
		t.Fatalf("row[\"spawn_hold_reason\"] = %v, want %q — the supervisor knows why this daemon cannot start but is not putting it on the wire, so no operator surface can ever show it",
			got, missingBinaryReasonID)
	}
	if got := rows[0]["spawn_hold_path"]; got != heldPath {
		t.Fatalf("row[\"spawn_hold_path\"] = %v, want %q", got, heldPath)
	}

	// A daemon with no hold must omit BOTH keys entirely, so the consumer's
	// absent-field-means-healthy semantic holds.
	rows2, err := supervisorStatusDaemons(stateDir, NewDaemonRuntimeTracker(), nil)
	if err != nil {
		t.Fatalf("supervisorStatusDaemons (healthy): %v", err)
	}
	if _, present := rows2[0]["spawn_hold_reason"]; present {
		t.Fatalf("healthy daemon emitted spawn_hold_reason: %+v", rows2[0])
	}
	if _, present := rows2[0]["spawn_hold_path"]; present {
		t.Fatalf("healthy daemon emitted spawn_hold_path: %+v", rows2[0])
	}
}

// --- Event-stream bounding ---------------------------------------------------

// TestSpawnHoldEmitDecision_BoundsTheEventStream proves a daemon held for hours
// does not reproduce the audit-flood class this repo already had to fix: the
// first observation emits at warn (an `install --upgrade` rename-aside window
// is legitimately binary-absent for a moment, so error there would cry wolf),
// repeats are suppressed, and a hold that outlives the escalation interval
// emits once at error carrying the counts.
func TestSpawnHoldEmitDecision_BoundsTheEventStream(t *testing.T) {
	c := &supervisorController{spawnHolds: newSpawnHoldMarkers()}
	task := `\mcp-local-hub-memory-default`
	t0 := time.Now().UTC()

	emit, sev, _, _ := c.spawnHoldEmitDecision(task, missingBinaryReasonID, "p", t0)
	if !emit || sev != "warn" {
		t.Fatalf("first observation: emit=%v sev=%q, want true/warn", emit, sev)
	}
	// Ticks strictly INSIDE the escalation window must all be suppressed. The
	// step is derived from the window so retuning the constant retunes the test
	// rather than silently pushing a tick past the boundary.
	const ticksInsideWindow = 9
	for i := 1; i <= ticksInsideWindow; i++ {
		at := t0.Add(time.Duration(i) * missingPathEscalateAfter / (ticksInsideWindow + 1))
		if emit, _, _, _ := c.spawnHoldEmitDecision(task, missingBinaryReasonID, "p", at); emit {
			t.Fatalf("tick %d (t0+%v, inside the %v window) re-emitted; a held daemon must not flood the log",
				i, at.Sub(t0), missingPathEscalateAfter)
		}
	}
	emit, sev, heldFor, suppressed := c.spawnHoldEmitDecision(task, missingBinaryReasonID, "p", t0.Add(missingPathEscalateAfter+time.Second))
	if !emit || sev != "error" {
		t.Fatalf("persisted hold: emit=%v sev=%q, want true/error (escalate past an upgrade window)", emit, sev)
	}
	if heldFor <= 0 || suppressed == 0 {
		t.Fatalf("rollup must carry held-for (%v) and suppressed count (%d)", heldFor, suppressed)
	}

	// A CHANGED cause always re-emits immediately, never swallowed by the window.
	if emit, sev, _, _ := c.spawnHoldEmitDecision(task, missingWorkspaceReasonID, "q", t0.Add(missingPathEscalateAfter+2*time.Second)); !emit || sev != "warn" {
		t.Fatalf("changed cause: emit=%v sev=%q, want true/warn", emit, sev)
	}
}

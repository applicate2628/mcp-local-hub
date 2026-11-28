package api

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"mcp-local-hub/internal/api/apitest"
)

// These tests replace package-level seams and must remain serial. Unexpected
// identity, metric, or kill work fails before any real process can be touched.
func isolatePortPIDPolicySeams(t *testing.T) {
	t.Helper()
	oldPID, oldMetrics := lookupPortPID, lookupProcess
	oldIdentity, oldSID := processIdentityByPID, processOwnerSIDMatchesCurrentFn
	oldNameParent := processNameAndParentByPID
	oldTreeKill, oldPIDKill := taskkillProcessTreeByPIDFn, stopForceKillPIDFn
	oldPortKill := forceKillByPortFn
	t.Cleanup(func() {
		lookupPortPID, lookupProcess = oldPID, oldMetrics
		processIdentityByPID, processOwnerSIDMatchesCurrentFn = oldIdentity, oldSID
		processNameAndParentByPID = oldNameParent
		taskkillProcessTreeByPIDFn, stopForceKillPIDFn = oldTreeKill, oldPIDKill
		forceKillByPortFn = oldPortKill
	})
	lookupPortPID = nil
	lookupProcess = func(int) (int, uint64, int64, bool) {
		t.Fatal("PID-only policy queried diagnostic metrics")
		return 0, 0, 0, false
	}
	processIdentityByPID = func(pid int) (string, string, bool) {
		t.Fatalf("unexpected process identity lookup for pid %d", pid)
		return "", "", false
	}
	processOwnerSIDMatchesCurrentFn = func(pid int) (bool, error) {
		t.Fatalf("unexpected owner SID lookup for pid %d", pid)
		return false, nil
	}
	processNameAndParentByPID = func(pid int) (string, int, error) {
		t.Fatalf("unexpected process ancestry lookup for pid %d", pid)
		return "", 0, nil
	}
	taskkillProcessTreeByPIDFn = func(pid int) error {
		t.Fatalf("unexpected taskkill for pid %d", pid)
		return nil
	}
	stopForceKillPIDFn = func(pid int) error {
		t.Fatalf("unexpected trusted PID kill for pid %d", pid)
		return nil
	}
	forceKillByPortFn = func(port int, _ time.Duration) (portKillOutcome, error) {
		t.Fatalf("unexpected fallback port kill for port %d", port)
		return portKillNoListener, nil
	}
}

func TestPortPIDInitialProbeFailureIsNotNoListener(t *testing.T) {
	const port, pid = 33831, 63831
	probeErr := errors.New("netstat observation failed")
	cases := []struct {
		name  string
		pid   int
		found bool
		err   error
		want  portKillOutcome
	}{
		{name: "query failure", err: probeErr, want: portKillProbeUnknown},
		{name: "query deadline", err: ErrProbeTimeout, want: portKillProbeUnknown},
		{name: "error overrides reported PID", pid: pid, found: true, err: probeErr, want: portKillProbeUnknown},
		{name: "successful absence", want: portKillNoListener},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			isolatePortPIDPolicySeams(t)
			lookupPortPID = func(gotPort int) (int, bool, error) {
				if gotPort != port {
					t.Fatalf("lookup port = %d, want %d", gotPort, port)
				}
				return tc.pid, tc.found, tc.err
			}
			outcome, err := killDaemonByPortOutcome(port, time.Second)
			if outcome != tc.want || !errors.Is(err, tc.err) {
				t.Fatalf("port kill = (%v, %v), want (%v, cause %v)", outcome, err, tc.want, tc.err)
			}

			// Exercise the real result consumer too: an unknown observation must
			// not fall through force stop's general "already stopped" success.
			forceKillByPortFn = killDaemonByPortOutcome
			row := forceKillOneSupervisorTarget(SupervisorDaemon{TaskName: `\mcp-local-hub-r16-default`, Port: port}, nil)
			if tc.err != nil {
				if !strings.Contains(row.Err, tc.err.Error()) || row.Warning != "" {
					t.Fatalf("force-stop row = %+v, want error preserving %q and no success warning", row, tc.err)
				}
			} else if row.Err != "" || row.Warning != "" {
				t.Fatalf("force-stop row = %+v, want clean success for observed absence", row)
			}
		})
	}
}

func TestPortPIDPostKillProbeFailureIsNotRelease(t *testing.T) {
	const port, pid = 33832, 63832
	probeErr := errors.New("post-kill netstat observation failed")
	cases := []struct {
		name string
		err  error
		want portKillOutcome
	}{
		{name: "query failure", err: probeErr, want: portKillProbeUnknown},
		{name: "query deadline", err: ErrProbeTimeout, want: portKillProbeUnknown},
		{name: "confirmed release", want: portKillKilled},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			isolatePortPIDPolicySeams(t)
			identityChecked, sidChecked := false, false
			processIdentityByPID = func(gotPID int) (string, string, bool) {
				if gotPID != pid {
					t.Fatalf("identity pid = %d, want %d", gotPID, pid)
				}
				identityChecked = true
				return mcphubProcessImageName, "parent.exe", true
			}
			processOwnerSIDMatchesCurrentFn = func(gotPID int) (bool, error) {
				if gotPID != pid {
					t.Fatalf("owner SID pid = %d, want %d", gotPID, pid)
				}
				sidChecked = true
				return true, nil
			}
			kills := 0
			taskkillProcessTreeByPIDFn = func(gotPID int) error {
				if gotPID != pid || !identityChecked || !sidChecked {
					t.Fatalf("taskkill pid = %d before both identity gates passed", gotPID)
				}
				kills++
				return nil
			}
			queries := 0
			lookupPortPID = func(gotPort int) (int, bool, error) {
				if gotPort != port {
					t.Fatalf("lookup port = %d, want %d", gotPort, port)
				}
				queries++
				if queries == 1 {
					return pid, true, nil
				}
				if kills != 1 {
					t.Fatalf("release observation after %d tree kills, want one completed kill", kills)
				}
				return 0, false, tc.err
			}

			outcome, err := killDaemonByPortOutcome(port, time.Second)
			if outcome != tc.want || !errors.Is(err, tc.err) {
				t.Fatalf("port kill = (%v, %v), want (%v, cause %v)", outcome, err, tc.want, tc.err)
			}
			if kills != 1 || queries != 2 {
				t.Fatalf("tree kills = %d, observations = %d; want one kill and its initial/release observations", kills, queries)
			}
		})
	}
}

func TestPortPIDWaitPreservesProbeFailure(t *testing.T) {
	const port, pid = 33833, 63833
	probeErr := errors.New("release query could not run")
	for _, cause := range []error{probeErr, ErrProbeTimeout} {
		t.Run(cause.Error(), func(t *testing.T) {
			isolatePortPIDPolicySeams(t)
			lookupPortPID = func(int) (int, bool, error) { return 0, false, cause }
			warning, err := waitPortReleasedAfterPIDKill(port, pid, 0)
			if !errors.Is(err, cause) || warning != "" {
				t.Fatalf("release wait = (%q, %v), want preserved cause %v and no success warning", warning, err, cause)
			}
		})
	}
}

func TestPortPIDForceStopRetainsTrustedKillResult(t *testing.T) {
	const port, pid = 33834, 63834
	const taskName = `\mcp-local-hub-r16-default`
	probeErr := errors.New("release result is unknown")
	cases := []struct {
		name       string
		wired      bool
		probeError error
	}{
		{name: "failed observation", wired: true, probeError: probeErr},
		{name: "confirmed release", wired: true},
		{name: "surface structurally unavailable"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			isolatePortPIDPolicySeams(t)
			processIdentityByPID = func(gotPID int) (string, string, bool) {
				if gotPID != pid {
					t.Fatalf("identity pid = %d, want %d", gotPID, pid)
				}
				return mcphubProcessImageName, "parent.exe", true
			}
			processOwnerSIDMatchesCurrentFn = func(gotPID int) (bool, error) {
				if gotPID != pid {
					t.Fatalf("owner SID pid = %d, want %d", gotPID, pid)
				}
				return true, nil
			}
			kills := 0
			stopForceKillPIDFn = func(gotPID int) error {
				if gotPID != pid {
					t.Fatalf("trusted kill pid = %d, want %d", gotPID, pid)
				}
				kills++
				return nil
			}
			if tc.wired {
				lookupPortPID = func(gotPort int) (int, bool, error) {
					if gotPort != port || kills != 1 {
						t.Fatalf("release lookup port = %d after %d kills; want port %d after one kill", gotPort, kills, port)
					}
					return 0, false, tc.probeError
				}
			}
			row := forceKillOneSupervisorTarget(
				SupervisorDaemon{TaskName: taskName, Port: port},
				map[string]int{strings.TrimPrefix(taskName, `\`): pid},
			)
			if row.TaskName != taskName || kills != 1 {
				t.Fatalf("force-stop row = %+v, trusted kills = %d; want original task and exactly one kill", row, kills)
			}
			switch {
			case tc.probeError != nil:
				if !strings.Contains(row.Err, tc.probeError.Error()) ||
					!strings.Contains(row.Err, fmt.Sprintf("force kill daemon by pid %d succeeded", pid)) || row.Warning != "" {
					t.Fatalf("force-stop row = %+v, want unknown-release error plus completed PID-kill context", row)
				}
			case !tc.wired:
				if row.Err != "" || !strings.Contains(row.Warning, "port-release proof unavailable") ||
					!strings.Contains(row.Warning, "trusted PID/tree kill succeeded") {
					t.Fatalf("force-stop row = %+v, want success with unavailable-proof warning", row)
				}
			default:
				if row.Err != "" || row.Warning != "" {
					t.Fatalf("force-stop row = %+v, want clean success for confirmed release", row)
				}
			}
		})
	}
}

func TestPortPIDUnknownOwnershipDoesNotPermitDowngrade(t *testing.T) {
	const port, wrapperPID = 33835, 63835
	probeErr := errors.New("port owner query failed")
	cases := []struct {
		name         string
		wired        bool
		pid          int
		found        bool
		err          error
		wantOutcome  probeOutcome
		wantOwned    bool
		wantResolved bool
	}{
		{name: "surface absent", wantOutcome: probeSurfaceUnavailable},
		{name: "successful snapshot without owner", wired: true, wantOutcome: probeUnknown, wantResolved: true},
		{name: "query failure", wired: true, err: probeErr, wantOutcome: probeUnknown, wantResolved: true},
		{name: "query timeout", wired: true, err: ErrProbeTimeout, wantOutcome: probeUnknown, wantResolved: true},
		{name: "error overrides matching wrapper", wired: true, pid: wrapperPID, found: true, err: probeErr, wantOutcome: probeUnknown, wantResolved: true},
		{name: "matching wrapper observed", wired: true, pid: wrapperPID, found: true, wantOutcome: probeResolved, wantOwned: true, wantResolved: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			isolatePortPIDPolicySeams(t)
			if tc.wired {
				lookupPortPID = func(int) (int, bool, error) { return tc.pid, tc.found, tc.err }
			}
			pid, outcome := supervisorOwnedPortPIDOutcome(port)
			wantPID := 0
			if tc.wantOutcome == probeResolved {
				wantPID = wrapperPID
			}
			if pid != wantPID || outcome != tc.wantOutcome {
				t.Fatalf("owner observation = (%d, %v), want (%d, %v)", pid, outcome, wantPID, tc.wantOutcome)
			}
			owned, resolved := internalPortListenerChainsToWrapperPID(port, wrapperPID)
			if owned != tc.wantOwned || resolved != tc.wantResolved {
				t.Fatalf("internal ownership = (%v, %v), want (%v, %v)", owned, resolved, tc.wantOwned, tc.wantResolved)
			}
		})
	}
}

func TestPortPIDSerenaRemovalPreservesIntentWhenProbeIsUnknown(t *testing.T) {
	isolatePortPIDPolicySeams(t)
	t.Cleanup(SetDaemonStateRootForTest(apitest.HardenedTempDir(t)))
	canonical, err := CanonicalWorkspacePath(t.TempDir())
	if err != nil {
		t.Fatalf("CanonicalWorkspacePath: %v", err)
	}
	descriptor := SupervisorDaemon{
		TaskName:  SerenaTaskNameForWorkspace(canonical),
		Server:    "serena",
		Daemon:    WorkspaceKey(canonical),
		Workspace: canonical,
		Port:      33836,
	}
	intentPath, err := DefaultSupervisorIntentPath()
	if err != nil {
		t.Fatalf("DefaultSupervisorIntentPath: %v", err)
	}
	if err := WriteSupervisorIntent(intentPath, &SupervisorIntentFile{
		Version: 1,
		Daemons: []SupervisorDaemon{descriptor},
		Stops: map[string]DaemonIntent{descriptor.TaskName: {
			Desired:   IntentDesiredStopped,
			Reason:    IntentReasonUserStop,
			UpdatedAt: time.Date(2026, 11, 18, 0, 0, 0, 0, time.UTC),
		}},
	}); err != nil {
		t.Fatalf("WriteSupervisorIntent: %v", err)
	}
	before, err := os.ReadFile(intentPath)
	if err != nil {
		t.Fatalf("read seeded supervisor intent: %v", err)
	}
	oldReconcile := registerSupervisorReconcileFn
	t.Cleanup(func() { registerSupervisorReconcileFn = oldReconcile })
	reconcileCalls := 0
	registerSupervisorReconcileFn = func(_ context.Context, apply bool) (ReconcileResponse, error) {
		reconcileCalls++
		if !apply {
			t.Fatal("Serena removal must request an applied reconcile")
		}
		return ReconcileResponse{}, nil
	}
	probeErr := errors.New("cannot observe Serena port owner")
	lookupPortPID = func(port int) (int, bool, error) {
		if port != descriptor.Port {
			t.Fatalf("lookup port = %d, want %d", port, descriptor.Port)
		}
		return 0, false, probeErr
	}
	// Use the real producer of portKillOutcome: a fake enum would not catch
	// accidental suppression of query errors as the benign NoListener result.
	forceKillByPortFn = killDaemonByPortOutcome
	a := NewAPI()
	removed, err := a.RemoveSerenaSupervisorIntentForWorkspace(canonical)
	if removed || !errors.Is(err, probeErr) {
		t.Fatalf("Serena removal = (%v, %v), want not removed with cause %v", removed, err, probeErr)
	}
	after, err := os.ReadFile(intentPath)
	if err != nil {
		t.Fatalf("read supervisor intent after unknown port observation: %v", err)
	}
	if !bytes.Equal(before, after) {
		t.Fatal("unknown port observation changed the persisted descriptor or stop artifacts")
	}
	if reconcileCalls != 0 {
		t.Fatalf("reconcile calls after failed observation = %d, want none", reconcileCalls)
	}

	// Positive control on the same saved intent: a successful empty port
	// observation permits the real descriptor/stop mutation and reconcile.
	lookupPortPID = func(int) (int, bool, error) { return 0, false, nil }
	removed, err = a.RemoveSerenaSupervisorIntentForWorkspace(canonical)
	if !removed || err != nil {
		t.Fatalf("Serena removal after confirmed absence = (%v, %v), want successful removal", removed, err)
	}
	got, err := ReadSupervisorIntent(intentPath)
	if err != nil {
		t.Fatalf("ReadSupervisorIntent after removal: %v", err)
	}
	if got.FindSupervisorDaemonByTaskName(descriptor.TaskName) != nil {
		t.Fatal("Serena descriptor survived successful removal")
	}
	if _, present := got.Stops[descriptor.TaskName]; present {
		t.Fatal("Serena stop artifact survived successful removal")
	}
	if reconcileCalls != 1 {
		t.Fatalf("reconcile calls after confirmed absence = %d, want one", reconcileCalls)
	}
}

package api

import (
	"context"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"mcp-local-hub/internal/scheduler"
)

// Scheduler.Stop normalizes an absent or completed native task to nil. Other
// failures must remain visible even after the port-owning daemon was killed;
// a still-live launch action is a separate resource. In particular, a nil
// killDaemonByPort result can also mean that no lookup was available.
//
// Exercise the public lifecycle methods with the production port-kill decision
// path. Every process observation, kill, scheduler operation and bind probe is
// replaced below; no native task, process or listener is touched.
func TestLegacySchedulerStopFailuresReachLifecycleResults(t *testing.T) {
	const (
		alpha = schedulerStopFailureAlpha
		beta  = schedulerStopFailureBeta
	)
	for _, operation := range []struct {
		name    string
		restart bool
		run     func(*API) ([]RestartResult, error)
	}{
		{"Stop", false, func(a *API) ([]RestartResult, error) { return a.Stop("schedulerstop", "") }},
		{"StopAll", false, (*API).StopAll},
		{"Restart", true, func(a *API) ([]RestartResult, error) { return a.Restart("schedulerstop", "") }},
		{"RestartAll", true, (*API).RestartAll},
	} {
		t.Run(operation.name, func(t *testing.T) {
			for _, scenario := range []struct {
				name    string
				port    string
				stopErr error
			}{
				{"completed_action_no_listener", "no-listener", nil},
				{"killed_then_stopped", "killed", nil},
				{"denied_after_kill", "killed", scheduler.ErrPermissionDenied},
				{"denied_without_listener", "no-listener", scheduler.ErrPermissionDenied},
				{"unavailable_lookup_and_stop", "lookup-unavailable", scheduler.ErrUnavailable},
				{"portless_canceled_stop", "no-port", context.Canceled},
			} {
				t.Run(scenario.name, func(t *testing.T) {
					fake, killed := schedulerStopFailureSetup(t, scenario.port, scenario.stopErr)

					results, err := operation.run(NewAPI())
					if err != nil {
						t.Fatalf("lifecycle orchestration error = %v, want per-task results", err)
					}
					if len(results) != 2 {
						t.Fatalf("results = %+v, want both task outcomes", results)
					}
					byName := make(map[string]RestartResult)
					for _, result := range results {
						byName[result.TaskName] = result
					}
					alphaResult, alphaOK := byName[alpha]
					betaResult, betaOK := byName[beta]
					if !alphaOK || !betaOK {
						t.Fatalf("results lost a task or duplicated a row: %+v", results)
					}
					wantErr := ""
					if scenario.stopErr != nil {
						wantErr = "stop scheduler task: " + scenario.stopErr.Error()
					}
					if alphaResult.Err != wantErr || betaResult.Err != "" {
						t.Errorf("results = %+v, want alpha error %q and beta success", results, wantErr)
					}
					if !slices.Equal(fake.stopNames, []string{alpha, beta}) {
						t.Errorf("stopped task names = %v, want both tasks despite a per-task failure", fake.stopNames)
					}
					var wantRun []string
					if operation.restart {
						if scenario.stopErr == nil {
							wantRun = append(wantRun, alpha)
						}
						wantRun = append(wantRun, beta)
					}
					if !slices.Equal(fake.runNames, wantRun) {
						t.Errorf("started task names = %v, want %v; a failed stop must not authorize Run", fake.runNames, wantRun)
					}
					if operation.name == "Restart" {
						wantDesired := ""
						if scenario.stopErr != nil {
							wantDesired = IntentDesiredStopped
						}
						if got := subBlockDesiredForTest(alpha); got != wantDesired {
							t.Errorf("alpha intent desired = %q, want %q", got, wantDesired)
						}
						if got := subBlockDesiredForTest(beta); got != "" {
							t.Errorf("successful beta restart retained stop intent %q", got)
						}
					}
					wantKilled := 0
					if scenario.port == "killed" {
						wantKilled = 2
					}
					if len(killed) != wantKilled {
						t.Errorf("killed process set = %v, want %d observed kills", killed, wantKilled)
					}
				})
			}
		})
	}
}

const (
	schedulerStopFailureAlpha = `\mcp-local-hub-schedulerstop-alpha`
	schedulerStopFailureBeta  = `\mcp-local-hub-schedulerstop-beta`
)

func schedulerStopFailureSetup(t *testing.T, portMode string, stopErr error) (*lifecycleStopErrorScheduler, map[int]bool) {
	t.Helper()
	const (
		alpha = schedulerStopFailureAlpha
		beta  = schedulerStopFailureBeta
	)
	stateDir := daemonIntentTestHelper(t)
	t.Setenv("LOCALAPPDATA", stateDir)
	t.Setenv("XDG_STATE_HOME", stateDir)
	alphaPort, betaPort := 33180, 33181
	if portMode == "no-port" {
		alphaPort, betaPort = 0, 0
	}
	seedManifestsWithDaemons(t, map[string]map[string]int{
		"schedulerstop": {"alpha": alphaPort, "beta": betaPort},
	})
	installRecordingAudit(t, &recordingAuditWriter{})
	// Empty daemon ownership forces the legacy scheduler path without
	// contacting a live supervisor. Seed stops so a failed Restart can
	// also prove that it did not re-enable the failed task in intent.
	if err := WriteSupervisorIntent(filepath.Join(stateDir, supervisorIntentFileLeaf), &SupervisorIntentFile{Version: 1}); err != nil {
		t.Fatalf("seed empty supervisor intent: %v", err)
	}
	t.Cleanup(RegisterSupervisorControlAdmission(func(context.Context) error { return nil }))
	seedSubBlockStop(t, alpha)
	seedSubBlockStop(t, beta)

	fake := &lifecycleStopErrorScheduler{
		restartAllFakeScheduler: restartAllFakeScheduler{tasks: []scheduler.TaskStatus{
			{Name: alpha, RuntimeState: scheduler.TaskRuntimeRunning},
			{Name: beta, RuntimeState: scheduler.TaskRuntimeRunning},
		}},
		failedTask: alpha,
		stopErr:    stopErr,
	}
	priorStop, priorRestart := stopSchedulerFactory, restartSchedulerFactory
	stopSchedulerFactory = func() (scheduler.Scheduler, error) { return fake, nil }
	restartSchedulerFactory = func() (scheduler.Scheduler, error) { return fake, nil }
	t.Cleanup(func() {
		stopSchedulerFactory, restartSchedulerFactory = priorStop, priorRestart
	})

	priorKill, priorWait := killByPortFn, waitForPortFreeFn
	priorLookup, priorIdentity := lookupProcess, processIdentityByPID
	priorOwner, priorTaskkill := processOwnerSIDMatchesCurrentFn, taskkillProcessTreeByPIDFn
	t.Cleanup(func() {
		killByPortFn, waitForPortFreeFn = priorKill, priorWait
		lookupProcess, processIdentityByPID = priorLookup, priorIdentity
		processOwnerSIDMatchesCurrentFn, taskkillProcessTreeByPIDFn = priorOwner, priorTaskkill
	})
	killByPortFn = killDaemonByPort
	waitForPortFreeFn = func(port int, _ time.Duration) error {
		if port != alphaPort && port != betaPort {
			t.Fatalf("unexpected bind probe port %d", port)
		}
		return nil
	}
	const alphaPID, betaPID = 63180, 63181
	killed := make(map[int]bool)
	lookupProcess = func(port int) (int, uint64, int64, bool) {
		if portMode == "no-port" {
			t.Fatal("portless task reached process lookup")
		}
		pid := alphaPID
		if port == betaPort {
			pid = betaPID
		} else if port != alphaPort {
			t.Fatalf("unexpected process lookup port %d", port)
		}
		if portMode == "no-listener" || killed[pid] {
			return 0, 0, 0, false
		}
		return pid, 0, 0, true
	}
	if portMode == "lookup-unavailable" {
		lookupProcess = nil
	}
	processIdentityByPID = func(pid int) (string, string, bool) {
		if pid != alphaPID && pid != betaPID {
			t.Fatalf("unexpected process identity PID %d", pid)
		}
		return mcphubProcessImageName, "", true
	}
	processOwnerSIDMatchesCurrentFn = func(int) (bool, error) { return true, nil }
	taskkillProcessTreeByPIDFn = func(pid int) error {
		if portMode != "killed" || (pid != alphaPID && pid != betaPID) {
			t.Fatalf("unexpected process kill PID %d in %s", pid, portMode)
		}
		killed[pid] = true
		return nil
	}

	return fake, killed
}

type lifecycleStopErrorScheduler struct {
	restartAllFakeScheduler
	failedTask string
	stopErr    error
}

func (s *lifecycleStopErrorScheduler) Stop(name string) error {
	s.stopNames = append(s.stopNames, name)
	if name == s.failedTask {
		return s.stopErr
	}
	return nil
}

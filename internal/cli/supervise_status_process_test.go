package cli

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"sync"
	"testing"
	"time"

	"mcp-local-hub/internal/api"
	"mcp-local-hub/internal/api/apitest"
	"mcp-local-hub/internal/process"
)

const statusObservationPID = 500901

func seedStatusPIDObservation(t *testing.T) (string, *DaemonRuntimeTracker, api.SupervisorDaemon, time.Time) {
	t.Helper()
	stateDir := apitest.HardenedTempDir(t)
	d := api.SupervisorDaemon{
		TaskName: `\mcp-local-hub-memory-default`,
		Server:   "memory",
		Daemon:   "default",
		Command:  filepath.Join(stateDir, "daemon-command"),
		Args:     []string{"daemon", "--server", "memory"},
		Port:     9123,
	}
	if err := api.WriteSupervisorIntent(filepath.Join(stateDir, "supervisor-intent.json"), &api.SupervisorIntentFile{
		Version: 1, Daemons: []api.SupervisorDaemon{d},
	}); err != nil {
		t.Fatalf("write intent: %v", err)
	}
	tracker := NewDaemonRuntimeTracker()
	startedAt := time.Now().UTC().Add(-time.Hour)
	tracker.MarkSpawned(d.TaskName, statusObservationPID, startedAt)
	prevSelf := supervisorSelfPIDFn
	supervisorSelfPIDFn = func() int { return 1 }
	t.Cleanup(func() { supervisorSelfPIDFn = prevSelf })
	return stateDir, tracker, d, startedAt
}

func TestSupervisorStatusPIDObservationProjectsResults(t *testing.T) {
	const ram = uint64(48 * 1024 * 1024)
	alive := process.PIDStatusObservation{State: process.PIDStateAlive, ResidentBytes: ram, ResidentBytesKnown: true}
	tests := []struct {
		name        string
		observation process.PIDStatusObservation
		observeErr  error
		ownerPID    int
		snapshotErr error
		wantState   string
		wantRAM     uint64
	}{
		{name: "matching", observation: alive, ownerPID: statusObservationPID, wantState: "Running", wantRAM: ram},
		{name: "memory-unknown", observation: process.PIDStatusObservation{State: process.PIDStateAlive}, ownerPID: statusObservationPID, wantState: "Running"},
		{name: "memory-zero", observation: process.PIDStatusObservation{State: process.PIDStateAlive, ResidentBytesKnown: true}, ownerPID: statusObservationPID, wantState: "Running"},
		{name: "required-query-error", observation: alive, observeErr: errors.New("identity query denied"), ownerPID: statusObservationPID, wantState: "Restarting"},
		{name: "liveness-unknown-error", observation: process.PIDStatusObservation{State: process.PIDStateUnknown}, observeErr: errors.New("wait failed"), ownerPID: statusObservationPID, wantState: "Restarting"},
		{name: "exited", observation: process.PIDStatusObservation{State: process.PIDStateDead}, ownerPID: statusObservationPID, wantState: "Restarting"},
		{name: "port-snapshot-error", observation: alive, snapshotErr: errors.New("port query denied"), wantState: "Running", wantRAM: ram},
		{name: "foreign-port-owner", observation: alive, ownerPID: statusObservationPID + 1, wantState: "Restarting"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			stateDir, tracker, d, startedAt := seedStatusPIDObservation(t)
			var observed int
			restore := setSupervisorLivenessProbeForTest(supervisorLivenessProbe{
				PIDAlive: func(int) bool {
					t.Error("status reopened the scalar liveness path")
					return false
				},
				PIDIdentity: func(process.PIDIdentityProof) error {
					t.Error("status reopened the scalar identity path")
					return errors.New("unexpected scalar identity")
				},
				PIDStatus: func(proof process.PIDIdentityProof) (process.PIDStatusObservation, error) {
					observed++
					if proof.PID != statusObservationPID || proof.ExecutablePath != daemonExpectedIdentityExe(d.Command) || proof.StartedAt != daemonRuntimeStartedAt(startedAt) {
						t.Errorf("observation proof = %+v, want configured daemon PID/image/start", proof)
					}
					return tt.observation, tt.observeErr
				},
				PortOwnerPID: func(int) (int, bool, error) {
					t.Error("status bypassed the shared port snapshot")
					return 0, false, errors.New("unexpected per-port probe")
				},
			})
			t.Cleanup(restore)
			withFakeRSS(t, func(int) (uint64, bool) {
				t.Error("status reopened RAM by PID after a combined observation")
				return 99 * ram, true
			})
			withFakePortOwnersSnapshot(t, func() (map[int]int, error) {
				snapshot := map[int]int{}
				if tt.ownerPID > 0 {
					snapshot[d.Port] = tt.ownerPID
				}
				return snapshot, tt.snapshotErr
			})

			rows, err := supervisorStatusDaemons(stateDir, tracker, nil)
			if err != nil || len(rows) != 1 {
				t.Fatalf("status rows=%+v err=%v, want one row", rows, err)
			}
			want := map[string]any{
				"task_name": d.TaskName, "server": d.Server, "daemon": d.Daemon,
				"command": d.Command, "args": d.Args, "workspace": d.Workspace,
				"port": d.Port, "state": tt.wantState, "current_pid": statusObservationPID,
				"started_at": daemonRuntimeStartedAt(startedAt), "is_maintenance": false, "crashes_in_window": 0,
			}
			if tt.wantState == "Restarting" {
				want["current_pid"] = 0
				want["stale_pid"] = statusObservationPID
			}
			if tt.wantRAM > 0 {
				want["ram_bytes"] = tt.wantRAM
			}
			if !reflect.DeepEqual(rows[0], want) {
				t.Fatalf("status row = %#v, want %#v", rows[0], want)
			}
			if observed != 1 {
				t.Fatalf("process observations = %d, want one for the row", observed)
			}
			entry, _ := tracker.Get(d.TaskName)
			if entry.CurrentPID != statusObservationPID || !entry.StartedAt.Equal(startedAt) {
				t.Fatalf("read-only status changed tracker identity: %+v", entry)
			}
		})
	}
}

func TestSupervisorStatusPIDObservationKeepsMissingIdentityGate(t *testing.T) {
	for _, tc := range []struct {
		name string
		pid  int
	}{{"missing-pid", 0}, {"missing-start", statusObservationPID}} {
		t.Run(tc.name, func(t *testing.T) {
			pid := tc.pid
			stateDir, tracker, d, _ := seedStatusPIDObservation(t)
			tracker.MarkSpawned(d.TaskName, pid, time.Time{})
			observed := 0
			restore := setSupervisorLivenessProbeForTest(supervisorLivenessProbe{
				PIDStatus: func(proof process.PIDIdentityProof) (process.PIDStatusObservation, error) {
					observed++
					if proof.StartedAt != "" {
						t.Errorf("zero tracker start became %q, want empty proof", proof.StartedAt)
					}
					// Even a permissive observer cannot bypass the canonical
					// missing-start gate or turn this RAM into an accepted row.
					return process.PIDStatusObservation{State: process.PIDStateAlive, ResidentBytes: 1024, ResidentBytesKnown: true}, nil
				},
				PortLive: func(int) bool { return true },
			})
			t.Cleanup(restore)
			withFakeRSS(t, func(int) (uint64, bool) {
				t.Error("missing identity reached scalar RAM")
				return 2048, true
			})
			rows, err := supervisorStatusDaemons(stateDir, tracker, nil)
			if err != nil || len(rows) != 1 || rows[0]["state"] != "Restarting" || rows[0]["current_pid"] != 0 {
				t.Fatalf("status rows=%+v err=%v, want Restarting/current_pid=0", rows, err)
			}
			if _, ok := rows[0]["ram_bytes"]; ok {
				t.Fatalf("missing identity emitted RAM: %+v", rows[0])
			}
			wantObserved := 0
			if pid > 0 {
				wantObserved = 1
			}
			if observed != wantObserved {
				t.Fatalf("observations=%d for PID %d, want %d", observed, pid, wantObserved)
			}
		})
	}
}

func TestSupervisorStatusPIDObservationKeepsPendingStopRAM(t *testing.T) {
	for _, failed := range []bool{false, true} {
		wantState := "Stopping"
		if failed {
			wantState = "Failed"
		}
		t.Run(wantState, func(t *testing.T) {
			stateDir, tracker, d, _ := seedStatusPIDObservation(t)
			statePath := filepath.Join(stateDir, "supervisor-state.json")
			receipt := beginStopSettlementForTest(t, tracker, statePath, d.TaskName)
			if failed {
				if _, err := tracker.AdvanceStopSettlement(statePath, receipt, api.StopSettlementPhaseFailed, api.StopSettlementFailureProcessAlive, "test process still present"); err != nil {
					t.Fatalf("mark stop failed: %v", err)
				}
			}
			restore := setSupervisorLivenessProbeForTest(supervisorLivenessProbe{
				PIDAlive: func(int) bool { t.Error("pending stop ran liveness"); return false },
				PIDStatus: func(process.PIDIdentityProof) (process.PIDStatusObservation, error) {
					t.Error("pending stop acquired a mandatory status observation")
					return process.PIDStatusObservation{}, errors.New("unexpected observation")
				},
			})
			t.Cleanup(restore)
			ramCalls := 0
			withFakeRSS(t, func(pid int) (uint64, bool) {
				ramCalls++
				if pid != statusObservationPID {
					t.Errorf("pending stop RAM PID = %d, want %d", pid, statusObservationPID)
				}
				return 4096, true
			})
			rows, err := supervisorStatusDaemons(stateDir, tracker, nil)
			if err != nil || len(rows) != 1 {
				t.Fatalf("status rows=%+v err=%v, want one pending-stop row", rows, err)
			}
			if rows[0]["state"] != wantState || rows[0]["current_pid"] != statusObservationPID || rows[0]["ram_bytes"] != uint64(4096) || ramCalls != 1 {
				t.Fatalf("pending-stop row=%+v RAM calls=%d, want %s with previous optional RAM", rows[0], ramCalls, wantState)
			}
			if _, ok := rows[0]["stop_settlement"]; !ok {
				t.Fatalf("pending stop receipt disappeared: %+v", rows[0])
			}
		})
	}
}

func TestSupervisorStatusPIDObservationRejectsChangedCommandProof(t *testing.T) {
	stateDir, tracker, d, _ := seedStatusPIDObservation(t)
	d.Command = "status-observation-command.exe"
	firstDir, secondDir := filepath.Join(stateDir, "first"), filepath.Join(stateDir, "second")
	for _, dir := range []string{firstDir, secondDir} {
		if err := os.Mkdir(dir, 0700); err != nil {
			t.Fatalf("create command fixture directory: %v", err)
		}
		// LookPath only checks these files; no fixture is executed.
		if err := os.WriteFile(filepath.Join(dir, d.Command), []byte("lookup fixture"), 0700); err != nil {
			t.Fatalf("create command fixture: %v", err)
		}
	}
	if err := api.WriteSupervisorIntent(filepath.Join(stateDir, "supervisor-intent.json"), &api.SupervisorIntentFile{Version: 1, Daemons: []api.SupervisorDaemon{d}}); err != nil {
		t.Fatalf("rewrite intent with bare command: %v", err)
	}
	t.Setenv("PATH", firstDir)
	t.Setenv("PATHEXT", ".EXE")
	firstExe := daemonExpectedIdentityExe(d.Command)
	t.Setenv("PATH", secondDir)
	if secondExe := daemonExpectedIdentityExe(d.Command); firstExe == secondExe {
		t.Fatalf("command fixtures resolved to the same image %q", firstExe)
	}
	t.Setenv("PATH", firstDir)
	restore := setSupervisorLivenessProbeForTest(supervisorLivenessProbe{
		PIDStatus: func(proof process.PIDIdentityProof) (process.PIDStatusObservation, error) {
			if proof.ExecutablePath != firstExe {
				t.Errorf("observed image=%q, want first resolved command %q", proof.ExecutablePath, firstExe)
			}
			t.Setenv("PATH", secondDir)
			return process.PIDStatusObservation{State: process.PIDStateAlive, ResidentBytes: 1024, ResidentBytesKnown: true}, nil
		},
		PortLive: func(int) bool { return true },
	})
	t.Cleanup(restore)
	withFakeRSS(t, func(int) (uint64, bool) {
		t.Error("changed command proof fell back to scalar RAM")
		return 2048, true
	})
	rows, err := supervisorStatusDaemons(stateDir, tracker, nil)
	if err != nil || len(rows) != 1 || rows[0]["state"] != "Restarting" || rows[0]["current_pid"] != 0 || rows[0]["stale_pid"] != statusObservationPID {
		t.Fatalf("changed proof status rows=%+v err=%v, want Restarting/current0/stale", rows, err)
	}
	if _, ok := rows[0]["ram_bytes"]; ok {
		t.Fatalf("changed command proof emitted RAM: %+v", rows[0])
	}
}

func TestSupervisorStatusPIDObservationDoesNotChangeCanonicalLiveness(t *testing.T) {
	_, _, d, startedAt := seedStatusPIDObservation(t)
	probe := supervisorLivenessProbe{
		PIDAlive:    func(int) bool { return true },
		PIDIdentity: func(process.PIDIdentityProof) error { return nil },
		PIDStatus: func(process.PIDIdentityProof) (process.PIDStatusObservation, error) {
			t.Error("canonical liveness used the status-only observer")
			return process.PIDStatusObservation{}, errors.New("unexpected observation")
		},
		PortOwnerPID: func(int) (int, bool, error) { return statusObservationPID, true, nil },
	}
	verdict := supervisorDaemonLivenessVerdictWithProbe(d, DaemonRuntimeEntry{CurrentPID: statusObservationPID, StartedAt: startedAt}, time.Now().UTC(), probe, time.Minute)
	if !verdict.Live || !verdict.PortBound || !verdict.TargetReady() {
		t.Fatalf("canonical liveness = %+v, want existing identity + owned-port result", verdict)
	}
}

func TestSupervisorStatusPIDObservationConcurrentRequestsStayLocal(t *testing.T) {
	stateDir, tracker, d, startedAt := seedStatusPIDObservation(t)
	entered := make(chan struct{})
	release := make(chan struct{})
	var releaseOnce sync.Once
	unblock := func() { releaseOnce.Do(func() { close(release) }) }
	restore := setSupervisorLivenessProbeForTest(supervisorLivenessProbe{
		PIDStatus: func(proof process.PIDIdentityProof) (process.PIDStatusObservation, error) {
			if proof.PID == statusObservationPID {
				close(entered)
				<-release
			}
			return process.PIDStatusObservation{State: process.PIDStateAlive, ResidentBytes: uint64(proof.PID), ResidentBytesKnown: true}, nil
		},
		PortOwnerPID: func(int) (int, bool, error) { return 0, false, errors.New("use snapshot") },
	})
	t.Cleanup(restore)
	oldSnapshot := loopbackPortOwnersSnapshotFn
	loopbackPortOwnersSnapshotFn = func() (map[int]int, error) {
		entry, _ := tracker.Get(d.TaskName)
		return map[int]int{d.Port: entry.CurrentPID}, nil
	}
	t.Cleanup(func() { loopbackPortOwnersSnapshotFn = oldSnapshot })
	withFakeRSS(t, func(int) (uint64, bool) {
		t.Error("concurrent status reached scalar RAM")
		return 0, false
	})
	var wg sync.WaitGroup
	t.Cleanup(func() { unblock(); wg.Wait() })
	type result struct {
		rows []map[string]any
		err  error
	}
	startRequest := func() <-chan result {
		out := make(chan result, 1)
		wg.Add(1)
		go func() {
			defer wg.Done()
			rows, err := supervisorStatusDaemons(stateDir, tracker, nil)
			out <- result{rows: rows, err: err}
		}()
		return out
	}
	first := startRequest()
	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		t.Fatal("first status did not enter observation")
	}
	updated := make(chan struct{})
	wg.Add(1)
	go func() {
		defer wg.Done()
		tracker.MarkSpawned(d.TaskName, statusObservationPID+1, startedAt.Add(time.Second))
		close(updated)
	}()
	select {
	case <-updated:
	case <-time.After(2 * time.Second):
		t.Fatal("status held the tracker lock while observing the process")
	}
	second := startRequest()
	assertResult := func(ch <-chan result, pid int, start time.Time) {
		t.Helper()
		select {
		case got := <-ch:
			if got.err != nil || len(got.rows) != 1 {
				t.Fatalf("status rows=%+v err=%v, want one row", got.rows, got.err)
			}
			row := got.rows[0]
			if row["state"] != "Running" || row["current_pid"] != pid || row["started_at"] != daemonRuntimeStartedAt(start) || row["ram_bytes"] != uint64(pid) {
				t.Fatalf("status mixed request identities: row=%+v, want PID/RAM %d start %s", row, pid, daemonRuntimeStartedAt(start))
			}
		case <-time.After(2 * time.Second):
			t.Fatal("status request did not complete")
		}
	}
	assertResult(second, statusObservationPID+1, startedAt.Add(time.Second))
	unblock()
	assertResult(first, statusObservationPID, startedAt)
}

func TestSupervisorStatusPIDObservationPlatformDefault(t *testing.T) {
	probe := defaultSupervisorLivenessProbe()
	if got, want := probe.PIDStatus != nil, runtime.GOOS == "windows"; got != want {
		t.Fatalf("PIDStatus present=%v on %s, want %v", got, runtime.GOOS, want)
	}
}

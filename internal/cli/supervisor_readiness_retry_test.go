package cli

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"mcp-local-hub/internal/api"
	"mcp-local-hub/internal/api/apitest"
	"mcp-local-hub/internal/mcpcompat/readinesswire"
)

func TestObserveSupervisorReadiness_RetryableBudgetExhaustionRecoversSlowlySameGeneration(t *testing.T) {
	tracker := NewDaemonRuntimeTracker()
	task := `\mcp-local-hub-fetch-default`
	started := time.Date(2026, 9, 11, 15, 30, 0, 0, time.UTC)
	generation := tracker.MarkSpawned(task, 4321, started)
	now := started
	var waits []time.Duration
	probeCalls := 0
	eventPath := filepath.Join(apitest.HardenedTempDir(t), "supervisor-events.log")
	events, err := api.OpenSupervisorEventLog(eventPath)
	if err != nil {
		t.Fatal(err)
	}
	defer events.Close()

	observeSupervisorReadinessWithDeps(
		context.Background(), tracker, events,
		api.SupervisorDaemon{TaskName: task, Server: "fetch", Daemon: "default", Port: 9133, StartupBindDeadlineSeconds: 1},
		generation, 4321, started,
		supervisorReadinessObserverDeps{
			now: func() time.Time { return now },
			wait: func(_ context.Context, delay time.Duration) bool {
				waits = append(waits, delay)
				now = now.Add(delay)
				return true
			},
			inspect: func(api.SupervisorDaemon, DaemonRuntimeEntry, time.Time) (bool, bool) {
				return true, true
			},
			probe: func(context.Context, int) api.MCPReadinessResult {
				probeCalls++
				if probeCalls <= 2 {
					if probeCalls == 1 {
						now = started.Add(time.Second + api.MCPReadinessProbeBudget)
					}
					return api.MCPReadinessResult{
						SchemaVersion: "mcp-readiness-v1", State: api.MCPReadinessUnready,
						Stage:     api.MCPReadinessStageInitialize,
						FailureID: readinesswire.FailureReadinessHostUnavailable, Retryable: true,
					}
				}
				return api.MCPReadinessResult{
					SchemaVersion: "mcp-readiness-v1", State: api.MCPReadinessReady,
					Stage: api.MCPReadinessStageToolsList, ToolCount: 3,
				}
			},
		},
	)

	if probeCalls != 3 || len(waits) != 2 ||
		waits[0] != supervisorLivenessInterval || waits[1] != supervisorLivenessInterval {
		t.Fatalf("calls probe=%d waits=%v, want three probes and two slow waits", probeCalls, waits)
	}
	entry, ok := tracker.Get(task)
	if !ok || entry.PIDGeneration != generation || entry.ReadinessObservation == nil {
		t.Fatalf("entry=%#v", entry)
	}
	got := api.ReduceReadinessV1(api.ReadinessRequest{
		Mode: api.ReadinessModeAwaitSettled, Observations: []api.DaemonReadinessObservationV1{*entry.ReadinessObservation},
	})
	if len(got.Daemons) != 1 || got.Daemons[0].ServiceState != api.ServiceStateRunning || !got.Daemons[0].Settled {
		t.Fatalf("readiness=%#v", got.Daemons)
	}
	settlements := readinessSettlementEvents(t, eventPath)
	if len(settlements) != 2 ||
		settlements[0].Body["service_state"] != string(api.ServiceStateDegraded) ||
		settlements[1].Body["service_state"] != string(api.ServiceStateRunning) {
		t.Fatalf("settlements=%#v, want Degraded then Running", settlements)
	}
}

func TestObserveSupervisorReadiness_RetryableHostFailureStatusAndScanRecoverSameGeneration(t *testing.T) {
	shortRoot, err := os.MkdirTemp("", "sr-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.RemoveAll(shortRoot); err != nil {
			t.Errorf("remove isolated status state: %v", err)
		}
	})
	stateDir := apitest.HardenedDir(t, shortRoot)
	t.Setenv("LOCALAPPDATA", stateDir)
	t.Setenv("MCPHUB_STATE_DIR_OVERRIDE", stateDir)
	t.Cleanup(api.SetDaemonStateRootForTest(stateDir))
	configPath := filepath.Join(stateDir, "claude.json")
	if err := os.WriteFile(configPath, []byte(`{"mcpServers":{"fetch":{"command":"npx","args":["-y","fixture"]}}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	manifestDir := filepath.Join(stateDir, "servers")
	if err := os.Mkdir(manifestDir, 0o700); err != nil {
		t.Fatal(err)
	}
	task := `\mcp-local-hub-fetch-default`
	const pid, port = 4321, 9133
	d := api.SupervisorDaemon{TaskName: task, Server: "fetch", Daemon: "default", Port: port, StartupBindDeadlineSeconds: 1}
	if err := api.WriteSupervisorIntent(filepath.Join(stateDir, "supervisor-intent.json"), &api.SupervisorIntentFile{
		Version: 1, Daemons: []api.SupervisorDaemon{d},
	}); err != nil {
		t.Fatal(err)
	}
	installOwnsPortProbe(t)
	withFakePortOwnersSnapshot(t, func() (map[int]int, error) { return map[int]int{port: pid}, nil })
	tracker := NewDaemonRuntimeTracker()
	started := time.Date(2026, 9, 11, 15, 30, 0, 0, time.UTC)
	generation := tracker.MarkSpawned(task, pid, started)
	now := started
	probes := 0
	owner := api.SupervisorLockOwner{PID: pid, StartedAt: started.Format(time.RFC3339Nano)}
	if err := api.WriteStateFileAtomic(filepath.Join(stateDir, "supervisor.lock.owner.json"), owner); err != nil {
		t.Fatal(err)
	}
	address := api.SupervisorIPCAddress(stateDir)
	if runtime.GOOS == "windows" && !strings.Contains(address, "mcphub-supervisor-test-") {
		t.Fatalf("refusing non-test supervisor pipe %q", address)
	}
	listener, err := NewSupervisorIPCListener(address, owner)
	if err != nil {
		t.Fatal(err)
	}
	served := make(chan error, 1)
	go func() {
		defer close(served)
		for range 2 {
			conn, err := listener.Accept()
			if err != nil {
				if errors.Is(err, net.ErrClosed) {
					served <- nil // Cleanup closed the isolated listener after an assertion failed.
				} else {
					served <- err
				}
				return
			}
			err = func() error {
				defer conn.Close()
				if err := listener.WriteHello(conn); err != nil {
					return err
				}
				if err := conn.SetDeadline(time.Now().Add(3 * time.Second)); err != nil {
					return err
				}
				line, err := bufio.NewReader(conn).ReadBytes('\n')
				if err != nil {
					return err
				}
				var request api.IPCRequest
				if err := json.Unmarshal(line, &request); err != nil {
					return err
				}
				if request.Cmd != "status" {
					return fmt.Errorf("unexpected IPC command %q", request.Cmd)
				}
				producerRows, err := supervisorStatusDaemons(stateDir, tracker, nil)
				if err != nil || len(producerRows) != 1 {
					return fmt.Errorf("supervisor status rows=%#v err=%v", producerRows, err)
				}
				response, err := json.Marshal(api.IPCResponse{
					ID: request.ID, OK: true, Result: map[string]any{"state": "running", "daemons": producerRows},
				})
				if err != nil {
					return err
				}
				_, err = conn.Write(append(response, '\n'))
				return err
			}()
			if err != nil {
				served <- err
				return
			}
		}
		served <- nil
	}()
	t.Cleanup(func() {
		_ = listener.Close()
		select {
		case err := <-served:
			if err != nil {
				t.Errorf("isolated supervisor IPC server: %v", err)
			}
		case <-time.After(3 * time.Second):
			t.Error("isolated supervisor IPC server did not stop")
		}
	})

	checkSurfaces := func(want api.ServiceStateV1) {
		t.Helper()
		rows, err := api.NewAPI().StatusContext(context.Background())
		if err != nil || len(rows) != 1 {
			t.Fatalf("plain status rows=%#v err=%v", rows, err)
		}
		if rows[0].TaskName != task || rows[0].Server != d.Server || rows[0].PID != pid ||
			rows[0].State != "Running" || rows[0].ReadinessObservation == nil ||
			rows[0].ReadinessObservation.CurrentPIDGeneration != uint64(generation) ||
			rows[0].ReadinessObservation.ObservedPIDGeneration != uint64(generation) ||
			rows[0].ServiceState != want {
			t.Fatalf("same-generation plain status=%#v, want %q", rows[0], want)
		}
		result, err := api.NewAPI().ScanFrom(api.ScanOpts{
			ClaudeConfigPath: configPath, ManifestDir: manifestDir, ReadinessRows: rows,
		})
		if err != nil {
			t.Fatalf("scan from status rows: %v", err)
		}
		if len(result.Entries) != 1 || result.Entries[0].Name != d.Server {
			t.Fatalf("scan entries=%#v", result.Entries)
		}
		entry := result.Entries[0]
		if entry.ManifestExists || entry.Status != "unknown" || entry.Readiness == nil ||
			len(entry.Readiness.Daemons) != 1 || entry.Readiness.Daemons[0].ServiceState != want ||
			entry.Readiness.Daemons[0].PIDGeneration != uint64(generation) {
			t.Fatalf("scan readiness/classification=%#v, want state %q with independent unknown manifest", entry, want)
		}
		if want == api.ServiceStateDegraded {
			if rows[0].ReadinessFailure == nil || rows[0].ReadinessFailure.FailureID != readinesswire.FailureReadinessHostUnavailable ||
				entry.Readiness.PrimaryFailure == nil || entry.Readiness.PrimaryFailure.FailureID != readinesswire.FailureReadinessHostUnavailable {
				t.Fatalf("first failure missing from status/scan: status=%#v scan=%#v", rows[0].ReadinessFailure, entry.Readiness)
			}
		} else if rows[0].ReadinessFailure != nil || entry.Readiness.PrimaryFailure != nil || len(entry.Readiness.Failures) != 0 ||
			entry.Readiness.MaterializationState != api.MaterializationReady {
			t.Fatalf("recovered status/scan retained failure: status=%#v scan=%#v", rows[0].ReadinessFailure, entry.Readiness)
		}
	}

	observeSupervisorReadinessWithDeps(context.Background(), tracker, nil, d, generation, pid, started, supervisorReadinessObserverDeps{
		now: func() time.Time { return now },
		wait: func(_ context.Context, delay time.Duration) bool {
			if probes != 1 {
				t.Fatalf("unexpected readiness wait after %d probes", probes)
			}
			checkSurfaces(api.ServiceStateDegraded)
			now = now.Add(delay)
			return true
		},
		inspect: func(api.SupervisorDaemon, DaemonRuntimeEntry, time.Time) (bool, bool) { return true, true },
		probe: func(context.Context, int) api.MCPReadinessResult {
			probes++
			if probes == 1 {
				now = started.Add(time.Second + api.MCPReadinessProbeBudget)
				return api.MCPReadinessResult{
					SchemaVersion: "mcp-readiness-v1", State: api.MCPReadinessUnready,
					Stage:     api.MCPReadinessStageInitialize,
					FailureID: readinesswire.FailureReadinessHostUnavailable, Retryable: true,
				}
			}
			return api.MCPReadinessResult{SchemaVersion: "mcp-readiness-v1", State: api.MCPReadinessReady}
		},
	})
	if probes != 2 {
		t.Fatalf("probe count=%d, want retry and success without respawn", probes)
	}
	checkSurfaces(api.ServiceStateRunning)
}

func TestObserveSupervisorReadiness_InitialProtocolDeadlineAnchorsAfterListenerDeadline(t *testing.T) {
	tracker := NewDaemonRuntimeTracker()
	task := `\mcp-local-hub-fetch-default`
	started := time.Date(2026, 9, 11, 15, 30, 0, 0, time.UTC)
	generation := tracker.MarkSpawned(task, 4321, started)
	now := started
	inspectCalls, probeCalls := 0, 0
	var waits []time.Duration
	observeSupervisorReadinessWithDeps(
		context.Background(), tracker, nil,
		api.SupervisorDaemon{TaskName: task, Server: "fetch", Daemon: "default", Port: 9133, StartupBindDeadlineSeconds: 5},
		generation, 4321, started,
		supervisorReadinessObserverDeps{
			now: func() time.Time { return now },
			wait: func(_ context.Context, delay time.Duration) bool {
				waits = append(waits, delay)
				if inspectCalls == 1 {
					now = started.Add(5 * time.Second)
				} else {
					now = now.Add(delay)
				}
				return true
			},
			inspect: func(api.SupervisorDaemon, DaemonRuntimeEntry, time.Time) (bool, bool) {
				inspectCalls++
				return inspectCalls >= 2, inspectCalls >= 2
			},
			probe: func(context.Context, int) api.MCPReadinessResult {
				probeCalls++
				if probeCalls == 1 {
					return api.MCPReadinessResult{
						SchemaVersion: "mcp-readiness-v1", State: api.MCPReadinessUnready,
						Stage:     api.MCPReadinessStageInitialize,
						FailureID: readinesswire.FailureReadinessHostUnavailable, Retryable: true,
					}
				}
				return api.MCPReadinessResult{SchemaVersion: "mcp-readiness-v1", State: api.MCPReadinessReady}
			},
		},
	)
	if inspectCalls != 2 || probeCalls != 2 || len(waits) != 2 ||
		waits[0] != 100*time.Millisecond || waits[1] != 100*time.Millisecond {
		t.Fatalf("inspect=%d probe=%d waits=%v, want 2/2 and bind+protocol 100ms waits", inspectCalls, probeCalls, waits)
	}
	entry, _ := tracker.Get(task)
	if entry.ReadinessObservation == nil || entry.ReadinessObservation.DeadlineExceeded ||
		!entry.ReadinessObservation.MCPToolsListReady {
		t.Fatalf("observation=%#v", entry.ReadinessObservation)
	}
}

func TestObserveSupervisorReadiness_NonretryableFailuresSettleOnce(t *testing.T) {
	for _, tc := range []struct {
		name      string
		stage     string
		failureID string
		wantStage api.ReadinessStageV1
	}{
		{"protocol", api.MCPReadinessStageInitialize, readinesswire.FailureBackingProtocolUnsupported, api.ReadinessStageMCPInitialize},
		{"invalid response", api.MCPReadinessStageToolsList, readinesswire.FailureChildResponseInvalid, api.ReadinessStageMCPToolsList},
		{"session cleanup", api.MCPReadinessStageSessionCleanup, api.MCPReadinessFailureSessionCleanup, api.ReadinessStageMCPInitialize},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tracker := NewDaemonRuntimeTracker()
			task := `\mcp-local-hub-test-default`
			started := time.Date(2026, 9, 11, 15, 30, 0, 0, time.UTC)
			generation := tracker.MarkSpawned(task, 4321, started)
			probes := 0
			observeSupervisorReadinessWithDeps(
				context.Background(), tracker, nil,
				api.SupervisorDaemon{TaskName: task, Server: "test", Daemon: "default", Port: 9133},
				generation, 4321, started,
				supervisorReadinessObserverDeps{
					now: func() time.Time { return started },
					wait: func(context.Context, time.Duration) bool {
						t.Fatal("terminal failure retried")
						return false
					},
					inspect: func(api.SupervisorDaemon, DaemonRuntimeEntry, time.Time) (bool, bool) {
						return true, true
					},
					probe: func(context.Context, int) api.MCPReadinessResult {
						probes++
						return api.MCPReadinessResult{
							SchemaVersion: "mcp-readiness-v1", State: api.MCPReadinessUnready,
							Stage: tc.stage, FailureID: tc.failureID,
						}
					},
				},
			)
			entry, _ := tracker.Get(task)
			if probes != 1 || entry.ReadinessObservation == nil || len(entry.ReadinessObservation.Failures) != 1 {
				t.Fatalf("probes=%d entry=%#v", probes, entry)
			}
			failure := entry.ReadinessObservation.Failures[0]
			if failure.FailureID != tc.failureID || failure.Stage != tc.wantStage || failure.Retryable {
				t.Fatalf("failure=%#v", failure)
			}
			got := api.ReduceReadinessV1(api.ReadinessRequest{Observations: []api.DaemonReadinessObservationV1{*entry.ReadinessObservation}})
			if len(got.Daemons) != 1 || got.Daemons[0].ServiceState != api.ServiceStateDegraded || !got.Daemons[0].Settled {
				t.Fatalf("readiness=%#v", got.Daemons)
			}
		})
	}
}

func TestObserveSupervisorReadiness_CancellationDropsBlockedProbe(t *testing.T) {
	tracker := NewDaemonRuntimeTracker()
	task := `\mcp-local-hub-fetch-default`
	started := time.Now().UTC()
	generation := tracker.MarkSpawned(task, 4321, started)
	ctx, cancel := context.WithCancel(context.Background())
	entered := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		observeSupervisorReadinessWithDeps(
			ctx, tracker, nil,
			api.SupervisorDaemon{TaskName: task, Server: "fetch", Daemon: "default", Port: 9133},
			generation, 4321, started,
			supervisorReadinessObserverDeps{
				now:  func() time.Time { return started },
				wait: func(context.Context, time.Duration) bool { return true },
				inspect: func(api.SupervisorDaemon, DaemonRuntimeEntry, time.Time) (bool, bool) {
					return true, true
				},
				probe: func(ctx context.Context, _ int) api.MCPReadinessResult {
					close(entered)
					<-ctx.Done()
					return api.MCPReadinessResult{
						SchemaVersion: "mcp-readiness-v1", State: api.MCPReadinessUnready,
						FailureID: readinesswire.FailureReadinessHostUnavailable, Retryable: true,
					}
				},
			},
		)
	}()
	<-entered
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("observer did not return after cancellation")
	}
	entry, _ := tracker.Get(task)
	if entry.ReadinessObservation != nil {
		t.Fatalf("cancelled observer wrote readiness=%#v", entry.ReadinessObservation)
	}
}

func TestObserveSupervisorReadiness_GenerationReplacementDropsBlockedProbe(t *testing.T) {
	tracker := NewDaemonRuntimeTracker()
	task := `\mcp-local-hub-fetch-default`
	started := time.Now().UTC()
	generation := tracker.MarkSpawned(task, 4321, started)
	entered := make(chan struct{})
	release := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		observeSupervisorReadinessWithDeps(
			context.Background(), tracker, nil,
			api.SupervisorDaemon{TaskName: task, Server: "fetch", Daemon: "default", Port: 9133},
			generation, 4321, started,
			supervisorReadinessObserverDeps{
				now:  func() time.Time { return started },
				wait: func(context.Context, time.Duration) bool { return true },
				inspect: func(api.SupervisorDaemon, DaemonRuntimeEntry, time.Time) (bool, bool) {
					return true, true
				},
				probe: func(context.Context, int) api.MCPReadinessResult {
					close(entered)
					<-release
					return api.MCPReadinessResult{
						SchemaVersion: "mcp-readiness-v1", State: api.MCPReadinessReady,
						Stage: api.MCPReadinessStageToolsList,
					}
				},
			},
		)
	}()
	<-entered
	replacementGeneration := tracker.MarkSpawned(task, 9876, started.Add(time.Second))
	close(release)
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("stale observer did not return")
	}
	entry, _ := tracker.Get(task)
	if entry.PIDGeneration != replacementGeneration || entry.CurrentPID != 9876 || entry.ReadinessObservation != nil {
		t.Fatalf("replacement entry changed by stale probe: %#v", entry)
	}
}

func TestObserveSupervisorReadiness_ListenerTimeoutNeverProbesMCP(t *testing.T) {
	tracker := NewDaemonRuntimeTracker()
	task := `\mcp-local-hub-fetch-default`
	started := time.Date(2026, 9, 11, 15, 30, 0, 0, time.UTC)
	generation := tracker.MarkSpawned(task, 4321, started)
	now := started
	observeSupervisorReadinessWithDeps(
		context.Background(), tracker, nil,
		api.SupervisorDaemon{TaskName: task, Server: "fetch", Daemon: "default", Port: 9133, StartupBindDeadlineSeconds: 1},
		generation, 4321, started,
		supervisorReadinessObserverDeps{
			now: func() time.Time { return now },
			wait: func(_ context.Context, delay time.Duration) bool {
				now = now.Add(delay)
				return true
			},
			inspect: func(api.SupervisorDaemon, DaemonRuntimeEntry, time.Time) (bool, bool) {
				return true, false
			},
			probe: func(context.Context, int) api.MCPReadinessResult {
				t.Fatal("listener timeout invoked MCP probe")
				return api.MCPReadinessResult{}
			},
		},
	)
	entry, _ := tracker.Get(task)
	if entry.ReadinessObservation == nil || !entry.ReadinessObservation.DeadlineExceeded ||
		entry.ReadinessObservation.ListenerReady {
		t.Fatalf("listener-timeout observation=%#v", entry.ReadinessObservation)
	}
	got := api.ReduceReadinessV1(api.ReadinessRequest{Observations: []api.DaemonReadinessObservationV1{*entry.ReadinessObservation}})
	if len(got.Daemons) != 1 || got.Daemons[0].Stage != api.ReadinessStageUpstreamListener ||
		got.Daemons[0].ServiceState != api.ServiceStateDegraded || !got.Daemons[0].Settled {
		t.Fatalf("readiness=%#v", got.Daemons)
	}
}

func TestObserveSupervisorReadiness_CancellationDuringRetryWaitPreservesLastObservation(t *testing.T) {
	for _, tc := range []struct {
		name       string
		atDeadline bool
		wantWait   time.Duration
		wantState  api.ServiceStateV1
		settled    bool
	}{
		{name: "initial 100ms", wantWait: 100 * time.Millisecond, wantState: api.ServiceStateStarting},
		{name: "post-budget 5s", atDeadline: true, wantWait: supervisorLivenessInterval, wantState: api.ServiceStateDegraded, settled: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tracker := NewDaemonRuntimeTracker()
			task := `\mcp-local-hub-fetch-default`
			started := time.Date(2026, 9, 11, 15, 30, 0, 0, time.UTC)
			generation := tracker.MarkSpawned(task, 4321, started)
			now := started
			ctx, cancel := context.WithCancel(context.Background())
			waitEntered := make(chan time.Duration, 1)
			done := make(chan struct{})
			go func() {
				defer close(done)
				observeSupervisorReadinessWithDeps(
					ctx, tracker, nil,
					api.SupervisorDaemon{TaskName: task, Server: "fetch", Daemon: "default", Port: 9133, StartupBindDeadlineSeconds: 1},
					generation, 4321, started,
					supervisorReadinessObserverDeps{
						now: func() time.Time { return now },
						wait: func(ctx context.Context, delay time.Duration) bool {
							waitEntered <- delay
							<-ctx.Done()
							return false
						},
						inspect: func(api.SupervisorDaemon, DaemonRuntimeEntry, time.Time) (bool, bool) {
							return true, true
						},
						probe: func(context.Context, int) api.MCPReadinessResult {
							if tc.atDeadline {
								now = started.Add(time.Second + api.MCPReadinessProbeBudget)
							}
							return api.MCPReadinessResult{
								SchemaVersion: "mcp-readiness-v1", State: api.MCPReadinessUnready,
								Stage:     api.MCPReadinessStageInitialize,
								FailureID: readinesswire.FailureReadinessHostUnavailable, Retryable: true,
							}
						},
					},
				)
			}()
			if delay := <-waitEntered; delay != tc.wantWait {
				t.Fatalf("wait=%s want=%s", delay, tc.wantWait)
			}
			before, _ := tracker.Get(task)
			if before.ReadinessObservation == nil {
				t.Fatal("retry wait began before visible readiness observation")
			}
			frozen := *before.ReadinessObservation
			cancel()
			select {
			case <-done:
			case <-time.After(time.Second):
				t.Fatal("observer did not return after wait cancellation")
			}
			after, _ := tracker.Get(task)
			if after.ReadinessObservation == nil || !reflect.DeepEqual(*after.ReadinessObservation, frozen) {
				t.Fatalf("cancellation changed observation: before=%#v after=%#v", frozen, after.ReadinessObservation)
			}
			got := api.ReduceReadinessV1(api.ReadinessRequest{Observations: []api.DaemonReadinessObservationV1{*after.ReadinessObservation}})
			if len(got.Daemons) != 1 || got.Daemons[0].ServiceState != tc.wantState || got.Daemons[0].Settled != tc.settled {
				t.Fatalf("readiness=%#v want state=%s settled=%v", got.Daemons, tc.wantState, tc.settled)
			}
		})
	}
}

func TestObserveSupervisorReadiness_GenerationReplacementDuringRetryWaitWritesNothing(t *testing.T) {
	tracker := NewDaemonRuntimeTracker()
	task := `\mcp-local-hub-fetch-default`
	started := time.Now().UTC()
	generation := tracker.MarkSpawned(task, 4321, started)
	waitEntered := make(chan struct{})
	releaseWait := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		observeSupervisorReadinessWithDeps(
			context.Background(), tracker, nil,
			api.SupervisorDaemon{TaskName: task, Server: "fetch", Daemon: "default", Port: 9133},
			generation, 4321, started,
			supervisorReadinessObserverDeps{
				now: func() time.Time { return started },
				wait: func(context.Context, time.Duration) bool {
					close(waitEntered)
					<-releaseWait
					return true
				},
				inspect: func(api.SupervisorDaemon, DaemonRuntimeEntry, time.Time) (bool, bool) {
					return true, true
				},
				probe: func(context.Context, int) api.MCPReadinessResult {
					return api.MCPReadinessResult{
						SchemaVersion: "mcp-readiness-v1", State: api.MCPReadinessUnready,
						Stage:     api.MCPReadinessStageInitialize,
						FailureID: readinesswire.FailureReadinessHostUnavailable, Retryable: true,
					}
				},
			},
		)
	}()
	<-waitEntered
	replacementGeneration := tracker.MarkSpawned(task, 9876, started.Add(time.Second))
	close(releaseWait)
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("observer did not return after generation replacement")
	}
	entry, _ := tracker.Get(task)
	if entry.PIDGeneration != replacementGeneration || entry.CurrentPID != 9876 || entry.ReadinessObservation != nil {
		t.Fatalf("replacement entry changed by old retry wait: %#v", entry)
	}
}

func TestObserveSupervisorReadiness_SessionCleanupFailureIsTerminalWithoutSecondSession(t *testing.T) {
	var initializeCalls atomic.Int32
	var toolsCalls atomic.Int32
	var deleteCalls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodDelete {
			deleteCalls.Add(1)
			http.Error(w, "cleanup unavailable", http.StatusInternalServerError)
			return
		}
		var request struct {
			Method string `json:"method"`
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			http.Error(w, "invalid request", http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		switch request.Method {
		case "initialize":
			initializeCalls.Add(1)
			w.Header().Set("Mcp-Session-Id", "cleanup-test-session")
			_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":{"protocolVersion":"2025-06-18","capabilities":{},"serverInfo":{"name":"cleanup-test","version":"1"}}}`))
		case "tools/list":
			toolsCalls.Add(1)
			_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":2,"result":{"tools":[]}}`))
		default:
			http.Error(w, "unsupported", http.StatusBadRequest)
		}
	}))
	defer server.Close()
	_, portText, err := net.SplitHostPort(strings.TrimPrefix(server.URL, "http://"))
	if err != nil {
		t.Fatal(err)
	}
	var port int
	if _, err := fmt.Sscan(portText, &port); err != nil {
		t.Fatal(err)
	}
	tracker := NewDaemonRuntimeTracker()
	task := `\mcp-local-hub-fetch-default`
	started := time.Now().UTC()
	generation := tracker.MarkSpawned(task, 4321, started)
	observeSupervisorReadinessWithDeps(
		context.Background(), tracker, nil,
		api.SupervisorDaemon{TaskName: task, Server: "fetch", Daemon: "default", Port: port, StartupBindDeadlineSeconds: 1},
		generation, 4321, started,
		supervisorReadinessObserverDeps{
			now: func() time.Time { return time.Now().UTC() },
			wait: func(context.Context, time.Duration) bool {
				t.Fatal("session cleanup failure retried")
				return false
			},
			inspect: func(api.SupervisorDaemon, DaemonRuntimeEntry, time.Time) (bool, bool) {
				return true, true
			},
			probe: api.ProbeMCPReadinessContext,
		},
	)
	if initializeCalls.Load() != 1 || toolsCalls.Load() != 1 || deleteCalls.Load() != 1 {
		t.Fatalf("requests initialize=%d tools=%d delete=%d, want 1/1/1", initializeCalls.Load(), toolsCalls.Load(), deleteCalls.Load())
	}
	entry, _ := tracker.Get(task)
	if entry.ReadinessObservation == nil || len(entry.ReadinessObservation.Failures) != 1 {
		t.Fatalf("entry=%#v", entry)
	}
	failure := entry.ReadinessObservation.Failures[0]
	if failure.FailureID != api.MCPReadinessFailureSessionCleanup || failure.Retryable {
		t.Fatalf("cleanup failure=%#v", failure)
	}
}

func TestWaitSupervisorReadiness_CancellationStopsTimer(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	start := time.Now()
	if waitSupervisorReadiness(ctx, time.Minute) {
		t.Fatal("cancelled readiness wait reported completion")
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("cancelled readiness wait took %s", elapsed)
	}
}

func TestObserveSupervisorReadiness_LoopbackColdServerConvergesStatusSameGeneration(t *testing.T) {
	var initializeCalls atomic.Int32
	var toolsCalls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request struct {
			Method string `json:"method"`
			ID     int    `json:"id"`
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			http.Error(w, "invalid request", http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		switch request.Method {
		case "initialize":
			if initializeCalls.Add(1) == 1 {
				<-r.Context().Done()
				return
			}
			_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":{"protocolVersion":"2025-06-18","capabilities":{},"serverInfo":{"name":"cold-test","version":"1"}}}`))
		case "tools/list":
			toolsCalls.Add(1)
			_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":2,"result":{"tools":[]}}`))
		default:
			http.Error(w, "unsupported", http.StatusBadRequest)
		}
	}))
	defer server.Close()
	_, portText, err := net.SplitHostPort(strings.TrimPrefix(server.URL, "http://"))
	if err != nil {
		t.Fatal(err)
	}
	var port int
	if _, err := fmt.Sscan(portText, &port); err != nil {
		t.Fatal(err)
	}

	tracker := NewDaemonRuntimeTracker()
	task := `\mcp-local-hub-fetch-default`
	started := time.Now().UTC()
	generation := tracker.MarkSpawned(task, 4321, started)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	observeSupervisorReadinessWithDeps(
		ctx, tracker, nil,
		api.SupervisorDaemon{TaskName: task, Server: "fetch", Daemon: "default", Port: port, StartupBindDeadlineSeconds: 1},
		generation, 4321, started,
		supervisorReadinessObserverDeps{
			now: func() time.Time { return time.Now().UTC() },
			wait: func(ctx context.Context, _ time.Duration) bool {
				return ctx.Err() == nil
			},
			inspect: func(api.SupervisorDaemon, DaemonRuntimeEntry, time.Time) (bool, bool) {
				return true, true
			},
			probe: api.ProbeMCPReadinessContext,
		},
	)
	if initializeCalls.Load() != 2 || toolsCalls.Load() != 1 {
		t.Fatalf("requests initialize=%d tools=%d, want 2/1", initializeCalls.Load(), toolsCalls.Load())
	}
	stateDir := apitest.HardenedTempDir(t)
	if err := api.WriteSupervisorIntent(filepath.Join(stateDir, "supervisor-intent.json"), &api.SupervisorIntentFile{
		Version: 1,
		Daemons: []api.SupervisorDaemon{{
			TaskName: task, Server: "fetch", Daemon: "default", Port: port,
		}},
	}); err != nil {
		t.Fatal(err)
	}
	rows, err := supervisorStatusDaemons(stateDir, tracker, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 {
		t.Fatalf("status rows=%#v", rows)
	}
	wire, ok := rows[0]["readiness_observation"].(*api.ReadinessObservationWireV1)
	if !ok || wire.ObservedPIDGeneration != uint64(generation) ||
		!wire.ListenerReady || !wire.MCPInitializeReady || !wire.MCPToolsListReady ||
		len(wire.Failures) != 0 {
		t.Fatalf("status readiness=%#v", rows[0]["readiness_observation"])
	}
}

func readinessSettlementEvents(t *testing.T, path string) []api.SupervisorEvent {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var events []api.SupervisorEvent
	for _, line := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
		var event api.SupervisorEvent
		if err := json.Unmarshal([]byte(line), &event); err != nil {
			t.Fatal(err)
		}
		if event.Event == "daemon-readiness-settled-v1" {
			events = append(events, event)
		}
	}
	return events
}

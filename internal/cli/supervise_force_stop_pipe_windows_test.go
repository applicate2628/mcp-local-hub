//go:build windows

package cli

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"mcp-local-hub/internal/api"
	"mcp-local-hub/internal/api/apitest"
)

// Real Windows pipe, native peer validation, production IPC dispatcher/FIFO,
// original creator/Job and original waiter. Only unrelated port enumeration and
// the timing of reap's confirming scan are controlled; no native receipt is
// modeled. This is a transport/lifecycle fixture, not installed-Product evidence.
func TestForceStopAuthenticatedWindowsPipeOriginalCompletion(t *testing.T) {
	for _, mode := range []string{"explicit", "removed-client-first", "removed-reconcile-first"} {
		t.Run(mode, func(t *testing.T) {
			dir := apitest.HardenedTempDir(t)
			t.Cleanup(api.SetDaemonStateRootForTest(dir))
			t.Setenv("MCPHUB_STATE_DIR_OVERRIDE", dir)
			address := api.SupervisorIPCAddress(dir)
			if !strings.Contains(address, "-test-") {
				t.Fatal("isolated named pipe prerequisite missing")
			}
			lock, err := api.AcquireSupervisorLock(filepath.Join(dir, "supervisor.lock"))
			if err != nil {
				t.Fatal(err)
			}
			defer lock.Release()
			events, err := api.OpenSupervisorEventLog(filepath.Join(dir, "events.log"))
			if err != nil {
				t.Fatal(err)
			}
			defer events.Close()
			ctx, cancel := context.WithCancel(context.Background())
			workers := &fixtureSpawnWorkers{}
			held, release := make(chan struct{}), make(chan struct{})
			var releaseOnce sync.Once
			var publications atomic.Int32
			unblock := func() { releaseOnce.Do(func() { close(release) }) }
			workers.beforeEmit = func() {
				if publications.Add(1) == 1 {
					close(held)
				}
				<-release
			}
			defer func() { unblock(); settleFixtureSpawnWorkers(t, workers, cancel) }()
			records, tracker := &daemonCreatorRecords{}, NewDaemonRuntimeTracker()
			crashCh := make(chan crashEvent, 2)
			image, err := os.Executable()
			if err != nil {
				t.Fatal(err)
			}
			descriptor := api.SupervisorDaemon{TaskName: `\mcp-local-hub-force-fixture-default`, Server: "force-fixture", Daemon: "default", Command: image,
				Args: []string{"-test.run=^TestProductionTerminateFn_HelperSleep$"}, Env: map[string]string{"MCPHUB_PRODUCTION_TERMINATE_HELPER": "1"}}
			intent := &api.SupervisorIntentFile{Version: 1, Daemons: []api.SupervisorDaemon{descriptor}}
			intentPath := filepath.Join(dir, "supervisor-intent.json")
			if err := api.WriteSupervisorIntent(intentPath, intent); err != nil {
				t.Fatal(err)
			}
			spawn := makeProductionSpawnFnWithCreatorRecords(events, tracker, "", nil, "", crashCh, ctx, nil, false, workers, records)
			if err := spawn(descriptor); err != nil {
				t.Fatal(err)
			}
			entry, found := tracker.Get(descriptor.TaskName)
			if !found || entry.CurrentPID <= 0 {
				t.Fatal("original child missing")
			}
			target := api.ForceStopTargetV1{TaskName: descriptor.TaskName, PID: entry.CurrentPID, StartedAt: entry.StartedAt.UTC().Format(time.RFC3339Nano), PIDGeneration: entry.PIDGeneration}
			loop := api.NewEventLoop(16)
			controller := &supervisorController{ctx: ctx, events: events, eventLoop: loop, tracker: tracker, intentCache: newIntentCache(), daemonIntent: newDaemonIntentCache(), daemonCreators: records,
				reapIntentReader: func() (*api.SupervisorIntentFile, error) { return api.ReadSupervisorIntent(intentPath) },
				// Confirming scans below are causal barriers, not wall-clock races.
				reapAfterFunc: func(time.Duration, func()) reapTimer { return noopReapTimer{} }}
			controller.intentCache.Refresh(intent)
			controller.smStates.Store(descriptor.TaskName, api.StRunning)
			controller.ownSpawned.Store(descriptor.TaskName, true)
			controller.reaperOutstanding.Store(descriptor.TaskName, true)
			var legacyActions atomic.Int32
			controller.terminateOutcome = guardForceStopTermination(records, func(api.SupervisorDaemon, terminationExpectedTuple) terminationOutcome {
				legacyActions.Add(1)
				return terminationOutcome{Kind: terminationOutcomeFailed, Cause: errors.New("legacy PID action forbidden in retained fixture")}
			})
			loop.RegisterHandler(controller.handleLoopEvent)
			loopDone := make(chan struct{})
			go func() { defer close(loopDone); loop.Run(ctx) }()
			defer func() { cancel(); <-loopDone }()
			var ready, loaded atomic.Bool
			ready.Store(true)
			loaded.Store(true)
			coalescer := newStatusPortOwnersCoalescer(tracker)
			coalescer.snapshotFn = func(context.Context) (map[int]int, error) { return map[int]int{}, nil }
			deps := ipcDispatchDeps{stateDir: dir, events: events, supervisorLock: lock, runtimeTracker: tracker, statusCoalescer: coalescer,
				reconcileReady: &ready, intentFilesLoaded: &loaded, controllerProvider: func() *supervisorController { return controller }}
			listener, err := NewSupervisorIPCListener(address, lock.Owner())
			if err != nil {
				t.Fatal(err)
			}
			session := newIPCSession(listener, nil)
			session.acceptDone = make(chan struct{})
			var accepted atomic.Int32
			go func() {
				defer close(session.acceptDone)
				for {
					conn, err := listener.Accept()
					if err != nil {
						return
					}
					if !session.register(conn) {
						_ = conn.Close()
						return
					}
					accepted.Add(1)
					go func() { defer session.finish(conn); serveIPCConn(conn, listener, deps, ipcAcceptTiming{}) }()
				}
			}()
			defer session.stop()

			callCtx, stopCall := context.WithTimeout(ctx, 20*time.Second)
			defer stopCall()
			// A real but wrong owner generation must fail before the native action.
			owner := lock.Owner()
			badOwner := owner
			badOwner.StartedAt = time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC).Format(time.RFC3339Nano)
			if err := api.WriteStateFileAtomic(filepath.Join(dir, "supervisor.lock.owner.json"), badOwner); err != nil {
				t.Fatal(err)
			}
			_, badErr := api.DialSupervisorIPCForceStop(callCtx, descriptor.TaskName, 0)
			if err := api.WriteStateFileAtomic(filepath.Join(dir, "supervisor.lock.owner.json"), owner); err != nil {
				t.Fatal(err)
			}
			if !errors.Is(badErr, api.ErrSupervisorControlPeerIdentity) || accepted.Load() != 0 {
				t.Fatalf("wrong native owner generation reached the pipe: accepted=%d err=%v", accepted.Load(), badErr)
			}

			var result api.ForceStopResultV1
			if mode == "explicit" {
				result, err = api.DialSupervisorIPCForceStop(callCtx, descriptor.TaskName, 0)
			} else {
				capture, prepareErr := api.PrepareSupervisorIPCRemovedForceStop(callCtx, descriptor.TaskName, 0)
				if prepareErr != nil {
					t.Fatal(prepareErr)
				}
				defer capture.Close()
				records.mu.Lock()
				operation := records.operations[0]
				preparedWithoutAction := operation.removalRetainers == 1 && !operation.forceStopRequested && operation.terminalDone == nil
				records.mu.Unlock()
				if !preparedWithoutAction {
					t.Fatal("preparation performed native termination")
				}
				removed := &api.SupervisorIntentFile{Version: 1}
				if err := api.WriteSupervisorIntent(intentPath, removed); err != nil {
					t.Fatal(err)
				}
				controller.refreshSupervisorIntentSync(removed, removed.StopsAsDaemonIntentFile())
				if mode == "removed-reconcile-first" {
					controller.refreshSupervisorIntentSync(removed, removed.StopsAsDaemonIntentFile())
					records.mu.Lock()
					closedByReconcile := operation.terminal != nil && operation.terminal.Closure() != nil && operation.terminalErr == nil
					records.mu.Unlock()
					if !closedByReconcile {
						t.Fatal("reconcile did not use original creator completion")
					}
				}
				result, err = capture.Complete(callCtx)
				if _, secondErr := capture.Complete(callCtx); secondErr == nil {
					t.Fatal("consumed capture allowed a second exchange")
				}
			}
			if err != nil || result.Target != target || result.State != api.ForceStopClosed || !result.RootExited || !result.TreeClosed || !result.HandlesClosed || result.Action != "request-succeeded" {
				t.Fatalf("real pipe did not return original native closure: %+v err=%v", result, err)
			}
			if accepted.Load() != 1 || legacyActions.Load() != 0 {
				t.Fatalf("redial or PID fallback: connections=%d legacy=%d", accepted.Load(), legacyActions.Load())
			}
			select {
			case <-held:
			case <-time.After(5 * time.Second):
				t.Fatal("original waiter missed closure barrier")
			}
			select {
			case event := <-crashCh:
				t.Fatalf("exit published before original waiter release: %+v", event)
			default:
			}
			unblock()
			select {
			case event := <-crashCh:
				if event.PID != target.PID || event.PIDGeneration != target.PIDGeneration || (event.ExitCode == 0 && event.WaitErr == nil) {
					t.Fatalf("wrong original exit: %+v", event)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("original child exit lost")
			}
			session.stop()
			settleFixtureSpawnWorkers(t, workers, cancel)
			if publications.Load() != 1 {
				t.Fatalf("exit publications=%d", publications.Load())
			}
			records.mu.Lock()
			retainers := records.operations[0].removalRetainers
			records.mu.Unlock()
			if retainers != 0 {
				t.Fatalf("connection closure leaked %d removal references", retainers)
			}
			select {
			case event := <-crashCh:
				t.Fatalf("duplicate original exit: %+v", event)
			default:
			}
		})
	}
}

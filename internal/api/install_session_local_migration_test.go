package api

import (
	"context"
	"errors"
	"path/filepath"
	"reflect"
	"testing"
	"time"
)

// Real intent files and their existing mutation/receipt owners; the native
// process receipt is a fixture here. The separate retained named-pipe test is
// required before treating this composition as native migration evidence.
func TestSessionLocalMigrationReceivesOriginalBeforePrivateAdmission(t *testing.T) {
	for _, mode := range []string{"complete", "prepare-failed", "descriptor-changed", "reconcile-failed", "lost-complete", "root-only", "concurrent-clear"} {
		t.Run(mode, func(t *testing.T) {
			dir, task := seedForceStopDescriptor(t, "private-fixture", 9124)
			path := filepath.Join(dir, supervisorIntentFileLeaf)
			intent, err := ReadSupervisorIntent(path)
			if err != nil {
				t.Fatal(err)
			}
			original := intent.Daemons[0]
			route := BuildBuiltinRouteDaemon("fixture", 9192)
			foreignTask := `\mcp-local-hub-foreign-default`
			foreignStop := DaemonIntent{Desired: IntentDesiredStopped, Reason: IntentReasonUserDisabled, UpdatedAt: time.Now().UTC()}
			intent.Daemons = append(intent.Daemons, route)
			intent.Stops = map[string]DaemonIntent{foreignTask: foreignStop}
			if err := WriteSupervisorIntent(path, intent); err != nil {
				t.Fatal(err)
			}
			target := &sessionLocalInstallTarget{Route: route, Legacy: []SupervisorDaemon{original}, Providers: []HubStdioEndpoint{{Server: "private-fixture", Daemon: "default", Port: 9124, Configuration: "fixture"}}}
			completed, nudged := 0, 0
			stop := &removedStopFixture{complete: func(context.Context) (ForceStopResultV1, error) {
				completed++
				assertSupervisorIntentFlockAvailableDuringIPCStatus(t, dir)
				current, err := ReadSupervisorIntent(path)
				if err != nil || current.FindSupervisorDaemonByTaskName(task) != nil || current.Stops[task].Reason != IntentReasonUninstalled || nudged != 1 {
					t.Fatal("original Complete preceded removal, release or reconcile")
				}
				if target.legacyRetired {
					t.Fatal("private admission preceded original receipt")
				}
				if mode == "concurrent-clear" {
					if err := NewAPI().ClearStopIntent(task, "fixture"); err != nil {
						t.Fatal(err)
					}
				}
				result := forceStopClosedFixture(task, 9124)
				if mode == "lost-complete" {
					return result, errors.New("original completion response lost")
				}
				if mode == "root-only" {
					result.TreeClosed = false
				}
				return result, nil
			}}
			installRemovedPrepareFixture(t, func(context.Context, string, int) (supervisorRemovedStop, error) {
				assertSupervisorIntentFlockAvailableDuringIPCStatus(t, dir)
				current, err := ReadSupervisorIntent(path)
				if err != nil || current.FindSupervisorDaemonByTaskName(task) == nil || current.Stops[task].Reason == IntentReasonUninstalled {
					t.Fatal("capture followed first durable mutation")
				}
				if mode == "prepare-failed" {
					return nil, errors.New("original preparation refused")
				}
				return stop, nil
			})
			captures := captureRemovedSupervisorTargets(context.Background(), target.Legacy)
			defer captures.close()
			if mode == "descriptor-changed" {
				if err := MutateSupervisorIntentIfChanged(path, func(f *SupervisorIntentFile) (bool, error) {
					for i := range f.Daemons {
						if f.Daemons[i].TaskName == task {
							f.Daemons[i].Command = "successor"
							return true, nil
						}
					}
					return false, errors.New("original fixture descriptor disappeared")
				}); err != nil {
					t.Fatal(err)
				}
				changed, err := ReadSupervisorIntent(path)
				if err != nil {
					t.Fatal(err)
				}
				row := changed.FindSupervisorDaemonByTaskName(task)
				if row == nil || row.Command != "successor" {
					t.Fatal("negative control did not persist its changed descriptor")
				}
			}
			t.Cleanup(setSupervisorReconcileApplyHookForTest(func(context.Context, bool) (ReconcileResponse, error) {
				nudged++
				assertSupervisorIntentFlockAvailableDuringIPCStatus(t, dir)
				if mode == "reconcile-failed" {
					return ReconcileResponse{}, errors.New("reconcile refusal")
				}
				return ReconcileResponse{}, nil
			}))
			err = retireCapturedSessionLocalLegacy(context.Background(), path, target, captures)
			current, readErr := ReadSupervisorIntent(path)
			if readErr != nil {
				t.Fatal(readErr)
			}
			if got := current.FindSupervisorDaemonByTaskName(BuiltinRouteTaskName); got == nil || !reflect.DeepEqual(*got, route) || current.Stops[foreignTask] != foreignStop {
				t.Fatal("migration changed foreign backend or stop")
			}
			if mode == "complete" {
				if err != nil || !target.legacyRetired || !target.legacyRemovalCommitted || completed != 1 {
					t.Fatalf("original chain not received: %v", err)
				}
				if _, err := sessionLocalLegacySelection(current, target.Providers); err != nil {
					t.Fatal(err)
				}
				return
			}
			if err == nil || target.legacyRetired {
				t.Fatal("incomplete original closure admitted private provider")
			}
			if mode == "prepare-failed" || mode == "descriptor-changed" {
				if current.FindSupervisorDaemonByTaskName(task) == nil || target.legacyRemovalCommitted || completed != 0 || nudged != 0 {
					t.Fatal("precommit refusal changed shared definition")
				}
				return
			}
			if current.FindSupervisorDaemonByTaskName(task) != nil || !target.legacyRemovalCommitted {
				t.Fatal("post-removal uncertainty was lost")
			}
			// Serialize/read/normalize as a fresh invocation would. Neither a
			// missing row nor ClearStop moving its record to watermarks is proof.
			normalizeAbsentOnlyStopWatermarks(current)
			if _, err := sessionLocalLegacySelection(current, target.Providers); err == nil {
				t.Fatal("lost completion was promoted to success on retry")
			}
		})
	}
}

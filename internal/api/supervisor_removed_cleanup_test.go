package api

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"mcp-local-hub/internal/scheduler"
)

// Mapping/ordering model only. Native closure and authenticated transport are
// exercised separately by the CLI Windows named-pipe fixture.
type removedStopFixture struct {
	complete func(context.Context) (ForceStopResultV1, error)
	closed   int
}

func (stop *removedStopFixture) Complete(ctx context.Context) (ForceStopResultV1, error) {
	return stop.complete(ctx)
}
func (stop *removedStopFixture) Close() error { stop.closed++; return nil }

func installRemovedPrepareFixture(t *testing.T, prepare func(context.Context, string, int) (supervisorRemovedStop, error)) {
	t.Helper()
	previous := prepareSupervisorRemovedStopFn
	prepareSupervisorRemovedStopFn = prepare
	t.Cleanup(func() { prepareSupervisorRemovedStopFn = previous })
	sealForceStopNumericalFallbacks(t)
}

func TestRemovedSupervisorCaptureCannotTransferToReplacementOrLateRow(t *testing.T) {
	descriptor := SupervisorDaemon{TaskName: `\mcp-local-hub-demo-old`, Server: "demo", Command: "original"}
	completed := 0
	stop := &removedStopFixture{complete: func(context.Context) (ForceStopResultV1, error) {
		completed++
		return forceStopClosedFixture(descriptor.TaskName, 0), nil
	}}
	installRemovedPrepareFixture(t, func(context.Context, string, int) (supervisorRemovedStop, error) { return stop, nil })
	captures := captureRemovedSupervisorTargets(context.Background(), []SupervisorDaemon{descriptor})
	defer captures.close()
	replacement := descriptor
	replacement.Command = "successor"
	late := descriptor
	late.TaskName += "-late"
	for _, target := range []SupervisorDaemon{replacement, late} {
		if err := captures.complete(context.Background(), target); !errors.Is(err, ErrOriginalProcessOwnerRequired) {
			t.Fatalf("uncaptured descriptor inherited authority: %v", err)
		}
	}
	if completed != 0 {
		t.Fatal("uncaptured target reached original stop owner")
	}
	if err := captures.complete(context.Background(), descriptor); err != nil || completed != 1 {
		t.Fatalf("original capture lost: count=%d err=%v", completed, err)
	}
}

func TestRemovedSupervisorCaptureRefusalAndIncompleteStayWarnings(t *testing.T) {
	for _, mode := range []string{"capture-failed", "root-only", "lost-response"} {
		t.Run(mode, func(t *testing.T) {
			dir, task := seedForceStopDescriptor(t, "time", 0)
			cause := errors.New("original connection failed")
			stop := &removedStopFixture{complete: func(context.Context) (ForceStopResultV1, error) {
				result := forceStopClosedFixture(task, 0)
				if mode == "lost-response" {
					return result, cause
				}
				result.TreeClosed = false
				return result, nil
			}}
			prepared := 0
			installRemovedPrepareFixture(t, func(context.Context, string, int) (supervisorRemovedStop, error) {
				prepared++
				assertSupervisorIntentFlockAvailableDuringIPCStatus(t, dir)
				if mode == "capture-failed" {
					return nil, cause
				}
				return stop, nil
			})
			t.Cleanup(setSupervisorReconcileApplyHookForTest(func(context.Context, bool) (ReconcileResponse, error) { return ReconcileResponse{}, nil }))
			report := &UninstallReport{}
			NewAPI().removeServerFromSupervisorIntentBestEffort("time", report)
			intent, err := ReadSupervisorIntent(filepath.Join(dir, supervisorIntentFileLeaf))
			if err != nil || len(intent.Daemons) != 0 || prepared != 1 {
				t.Fatalf("durable removal or no-redial contract failed: prepared=%d intent=%+v err=%v", prepared, intent, err)
			}
			if len(report.TaskDeleteWarns) != 1 || !strings.Contains(report.TaskDeleteWarns[0], "cleanup incomplete") {
				t.Fatalf("missing cleanup refusal: %+v", report.TaskDeleteWarns)
			}
			if mode != "capture-failed" && stop.closed != 1 {
				t.Fatalf("original capture close count=%d", stop.closed)
			}
		})
	}
}

func TestRemovedSupervisorCaptureClosesWhenMutationIsNoop(t *testing.T) {
	dir, _ := seedForceStopDescriptor(t, "time", 0)
	stop := &removedStopFixture{complete: func(context.Context) (ForceStopResultV1, error) {
		t.Fatal("no-op mutation consumed a capture")
		return ForceStopResultV1{}, nil
	}}
	installRemovedPrepareFixture(t, func(context.Context, string, int) (supervisorRemovedStop, error) {
		// A concurrent owner removes the row between preliminary capture and
		// the lock-held merge. The no-op return must still close our connection.
		if err := WriteSupervisorIntent(filepath.Join(dir, supervisorIntentFileLeaf), &SupervisorIntentFile{Version: 1}); err != nil {
			t.Fatal(err)
		}
		return stop, nil
	})
	changed, _, captures, err := NewAPI().removeServerFromSupervisorIntentCore(context.Background(), "time", nil, true)
	if err != nil || changed || captures != nil || stop.closed != 1 {
		t.Fatalf("no-op leaked retained connection: changed=%t captures=%v closed=%d err=%v", changed, captures, stop.closed, err)
	}
}

func TestUninstallCapturesOriginalBeforeStopIntentAndSchedulerDeletion(t *testing.T) {
	for _, server := range []string{"time", "gdb"} {
		t.Run(server, func(t *testing.T) {
			dir, task := seedForceStopDescriptor(t, server, 0)
			installRecordingAudit(t, &recordingAuditWriter{})
			sch := newInstallFakeScheduler()
			sch.listSeed = []scheduler.TaskStatus{{Name: task}}
			sch.tasks[task] = true
			installFakeScheduler(t, sch)
			completed := false
			stop := &removedStopFixture{complete: func(context.Context) (ForceStopResultV1, error) {
				assertSupervisorIntentFlockAvailableDuringIPCStatus(t, dir)
				current, err := ReadSupervisorIntent(filepath.Join(dir, supervisorIntentFileLeaf))
				if err != nil || current.FindSupervisorDaemonByTaskName(task) != nil || len(sch.deleteNames) != 1 {
					t.Fatalf("completion preceded removal/delete: intent=%+v deleted=%v err=%v", current, sch.deleteNames, err)
				}
				completed = true
				return forceStopClosedFixture(task, 0), nil
			}}
			installRemovedPrepareFixture(t, func(context.Context, string, int) (supervisorRemovedStop, error) {
				assertSupervisorIntentFlockAvailableDuringIPCStatus(t, dir)
				current, err := ReadSupervisorIntent(filepath.Join(dir, supervisorIntentFileLeaf))
				if err != nil || current.FindSupervisorDaemonByTaskName(task) == nil || len(current.Stops) != 0 || len(sch.deleteNames) != 0 {
					t.Fatalf("capture followed first intent/task mutation: intent=%+v deleted=%v err=%v", current, sch.deleteNames, err)
				}
				return stop, nil
			})
			t.Cleanup(setSupervisorReconcileApplyHookForTest(func(context.Context, bool) (ReconcileResponse, error) { return ReconcileResponse{}, nil }))
			report, err := NewAPI().Uninstall(server)
			if err != nil || !completed || stop.closed != 1 || len(report.TaskDeleteWarns) != 0 {
				t.Fatalf("original cleanup chain failed: completed=%t close=%d report=%+v err=%v", completed, stop.closed, report, err)
			}
		})
	}
}

func TestRemovedForceStopOlderServerRefusesPreparationBeforeEffect(t *testing.T) {
	client, server := net.Pipe()
	defer client.Close()
	defer server.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	deadline, _ := ctx.Deadline()
	if err := server.SetDeadline(deadline); err != nil {
		t.Fatal(err)
	}
	finished := make(chan error, 1)
	oldActions := 0
	fixture := forceStopClosedFixture(`\mcp-local-hub-demo-default`, 0)
	go func() {
		var request IPCRequest
		if err := json.NewDecoder(server).Decode(&request); err != nil {
			finished <- err
			return
		}
		response := IPCResponse{ID: request.ID, Final: true, Error: &IPCErr{Code: "UNKNOWN_COMMAND", Message: "old server"}}
		if request.Cmd == "force_stop" {
			// The published v1 implementation ignores unknown JSON fields.
			// Sending preparation to this verb would therefore kill immediately.
			oldActions++
			response.Error, response.OK, response.Result = nil, true, fixture
		}
		finished <- json.NewEncoder(server).Encode(response)
	}()
	command := ForceStopCommandV1{ProtocolVersion: 1, Target: fixture.Target, RemovalPhase: ForceStopRetainRemoved}
	_, err := withForceStopExchangeContext(ctx, client, func() (ForceStopResultV1, error) {
		return exchangeSupervisorForceStopCommand(ctx, client, 1, command)
	})
	if serverErr := <-finished; serverErr != nil {
		t.Fatal(serverErr)
	}
	var refusal *IPCErr
	if !errors.As(err, &refusal) || refusal.Code != "UNKNOWN_COMMAND" || oldActions != 0 {
		t.Fatalf("preparation fell into older destructive verb: actions=%d err=%v", oldActions, err)
	}
}

func TestRemovedForceStopLostResponseConsumesOriginalConnection(t *testing.T) {
	client, server := net.Pipe()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	fixture := forceStopClosedFixture(`\mcp-local-hub-demo-default`, 0)
	capture := &RetainedSupervisorForceStop{conn: client, command: ForceStopCommandV1{ProtocolVersion: 1, Target: fixture.Target, RemovalPhase: ForceStopRetainRemoved}, nextID: 3}
	defer capture.Close()
	finished := make(chan IPCRequest, 1)
	go func() {
		var request IPCRequest
		_ = json.NewDecoder(server).Decode(&request)
		_ = server.Close() // action may have happened; its receipt was lost.
		finished <- request
	}()
	_, firstErr := capture.Complete(ctx)
	request := <-finished
	_, secondErr := capture.Complete(ctx)
	if firstErr == nil || !strings.Contains(firstErr.Error(), "not retried") || secondErr == nil || request.Cmd != "force_stop_removed" || request.ID != 3 || capture.conn != nil {
		t.Fatalf("ambiguous original exchange retried or lost: request=%+v first=%v second=%v", request, firstErr, secondErr)
	}
}

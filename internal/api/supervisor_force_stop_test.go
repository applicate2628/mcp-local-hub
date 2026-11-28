package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func seedForceStopDescriptor(t *testing.T, server string, port int) (string, string) {
	t.Helper()
	dir := phaseFStateDir(t)
	task := `\mcp-local-hub-` + server + "-default"
	intent := &SupervisorIntentFile{Version: 1, Daemons: []SupervisorDaemon{{TaskName: task, Server: server, Daemon: "default", Port: port}}}
	if err := WriteSupervisorIntent(filepath.Join(dir, supervisorIntentFileLeaf), intent); err != nil {
		t.Fatal(err)
	}
	return dir, task
}

func installForceStopReceipt(t *testing.T, fn func(context.Context, string, int) (ForceStopResultV1, error)) {
	t.Helper()
	previous := supervisorForceStopFn
	supervisorForceStopFn = fn
	t.Cleanup(func() { supervisorForceStopFn = previous })
}

// An API mapping fixture, not a native creator proof.
func forceStopClosedFixture(task string, port int) ForceStopResultV1 {
	portState := "free"
	if port == 0 {
		portState = "unspecified"
	}
	return ForceStopResultV1{ProtocolVersion: 1, Target: ForceStopTargetV1{TaskName: task, PID: 4242, StartedAt: "2026-11-24T00:00:00Z", PIDGeneration: 7, ExpectedPort: port},
		State: ForceStopClosed, Reason: ForceStopClosed, RootExited: true, TreeClosed: true, HandlesClosed: true, Action: "request-succeeded", PortState: portState}
}

func sealForceStopNumericalFallbacks(t *testing.T) {
	t.Helper()
	previousPID, previousPort, previousLookup, previousKill := stopForceKillPIDFn, forceKillByPortFn, lookupPortPID, killByPortFn
	stopForceKillPIDFn = func(int) error { t.Fatal("force-stop used numerical PID termination"); return nil }
	forceKillByPortFn = func(int, time.Duration) (portKillOutcome, error) {
		t.Fatal("force-stop used listener-based termination")
		return portKillNoListener, nil
	}
	killByPortFn = func(int, time.Duration) error { t.Fatal("force-stop fell through to the legacy kill path"); return nil }
	lookupPortPID = func(int) (int, bool, error) {
		t.Fatal("force-stop rediscovered a port owner in the API")
		return 0, false, nil
	}
	t.Cleanup(func() {
		stopForceKillPIDFn, forceKillByPortFn, lookupPortPID, killByPortFn = previousPID, previousPort, previousLookup, previousKill
	})
}

func TestStopWithOptsForceRefusesManifestOnlyTaskWithoutFallback(t *testing.T) {
	phaseFStateDir(t)
	installRecordingAudit(t, &recordingAuditWriter{})
	sealForceStopNumericalFallbacks(t)
	installForceStopReceipt(t, func(context.Context, string, int) (ForceStopResultV1, error) {
		t.Fatal("manifest-only task has no Supervisor descriptor")
		return ForceStopResultV1{}, nil
	})
	rows, err := NewAPI().StopWithOpts(StopOpts{Server: "time", Force: true})
	if err != nil || len(rows) != 1 || rows[0].Code != ForceStopOwnerUnavailable || rows[0].Err == "" {
		t.Fatalf("legacy refusal rows=%+v err=%v", rows, err)
	}
}

func TestForceStopRootExitAloneIsNotTreeClosure(t *testing.T) {
	seedForceStopDescriptor(t, "time", 9128)
	sealForceStopNumericalFallbacks(t)
	installForceStopReceipt(t, func(ctx context.Context, task string, port int) (ForceStopResultV1, error) {
		receipt := forceStopClosedFixture(task, port)
		receipt.TreeClosed = false
		return receipt, nil
	})
	rows, handled, err := stopForceKillSupervisorOwned(context.Background(), "time", "")
	if err != nil || !handled || len(rows) != 1 || rows[0].Err == "" {
		t.Fatalf("root-only result accepted: handled=%t rows=%+v err=%v", handled, rows, err)
	}
}

func TestForceStopClientUnsupportedPlatformHasTypedRefusal(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("unsupported-platform contract")
	}
	_, err := DialSupervisorIPCForceStop(context.Background(), `\mcp-local-hub-time-default`, 9128)
	var refusal *IPCErr
	if !errors.As(err, &refusal) || refusal.Code != ForceStopUnsupported {
		t.Fatalf("unsupported platform result=%v", err)
	}
}

// Framing/generation model only. Production calls this exchange only after the
// native peer check, which an in-memory net.Pipe deliberately cannot provide.
func TestForceStopExchangePinsGenerationAndNeverRetries(t *testing.T) {
	for _, mode := range []string{"closed", "stale-status-pid", "old-status", "old-verb", "wrong-generation", "root-only", "not-final", "lost-response"} {
		t.Run(mode, func(t *testing.T) {
			client, server := net.Pipe()
			deadline := time.Now().Add(5 * time.Second)
			if err := client.SetDeadline(deadline); err != nil {
				t.Fatal(err)
			}
			if err := server.SetDeadline(deadline); err != nil {
				t.Fatal(err)
			}
			defer client.Close()
			defer server.Close()
			owner := SupervisorLockOwner{PID: 1001, StartedAt: "2026-11-24T00:00:00Z"}
			task := `\mcp-local-hub-time-default`
			fixture := forceStopClosedFixture(task, 9128)
			finished := make(chan error, 1)
			go func() {
				defer server.Close()
				encoder, decoder := json.NewEncoder(server), json.NewDecoder(server)
				if err := encoder.Encode(map[string]any{"hello": IPCHello{Version: 1, PID: owner.PID, StartedAt: owner.StartedAt}}); err != nil {
					finished <- err
					return
				}
				var request IPCRequest
				if err := decoder.Decode(&request); err != nil {
					finished <- err
					return
				}
				if request.Cmd != "status" {
					finished <- fmt.Errorf("first command=%s", request.Cmd)
					return
				}
				daemon := supervisorIPCStatusDaemon{TaskName: task, CurrentPID: fixture.Target.PID, StartedAt: fixture.Target.StartedAt, PIDGeneration: fixture.Target.PIDGeneration}
				if mode == "old-status" {
					daemon.PIDGeneration = 0
				}
				if mode == "stale-status-pid" {
					daemon.StalePID, daemon.CurrentPID = daemon.CurrentPID, 0
				}
				if err := encoder.Encode(IPCResponse{ID: request.ID, OK: true, Result: map[string]any{"daemons": []supervisorIPCStatusDaemon{daemon}}}); err != nil {
					finished <- err
					return
				}
				err := decoder.Decode(&request)
				if mode == "old-status" {
					if err == nil {
						finished <- fmt.Errorf("destructive request sent without generation")
						return
					}
					finished <- nil
					return
				}
				if err != nil {
					finished <- err
					return
				}
				if request.Cmd != "force_stop" {
					finished <- fmt.Errorf("second command=%s", request.Cmd)
					return
				}
				raw, err := json.Marshal(request.Args["command"])
				var command ForceStopCommandV1
				if err == nil {
					err = json.Unmarshal(raw, &command)
				}
				if err != nil || command.Target != fixture.Target {
					finished <- fmt.Errorf("target changed: %+v, %v", command.Target, err)
					return
				}
				if mode == "lost-response" {
					finished <- nil
					return
				}
				response := IPCResponse{ID: request.ID, OK: true, Result: fixture, Final: true}
				switch mode {
				case "old-verb":
					response.OK, response.Result, response.Error = false, nil, &IPCErr{Code: "UNKNOWN_COMMAND", Message: "old Supervisor"}
				case "wrong-generation":
					fixture.Target.PIDGeneration++
					response.Result = fixture
				case "root-only":
					fixture.TreeClosed = false
					response.Result = fixture
				case "not-final":
					response.Final = false
				}
				if err := encoder.Encode(response); err != nil {
					finished <- err
					return
				}
				if err := decoder.Decode(&request); err == nil {
					finished <- fmt.Errorf("unexpected retry %s", request.Cmd)
					return
				}
				finished <- nil
			}()
			ctx, cancel := context.WithDeadline(context.Background(), deadline)
			defer cancel()
			result, err := exchangeSupervisorForceStop(ctx, client, owner, task, 9128)
			client.Close()
			if serverErr := <-finished; serverErr != nil {
				t.Fatal(serverErr)
			}
			wantSuccess := mode == "closed" || mode == "stale-status-pid"
			if (err == nil) != wantSuccess {
				t.Fatalf("result=%+v err=%v", result, err)
			}
			if mode == "old-verb" {
				var refusal *IPCErr
				if !errors.As(err, &refusal) || refusal.Code != "UNKNOWN_COMMAND" {
					t.Fatalf("old verb refusal=%v", err)
				}
			}
			if mode == "lost-response" && !strings.Contains(err.Error(), "outcome unknown; not retried") {
				t.Fatalf("ambiguous error=%v", err)
			}
		})
	}
}

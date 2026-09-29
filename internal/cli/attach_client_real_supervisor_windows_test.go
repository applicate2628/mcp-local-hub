//go:build windows && test_state_path_env

package cli

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"mcp-local-hub/internal/api"
	"mcp-local-hub/internal/api/apitest"
	"mcp-local-hub/internal/process"
	"mcp-local-hub/internal/scheduler"
)

// The supervisor's real daemon launches this selected helper test binary as
// its stdio MCP backend. It never binds a separate port or changes host state.
func TestRestoreStdioHelper(t *testing.T) {
	if os.Getenv("MCPHUB_RESTORE_STDIO_HELPER") != "1" {
		return
	}
	scanner := bufio.NewScanner(os.Stdin)
	for scanner.Scan() {
		var request struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
		}
		if json.Unmarshal(scanner.Bytes(), &request) != nil || len(request.ID) == 0 {
			continue
		}
		result := `{"tools":[]}`
		if request.Method == "initialize" {
			result = `{"protocolVersion":"2025-03-26","capabilities":{},"serverInfo":{"name":"restore-fixture","version":"1"}}`
		}
		_, _ = fmt.Fprintf(os.Stdout, `{"jsonrpc":"2.0","id":%s,"result":%s}`+"\n", request.ID, result)
	}
}

func TestRestore_NoRestart_RealSupervisorOccupiedPortAndReceiptRollback(t *testing.T) {
	bin := os.Getenv("MCPHUB_RESTORE_TEST_BIN")
	if bin == "" || !filepath.IsAbs(bin) {
		t.Skip("set MCPHUB_RESTORE_TEST_BIN to an isolated tagged mcphub test binary")
	}
	if info, err := os.Lstat(bin); err != nil || !info.Mode().IsRegular() {
		t.Fatalf("test-owned binary unavailable: %v", err)
	}
	root := apitest.HardenedTempDir(t)
	stateDir := root
	t.Setenv("MCPHUB_STATE_DIR_OVERRIDE", stateDir)
	t.Setenv("HOME", root)
	t.Setenv("USERPROFILE", root)
	t.Setenv("LOCALAPPDATA", root)
	t.Setenv("APPDATA", root)
	t.Setenv("MIMOCODE_HOME", filepath.Join(root, "mimo"))
	t.Setenv("MIMOCODE_DISABLE_CLAUDE_CODE_MCP", "1")
	restoreState := api.SetDaemonStateRootForTest(stateDir)
	defer restoreState()
	manifestDir := filepath.Join(root, "manifests")
	t.Setenv("MCPHUB_MANIFEST_DIR_OVERRIDE", manifestDir)
	priorBuiltin := builtinRouteSeedingDisabledForTest
	builtinRouteSeedingDisabledForTest = true
	defer func() { builtinRouteSeedingDisabledForTest = priorBuiltin }()
	undoSpawn := setReconcileSpawnFnForTest(nil)
	defer undoSpawn()
	undoList := setReconcileSchedulerListFnForTest(func(context.Context) ([]scheduler.TaskStatus, error) { return nil, nil })
	defer undoList()

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := listener.Addr().(*net.TCPAddr).Port
	if err := listener.Close(); err != nil {
		t.Fatal(err)
	}
	name := "restore-real"
	raw := fmt.Sprintf("name: %s\nkind: global\ntransport: stdio-bridge\ncommand: %s\nbase_args:\n  - '-test.run=^TestRestoreStdioHelper$'\nenv:\n  MCPHUB_RESTORE_STDIO_HELPER: '1'\ndaemons:\n  - name: default\n    port: %d\nclient_bindings:\n  - client: mimocode\n    daemon: default\n    url_path: /mcp\nweekly_refresh: false\n", name, strconv.Quote(os.Args[0]), port)
	serverDir := filepath.Join(manifestDir, name)
	if err := os.MkdirAll(serverDir, 0700); err != nil {
		t.Fatal(err)
	}
	manifest := filepath.Join(serverDir, "manifest.yaml")
	if err := os.WriteFile(manifest, []byte(raw), 0600); err != nil {
		t.Fatal(err)
	}
	selected := filepath.Join(root, "mimo", "config", "mimocode.json")
	if err := os.MkdirAll(filepath.Dir(selected), 0700); err != nil {
		t.Fatal(err)
	}
	direct := []byte(`{"mcp":{"restore-real":{"type":"local","command":["node","server.js"],"enabled":true}}}`)
	if err := os.WriteFile(selected, direct, 0600); err != nil {
		t.Fatal(err)
	}
	intent := &api.SupervisorIntentFile{Version: 1, IntentGeneration: 7, OwnerMode: api.OwnerModeSupervise, Daemons: []api.SupervisorDaemon{{
		TaskName: `\mcp-local-hub-restore-real-default`, Server: name, Daemon: "default", Command: bin,
		Args: []string{"daemon", "--server", name, "--daemon", "default"}, Port: port, ManifestHash: api.ManifestHashContent([]byte(raw)),
	}}}
	if err := api.WriteSupervisorIntent(filepath.Join(stateDir, "supervisor-intent.json"), intent); err != nil {
		t.Fatal(err)
	}
	conn, reader, stop := startSuperviseForIPCTest(t, stateDir)
	stopped := false
	defer func() {
		if !stopped {
			stop()
		}
	}()
	var beforePID int
	var beforeStart string
	deadline := time.Now().Add(12 * time.Second)
	for time.Now().Before(deadline) {
		beforePID, beforeStart = restoreRealStatus(t, conn, reader, name)
		if beforePID > 0 && api.ProbeMCPReadinessContext(context.Background(), port).State == api.MCPReadinessReady {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if beforePID <= 0 || beforeStart == "" {
		t.Fatal("real supervisor child did not become ready")
	}
	reapOwnedChild := func() {
		proof := process.PIDIdentityProof{PID: beforePID, ExecutablePath: bin, StartedAt: beforeStart, StartTolerance: time.Nanosecond}
		if process.VerifyPIDIdentity(proof) == nil {
			if child, err := os.FindProcess(beforePID); err == nil {
				_ = child.Kill()
				_, _ = child.Wait()
			}
		}
	}
	defer reapOwnedChild()
	if owner, found, err := api.LoopbackPortOwnerPIDContext(context.Background(), port); err != nil || !found || owner != beforePID {
		t.Fatalf("initial listener owner: pid=%d found=%v err=%v", owner, found, err)
	}
	source := filepath.Join(stateDir, "exact-source.yaml")
	if err := api.WriteStateFileBytesAtomic(source, []byte(raw)); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(manifest); err != nil {
		t.Fatal(err)
	}
	a := api.NewAPI()
	req := api.AttachClientRequest{Server: name, Daemon: "default", Client: "mimocode", ManifestSource: source}
	plan, err := a.BuildAttachClientPlan(context.Background(), req)
	if err != nil || plan == nil || plan.State != "would-restore-and-attach" {
		t.Fatalf("real missing-manifest preview: plan=%+v err=%v", plan, err)
	}
	result, err := a.ApplyAttachClientPlan(context.Background(), plan)
	if err != nil || result == nil || result.State != "committed" || !result.ManifestRestored {
		leaseReport, leaseErr := api.InspectAdoptLeaseNamespace()
		t.Fatalf("real restore+attach: result=%+v err=%v lease_state=%s lease_reason=%s lease_error=%v", result, err, leaseReport.State, leaseReport.ReasonID, leaseErr)
	}
	if got, err := os.ReadFile(manifest); err != nil || string(got) != raw {
		t.Fatalf("restored source mismatch: err=%v", err)
	}
	if err := restoreRealReconcile(t, conn, reader); err != nil {
		t.Fatal(err)
	}
	afterPID, afterStart := restoreRealStatus(t, conn, reader, name)
	owner, found, ownerErr := api.LoopbackPortOwnerPIDContext(context.Background(), port)
	if afterPID != beforePID || afterStart != beforeStart || ownerErr != nil || !found || owner != beforePID {
		t.Fatalf("restore restarted or duplicated child: before=%d/%s after=%d/%s listener=%d found=%v err=%v", beforePID, beforeStart, afterPID, afterStart, owner, found, ownerErr)
	}
	rollback, err := a.BuildAttachClientRollbackPlan(context.Background(), api.AttachClientRequest{Server: name, Daemon: "default", Client: "mimocode"}, result.ReceiptID)
	if err != nil || rollback == nil || rollback.State != "would-rollback" {
		t.Fatalf("receipt rollback preview: plan=%+v err=%v", rollback, err)
	}
	rolled, err := a.RollbackAttachClient(context.Background(), api.AttachClientRequest{Server: name, Daemon: "default", Client: "mimocode"}, result.ReceiptID)
	if err != nil || rolled == nil || rolled.State != "rolled-back" {
		t.Fatalf("receipt rollback: result=%+v err=%v", rolled, err)
	}
	afterRollback, err := a.BuildAttachClientPlan(context.Background(), api.AttachClientRequest{Server: name, Daemon: "default", Client: "mimocode"})
	if err != nil || afterRollback == nil || afterRollback.BeforeTransport != "direct" {
		t.Fatalf("receipt rollback did not restore selected direct origin: plan=%+v err=%v", afterRollback, err)
	}
	if got, err := os.ReadFile(manifest); err != nil || string(got) != raw {
		t.Fatalf("rollback removed recovered manifest: err=%v", err)
	}
	stop()
	stopped = true
	reapOwnedChild()
	settled := false
	for until := time.Now().Add(3 * time.Second); time.Now().Before(until); {
		owner, found, err := api.LoopbackPortOwnerPIDContext(context.Background(), port)
		if err == nil && (!found || owner != beforePID) {
			settled = true
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if !settled {
		t.Fatal("test-owned child listener remained after supervisor shutdown")
	}
}

func restoreRealStatus(t *testing.T, conn net.Conn, reader *bufio.Reader, server string) (int, string) {
	t.Helper()
	_ = conn.SetDeadline(time.Now().Add(3 * time.Second))
	raw, _ := json.Marshal(api.IPCRequest{ID: 8101, Cmd: "status"})
	if _, err := conn.Write(append(raw, '\n')); err != nil {
		t.Fatal(err)
	}
	line, err := reader.ReadString('\n')
	if err != nil {
		t.Fatal(err)
	}
	var response api.IPCResponse
	if err := json.Unmarshal([]byte(line), &response); err != nil || !response.OK {
		t.Fatalf("status response: %+v err=%v", response, err)
	}
	result, _ := response.Result.(map[string]any)
	daemons, _ := result["daemons"].([]any)
	var pid int
	var started string
	for _, item := range daemons {
		row, _ := item.(map[string]any)
		if row["server"] != server || row["daemon"] != "default" {
			continue
		}
		if pid != 0 {
			t.Fatal("real supervisor reported duplicate selected daemon")
		}
		pid = int(row["current_pid"].(float64))
		started, _ = row["started_at"].(string)
	}
	return pid, started
}

func restoreRealReconcile(t *testing.T, conn net.Conn, reader *bufio.Reader) error {
	t.Helper()
	_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
	raw, _ := json.Marshal(api.IPCRequest{ID: 8102, Cmd: "reconcile", Args: map[string]any{"apply": true}})
	if _, err := conn.Write(append(raw, '\n')); err != nil {
		return err
	}
	line, err := reader.ReadString('\n')
	if err != nil {
		return err
	}
	var response api.IPCResponse
	if err := json.Unmarshal([]byte(strings.TrimSpace(line)), &response); err != nil || !response.OK {
		return fmt.Errorf("reconcile refused: response=%+v err=%v", response, err)
	}
	return nil
}

//go:build windows && test_state_path_env

package api

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Microsoft/go-winio"
	"mcp-local-hub/internal/clients"
	"mcp-local-hub/internal/process"
)

// This is a cross-process public CLI receiving test, not a real supervisor
// lifecycle test. The Go test PID is the simulated managed child, owns the
// HTTP listener, and serves only the supervisor status IPC fixture.
func TestAttachExisting_PublicCLIReceiving(t *testing.T) {
	bin := os.Getenv("MCPHUB_ATTACH_TEST_BIN")
	wantBinHash := os.Getenv("MCPHUB_ATTACH_TEST_BIN_SHA256")
	if bin == "" || wantBinHash == "" {
		t.Fatal("tagged receiving binary and its expected digest are required")
	}
	if !filepath.IsAbs(bin) {
		t.Fatal("receiving binary must be absolute")
	}
	info, err := os.Lstat(bin)
	if err != nil || !info.Mode().IsRegular() {
		t.Fatalf("receiving binary is not a regular file: %v", err)
	}
	if got := publicCLIFileHash(t, bin); !strings.EqualFold(got, wantBinHash) {
		t.Fatal("receiving binary digest drifted")
	}

	root := hardenedTempDir(t)
	t.Logf("fixture_temp_root=%s", root) // raw test log is kept only in task-owned .scratch
	stateDir := filepath.Join(root, "state")
	if err := os.Mkdir(stateDir, 0700); err != nil {
		t.Fatal(err)
	}
	entries, err := allowlistExplicitAccess()
	if err != nil {
		t.Fatal(err)
	}
	applyProtectedDACLFromEntries(t, stateDir, entries)
	restoreClientEnv, err := clients.ApplyClientConfigSandboxEnvironment(root)
	if err != nil {
		t.Fatalf("prepare public CLI client sandbox: %v", err)
	}
	t.Cleanup(restoreClientEnv)
	t.Setenv("MCPHUB_STATE_DIR_OVERRIDE", stateDir)
	withDaemonStateRootOverride(t, stateDir)
	leaseReport, leaseErr := InspectAdoptLeaseNamespace()
	if leaseErr != nil || leaseReport.State != AdoptLeaseNamespaceMissing {
		t.Fatalf("fixture lease-root preflight: state=%s reason=%s err=%v", leaseReport.State, leaseReport.ReasonID, leaseErr)
	}
	t.Setenv("MIMOCODE_HOME", filepath.Join(root, "mimo"))
	t.Setenv("MIMOCODE_DISABLE_CLAUDE_CODE_MCP", "1")
	codexHome := filepath.Join(root, "codex")
	t.Setenv("CODEX_HOME", codexHome)
	for _, dir := range []string{filepath.Join(root, "mimo", "config"), codexHome} {
		if err := os.MkdirAll(dir, 0700); err != nil {
			t.Fatal(err)
		}
	}
	selected := filepath.Join(root, "mimo", "config", "mimocode.json")
	selectedBefore := []byte(`{"mcp":{"codegraph":{"type":"local","command":["node","server.js"],"enabled":true},"sibling":{"type":"local","command":["node","sibling.js"],"enabled":true}}}`)
	if err := os.WriteFile(selected, selectedBefore, 0600); err != nil {
		t.Fatal(err)
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/mcp" {
			http.NotFound(w, r)
			return
		}
		if r.Method == http.MethodDelete {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		body, _ := io.ReadAll(io.LimitReader(r.Body, 4096))
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Mcp-Session-Id", "attach-test-session")
		switch {
		case bytes.Contains(body, []byte(`"initialize"`)):
			_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":{"protocolVersion":"2025-03-26","capabilities":{}}}`))
		case bytes.Contains(body, []byte(`"tools/list"`)):
			_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":2,"result":{"tools":[{"name":"probe"}]}}`))
		default:
			http.Error(w, "unexpected", http.StatusBadRequest)
		}
	}))
	defer srv.Close()
	port := parsePort(t, srv.URL)
	otherHTTP := filepath.Join(codexHome, "config.toml")
	otherDirect := filepath.Join(root, ".claude.json")
	if err := os.WriteFile(otherHTTP, []byte("[mcp_servers.codegraph]\nurl = \"http://127.0.0.1:"+strconv.Itoa(port)+"/mcp\"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(otherDirect, []byte(`{"mcpServers":{"codegraph":{"type":"stdio","command":"node","args":["excluded.js"]}}}`), 0600); err != nil {
		t.Fatal(err)
	}
	manifestDir := writeMimoManifest(t, root, "codegraph", port)
	t.Setenv("MCPHUB_MANIFEST_DIR_OVERRIDE", manifestDir)
	raw, err := NewAPI().ManifestGet("codegraph")
	if err != nil {
		t.Fatal(err)
	}
	started, ok := process.ProcessStartTime(os.Getpid())
	if !ok {
		t.Fatal("test process kernel start unavailable")
	}
	image, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	owner := SupervisorLockOwner{PID: os.Getpid(), StartedAt: started.UTC().Format(time.RFC3339Nano)}
	writeSupervisorOwnerForTest(t, stateDir, owner)
	intent := SupervisorIntentFile{Version: 1, IntentGeneration: 8, Daemons: []SupervisorDaemon{{
		TaskName: `\mcp-local-hub-codegraph-default`, Server: "codegraph", Daemon: "default",
		Command: image, Args: []string{"daemon", "--server", "codegraph", "--daemon", "default"},
		Port: port, ManifestHash: ManifestHashContent([]byte(raw)),
	}}}
	if err := WriteSupervisorIntent(filepath.Join(stateDir, supervisorIntentFileLeaf), &intent); err != nil {
		t.Fatal(err)
	}
	pipeName := SupervisorIPCAddress(stateDir)
	pipeKey := sha256.Sum256([]byte(stateDir))
	expectedPipe := `\\.\pipe\mcphub-supervisor-test-` + hex.EncodeToString(pipeKey[:8])
	if pipeName != expectedPipe {
		t.Fatal("fixture pipe is not isolated")
	}
	pipe, err := winio.ListenPipe(pipeName, &winio.PipeConfig{InputBufferSize: 4096, OutputBufferSize: 4096})
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			conn, err := pipe.Accept()
			if err != nil {
				return
			}
			serveFakeSupervisorIPCStatusConn(t, conn, owner, func(req IPCRequest) IPCResponse {
				return IPCResponse{ID: req.ID, OK: true, Result: map[string]any{"state": "running", "daemons": []map[string]any{{
					"task_name": intent.Daemons[0].TaskName, "server": "codegraph", "daemon": "default", "command": image,
					"args": intent.Daemons[0].Args, "port": port, "state": "Running", "current_pid": os.Getpid(),
					"started_at": owner.StartedAt, "is_maintenance": false,
				}}}}
			})
		}
	}()
	defer func() {
		_ = pipe.Close()
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			t.Error("fixture status pipe did not settle")
		}
	}()

	selectedClient, err := clients.NewMimoCode()
	if err != nil || selectedClient.ConfigPath() != selected {
		t.Fatalf("selected MiMoCode path escaped fixture: %v", err)
	}
	for _, p := range []string{otherHTTP, otherDirect} {
		if rel, err := filepath.Rel(root, p); err != nil || strings.HasPrefix(rel, "..") {
			t.Fatal("excluded config escaped fixture")
		}
	}
	excluded := []string{otherHTTP, otherDirect, filepath.Join(stateDir, supervisorIntentFileLeaf),
		filepath.Join(stateDir, "supervisor.lock.owner.json"), filepath.Join(manifestDir, "codegraph", "manifest.yaml")}
	before := publicCLIHashes(t, append([]string{selected}, excluded...))
	assertListener := func() {
		t.Helper()
		pid, found, err := LoopbackPortOwnerPIDContext(context.Background(), port)
		if err != nil || !found || pid != os.Getpid() {
			t.Fatalf("fixture listener owner changed: pid=%d found=%v err=%v", pid, found, err)
		}
	}
	assertListener()

	var pids []int
	run := func(step, operation, state, transport, receipt string, extra ...string) string {
		t.Helper()
		// The tag-built child's state and pipe must both be task-local before
		// EVERY spawn. Never rely on a parent-only in-memory override here.
		resolved, err := DaemonStateDir()
		rel, relErr := filepath.Rel(root, stateDir)
		if err != nil || resolved != stateDir || os.Getenv("MCPHUB_STATE_DIR_OVERRIDE") != stateDir ||
			relErr != nil || rel == "." || strings.HasPrefix(rel, "..") ||
			SupervisorIPCAddress(stateDir) != expectedPipe || pipeName != expectedPipe {
			t.Fatal("unsafe cross-process state or pipe resolution")
		}
		args := []string{"attach-client", "codegraph", "--daemon", "default", "--client", "mimocode"}
		args = append(args, extra...)
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		cmd := exec.CommandContext(ctx, bin, args...)
		cmd.Env = os.Environ()
		var stdout, stderr bytes.Buffer
		cmd.Stdout, cmd.Stderr = &stdout, &stderr
		if err := cmd.Start(); err != nil {
			t.Fatalf("%s CLI start: %v", step, err)
		}
		pid := cmd.Process.Pid
		pids = append(pids, pid)
		err = cmd.Wait() // reap this exact child even on timeout or failure
		if ctx.Err() != nil || err != nil || stderr.Len() != 0 {
			t.Fatalf("%s CLI failed or timed out: pid=%d err=%v timeout=%v stderr=%q", step, pid, err, ctx.Err(), stderr.String())
		}
		out := stdout.String()
		if !strings.Contains(out, "operation="+operation+" state="+state+" ") ||
			!strings.Contains(out, "transport="+transport+" ") ||
			!strings.Contains(out, "entry_token_sha256=") ||
			!strings.Contains(out, `server="codegraph" daemon="default" client="mimocode" entry="codegraph"`) ||
			strings.Contains(out, root) || strings.Contains(out, image) || strings.Contains(out, "server.js") ||
			strings.Contains(out, "excluded.js") || strings.Contains(out, "sibling.js") {
			t.Fatalf("%s CLI stdout is missing or not redacted: pid=%d output=%q", step, pid, out)
		}
		if (len(extra) == 0 || extra[len(extra)-1] != "--apply") && !strings.Contains(out, "target_bytes_sha256=") {
			t.Fatalf("%s preview omitted selected target hash", step)
		}
		if len(extra) != 0 && extra[len(extra)-1] == "--apply" &&
			(!strings.Contains(out, "before_sha256=") || !strings.Contains(out, "after_sha256=")) {
			t.Fatalf("%s apply omitted selected before/after hashes", step)
		}
		if receipt != "" && !strings.Contains(out, `receipt_id="`+receipt+`"`) {
			t.Fatalf("%s lost exact receipt identifier", step)
		}
		assertListener()
		t.Logf("%s: cli_pid=%d exit=0 stdout_nonempty=true state=%s listener_pid=%d port=%d", step, pid, state, os.Getpid(), port)
		return out
	}
	checkExcluded := func(stage string) {
		t.Helper()
		for _, p := range excluded {
			if got := publicCLIFileHash(t, p); got != before[p] {
				t.Fatalf("%s changed excluded/manifest/owner input %s", stage, filepath.Base(p))
			}
		}
	}

	run("attach-plan", "attach", "would-attach", "direct->http", "")
	if got := publicCLIFileHash(t, selected); got != before[selected] {
		t.Fatal("attach preview changed selected config")
	}
	checkExcluded("attach preview")
	plan, err := NewAPI().BuildAttachClientPlan(context.Background(), AttachClientRequest{Server: "codegraph", Daemon: "default", Client: "mimocode"})
	if err != nil {
		t.Fatal(err)
	}
	receiptLeaf, snapshotLeaf := attachReceiptLeaves(plan)
	receiptPath, snapshotPath := filepath.Join(stateDir, receiptLeaf), filepath.Join(stateDir, snapshotLeaf)
	if _, err := os.Stat(receiptPath); !os.IsNotExist(err) {
		t.Fatalf("attach preview left a receipt: %v", err)
	}
	if _, err := os.Stat(snapshotPath); !os.IsNotExist(err) {
		t.Fatalf("attach preview left a snapshot: %v", err)
	}

	first := run("attach-apply", "attach", "committed", "direct->http", "", "--apply")
	id := regexp.MustCompile(`receipt_id="([^"]+)"`).FindStringSubmatch(first)
	if len(id) != 2 || id[1] == "" {
		t.Fatal("first apply omitted opaque receipt ID")
	}
	receiptID := id[1]
	selectedApplied := publicCLIFileHash(t, selected)
	if selectedApplied == before[selected] || publicCLIFileHash(t, snapshotPath) != before[selected] {
		t.Fatal("first apply did not write selected target and pin exact preimage")
	}
	rec, err := attachReadReceipt(receiptLeaf)
	if err != nil || rec.ID != receiptID || rec.State != "committed" {
		t.Fatalf("first apply receipt invalid: %v", err)
	}
	receiptApplied := publicCLIFileHash(t, receiptPath)
	snapshotApplied := publicCLIFileHash(t, snapshotPath)
	entry, err := selectedClient.GetEntry("codegraph")
	if err != nil || entry == nil || entry.URL != srv.URL+"/mcp" {
		t.Fatal("first apply did not create selected effective HTTP entry")
	}
	checkExcluded("attach apply")

	run("attach-repeat", "attach", "already-committed", "http->http", receiptID, "--apply")
	if publicCLIFileHash(t, selected) != selectedApplied || publicCLIFileHash(t, receiptPath) != receiptApplied || publicCLIFileHash(t, snapshotPath) != snapshotApplied {
		t.Fatal("fresh-process attach repeat wrote selected/receipt/snapshot")
	}
	run("rollback-plan", "rollback", "would-rollback", "http->direct", receiptID, "--rollback-receipt", receiptID)
	if publicCLIFileHash(t, selected) != selectedApplied || publicCLIFileHash(t, receiptPath) != receiptApplied || publicCLIFileHash(t, snapshotPath) != snapshotApplied {
		t.Fatal("rollback preview wrote selected/receipt/snapshot")
	}
	run("rollback-apply", "rollback", "rolled-back", "http->direct", receiptID, "--rollback-receipt", receiptID, "--apply")
	selectedRolled := publicCLIFileHash(t, selected)
	receiptRolled := publicCLIFileHash(t, receiptPath)
	if selectedRolled == selectedApplied || receiptRolled == receiptApplied || publicCLIFileHash(t, snapshotPath) != snapshotApplied {
		t.Fatal("rollback did not settle selected config and receipt while preserving snapshot")
	}
	rec, err = attachReadReceipt(receiptLeaf)
	if err != nil || rec.ID != receiptID || rec.State != "rolled-back" {
		t.Fatalf("rollback receipt invalid: %v", err)
	}
	rolledBytes, err := os.ReadFile(selected)
	if err != nil || !attachRestoredEntryMatches(selectedClient, selectedBefore, rolledBytes, "codegraph") ||
		!attachRestoredEntryMatches(selectedClient, selectedBefore, rolledBytes, "sibling") {
		t.Fatal("rollback did not restore selected direct entry and sibling")
	}
	run("rollback-repeat", "rollback", "already-rolled-back", "direct->direct", receiptID, "--rollback-receipt", receiptID, "--apply")
	if publicCLIFileHash(t, selected) != selectedRolled || publicCLIFileHash(t, receiptPath) != receiptRolled || publicCLIFileHash(t, snapshotPath) != snapshotApplied {
		t.Fatal("fresh-process rollback repeat wrote selected/receipt/snapshot")
	}
	checkExcluded("final")
	t.Logf("receiving hashes: selected_before=%s selected_applied=%s selected_rolled=%s receipt_committed=%s receipt_rolled=%s snapshot=%s", before[selected], selectedApplied, selectedRolled, receiptApplied, receiptRolled, snapshotApplied)
	t.Logf("excluded hashes: codex=%s claude=%s manifest=%s intent=%s owner=%s", before[otherHTTP], before[otherDirect], before[filepath.Join(manifestDir, "codegraph", "manifest.yaml")], before[filepath.Join(stateDir, supervisorIntentFileLeaf)], before[filepath.Join(stateDir, "supervisor.lock.owner.json")])
	t.Logf("receiving process census: fixture_pid=%d listener_port=%d cli_pids=%v; every CLI Wait returned; fixture pipe/listener close on test return", os.Getpid(), port, pids)
}

func publicCLIFileHash(t *testing.T, path string) string {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

func publicCLIHashes(t *testing.T, paths []string) map[string]string {
	t.Helper()
	result := make(map[string]string, len(paths))
	for _, path := range paths {
		result[path] = publicCLIFileHash(t, path)
	}
	return result
}

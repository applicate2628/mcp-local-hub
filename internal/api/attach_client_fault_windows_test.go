//go:build windows

package api

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Microsoft/go-winio"
	"github.com/gofrs/flock"
	"mcp-local-hub/internal/clients"
	"mcp-local-hub/internal/process"
)

type attachFaultFixture struct {
	api            *API
	plan           *AttachClientPlan
	selected       string
	selectedBefore []byte
	excluded       map[string][]byte
	stateDir       string
	intent         SupervisorIntentFile
}

func newAttachFaultFixture(t *testing.T) *attachFaultFixture {
	t.Helper()
	root := t.TempDir()
	stateDir := hardenedTempDir(t)
	withDaemonStateRootOverride(t, stateDir)
	t.Setenv("MCPHUB_STATE_DIR_OVERRIDE", filepath.Join(root, "pipe-isolation"))
	t.Setenv("MIMOCODE_HOME", filepath.Join(root, "mimo"))
	t.Setenv("MIMOCODE_CONFIG", "")
	t.Setenv("MIMOCODE_CONFIG_DIR", "")
	t.Setenv("MIMOCODE_CONFIG_CONTENT", "")
	t.Setenv("MIMOCODE_DISABLE_CLAUDE_CODE_MCP", "1")
	t.Setenv("MIMOCODE_TEST_MANAGED_CONFIG_DIR", filepath.Join(root, "managed"))
	t.Setenv("HOME", root)
	t.Setenv("USERPROFILE", root)
	configDir := filepath.Join(root, "mimo", "config")
	if err := os.MkdirAll(configDir, 0700); err != nil {
		t.Fatal(err)
	}
	selected := filepath.Join(configDir, "mimocode.json")
	selectedBefore := []byte(`{"mcp":{"codegraph":{"type":"local","command":["node","server.js"],"enabled":true}}}`)
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
		body := make([]byte, 4096)
		n, _ := r.Body.Read(body)
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Mcp-Session-Id", "attach-fault-session")
		switch {
		case strings.Contains(string(body[:n]), `"initialize"`):
			_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":{"protocolVersion":"2025-03-26","capabilities":{}}}`))
		case strings.Contains(string(body[:n]), `"tools/list"`):
			_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":2,"result":{"tools":[{"name":"probe"}]}}`))
		default:
			http.Error(w, "unexpected", http.StatusBadRequest)
		}
	}))
	t.Cleanup(srv.Close)
	port := parsePort(t, srv.URL)
	codexHome := filepath.Join(root, "codex")
	if err := os.Mkdir(codexHome, 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CODEX_HOME", codexHome)
	excluded := map[string][]byte{
		filepath.Join(codexHome, "config.toml"): []byte("[mcp_servers.codegraph]\nurl = \"http://127.0.0.1:" + strconv.Itoa(port) + "/mcp\"\n"),
		filepath.Join(root, ".claude.json"):     []byte(`{"mcpServers":{"codegraph":{"type":"stdio","command":"node","args":["excluded.js"]}}}`),
	}
	for path, contents := range excluded {
		if err := os.WriteFile(path, contents, 0600); err != nil {
			t.Fatal(err)
		}
	}
	manifestDir := writeMimoManifest(t, root, "codegraph", port)
	t.Setenv("MCPHUB_MANIFEST_DIR_OVERRIDE", manifestDir)
	a := NewAPI()
	raw, err := a.ManifestGet("codegraph")
	if err != nil {
		t.Fatal(err)
	}
	start, ok := process.ProcessStartTime(os.Getpid())
	if !ok {
		t.Fatal("test process kernel start unavailable")
	}
	imagePath, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	owner := SupervisorLockOwner{PID: os.Getpid(), StartedAt: start.UTC().Format(time.RFC3339Nano)}
	writeSupervisorOwnerForTest(t, stateDir, owner)
	intent := SupervisorIntentFile{Version: 1, IntentGeneration: 8, Daemons: []SupervisorDaemon{{
		TaskName: `\mcp-local-hub-codegraph-default`, Server: "codegraph", Daemon: "default",
		Command: imagePath, Args: []string{"daemon", "--server", "codegraph", "--daemon", "default"},
		Port: port, ManifestHash: ManifestHashContent([]byte(raw)),
	}}}
	if err := WriteSupervisorIntent(filepath.Join(stateDir, supervisorIntentFileLeaf), &intent); err != nil {
		t.Fatal(err)
	}
	pipe, err := winio.ListenPipe(SupervisorIPCAddress(stateDir), &winio.PipeConfig{InputBufferSize: 4096, OutputBufferSize: 4096})
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			conn, acceptErr := pipe.Accept()
			if acceptErr != nil {
				return
			}
			serveFakeSupervisorIPCStatusConn(t, conn, owner, func(req IPCRequest) IPCResponse {
				return IPCResponse{ID: req.ID, OK: true, Result: map[string]any{"state": "running", "daemons": []map[string]any{{
					"task_name": intent.Daemons[0].TaskName, "server": "codegraph", "daemon": "default", "command": imagePath,
					"args": intent.Daemons[0].Args, "port": port, "state": "Running", "current_pid": os.Getpid(),
					"started_at": start.UTC().Format(time.RFC3339Nano), "is_maintenance": false,
				}}}}
			})
			_ = conn.Close()
		}
	}()
	t.Cleanup(func() {
		_ = pipe.Close()
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			t.Error("fake supervisor status pipe did not settle")
		}
	})
	plan, err := a.BuildAttachClientPlan(context.Background(), AttachClientRequest{Server: "codegraph", Daemon: "default", Client: "mimocode"})
	if err != nil || plan == nil {
		t.Fatalf("fault fixture plan refused: plan=%+v err=%v", plan, err)
	}
	return &attachFaultFixture{api: a, plan: plan, selected: selected, selectedBefore: selectedBefore,
		excluded: excluded, stateDir: stateDir, intent: intent}
}

func (f *attachFaultFixture) assertExcludedBytes(t *testing.T) {
	t.Helper()
	for path, prior := range f.excluded {
		actual, err := os.ReadFile(path)
		if err != nil || !bytes.Equal(actual, prior) {
			t.Fatalf("excluded client changed at %s: %v", filepath.Base(path), err)
		}
	}
}

func (f *attachFaultFixture) assertSnapshot(t *testing.T) {
	t.Helper()
	_, snapshotLeaf := attachReceiptLeaves(f.plan)
	pinned, err := readHubMcpStateFile(snapshotLeaf)
	if err != nil || !bytes.Equal(pinned, f.selectedBefore) {
		t.Fatalf("pinned preimage changed or missing: %v", err)
	}
}

func (f *attachFaultFixture) assertNoEvidence(t *testing.T) {
	t.Helper()
	receiptLeaf, snapshotLeaf := attachReceiptLeaves(f.plan)
	if _, err := attachReadReceipt(receiptLeaf); !isHubMcpStateMissingErr(err) {
		t.Fatalf("prewrite failure left receipt: %v", err)
	}
	if _, err := readHubMcpStateFile(snapshotLeaf); !isHubMcpStateMissingErr(err) {
		t.Fatalf("prewrite failure left snapshot: %v", err)
	}
}

func TestAttachExisting_FaultPreparedReceiptWrite(t *testing.T) {
	f := newAttachFaultFixture(t)
	receiptLeaf, _ := attachReceiptLeaves(f.plan)
	fault := errors.New("injected prepared receipt write fault")
	fired := false
	result, err := f.api.applyAttachClientPlan(context.Background(), f.plan, &attachApplyHooks{
		writeReceiptFile: func(leaf string, raw []byte) error {
			if leaf == receiptLeaf && bytes.Contains(raw, []byte(`"state":"prepared"`)) && !fired {
				fired = true
				return fault
			}
			return writeHubMcpStateFile(leaf, raw)
		},
	})
	var failure *AttachClientApplyError
	if !fired || result != nil || !errors.As(err, &failure) || failure.Code != "ATTACH_PREPARE_FAILED" || failure.Invoked || failure.RecoveryRequired || !errors.Is(err, fault) {
		t.Fatalf("prepared write fault escaped prewrite refusal: result=%+v err=%v", result, err)
	}
	actual, readErr := os.ReadFile(f.selected)
	if readErr != nil || !bytes.Equal(actual, f.selectedBefore) {
		t.Fatalf("prepared write fault changed selected bytes: %v", readErr)
	}
	f.assertNoEvidence(t)
	f.assertExcludedBytes(t)
}

func TestAttachExisting_FaultPreparedReceiptReadback(t *testing.T) {
	f := newAttachFaultFixture(t)
	receiptLeaf, _ := attachReceiptLeaves(f.plan)
	fault := errors.New("injected prepared receipt readback fault")
	fired := false
	result, err := f.api.applyAttachClientPlan(context.Background(), f.plan, &attachApplyHooks{
		readReceiptFile: func(leaf string) ([]byte, error) {
			raw, readErr := readHubMcpStateFile(leaf)
			if leaf == receiptLeaf && readErr == nil && !fired {
				fired = true
				return nil, fault
			}
			return raw, readErr
		},
	})
	var failure *AttachClientApplyError
	if !fired || result != nil || !errors.As(err, &failure) || failure.Code != "ATTACH_PREPARE_FAILED" || failure.Invoked || failure.RecoveryRequired || !errors.Is(err, fault) {
		t.Fatalf("prepared readback fault escaped prewrite refusal: result=%+v err=%v", result, err)
	}
	actual, readErr := os.ReadFile(f.selected)
	if readErr != nil || !bytes.Equal(actual, f.selectedBefore) {
		t.Fatalf("prepared readback fault changed selected bytes: %v", readErr)
	}
	f.assertNoEvidence(t)
	f.assertExcludedBytes(t)
}

func TestAttachExisting_FaultPostwritePhysicalReadback(t *testing.T) {
	f := newAttachFaultFixture(t)
	fault := errors.New("injected postwrite physical readback fault")
	selectedReads := 0
	result, err := f.api.applyAttachClientPlan(context.Background(), f.plan, &attachApplyHooks{
		readTarget: func(path string) ([]byte, error) {
			if path == f.selected {
				selectedReads++
				if selectedReads == 2 {
					return nil, fault
				}
			}
			return attachReadRegular(path)
		},
	})
	var failure *AttachClientApplyError
	if selectedReads != 2 || result != nil || !errors.As(err, &failure) || failure.Code != "ATTACH_COMMIT_UNVERIFIED" || !failure.Invoked || !failure.RecoveryRequired || !errors.Is(err, fault) {
		t.Fatalf("postwrite readback fault reported success or lost cause: result=%+v err=%v", result, err)
	}
	actual, readErr := os.ReadFile(f.selected)
	if readErr != nil || bytes.Equal(actual, f.selectedBefore) {
		t.Fatalf("postwrite readback fault lacked selected mutation: %v", readErr)
	}
	client, clientErr := clients.NewMimoCode()
	if clientErr != nil {
		t.Fatal(clientErr)
	}
	entry, entryErr := client.GetEntry(f.plan.EntryKey)
	if entryErr != nil || entry == nil || entry.URL != f.plan.Endpoint {
		t.Fatalf("postwrite physical entry mismatch: entry=%+v err=%v", entry, entryErr)
	}
	receiptLeaf, _ := attachReceiptLeaves(f.plan)
	receipt, receiptErr := attachReadReceipt(receiptLeaf)
	if receiptErr != nil || receipt == nil || receipt.State != "recovery-required" || receipt.BeforeSHA256 != f.plan.TargetBytesSHA256 {
		t.Fatalf("postwrite fault lost recovery receipt: receipt=%+v err=%v", receipt, receiptErr)
	}
	f.assertSnapshot(t)
	f.assertExcludedBytes(t)
}

func TestAttachExisting_FaultConfigReleaseAfterPreparedVeto(t *testing.T) {
	f := newAttachFaultFixture(t)
	fault := errors.New("injected config release confirmation fault")
	fired := false
	physicalLockReleased := false
	result, err := f.api.applyAttachClientPlan(context.Background(), f.plan, &attachApplyHooks{
		afterPrelock: func() {
			f.intent.IntentGeneration++
			if writeErr := WriteSupervisorIntent(filepath.Join(f.stateDir, supervisorIntentFileLeaf), &f.intent); writeErr != nil {
				t.Fatal(writeErr)
			}
		},
		configReleaseFault: func() error {
			fired = true
			probe := flock.New(f.selected + ".lock")
			acquired, probeErr := probe.TryLock()
			if probeErr != nil || !acquired {
				t.Errorf("release downgrade ran before the real config lock settled: acquired=%v err=%v", acquired, probeErr)
			} else {
				physicalLockReleased = true
				if unlockErr := probe.Unlock(); unlockErr != nil {
					t.Errorf("release probe failed to unlock: %v", unlockErr)
				}
			}
			return fault
		},
	})
	var failure *AttachClientApplyError
	if !fired || !physicalLockReleased || result != nil || !errors.As(err, &failure) || failure.Code != "ATTACH_LOCK_RELEASE_UNVERIFIED" || failure.Invoked || !failure.RecoveryRequired || !errors.Is(err, fault) {
		t.Fatalf("config release fault lost prepared recovery: result=%+v err=%v", result, err)
	}
	actual, readErr := os.ReadFile(f.selected)
	if readErr != nil || !bytes.Equal(actual, f.selectedBefore) {
		t.Fatalf("config release fault changed selected bytes: %v", readErr)
	}
	receiptLeaf, _ := attachReceiptLeaves(f.plan)
	receipt, receiptErr := attachReadReceipt(receiptLeaf)
	if receiptErr != nil || receipt == nil || receipt.State != "recovery-required" || receipt.BeforeSHA256 != f.plan.TargetBytesSHA256 {
		t.Fatalf("config release fault lost receipt: receipt=%+v err=%v", receipt, receiptErr)
	}
	f.assertSnapshot(t)
	f.assertExcludedBytes(t)
}

//go:build windows

package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Microsoft/go-winio"
	"github.com/gofrs/flock"
	"mcp-local-hub/internal/clients"
	"mcp-local-hub/internal/process"
)

func TestAttachExisting_OneClientOccupiedPort_ReadOnlyPlan(t *testing.T) {
	runAttachExistingOccupiedPortFixture(t, false)
}

func TestAttachExisting_ChildImageMismatchRefusesOtherwiseHealthyRoute(t *testing.T) {
	runAttachExistingOccupiedPortFixture(t, true)
}

func TestAttachExisting_OneClientOccupiedPort_ApplySelectedOnly(t *testing.T) {
	runAttachExistingOccupiedPortFixtureWithApply(t)
}

func TestRestore_OneClientOccupiedPort_ApplySelectedOnly(t *testing.T) {
	runAttachExistingOccupiedPortFixtureMode(t, false, true, false, "restore")
}

func TestRestore_ConflictingPresentAdoptRecordRefusesBeforeAttach(t *testing.T) {
	runAttachExistingOccupiedPortFixtureMode(t, false, false, false, "restore-conflicting-adopt")
}

func TestRestore_OneClientOccupiedPort_RollbackSelectedOnly(t *testing.T) {
	runAttachExistingOccupiedPortFixtureMode(t, false, true, false, "restore-rollback")
}

func TestRestore_PostPublishReleaseFailureRetainsExactSource(t *testing.T) {
	runAttachExistingOccupiedPortFixtureMode(t, false, true, false, "restore-release-fault")
}

func TestRestore_DriftFenceAfterLeaseReleaseRetainsExactSource(t *testing.T) {
	runAttachExistingOccupiedPortFixtureMode(t, false, true, false, "restore-replan-drift")
}

func TestRestore_DriftFence_PrePublishSourceChange(t *testing.T) {
	runAttachExistingOccupiedPortFixtureMode(t, false, true, false, "restore-prelink-source-drift")
}

func TestRestore_DriftFence_PostPublishSourceOnlyChange(t *testing.T) {
	runAttachExistingOccupiedPortFixtureMode(t, false, true, false, "restore-postlink-source-drift")
}

func TestAttachExisting_EmptyRestoreRecoveryDirectorySettlesBeforeApply(t *testing.T) {
	runAttachExistingOccupiedPortFixtureMode(t, false, true, false, "present-empty-reentry")
}

func TestAttachExisting_CommittedReceiptProducesNoWriteRepeatPlan(t *testing.T) {
	runAttachExistingOccupiedPortFixtureMode(t, false, true, false, "committed-repeat")
}

func TestAttachExisting_FreshCommittedPlanApplyIsNoWrite(t *testing.T) {
	runAttachExistingOccupiedPortFixtureMode(t, false, true, false, "committed-repeat-fresh-apply")
}

func TestAttachExisting_FreshCommittedPlanApplyRefusesDrift(t *testing.T) {
	runAttachExistingOccupiedPortFixtureMode(t, false, true, false, "committed-repeat-fresh-apply-drift")
}

func TestAttachExisting_CommittedRepeatRefusesUnownedRecoveryAndDrift(t *testing.T) {
	runAttachExistingOccupiedPortFixtureMode(t, false, true, false, "committed-repeat-negative")
}

func TestAttachExisting_CommittedApplyRepeatSourceDrift(t *testing.T) {
	runAttachExistingOccupiedPortFixtureMode(t, false, true, false, "committed-apply-repeat-negative")
}

func TestAttachExisting_WriteTimeSymlinkRefusedUnderGlobalOptIn(t *testing.T) {
	runAttachExistingOccupiedPortFixtureMode(t, false, true, true, "")
}

func TestAttachExisting_FinalInLockAdmissionVetoesIntentDrift(t *testing.T) {
	runAttachExistingOccupiedPortFixtureMode(t, false, true, false, "drift")
}

func TestAttachExisting_ConfigLockWaitVetoesSelectedSourceDrift(t *testing.T) {
	runAttachExistingOccupiedPortFixtureMode(t, false, true, false, "source-drift")
}

func TestAttachExisting_CancelledConfigLockWaitHasNoBodyOrReceipt(t *testing.T) {
	runAttachExistingOccupiedPortFixtureMode(t, false, true, false, "cancel")
}

func TestAttachExisting_LeaseReleaseFailureAfterPreparedVetoRetainsReceipt(t *testing.T) {
	runAttachExistingOccupiedPortFixtureMode(t, false, true, false, "lease-release-veto")
}

func TestAttachExisting_ConfigLockWaitExpiresAtTenSeconds(t *testing.T) {
	runAttachExistingOccupiedPortFixtureMode(t, false, true, false, "deadline")
}

func TestAttachExisting_CancelDuringSlowSessionCleanupVetoesWrite(t *testing.T) {
	runAttachExistingOccupiedPortFixtureMode(t, false, true, false, "cleanup-cancel")
}

func TestAttachExisting_PostwriteGenerationDriftRetainsRecoveryReceipt(t *testing.T) {
	runAttachExistingOccupiedPortFixtureMode(t, false, true, false, "postwrite-drift")
}

func TestAttachExisting_LeaseReleaseFailureAfterWriteRetainsRecoveryReceipt(t *testing.T) {
	runAttachExistingOccupiedPortFixtureMode(t, false, true, false, "lease-release-postwrite")
}

func TestAttachExisting_ForegroundAdmissionExpiresAtFiveSeconds(t *testing.T) {
	runAttachExistingOccupiedPortFixtureMode(t, false, true, false, "foreground-timeout")
}

func TestAttachExisting_RegularTargetSucceedsWithGlobalSymlinkOptIn(t *testing.T) {
	runAttachExistingOccupiedPortFixtureMode(t, false, true, false, "regular-opt-in")
}

func TestAttachExisting_OneClientOccupiedPort_RollbackSelectedOnly(t *testing.T) {
	runAttachExistingOccupiedPortFixtureMode(t, false, true, false, "rollback")
}

func TestAttachExisting_RollbackPreviewCommittedReceiptIsReadOnly(t *testing.T) {
	runAttachExistingOccupiedPortFixtureMode(t, false, true, false, "rollback-preview")
}

func TestAttachExisting_RollbackPreviewRolledBackReceiptIsReadOnly(t *testing.T) {
	runAttachExistingOccupiedPortFixtureMode(t, false, true, false, "rollback-preview-rolled-back")
}

func TestAttachExisting_RollbackConflictRetainsRecovery(t *testing.T) {
	runAttachExistingOccupiedPortFixtureMode(t, false, true, false, "rollback-conflict")
}

func TestAttachExisting_RollbackSymlinkRefusesForeignTarget(t *testing.T) {
	runAttachExistingOccupiedPortFixtureMode(t, false, true, false, "rollback-symlink")
}

func TestAttachExisting_CompetingReceiptStatesRefuse(t *testing.T) {
	runAttachExistingOccupiedPortFixtureMode(t, false, true, false, "rollback-states")
}

func TestAttachExisting_NewOperationAfterRollbackPreservesOldReceipt(t *testing.T) {
	runAttachExistingOccupiedPortFixtureMode(t, false, true, false, "rollback-history")
}

func TestAttachExisting_ArchiveFailurePreservesCurrentRolledBackReceipt(t *testing.T) {
	runAttachExistingOccupiedPortFixtureMode(t, false, true, false, "rollback-archive-fail")
}

func TestAttachExisting_RollbackCASRacePreservesChangedTarget(t *testing.T) {
	runAttachExistingOccupiedPortFixtureMode(t, false, true, false, "rollback-cas-race")
}

func TestAttachExisting_UnrelatedClientMigrationRemainsAvailable(t *testing.T) {
	runAttachExistingOccupiedPortFixtureMode(t, false, true, false, "rollback-unrelated")
}

func runAttachExistingOccupiedPortFixtureWithApply(t *testing.T) {
	t.Helper()
	runAttachExistingOccupiedPortFixtureMode(t, false, true, false, "")
}

func runAttachExistingOccupiedPortFixture(t *testing.T, wrongImage bool) {
	runAttachExistingOccupiedPortFixtureMode(t, wrongImage, false, false, "")
}

func runAttachExistingOccupiedPortFixtureMode(t *testing.T, wrongImage, apply, swapToSymlink bool, lockScenario string) {
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
	if strings.HasPrefix(lockScenario, "rollback") || lockScenario == "restore-rollback" {
		selectedBefore = []byte(`{"mcp":{"codegraph":{"type":"local","command":["node","server.js"],"enabled":true},"sibling":{"type":"local","command":["node","sibling.js"],"enabled":true}}}`)
	}
	if err := os.WriteFile(selected, selectedBefore, 0600); err != nil {
		t.Fatal(err)
	}
	var stallDelete atomic.Bool
	deleteStarted := make(chan struct{}, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/mcp" {
			http.NotFound(w, r)
			return
		}
		if r.Method == http.MethodDelete {
			if stallDelete.Load() {
				select {
				case deleteStarted <- struct{}{}:
				default:
				}
				<-r.Context().Done()
				return
			}
			w.WriteHeader(http.StatusNoContent)
			return
		}
		body := make([]byte, 4096)
		n, _ := r.Body.Read(body)
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Mcp-Session-Id", "attach-test-session")
		switch {
		case strings.Contains(string(body[:n]), `"initialize"`):
			_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":{"protocolVersion":"2025-03-26","capabilities":{}}}`))
		case strings.Contains(string(body[:n]), `"tools/list"`):
			_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":2,"result":{"tools":[{"name":"probe"}]}}`))
		default:
			http.Error(w, "unexpected", http.StatusBadRequest)
		}
	}))
	defer srv.Close()
	port := parsePort(t, srv.URL)
	codexHome := filepath.Join(root, "codex")
	if err := os.Mkdir(codexHome, 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CODEX_HOME", codexHome)
	alreadyHTTP := filepath.Join(codexHome, "config.toml")
	excluded := filepath.Join(root, ".claude.json")
	if err := os.WriteFile(alreadyHTTP, []byte("[mcp_servers.codegraph]\nurl = \"http://127.0.0.1:"+strconv.Itoa(port)+"/mcp\"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(excluded, []byte(`{"mcpServers":{"codegraph":{"type":"stdio","command":"node","args":["excluded.js"]}}}`), 0600); err != nil {
		t.Fatal(err)
	}
	codexClient, err := clients.NewCodexCLI()
	if err != nil || codexClient.ConfigPath() != alreadyHTTP {
		t.Fatalf("Codex fixture path: client=%v err=%v", codexClient, err)
	}
	httpEntry, err := codexClient.GetEntry("codegraph")
	if err != nil || httpEntry == nil || httpEntry.URL != "http://127.0.0.1:"+strconv.Itoa(port)+"/mcp" {
		t.Fatalf("Codex fixture not HTTP: entry=%+v err=%v", httpEntry, err)
	}
	claudeClient, err := clients.NewClaudeCode()
	if err != nil || claudeClient.ConfigPath() != excluded {
		t.Fatalf("Claude fixture path: client=%v err=%v", claudeClient, err)
	}
	directEntry, err := claudeClient.GetEntry("codegraph")
	if err != nil || directEntry == nil || directEntry.URL != "" {
		t.Fatalf("Claude fixture not direct: entry=%+v err=%v", directEntry, err)
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
	if wrongImage {
		imagePath = filepath.Join(root, "foreign-mcphub.exe")
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
	var stallFinalStatus atomic.Bool
	go func() {
		defer close(done)
		for {
			conn, err := pipe.Accept()
			if err != nil {
				return
			}
			serveFakeSupervisorIPCStatusConn(t, conn, owner, func(req IPCRequest) IPCResponse {
				if stallFinalStatus.Swap(false) {
					time.Sleep(6 * time.Second)
				}
				return IPCResponse{ID: req.ID, OK: true, Result: map[string]any{"state": "running", "daemons": []map[string]any{{
					"task_name": intent.Daemons[0].TaskName, "server": "codegraph", "daemon": "default", "command": imagePath,
					"args": intent.Daemons[0].Args, "port": port, "state": "Running", "current_pid": os.Getpid(),
					"started_at": start.UTC().Format(time.RFC3339Nano), "is_maintenance": false,
				}}}}
			})
			_ = conn.Close()
		}
	}()
	defer func() {
		_ = pipe.Close()
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			t.Error("fake status pipe did not settle")
		}
	}()
	manifestPath := filepath.Join(manifestDir, "codegraph", "manifest.yaml")
	if lockScenario == "present-empty-reentry" {
		if err := os.Mkdir(filepath.Join(manifestDir, "codegraph", ".manifest-recovery"), 0700); err != nil {
			t.Fatal(err)
		}
	}
	var manifestSource string
	if strings.HasPrefix(lockScenario, "restore") {
		manifestSource = filepath.Join(stateDir, "restore-source.yaml")
		if err := WriteStateFileBytesAtomic(manifestSource, []byte(raw)); err != nil {
			t.Fatal(err)
		}
		if err := os.Remove(manifestPath); err != nil {
			t.Fatal(err)
		}
	}
	if lockScenario == "restore-conflicting-adopt" {
		wrongHash := ManifestHashContent([]byte(raw + "\n"))
		rec := AdoptProvenanceRecord{ManifestName: "codegraph", SourceClient: "codex-cli", SourceEntryName: "codegraph",
			Port: port, AdoptClients: []string{"codex-cli"}, OperationState: AdoptOperationStateAdopted,
			AdoptManifestHash: wrongHash, ExpectedManifestHash: wrongHash}
		if err := writeAdoptedEntries(&AdoptedEntries{Records: []AdoptProvenanceRecord{rec}}); err != nil {
			t.Fatal(err)
		}
	}
	before := map[string][]byte{}
	paths := []string{selected, alreadyHTTP, excluded, filepath.Join(stateDir, supervisorIntentFileLeaf), filepath.Join(stateDir, "supervisor.lock.owner.json")}
	if !strings.HasPrefix(lockScenario, "restore") {
		paths = append(paths, manifestPath)
	}
	for _, p := range paths {
		before[p], err = os.ReadFile(p)
		if err != nil {
			t.Fatal(err)
		}
	}
	plan, err := a.BuildAttachClientPlan(context.Background(), AttachClientRequest{Server: "codegraph", Daemon: "default", Client: "mimocode", ManifestSource: manifestSource})
	if lockScenario == "restore-conflicting-adopt" {
		var refusal *AttachPlanError
		if plan != nil || !errors.As(err, &refusal) || refusal.Code != "adopt-provenance-conflict" {
			t.Fatalf("conflicting present adopt record admitted: plan=%+v err=%v", plan, err)
		}
		for p, prior := range before {
			after, readErr := os.ReadFile(p)
			if readErr != nil || string(after) != string(prior) {
				t.Fatalf("conflicting provenance preview changed %s: %v", filepath.Base(p), readErr)
			}
		}
		if _, err := os.Lstat(manifestPath); !os.IsNotExist(err) {
			t.Fatalf("conflicting provenance published manifest: %v", err)
		}
		return
	}
	if wrongImage {
		var refusal *AttachPlanError
		if plan != nil || !errors.As(err, &refusal) || refusal.Code != "child-identity-mismatch" {
			t.Fatalf("wrong child image admitted: plan=%+v err=%v", plan, err)
		}
	} else if err != nil || plan == nil {
		t.Fatalf("positive occupied-port plan refused: plan=%+v err=%v port=%s", plan, err, strconv.Itoa(port))
	}
	if !wrongImage {
		if strings.HasPrefix(lockScenario, "restore") && plan.State != "would-restore-and-attach" {
			t.Fatalf("missing-manifest preview did not expose restore stage: %+v", plan)
		}
		if plan.Endpoint != "http://127.0.0.1:"+strconv.Itoa(port)+"/mcp" || plan.BeforeTransport != "direct" || plan.AfterTransport != "http" {
			t.Fatalf("wrong plan: %+v", plan)
		}
		serialized, err := json.Marshal(plan)
		if err != nil {
			t.Fatal(err)
		}
		for _, secret := range []string{root, imagePath, "server.js", "excluded.js", "foreign-mcphub.exe"} {
			if strings.Contains(string(serialized), secret) {
				t.Fatalf("plan leaked private input: %s", serialized)
			}
		}
		if !strings.Contains(string(serialized), `"entry_key":"codegraph"`) || !strings.Contains(string(serialized), `"before_transport":"direct"`) || !strings.Contains(string(serialized), `"after_transport":"http"`) {
			t.Fatalf("redacted single-client diff missing: %s", serialized)
		}
	}
	for p, prior := range before {
		after, err := os.ReadFile(p)
		if err != nil {
			t.Fatal(err)
		}
		if string(after) != string(prior) {
			t.Fatalf("dry-run changed %s", filepath.Base(p))
		}
	}
	if pid, found, err := LoopbackPortOwnerPIDContext(context.Background(), port); err != nil || !found || pid != os.Getpid() {
		t.Fatalf("listener changed: pid=%d found=%v err=%v", pid, found, err)
	}
	if !apply {
		return
	}
	if lockScenario == "foreground-timeout" {
		var foregroundStart time.Time
		result, err := a.applyAttachClientPlan(context.Background(), plan, &attachApplyHooks{afterPrelock: func() {
			foregroundStart = time.Now()
			stallFinalStatus.Store(true)
		}})
		elapsed := time.Since(foregroundStart)
		t.Logf("foreground final admission settled in %s; result=%v err=%v", elapsed, result, err)
		var failure *AttachClientApplyError
		if result != nil || !errors.As(err, &failure) || failure.Code != "ATTACH_FINAL_ADMISSION_DRIFT" || failure.Invoked || failure.RecoveryRequired || elapsed < 4700*time.Millisecond || elapsed > 6800*time.Millisecond {
			t.Fatalf("foreground timeout did not veto after five seconds: result=%+v err=%v elapsed=%s", result, err, elapsed)
		}
		current, readErr := os.ReadFile(selected)
		if readErr != nil || string(current) != string(selectedBefore) {
			t.Fatalf("foreground timeout changed selected bytes: err=%v", readErr)
		}
		receiptLeaf, _ := attachReceiptLeaves(plan)
		if _, readErr := attachReadReceipt(receiptLeaf); !isHubMcpStateMissingErr(readErr) {
			t.Fatalf("confirmed foreground veto left a receipt: %v", readErr)
		}
		return
	}
	if lockScenario == "cleanup-cancel" {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		type outcome struct {
			result *AttachClientApplyResult
			err    error
		}
		outcomes := make(chan outcome, 1)
		go func() {
			result, err := a.applyAttachClientPlan(ctx, plan, &attachApplyHooks{afterPrelock: func() { stallDelete.Store(true) }})
			outcomes <- outcome{result, err}
		}()
		select {
		case <-deleteStarted:
		case <-time.After(6 * time.Second):
			t.Fatal("final-admission session DELETE did not start")
		}
		cleanupStart := time.Now()
		cancel()
		var got outcome
		select {
		case got = <-outcomes:
		case <-time.After(5 * time.Second):
			t.Fatal("cancelled session cleanup did not settle within its allowance")
		}
		elapsed := time.Since(cleanupStart)
		t.Logf("[ATTACH-QA %s] canceled cleanup settled in %s; result=%v err=%v", time.Now().UTC().Format(time.RFC3339Nano), elapsed, got.result, got.err)
		var failure *AttachClientApplyError
		if got.result != nil || !errors.As(got.err, &failure) || failure.Code != "ATTACH_PREWRITE_CANCELLED" || failure.Invoked || failure.RecoveryRequired || elapsed < 2500*time.Millisecond || elapsed > 4500*time.Millisecond {
			t.Fatalf("cancelled DELETE must be awaited then veto the write: result=%+v err=%v elapsed=%s", got.result, got.err, elapsed)
		}
		current, readErr := os.ReadFile(selected)
		if readErr != nil || string(current) != string(selectedBefore) {
			t.Fatalf("cancelled cleanup changed selected bytes: err=%v", readErr)
		}
		receiptLeaf, _ := attachReceiptLeaves(plan)
		if _, readErr := attachReadReceipt(receiptLeaf); !isHubMcpStateMissingErr(readErr) {
			t.Fatalf("confirmed cleanup veto left a receipt: %v", readErr)
		}
		return
	}
	if lockScenario != "" && lockScenario != "present-empty-reentry" && !strings.HasPrefix(lockScenario, "restore") && lockScenario != "regular-opt-in" && lockScenario != "committed-apply-repeat-negative" && !strings.HasPrefix(lockScenario, "committed-repeat") && !strings.HasPrefix(lockScenario, "rollback") {
		if lockScenario == "postwrite-drift" || lockScenario == "lease-release-postwrite" {
			if lockScenario == "lease-release-postwrite" {
				previous := adoptLeaseBeforeSettlementHook
				adoptLeaseBeforeSettlementHook = func() error { return errors.New("injected postwrite lease release uncertainty") }
				t.Cleanup(func() { adoptLeaseBeforeSettlementHook = previous })
			}
			var hook *attachApplyHooks
			if lockScenario == "postwrite-drift" {
				hook = &attachApplyHooks{beforeWrite: func() {
					intent.IntentGeneration++
					if err := WriteSupervisorIntent(filepath.Join(stateDir, supervisorIntentFileLeaf), &intent); err != nil {
						t.Fatal(err)
					}
				}}
			}
			result, err := a.applyAttachClientPlan(context.Background(), plan, hook)
			var failure *AttachClientApplyError
			if result != nil || !errors.As(err, &failure) || failure.Code != "ATTACH_COMMIT_UNVERIFIED" || !failure.Invoked || !failure.RecoveryRequired {
				t.Fatalf("postwrite uncertainty returned success or wrong failure: result=%+v err=%v", result, err)
			}
			current, readErr := os.ReadFile(selected)
			if readErr != nil || string(current) == string(selectedBefore) {
				t.Fatalf("postwrite uncertainty did not establish selected write: err=%v", readErr)
			}
			receiptLeaf, snapshotLeaf := attachReceiptLeaves(plan)
			receipt, readErr := attachReadReceipt(receiptLeaf)
			if readErr != nil || receipt == nil || receipt.State != "recovery-required" || receipt.BeforeSHA256 != plan.TargetBytesSHA256 {
				t.Fatalf("postwrite uncertainty lost recovery receipt: receipt=%+v err=%v", receipt, readErr)
			}
			pinned, readErr := readHubMcpStateFile(snapshotLeaf)
			if readErr != nil || string(pinned) != string(selectedBefore) {
				t.Fatalf("postwrite uncertainty lost pinned preimage: err=%v", readErr)
			}
			for p, prior := range before {
				if p == selected || (lockScenario == "postwrite-drift" && p == filepath.Join(stateDir, supervisorIntentFileLeaf)) {
					continue
				}
				current, readErr := os.ReadFile(p)
				if readErr != nil || string(current) != string(prior) {
					t.Fatalf("postwrite uncertainty changed excluded input %s: err=%v", filepath.Base(p), readErr)
				}
			}
			return
		}
		if lockScenario == "lease-release-veto" {
			previous := adoptLeaseBeforeSettlementHook
			adoptLeaseBeforeSettlementHook = func() error { return errors.New("injected lease release uncertainty") }
			t.Cleanup(func() { adoptLeaseBeforeSettlementHook = previous })
		}
		held := flock.New(selected + ".lock")
		if err := held.Lock(); err != nil {
			t.Fatal(err)
		}
		defer func() { _ = held.Unlock() }()
		prelockPassed := make(chan struct{})
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		type outcome struct {
			result *AttachClientApplyResult
			err    error
		}
		outcomes := make(chan outcome, 1)
		go func() {
			result, err := a.applyAttachClientPlan(ctx, plan, &attachApplyHooks{afterPrelock: func() { close(prelockPassed) }})
			outcomes <- outcome{result, err}
		}()
		select {
		case <-prelockPassed:
		case <-time.After(5 * time.Second):
			t.Fatal("pre-lock F1 did not finish")
		}
		waitStart := time.Now()
		sourceDrift := []byte(`{"mcp":{"codegraph":{"type":"local","command":["node","changed.js"],"enabled":true}}}`)
		if lockScenario == "drift" || lockScenario == "lease-release-veto" {
			intent.IntentGeneration++
			if err := WriteSupervisorIntent(filepath.Join(stateDir, supervisorIntentFileLeaf), &intent); err != nil {
				t.Fatal(err)
			}
		} else if lockScenario == "cancel" {
			time.Sleep(75 * time.Millisecond)
			cancel()
		} else if lockScenario == "source-drift" {
			if err := os.WriteFile(selected, sourceDrift, 0600); err != nil {
				t.Fatal(err)
			}
		}
		if lockScenario != "deadline" {
			if err := held.Unlock(); err != nil {
				t.Fatal(err)
			}
		}
		var got outcome
		select {
		case got = <-outcomes:
		case <-time.After(13 * time.Second):
			t.Fatal("bounded config-lock path did not settle")
		}
		if lockScenario == "deadline" {
			elapsed := time.Since(waitStart)
			t.Logf("[ATTACH-QA %s] config-lock deadline settled in %s; result=%v err=%v", time.Now().UTC().Format(time.RFC3339Nano), elapsed, got.result, got.err)
			if elapsed < 9500*time.Millisecond || elapsed > 12*time.Second {
				t.Fatalf("config-lock wait missed ten-second cap: %s", elapsed)
			}
			if err := held.Unlock(); err != nil {
				t.Fatal(err)
			}
		}
		if lockScenario == "lease-release-veto" {
			t.Logf("[ATTACH-QA %s] lease fault result=%v err=%v", time.Now().UTC().Format(time.RFC3339Nano), got.result, got.err)
			if got.result != nil || got.err == nil || !strings.Contains(got.err.Error(), "ATTACH_LEASE_RELEASE_UNVERIFIED") {
				t.Fatalf("lease release uncertainty was not returned: result=%+v err=%v", got.result, got.err)
			}
			current, readErr := os.ReadFile(selected)
			if readErr != nil || string(current) != string(selectedBefore) {
				t.Fatalf("lease release uncertainty changed selected bytes: err=%v", readErr)
			}
			receiptLeaf, _ := attachReceiptLeaves(plan)
			receipt, receiptErr := attachReadReceipt(receiptLeaf)
			t.Logf("[ATTACH-QA %s] receipt after uncertain release state=%v err=%v", time.Now().UTC().Format(time.RFC3339Nano), func() string {
				if receipt == nil {
					return "<nil>"
				}
				return receipt.State
			}(), receiptErr)
			if receiptErr != nil || receipt == nil || receipt.State != "recovery-required" || receipt.BeforeSHA256 != plan.TargetBytesSHA256 {
				t.Fatalf("uncertain lease release lost prepared recovery evidence: receipt=%+v err=%v", receipt, receiptErr)
			}
			return
		}
		var failure *AttachClientApplyError
		if got.result != nil || !errors.As(got.err, &failure) || failure.Invoked || failure.RecoveryRequired || ((lockScenario == "cancel" || lockScenario == "deadline") && failure.Code != "ATTACH_LOCK_WAIT_TIMEOUT") || (lockScenario == "drift" && failure.Code != "ATTACH_FINAL_ADMISSION_DRIFT") || (lockScenario == "source-drift" && failure.Code != "ATTACH_SOURCE_DRIFT") {
			t.Fatalf("lock scenario %s did not veto before write: result=%+v err=%v", lockScenario, got.result, got.err)
		}
		current, err := os.ReadFile(selected)
		wantSelected := selectedBefore
		if lockScenario == "source-drift" {
			wantSelected = sourceDrift
		}
		if err != nil || string(current) != string(wantSelected) {
			t.Fatalf("lock scenario changed selected bytes: err=%v", err)
		}
		receiptLeaf, _ := attachReceiptLeaves(plan)
		if _, err := attachReadReceipt(receiptLeaf); !isHubMcpStateMissingErr(err) {
			t.Fatalf("lock scenario left active receipt: %v", err)
		}
		return
	}
	var beforeWrite func()
	if lockScenario == "regular-opt-in" {
		t.Setenv("MCPHUB_ALLOW_CLIENT_CONFIG_SYMLINK", "1")
		priorConsent := InteractiveSymlinkConsent
		consentCalls := 0
		InteractiveSymlinkConsent = func(string, string, string) bool {
			consentCalls++
			return true
		}
		t.Cleanup(func() { InteractiveSymlinkConsent = priorConsent })
		defer func() {
			if consentCalls != 0 {
				t.Errorf("regular target unexpectedly sought symlink consent %d times", consentCalls)
			}
		}()
	}
	if swapToSymlink {
		t.Setenv("MCPHUB_ALLOW_CLIENT_CONFIG_SYMLINK", "1")
		foreign := filepath.Join(root, "foreign-mimocode.json")
		if err := os.WriteFile(foreign, selectedBefore, 0600); err != nil {
			t.Fatal(err)
		}
		probe := filepath.Join(root, "symlink-probe")
		if err := os.Symlink(foreign, probe); err != nil {
			t.Skipf("test host cannot create file symlink: %v", err)
		}
		if err := os.Remove(probe); err != nil {
			t.Fatal(err)
		}
		beforeWrite = func() {
			parked := selected + ".parked"
			if err := os.Rename(selected, parked); err != nil {
				t.Errorf("park target before write: %v", err)
				return
			}
			if err := os.Symlink(foreign, selected); err != nil {
				t.Errorf("swap target to symlink: %v", err)
				_ = os.Rename(parked, selected)
			}
		}
		result, err := a.applyAttachClientPlan(context.Background(), plan, &attachApplyHooks{beforeWrite: beforeWrite})
		var failure *AttachClientApplyError
		if result != nil || !errors.As(err, &failure) || failure.Code != "ATTACH_TARGET_SYMLINK_REFUSED" || !failure.Invoked || !failure.RecoveryRequired {
			t.Fatalf("write-time symlink did not fail closed: result=%+v err=%v", result, err)
		}
		foreignAfter, err := os.ReadFile(foreign)
		if err != nil || string(foreignAfter) != string(selectedBefore) {
			t.Fatalf("foreign same-preimage file changed: err=%v", err)
		}
		receiptLeaf, _ := attachReceiptLeaves(plan)
		receipt, err := attachReadReceipt(receiptLeaf)
		if err != nil || receipt.State != "recovery-required" || receipt.BeforeSHA256 != plan.TargetBytesSHA256 {
			t.Fatalf("symlink refusal lost private recovery receipt: receipt=%+v err=%v", receipt, err)
		}
		return
	}
	if lockScenario == "restore-release-fault" {
		prior := adoptLeaseUnlockFailureHook
		adoptLeaseUnlockFailureHook = func() error { return errors.New("injected restore lease release uncertainty") }
		t.Cleanup(func() { adoptLeaseUnlockFailureHook = prior })
	}
	var result *AttachClientApplyResult
	if lockScenario == "restore-prelink-source-drift" {
		result, err = a.applyRestoreThenAttachClientPlanWithHooks(context.Background(), plan, &attachRestoreHooks{beforeLink: func() {
			if writeErr := WriteStateFileBytesAtomic(manifestSource, []byte(raw+"\n")); writeErr != nil {
				t.Error(writeErr)
			}
		}})
	} else if lockScenario == "restore-postlink-source-drift" {
		result, err = a.applyRestoreThenAttachClientPlanWithHooks(context.Background(), plan, &attachRestoreHooks{afterRelease: func() {
			if writeErr := WriteStateFileBytesAtomic(manifestSource, []byte(raw+"\n")); writeErr != nil {
				t.Error(writeErr)
			}
		}})
	} else if lockScenario == "restore-replan-drift" {
		result, err = a.applyRestoreThenAttachClientPlanWithHooks(context.Background(), plan, &attachRestoreHooks{afterRelease: func() {
			intent.IntentGeneration++
			if writeErr := WriteSupervisorIntent(filepath.Join(stateDir, supervisorIntentFileLeaf), &intent); writeErr != nil {
				t.Error(writeErr)
			}
		}})
	} else {
		result, err = a.ApplyAttachClientPlan(context.Background(), plan)
	}
	if lockScenario == "restore-prelink-source-drift" {
		var failure *AttachClientApplyError
		selectedAfter, selectedErr := os.ReadFile(selected)
		_, manifestErr := os.Lstat(manifestPath)
		_, recoveryErr := os.Lstat(filepath.Join(manifestDir, "codegraph", ".manifest-recovery"))
		if result != nil || !errors.As(err, &failure) || failure.Code != "ATTACH_MANIFEST_SOURCE_DRIFT" ||
			!os.IsNotExist(manifestErr) || !os.IsNotExist(recoveryErr) || selectedErr != nil || string(selectedAfter) != string(selectedBefore) {
			t.Fatalf("pre-Link source drift published or attached: result=%+v err=%v manifest=%v recovery=%v selected=%v", result, err, manifestErr, recoveryErr, selectedErr)
		}
		return
	}
	if lockScenario == "restore-replan-drift" {
		manifestAfter, readErr := os.ReadFile(manifestPath)
		selectedAfter, selectedErr := os.ReadFile(selected)
		var failure *AttachClientApplyError
		if result != nil || !errors.As(err, &failure) || failure.Code != "ATTACH_MANIFEST_RESTORED_CLIENT_NOT_ATTACHED" ||
			readErr != nil || string(manifestAfter) != raw || selectedErr != nil || string(selectedAfter) != string(selectedBefore) {
			t.Fatalf("interstage drift was not fenced: result=%+v err=%v read=%v selected=%v", result, err, readErr, selectedErr)
		}
		return
	}
	if lockScenario == "restore-release-fault" {
		manifestAfter, readErr := os.ReadFile(manifestPath)
		selectedAfter, selectedErr := os.ReadFile(selected)
		var failure *AttachClientApplyError
		if result != nil || !errors.As(err, &failure) || failure.Code != "ATTACH_MANIFEST_RESTORE_UNVERIFIED" ||
			!failure.RecoveryRequired || readErr != nil || string(manifestAfter) != raw || selectedErr != nil || string(selectedAfter) != string(selectedBefore) {
			t.Fatalf("release uncertainty did not retain source and direct client: result=%+v err=%v read=%v selected=%v", result, err, readErr, selectedErr)
		}
		return
	}
	if lockScenario == "restore" || lockScenario == "restore-postlink-source-drift" || lockScenario == "restore-rollback" {
		manifestAfter, readErr := os.ReadFile(manifestPath)
		if readErr != nil || string(manifestAfter) != raw || result == nil || !result.ManifestRestored {
			t.Fatalf("restore did not retain exact manifest: result=%+v err=%v read=%v", result, err, readErr)
		}
		store, storeErr := readAdoptedEntries()
		if storeErr != nil || store == nil || len(store.Records) != 0 {
			t.Fatalf("ordinary intent restore created adopt provenance: store=%+v err=%v", store, storeErr)
		}
	}
	if err != nil || result == nil || result.State != "committed" || result.IdempotentNoWrite || result.ReceiptID == "" || result.EntryKey != "codegraph" || result.Endpoint != srv.URL+"/mcp" || result.BeforeSHA256 == "" || result.AfterSHA256 == "" || result.BeforeSHA256 == result.AfterSHA256 {
		var leaseFailure *LeaseFailure
		_ = errors.As(err, &leaseFailure)
		if leaseFailure != nil {
			t.Fatalf("selected apply lacks committed receipt: result=%+v err=%v lease_reason=%s lease_category=%s lease_cause=%v", result, err, leaseFailure.ReasonID, leaseFailure.Category, errors.Unwrap(leaseFailure))
		}
		t.Fatalf("selected apply lacks committed receipt: result=%+v err=%v cause=%v", result, err, errors.Unwrap(err))
	}
	if lockScenario == "present-empty-reentry" {
		if _, err := os.Lstat(filepath.Join(manifestDir, "codegraph", ".manifest-recovery")); !os.IsNotExist(err) {
			t.Fatalf("present attach retained empty owned recovery directory: %v", err)
		}
	}
	receiptLeaf, snapshotLeaf := attachReceiptLeaves(plan)
	receipt, err := attachReadReceipt(receiptLeaf)
	if err != nil || receipt.ID != result.ReceiptID || receipt.State != "committed" || receipt.AfterTokenSHA256 == "" {
		t.Fatalf("committed receipt lacks exact selected postimage: receipt=%+v err=%v", receipt, err)
	}
	pinned, err := readHubMcpStateFile(snapshotLeaf)
	if err != nil || string(pinned) != string(selectedBefore) {
		t.Fatalf("private pinned preimage not exact: err=%v", err)
	}
	selectedClient, err := clients.NewMimoCode()
	if err != nil {
		t.Fatal(err)
	}
	entry, err := selectedClient.GetEntry("codegraph")
	if err != nil || entry == nil || entry.URL != srv.URL+"/mcp" {
		t.Fatalf("selected MiMoCode entry did not become HTTP /mcp: entry=%+v err=%v", entry, err)
	}
	selectedAfter, err := os.ReadFile(selected)
	if err != nil || string(selectedAfter) == string(selectedBefore) {
		t.Fatalf("selected physical bytes did not change: err=%v", err)
	}
	if lockScenario == "rollback-preview" {
		request := AttachClientRequest{Server: "codegraph", Daemon: "default", Client: "mimocode"}
		receiptBefore, err := readHubMcpStateFile(receiptLeaf)
		if err != nil {
			t.Fatal(err)
		}
		snapshotBefore, err := readHubMcpStateFile(snapshotLeaf)
		if err != nil {
			t.Fatal(err)
		}
		preview, err := NewAPI().BuildAttachClientRollbackPlan(context.Background(), request, result.ReceiptID)
		if err != nil || preview == nil || preview.State != "would-rollback" || preview.ReceiptID != result.ReceiptID ||
			preview.BeforeTransport != "http" || preview.AfterTransport != "direct" ||
			preview.TargetBytesSHA256 != receipt.AfterSHA256 || preview.EntryTokenSHA256 != receipt.AfterTokenSHA256 {
			t.Fatalf("committed rollback preview not exact: preview=%+v err=%v", preview, err)
		}
		selectedAgain, selectedErr := os.ReadFile(selected)
		receiptAgain, receiptErr := readHubMcpStateFile(receiptLeaf)
		snapshotAgain, snapshotErr := readHubMcpStateFile(snapshotLeaf)
		if selectedErr != nil || receiptErr != nil || snapshotErr != nil || !reflect.DeepEqual(selectedAgain, selectedAfter) ||
			!reflect.DeepEqual(receiptAgain, receiptBefore) || !reflect.DeepEqual(snapshotAgain, snapshotBefore) {
			t.Fatalf("rollback preview changed selected or evidence: selected=%v receipt=%v snapshot=%v", selectedErr, receiptErr, snapshotErr)
		}
		for p, prior := range before {
			if p == selected {
				continue
			}
			actual, readErr := os.ReadFile(p)
			if readErr != nil || !reflect.DeepEqual(actual, prior) {
				t.Fatalf("rollback preview changed excluded or daemon source %s: %v", filepath.Base(p), readErr)
			}
		}
		if pid, found, ownerErr := LoopbackPortOwnerPIDContext(context.Background(), port); ownerErr != nil || !found || pid != os.Getpid() {
			t.Fatalf("rollback preview changed listener: pid=%d found=%v err=%v", pid, found, ownerErr)
		}
		serialized, err := json.Marshal(preview)
		if err != nil {
			t.Fatal(err)
		}
		for _, secret := range []string{root, imagePath, "server.js", "sibling.js", "excluded.js"} {
			if strings.Contains(string(serialized), secret) {
				t.Fatalf("rollback preview leaked private input: %s", serialized)
			}
		}
		assertRefusal := func(id, want string) {
			t.Helper()
			selectedBeforeCall, selectedErr := os.ReadFile(selected)
			receiptBeforeCall, receiptErr := readHubMcpStateFile(receiptLeaf)
			snapshotBeforeCall, snapshotErr := readHubMcpStateFile(snapshotLeaf)
			if selectedErr != nil || receiptErr != nil || snapshotErr != nil {
				t.Fatal(selectedErr, receiptErr, snapshotErr)
			}
			got, planErr := NewAPI().BuildAttachClientRollbackPlan(context.Background(), request, id)
			var refusal *AttachPlanError
			if got != nil || !errors.As(planErr, &refusal) || refusal.Code != want {
				t.Fatalf("rollback preview refusal %q: plan=%+v err=%v", want, got, planErr)
			}
			selectedAgain, selectedErr := os.ReadFile(selected)
			receiptAgain, receiptErr := readHubMcpStateFile(receiptLeaf)
			snapshotAgain, snapshotErr := readHubMcpStateFile(snapshotLeaf)
			if selectedErr != nil || receiptErr != nil || snapshotErr != nil || !reflect.DeepEqual(selectedAgain, selectedBeforeCall) ||
				!reflect.DeepEqual(receiptAgain, receiptBeforeCall) || !reflect.DeepEqual(snapshotAgain, snapshotBeforeCall) {
				t.Fatalf("rollback preview refusal mutated selected or evidence: selected=%v receipt=%v snapshot=%v", selectedErr, receiptErr, snapshotErr)
			}
		}
		assertRefusal("", "rollback-invalid")
		assertRefusal("old-receipt-id", "rollback-receipt-unverified")
		higher := filepath.Join(configDir, "mimocode.jsonc")
		higherBytes := []byte(`{"mcp":{"codegraph":{"type":"remote","url":` + strconv.Quote(srv.URL+"/mcp") + `,"enabled":true}}}`)
		if err := os.WriteFile(higher, higherBytes, 0600); err != nil {
			t.Fatal(err)
		}
		assertRefusal(result.ReceiptID, "rollback-conflict")
		higherAgain, err := os.ReadFile(higher)
		if err != nil || !reflect.DeepEqual(higherAgain, higherBytes) {
			t.Fatalf("higher same-URL source changed: %v", err)
		}
		if err := os.Remove(higher); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(selected, []byte(`{"mcp":{"codegraph":{"type":"remote","url":"http://127.0.0.1:9/mcp"}}}`), 0600); err != nil {
			t.Fatal(err)
		}
		assertRefusal(result.ReceiptID, "rollback-conflict")
		if err := os.WriteFile(selected, selectedAfter, 0600); err != nil {
			t.Fatal(err)
		}
		receipt.State = "recovery-required"
		if err := attachWriteReceipt(receiptLeaf, receipt); err != nil {
			t.Fatal(err)
		}
		assertRefusal(result.ReceiptID, "rollback-recovery-required")
		receipt.State = "committed"
		if err := attachWriteReceipt(receiptLeaf, receipt); err != nil {
			t.Fatal(err)
		}
		if err := os.Remove(filepath.Join(stateDir, snapshotLeaf)); err != nil {
			t.Fatal(err)
		}
		missingSnapshot, snapshotErr := NewAPI().BuildAttachClientRollbackPlan(context.Background(), request, result.ReceiptID)
		var snapshotRefusal *AttachPlanError
		if missingSnapshot != nil || !errors.As(snapshotErr, &snapshotRefusal) || snapshotRefusal.Code != "rollback-snapshot-unverified" {
			t.Fatalf("missing pinned snapshot admitted: plan=%+v err=%v", missingSnapshot, snapshotErr)
		}
		if _, err := os.Stat(filepath.Join(stateDir, snapshotLeaf)); !os.IsNotExist(err) {
			t.Fatalf("rollback preview recreated absent snapshot: %v", err)
		}
		if err := os.Remove(filepath.Join(stateDir, receiptLeaf)); err != nil {
			t.Fatal(err)
		}
		missing, missingErr := NewAPI().BuildAttachClientRollbackPlan(context.Background(), request, result.ReceiptID)
		var missingRefusal *AttachPlanError
		if missing != nil || !errors.As(missingErr, &missingRefusal) || missingRefusal.Code != "rollback-unowned" {
			t.Fatalf("missing current receipt admitted: plan=%+v err=%v", missing, missingErr)
		}
		return
	}
	if lockScenario == "committed-apply-repeat-negative" {
		receiptBefore, err := readHubMcpStateFile(receiptLeaf)
		if err != nil {
			t.Fatal(err)
		}
		snapshotBefore, err := readHubMcpStateFile(snapshotLeaf)
		if err != nil {
			t.Fatal(err)
		}
		higher := filepath.Join(configDir, "mimocode.jsonc")
		for _, tc := range []struct{ name, content string }{
			{name: "same-url", content: `{"mcp":{"codegraph":{"type":"remote","url":` + strconv.Quote(srv.URL+"/mcp") + `,"enabled":true}}}`},
			{name: "enabled-only", content: `{"mcp":{"codegraph":{"enabled":true}}}`},
		} {
			if err := os.WriteFile(higher, []byte(tc.content), 0600); err != nil {
				t.Fatal(err)
			}
			repeated, repeatErr := a.ApplyAttachClientPlan(context.Background(), plan)
			selectedAgain, selectedErr := os.ReadFile(selected)
			higherAgain, higherErr := os.ReadFile(higher)
			receiptAgain, receiptErr := readHubMcpStateFile(receiptLeaf)
			snapshotAgain, snapshotErr := readHubMcpStateFile(snapshotLeaf)
			if selectedErr != nil || higherErr != nil || receiptErr != nil || snapshotErr != nil ||
				!reflect.DeepEqual(selectedAgain, selectedAfter) || !reflect.DeepEqual(higherAgain, []byte(tc.content)) ||
				!reflect.DeepEqual(receiptAgain, receiptBefore) || !reflect.DeepEqual(snapshotAgain, snapshotBefore) {
				t.Fatalf("%s apply repeat changed selected or receipt evidence: selected=%v higher=%v receipt=%v snapshot=%v", tc.name, selectedErr, higherErr, receiptErr, snapshotErr)
			}
			for p, prior := range before {
				if p == selected {
					continue
				}
				actual, readErr := os.ReadFile(p)
				if readErr != nil || !reflect.DeepEqual(actual, prior) {
					t.Fatalf("%s apply repeat changed excluded or daemon source %s: %v", tc.name, filepath.Base(p), readErr)
				}
			}
			var failure *AttachClientApplyError
			if repeated != nil || !errors.As(repeatErr, &failure) || failure.Code != "ATTACH_COMMITTED_DRIFT" || failure.Stage != "receipt-readback" || failure.Invoked {
				t.Fatalf("%s apply repeat did not refuse higher source: result=%+v err=%v", tc.name, repeated, repeatErr)
			}
		}
		return
	}
	if strings.HasPrefix(lockScenario, "committed-repeat") {
		receiptBefore, err := readHubMcpStateFile(receiptLeaf)
		if err != nil {
			t.Fatal(err)
		}
		snapshotBefore, err := readHubMcpStateFile(snapshotLeaf)
		if err != nil {
			t.Fatal(err)
		}
		repeated, err := NewAPI().BuildAttachClientPlan(context.Background(), AttachClientRequest{Server: "codegraph", Daemon: "default", Client: "mimocode"})
		if err != nil || repeated == nil || repeated.State != "already-committed" || repeated.ReceiptID != result.ReceiptID || repeated.BeforeTransport != "http" || repeated.AfterTransport != "http" || repeated.EntryTokenSHA256 != receipt.AfterTokenSHA256 || repeated.TargetBytesSHA256 != receipt.AfterSHA256 {
			t.Fatalf("fresh API did not project exact committed receipt: plan=%+v err=%v", repeated, err)
		}
		selectedAgain, selectedErr := os.ReadFile(selected)
		receiptAgain, receiptErr := readHubMcpStateFile(receiptLeaf)
		snapshotAgain, snapshotErr := readHubMcpStateFile(snapshotLeaf)
		if selectedErr != nil || receiptErr != nil || snapshotErr != nil || !reflect.DeepEqual(selectedAgain, selectedAfter) || !reflect.DeepEqual(receiptAgain, receiptBefore) || !reflect.DeepEqual(snapshotAgain, snapshotBefore) {
			t.Fatalf("repeat plan changed selected bytes or receipt evidence: selected=%v receipt=%v snapshot=%v", selectedErr, receiptErr, snapshotErr)
		}
		for p, prior := range before {
			if p == selected {
				continue
			}
			actual, readErr := os.ReadFile(p)
			if readErr != nil || !reflect.DeepEqual(actual, prior) {
				t.Fatalf("repeat plan changed excluded or daemon source %s: %v", filepath.Base(p), readErr)
			}
		}
		if lockScenario == "committed-repeat-fresh-apply-drift" {
			higher := filepath.Join(configDir, "mimocode.jsonc")
			assertDrift := func(candidate *AttachClientPlan) {
				t.Helper()
				higherBefore, higherReadErr := os.ReadFile(higher)
				applied, applyErr := NewAPI().ApplyAttachClientPlan(context.Background(), candidate)
				selectedAgain, selectedErr := os.ReadFile(selected)
				receiptAgain, receiptErr := readHubMcpStateFile(receiptLeaf)
				snapshotAgain, snapshotErr := readHubMcpStateFile(snapshotLeaf)
				higherAgain, higherAfterErr := os.ReadFile(higher)
				if selectedErr != nil || receiptErr != nil || snapshotErr != nil || !reflect.DeepEqual(selectedAgain, selectedAfter) ||
					!reflect.DeepEqual(receiptAgain, receiptBefore) || !reflect.DeepEqual(snapshotAgain, snapshotBefore) ||
					(higherReadErr == nil) != (higherAfterErr == nil) || !reflect.DeepEqual(higherAgain, higherBefore) {
					t.Fatalf("fresh committed drift call changed selected or receipt evidence: selected=%v receipt=%v snapshot=%v higher=%v", selectedErr, receiptErr, snapshotErr, higherAfterErr)
				}
				for p, prior := range before {
					if p == selected {
						continue
					}
					actual, readErr := os.ReadFile(p)
					if readErr != nil || !reflect.DeepEqual(actual, prior) {
						t.Fatalf("fresh committed drift call changed excluded or daemon source %s: %v", filepath.Base(p), readErr)
					}
				}
				var refusal *AttachClientApplyError
				if applied != nil || !errors.As(applyErr, &refusal) || refusal.Code != "ATTACH_COMMITTED_DRIFT" || refusal.Stage != "receipt-readback" || refusal.Invoked {
					t.Fatalf("fresh committed drift was not typed no-write refusal: result=%+v err=%v", applied, applyErr)
				}
			}
			wrongID := *repeated
			wrongID.ReceiptID = "old-receipt-id"
			assertDrift(&wrongID)
			higherBytes := []byte(`{"mcp":{"codegraph":{"type":"remote","url":` + strconv.Quote(srv.URL+"/mcp") + `,"enabled":true}}}`)
			if err := os.WriteFile(higher, higherBytes, 0600); err != nil {
				t.Fatal(err)
			}
			assertDrift(repeated)
			return
		}
		if lockScenario == "committed-repeat-fresh-apply" {
			applied, applyErr := NewAPI().ApplyAttachClientPlan(context.Background(), repeated)
			if applyErr != nil || applied == nil || applied.State != "already-committed" || !applied.IdempotentNoWrite || applied.ReceiptID != result.ReceiptID ||
				applied.Server != "codegraph" || applied.Daemon != "default" || applied.Client != "mimocode" || applied.EntryKey != "codegraph" ||
				applied.Endpoint != srv.URL+"/mcp" || applied.BeforeSHA256 != receipt.BeforeSHA256 || applied.AfterSHA256 != receipt.AfterSHA256 {
				t.Fatalf("fresh committed plan did not idempotently return current receipt: result=%+v err=%v", applied, applyErr)
			}
			selectedAgain, selectedErr := os.ReadFile(selected)
			receiptAgain, receiptErr := readHubMcpStateFile(receiptLeaf)
			snapshotAgain, snapshotErr := readHubMcpStateFile(snapshotLeaf)
			if selectedErr != nil || receiptErr != nil || snapshotErr != nil || !reflect.DeepEqual(selectedAgain, selectedAfter) ||
				!reflect.DeepEqual(receiptAgain, receiptBefore) || !reflect.DeepEqual(snapshotAgain, snapshotBefore) {
				t.Fatalf("fresh committed apply changed selected or receipt evidence: selected=%v receipt=%v snapshot=%v", selectedErr, receiptErr, snapshotErr)
			}
			for p, prior := range before {
				if p == selected {
					continue
				}
				actual, readErr := os.ReadFile(p)
				if readErr != nil || !reflect.DeepEqual(actual, prior) {
					t.Fatalf("fresh committed apply changed excluded or daemon source %s: %v", filepath.Base(p), readErr)
				}
			}
			return
		}
		if lockScenario == "committed-repeat-negative" {
			request := AttachClientRequest{Server: "codegraph", Daemon: "default", Client: "mimocode"}
			higher := filepath.Join(configDir, "mimocode.jsonc")
			higherBefore := []byte(`{"mcp":{"codegraph":{"type":"remote","url":` + strconv.Quote(srv.URL+"/mcp") + `,"enabled":true}}}`)
			if err := os.WriteFile(higher, higherBefore, 0600); err != nil {
				t.Fatal(err)
			}
			got, planErr := NewAPI().BuildAttachClientPlan(context.Background(), request)
			selectedAgain, selectedErr := os.ReadFile(selected)
			higherAgain, higherErr := os.ReadFile(higher)
			receiptAgain, receiptErr := readHubMcpStateFile(receiptLeaf)
			snapshotAgain, snapshotErr := readHubMcpStateFile(snapshotLeaf)
			if selectedErr != nil || higherErr != nil || receiptErr != nil || snapshotErr != nil ||
				!reflect.DeepEqual(selectedAgain, selectedAfter) || !reflect.DeepEqual(higherAgain, higherBefore) ||
				!reflect.DeepEqual(receiptAgain, receiptBefore) || !reflect.DeepEqual(snapshotAgain, snapshotBefore) {
				t.Fatalf("same-URL higher-source preview changed config or receipt evidence: selected=%v higher=%v receipt=%v snapshot=%v", selectedErr, higherErr, receiptErr, snapshotErr)
			}
			for p, prior := range before {
				if p == selected {
					continue
				}
				actual, readErr := os.ReadFile(p)
				if readErr != nil || !reflect.DeepEqual(actual, prior) {
					t.Fatalf("same-URL higher-source preview changed excluded or daemon source %s: %v", filepath.Base(p), readErr)
				}
			}
			var sameURLRefusal *AttachPlanError
			if got != nil || !errors.As(planErr, &sameURLRefusal) {
				t.Fatalf("same-URL higher source was not typed no-write refusal: plan=%+v err=%v", got, planErr)
			}
			if err := os.Remove(higher); err != nil {
				t.Fatal(err)
			}
			assertRefusal := func(wantCode string) {
				t.Helper()
				configBefore, configErr := os.ReadFile(selected)
				receiptOnDiskBefore, receiptErr := os.ReadFile(filepath.Join(stateDir, receiptLeaf))
				snapshotOnDiskBefore, snapshotErr := readHubMcpStateFile(snapshotLeaf)
				if configErr != nil || snapshotErr != nil {
					t.Fatalf("pre-refusal evidence unavailable: config=%v snapshot=%v", configErr, snapshotErr)
				}
				got, planErr := NewAPI().BuildAttachClientPlan(context.Background(), request)
				var refusal *AttachPlanError
				if got != nil || !errors.As(planErr, &refusal) || refusal.Code != wantCode {
					t.Fatalf("%s was not typed refusal: plan=%+v err=%v", wantCode, got, planErr)
				}
				configAfter, configAfterErr := os.ReadFile(selected)
				receiptOnDiskAfter, receiptAfterErr := os.ReadFile(filepath.Join(stateDir, receiptLeaf))
				snapshotOnDiskAfter, snapshotAfterErr := readHubMcpStateFile(snapshotLeaf)
				if configAfterErr != nil || snapshotAfterErr != nil || !reflect.DeepEqual(configAfter, configBefore) || !reflect.DeepEqual(snapshotOnDiskAfter, snapshotOnDiskBefore) || !reflect.DeepEqual(receiptOnDiskAfter, receiptOnDiskBefore) ||
					(receiptErr == nil) != (receiptAfterErr == nil) || (receiptErr != nil && (!os.IsNotExist(receiptErr) || !os.IsNotExist(receiptAfterErr))) {
					t.Fatalf("%s refusal changed config or receipt evidence: config=%v receipt=%v snapshot=%v", wantCode, configAfterErr, receiptAfterErr, snapshotAfterErr)
				}
			}
			receipt.State = "prepared"
			if err := attachWriteReceipt(receiptLeaf, receipt); err != nil {
				t.Fatal(err)
			}
			assertRefusal("receipt-recovery-required")
			receipt.State = "committed"
			receipt.AfterTokenSHA256 = attachHash([]byte("foreign-token"))
			if err := attachWriteReceipt(receiptLeaf, receipt); err != nil {
				t.Fatal(err)
			}
			assertRefusal("receipt-postimage-drift")
			receipt.AfterTokenSHA256 = repeated.EntryTokenSHA256
			if err := attachWriteReceipt(receiptLeaf, receipt); err != nil {
				t.Fatal(err)
			}
			t.Setenv("MIMOCODE_CONFIG_CONTENT", `{"mcp":{"codegraph":{"type":"remote","url":"http://127.0.0.1:9/mcp","enabled":true}}}`)
			assertRefusal("client-origin-higher-or-mixed")
			t.Setenv("MIMOCODE_CONFIG_CONTENT", "")
			if err := os.WriteFile(selected, []byte(`{"mcp":{"codegraph":{"type":"remote","url":"http://127.0.0.1:9/mcp","enabled":true}}}`), 0600); err != nil {
				t.Fatal(err)
			}
			assertRefusal("receipt-postimage-drift")
			if err := os.WriteFile(selected, selectedAfter, 0600); err != nil {
				t.Fatal(err)
			}
			if err := os.Remove(filepath.Join(stateDir, receiptLeaf)); err != nil {
				t.Fatal(err)
			}
			assertRefusal("client-origin-disabled-or-unsupported-transport")
		}
		return
	}
	for p, prior := range before {
		if p == selected {
			continue
		}
		after, err := os.ReadFile(p)
		if err != nil || string(after) != string(prior) {
			t.Fatalf("apply changed excluded or daemon/manifest/intent source %s: err=%v", filepath.Base(p), err)
		}
	}
	if pid, found, err := LoopbackPortOwnerPIDContext(context.Background(), port); err != nil || !found || pid != os.Getpid() {
		t.Fatalf("apply changed occupied listener: pid=%d found=%v err=%v", pid, found, err)
	}
	if lockScenario == "restore-rollback" {
		if !attachRestoredEntryMatches(selectedClient, selectedBefore, selectedAfter, "sibling") {
			t.Fatal("restore attach changed selected file's unrelated sibling")
		}
		rolled, rollbackErr := a.RollbackAttachClient(context.Background(), AttachClientRequest{Server: "codegraph", Daemon: "default", Client: "mimocode"}, result.ReceiptID)
		if rollbackErr != nil || rolled == nil || rolled.State != "rolled-back" || rolled.ReceiptID != result.ReceiptID {
			t.Fatalf("restored-manifest receipt rollback failed: result=%+v err=%v", rolled, rollbackErr)
		}
		restored, readErr := os.ReadFile(selected)
		if readErr != nil || !attachRestoredEntryMatches(selectedClient, selectedBefore, restored, "codegraph") ||
			!attachRestoredEntryMatches(selectedClient, selectedBefore, restored, "sibling") {
			t.Fatalf("receipt rollback changed direct or sibling entry: err=%v", readErr)
		}
		manifestAfter, manifestErr := os.ReadFile(manifestPath)
		if manifestErr != nil || string(manifestAfter) != raw {
			t.Fatalf("receipt rollback removed recovered manifest: %v", manifestErr)
		}
		store, storeErr := readAdoptedEntries()
		if storeErr != nil || store == nil || len(store.Records) != 0 {
			t.Fatalf("receipt rollback created adopt provenance: store=%+v err=%v", store, storeErr)
		}
		for p, prior := range before {
			if p == selected {
				continue
			}
			actual, err := os.ReadFile(p)
			if err != nil || string(actual) != string(prior) {
				t.Fatalf("restore/rollback changed excluded or daemon source %s: %v", filepath.Base(p), err)
			}
		}
		if pid, found, ownerErr := LoopbackPortOwnerPIDContext(context.Background(), port); ownerErr != nil || !found || pid != os.Getpid() {
			t.Fatalf("restore/rollback changed occupied listener: pid=%d found=%v err=%v", pid, found, ownerErr)
		}
		return
	}
	if strings.HasPrefix(lockScenario, "rollback") {
		if !attachRestoredEntryMatches(selectedClient, selectedBefore, selectedAfter, "sibling") {
			t.Fatal("apply changed selected file's unrelated sibling value")
		}
		repeated, repeatErr := a.ApplyAttachClientPlan(context.Background(), plan)
		if repeatErr != nil || repeated == nil || repeated.State != "committed" || !repeated.IdempotentNoWrite || repeated.ReceiptID != result.ReceiptID {
			t.Fatalf("repeat attach did not return exact committed receipt: result=%+v err=%v", repeated, repeatErr)
		}
		unchanged, readErr := os.ReadFile(selected)
		if readErr != nil || string(unchanged) != string(selectedAfter) {
			t.Fatalf("repeat attach rewrote selected target: err=%v", readErr)
		}
		migrate, migrateErr := a.MigrateFrom(MigrateOpts{Servers: []string{"codegraph"}, ClientsInclude: []string{"mimocode"}, ScanOpts: ScanOpts{ManifestDir: manifestDir}})
		if migrateErr != nil || migrate == nil || len(migrate.Applied) != 0 || len(migrate.Failed) != 1 || !strings.Contains(migrate.Failed[0].Err, "ATTACH_COMPETING_OWNER") {
			t.Fatalf("migrate bypassed committed attachment: report=%+v err=%v", migrate, migrateErr)
		}
		demigrate, demigrateErr := a.Demigrate(DemigrateOpts{Servers: []string{"codegraph"}, ClientsInclude: []string{"mimocode"}, ScanOpts: ScanOpts{ManifestDir: manifestDir}})
		if demigrateErr != nil || demigrate == nil || len(demigrate.Restored) != 0 || len(demigrate.Failed) != 1 || !strings.Contains(demigrate.Failed[0].Err, "ATTACH_COMPETING_OWNER") {
			t.Fatalf("demigrate bypassed committed attachment: report=%+v err=%v", demigrate, demigrateErr)
		}
		unchanged, readErr = os.ReadFile(selected)
		if readErr != nil || string(unchanged) != string(selectedAfter) {
			t.Fatalf("competing path changed selected target: err=%v", readErr)
		}
		if lockScenario == "rollback-unrelated" {
			other, otherErr := a.MigrateFrom(MigrateOpts{Servers: []string{"codegraph"}, ClientsInclude: []string{codexClient.Name()}, ScanOpts: ScanOpts{ManifestDir: manifestDir}})
			if otherErr != nil || other == nil || len(other.Applied) != 1 || len(other.Failed) != 0 || other.Applied[0].Client != codexClient.Name() {
				t.Fatalf("unrelated client migration blocked by attachment: report=%+v err=%v", other, otherErr)
			}
			return
		}
		if lockScenario == "rollback-states" {
			for _, state := range []string{"prepared", "rollback-pending", "recovery-required"} {
				receipt.State = state
				if err := attachWriteReceipt(receiptLeaf, receipt); err != nil {
					t.Fatal(err)
				}
				migrate, migrateErr := a.MigrateFrom(MigrateOpts{Servers: []string{"codegraph"}, ClientsInclude: []string{"mimocode"}, ScanOpts: ScanOpts{ManifestDir: manifestDir}})
				if migrateErr != nil || len(migrate.Applied) != 0 || len(migrate.Failed) != 1 || !strings.Contains(migrate.Failed[0].Err, "ATTACH_COMPETING_OWNER") {
					t.Fatalf("migrate bypassed %s receipt: report=%+v err=%v", state, migrate, migrateErr)
				}
				demigrate, demigrateErr := a.Demigrate(DemigrateOpts{Servers: []string{"codegraph"}, ClientsInclude: []string{"mimocode"}, ScanOpts: ScanOpts{ManifestDir: manifestDir}})
				if demigrateErr != nil || len(demigrate.Restored) != 0 || len(demigrate.Failed) != 1 || !strings.Contains(demigrate.Failed[0].Err, "ATTACH_COMPETING_OWNER") {
					t.Fatalf("demigrate bypassed %s receipt: report=%+v err=%v", state, demigrate, demigrateErr)
				}
				if repeated, repeatErr := a.ApplyAttachClientPlan(context.Background(), plan); repeated != nil || repeatErr == nil {
					t.Fatalf("repeat attach bypassed %s receipt: result=%+v err=%v", state, repeated, repeatErr)
				}
				if rolled, rollbackErr := a.RollbackAttachClient(context.Background(), AttachClientRequest{Server: "codegraph", Daemon: "default", Client: "mimocode"}, result.ReceiptID); rolled != nil || rollbackErr == nil {
					t.Fatalf("rollback bypassed %s receipt: result=%+v err=%v", state, rolled, rollbackErr)
				}
				current, readErr := os.ReadFile(selected)
				if readErr != nil || string(current) != string(selectedAfter) {
					t.Fatalf("receipt state %s changed selected bytes: err=%v", state, readErr)
				}
			}
			return
		}
		if lockScenario == "rollback-conflict" {
			if err := os.WriteFile(selected, []byte(`{"mcp":{"codegraph":{"type":"remote","url":"http://127.0.0.1:9/mcp"}}}`), 0600); err != nil {
				t.Fatal(err)
			}
		}
		var hooks *attachRollbackHooks
		var racedBytes []byte
		if lockScenario == "rollback-cas-race" {
			racedBytes = []byte(`{"mcp":{"codegraph":{"type":"remote","url":"http://127.0.0.1:9/mcp"},"sibling":{"type":"local","command":["node","sibling.js"],"enabled":true}}}`)
			hooks = &attachRollbackHooks{beforeCAS: func() {
				if err := os.WriteFile(selected, racedBytes, 0600); err != nil {
					t.Error(err)
				}
			}}
		}
		if lockScenario == "rollback-symlink" {
			t.Setenv("MCPHUB_ALLOW_CLIENT_CONFIG_SYMLINK", "1")
			foreign := filepath.Join(root, "foreign-rollback.json")
			if err := os.WriteFile(foreign, selectedAfter, 0600); err != nil {
				t.Fatal(err)
			}
			probe := filepath.Join(root, "rollback-symlink-probe")
			if err := os.Symlink(foreign, probe); err != nil {
				t.Skipf("test host cannot create file symlink: %v", err)
			}
			if err := os.Remove(probe); err != nil {
				t.Fatal(err)
			}
			hooks = &attachRollbackHooks{beforeWrite: func() {
				parked := selected + ".parked"
				if err := os.Rename(selected, parked); err != nil {
					t.Errorf("park rollback target: %v", err)
					return
				}
				if err := os.Symlink(foreign, selected); err != nil {
					t.Errorf("swap rollback target: %v", err)
					_ = os.Rename(parked, selected)
				}
			}}
			defer func() {
				actual, err := os.ReadFile(foreign)
				if err != nil || string(actual) != string(selectedAfter) {
					t.Errorf("foreign rollback target modified: err=%v", err)
				}
			}()
		}
		rolled, rollbackErr := a.rollbackAttachClient(context.Background(), AttachClientRequest{Server: "codegraph", Daemon: "default", Client: "mimocode"}, result.ReceiptID, hooks)
		if lockScenario == "rollback" || lockScenario == "rollback-history" || lockScenario == "rollback-archive-fail" || lockScenario == "rollback-preview-rolled-back" {
			if rollbackErr != nil || rolled == nil || rolled.State != "rolled-back" || rolled.IdempotentNoWrite || rolled.ReceiptID != result.ReceiptID {
				t.Fatalf("receipt rollback failed: result=%+v err=%v cause=%v", rolled, rollbackErr, errors.Unwrap(rollbackErr))
			}
			restored, err := os.ReadFile(selected)
			if err != nil || !attachRestoredEntryMatches(selectedClient, selectedBefore, restored, "codegraph") {
				t.Fatalf("selected direct entry not restored: err=%v", err)
			}
			if !attachRestoredEntryMatches(selectedClient, selectedBefore, restored, "sibling") || !attachRestoredEntryMatches(selectedClient, selectedAfter, restored, "sibling") {
				t.Fatal("rollback changed unrelated sibling value")
			}
			direct, err := selectedClient.GetEntry("codegraph")
			if err != nil || direct == nil || direct.URL != "" || direct.Disabled {
				t.Fatalf("effective direct entry not restored: entry=%+v err=%v", direct, err)
			}
			again, againErr := a.RollbackAttachClient(context.Background(), AttachClientRequest{Server: "codegraph", Daemon: "default", Client: "mimocode"}, result.ReceiptID)
			if againErr != nil || again == nil || again.State != "rolled-back" || !again.IdempotentNoWrite || again.ReceiptID != result.ReceiptID {
				t.Fatalf("repeated rollback was not no-write: result=%+v err=%v", again, againErr)
			}
			current, readErr := os.ReadFile(selected)
			if readErr != nil || string(current) != string(restored) {
				t.Fatalf("repeated rollback changed selected target: err=%v", readErr)
			}
			if lockScenario == "rollback-preview-rolled-back" {
				request := AttachClientRequest{Server: "codegraph", Daemon: "default", Client: "mimocode"}
				receiptBefore, err := readHubMcpStateFile(receiptLeaf)
				if err != nil {
					t.Fatal(err)
				}
				snapshotBefore, err := readHubMcpStateFile(snapshotLeaf)
				if err != nil {
					t.Fatal(err)
				}
				preview, err := NewAPI().BuildAttachClientRollbackPlan(context.Background(), request, result.ReceiptID)
				token, tokenErr := clients.SelectedEntryPhysicalTokenSHA256(selectedClient, restored, "codegraph")
				if err != nil || tokenErr != nil || preview == nil || preview.State != "already-rolled-back" ||
					preview.ReceiptID != result.ReceiptID || preview.BeforeTransport != "direct" || preview.AfterTransport != "direct" ||
					preview.TargetBytesSHA256 != attachHash(restored) || preview.EntryTokenSHA256 != token {
					t.Fatalf("rolled-back preview lacks exact direct proof: preview=%+v err=%v tokenErr=%v", preview, err, tokenErr)
				}
				selectedAgain, selectedErr := os.ReadFile(selected)
				receiptAgain, receiptErr := readHubMcpStateFile(receiptLeaf)
				snapshotAgain, snapshotErr := readHubMcpStateFile(snapshotLeaf)
				if selectedErr != nil || receiptErr != nil || snapshotErr != nil || !reflect.DeepEqual(selectedAgain, restored) ||
					!reflect.DeepEqual(receiptAgain, receiptBefore) || !reflect.DeepEqual(snapshotAgain, snapshotBefore) {
					t.Fatalf("rolled-back preview changed selected or evidence: selected=%v receipt=%v snapshot=%v", selectedErr, receiptErr, snapshotErr)
				}
				for p, prior := range before {
					if p == selected {
						continue
					}
					actual, readErr := os.ReadFile(p)
					if readErr != nil || !reflect.DeepEqual(actual, prior) {
						t.Fatalf("rolled-back preview changed excluded or daemon source %s: %v", filepath.Base(p), readErr)
					}
				}
				if pid, found, ownerErr := LoopbackPortOwnerPIDContext(context.Background(), port); ownerErr != nil || !found || pid != os.Getpid() {
					t.Fatalf("rolled-back preview changed listener: pid=%d found=%v err=%v", pid, found, ownerErr)
				}
				higher := filepath.Join(configDir, "mimocode.jsonc")
				higherBytes := []byte(`{"mcp":{"codegraph":{"enabled":true}}}`)
				if err := os.WriteFile(higher, higherBytes, 0600); err != nil {
					t.Fatal(err)
				}
				conflicted, conflictErr := NewAPI().BuildAttachClientRollbackPlan(context.Background(), request, result.ReceiptID)
				var refusal *AttachPlanError
				if conflicted != nil || !errors.As(conflictErr, &refusal) || refusal.Code != "rollback-conflict" {
					t.Fatalf("higher source admitted rolled-back preview: plan=%+v err=%v", conflicted, conflictErr)
				}
				higherAgain, higherErr := os.ReadFile(higher)
				if higherErr != nil || !reflect.DeepEqual(higherAgain, higherBytes) {
					t.Fatalf("higher source changed by rolled-back preview: %v", higherErr)
				}
				return
			}
			demigrate, demigrateErr := a.Demigrate(DemigrateOpts{Servers: []string{"codegraph"}, ClientsInclude: []string{"mimocode"}, ScanOpts: ScanOpts{ManifestDir: manifestDir}})
			if demigrateErr != nil || demigrate == nil || len(demigrate.Restored) != 1 || len(demigrate.Failed) != 0 {
				t.Fatalf("rolled-back demigrate not already-restored: report=%+v err=%v", demigrate, demigrateErr)
			}
			current, readErr = os.ReadFile(selected)
			if readErr != nil || string(current) != string(restored) {
				t.Fatalf("rolled-back demigrate changed selected target: err=%v", readErr)
			}
			if lockScenario == "rollback-history" || lockScenario == "rollback-archive-fail" {
				freshPlan, planErr := a.BuildAttachClientPlan(context.Background(), AttachClientRequest{Server: "codegraph", Daemon: "default", Client: "mimocode"})
				if planErr != nil {
					t.Fatalf("fresh direct plan refused: %v", planErr)
				}
				historyLeaf := strings.TrimSuffix(receiptLeaf, ".receipt.json") + "-" + result.ReceiptID + ".receipt.json"
				oldReceiptRaw, oldReceiptErr := readHubMcpStateFile(receiptLeaf)
				if oldReceiptErr != nil {
					t.Fatal(oldReceiptErr)
				}
				var fresh *AttachClientApplyResult
				var freshErr error
				if lockScenario == "rollback-archive-fail" {
					fresh, freshErr = a.applyAttachClientPlan(context.Background(), freshPlan, &attachApplyHooks{writeReceiptFile: func(leaf string, raw []byte) error {
						if leaf == historyLeaf {
							return errors.New("injected archive write failure")
						}
						return writeHubMcpStateFile(leaf, raw)
					}})
					var failure *AttachClientApplyError
					if fresh != nil || !errors.As(freshErr, &failure) || failure.Code != "ATTACH_HISTORY_UNVERIFIED" {
						t.Fatalf("archive failure advanced current receipt: result=%+v err=%v", fresh, freshErr)
					}
					currentReceiptRaw, readErr := readHubMcpStateFile(receiptLeaf)
					if readErr != nil || string(currentReceiptRaw) != string(oldReceiptRaw) {
						t.Fatalf("archive failure overwrote rolled-back current receipt: err=%v", readErr)
					}
					current, readErr := os.ReadFile(selected)
					if readErr != nil || string(current) != string(restored) {
						t.Fatalf("archive failure wrote selected target: err=%v", readErr)
					}
					if _, readErr := attachReadReceipt(historyLeaf); !isHubMcpStateMissingErr(readErr) {
						t.Fatalf("archive failure left an unverified history receipt: %v", readErr)
					}
					return
				}
				fresh, freshErr = a.ApplyAttachClientPlan(context.Background(), freshPlan)
				if freshErr != nil || fresh == nil || fresh.ReceiptID == result.ReceiptID || fresh.State != "committed" {
					t.Fatalf("fresh attach after rollback failed: result=%+v err=%v", fresh, freshErr)
				}
				history, historyErr := attachReadReceipt(historyLeaf)
				if historyErr != nil || history == nil || history.ID != result.ReceiptID || history.State != "rolled-back" || history.SnapshotLeaf != snapshotLeaf {
					t.Fatalf("old rolled-back receipt not preserved: receipt=%+v err=%v", history, historyErr)
				}
				oldPin, pinErr := readHubMcpStateFile(snapshotLeaf)
				if pinErr != nil || string(oldPin) != string(selectedBefore) {
					t.Fatalf("old pinned snapshot overwritten: err=%v", pinErr)
				}
				freshReceipt, receiptErr := attachReadReceipt(receiptLeaf)
				if receiptErr != nil || freshReceipt.ID != fresh.ReceiptID || freshReceipt.SnapshotLeaf == snapshotLeaf {
					t.Fatalf("new operation reused old snapshot: receipt=%+v err=%v", freshReceipt, receiptErr)
				}
				oldPreview, oldPreviewErr := NewAPI().BuildAttachClientRollbackPlan(context.Background(), AttachClientRequest{Server: "codegraph", Daemon: "default", Client: "mimocode"}, result.ReceiptID)
				var oldRefusal *AttachPlanError
				if oldPreview != nil || !errors.As(oldPreviewErr, &oldRefusal) || oldRefusal.Code != "rollback-receipt-unverified" {
					t.Fatalf("historical receipt selected instead of current: preview=%+v err=%v", oldPreview, oldPreviewErr)
				}
				repeatPlan, repeatErr := NewAPI().BuildAttachClientPlan(context.Background(), AttachClientRequest{Server: "codegraph", Daemon: "default", Client: "mimocode"})
				if repeatErr != nil || repeatPlan == nil || repeatPlan.State != "already-committed" || repeatPlan.ReceiptID != fresh.ReceiptID || repeatPlan.ReceiptID == result.ReceiptID {
					t.Fatalf("fresh repeat projected old receipt: plan=%+v err=%v", repeatPlan, repeatErr)
				}
				freshRolled, freshRollbackErr := a.RollbackAttachClient(context.Background(), AttachClientRequest{Server: "codegraph", Daemon: "default", Client: "mimocode"}, fresh.ReceiptID)
				if freshRollbackErr != nil || freshRolled == nil || freshRolled.State != "rolled-back" {
					t.Fatalf("fresh operation could not roll back: result=%+v err=%v", freshRolled, freshRollbackErr)
				}
				historyAfter, historyErr := attachReadReceipt(historyLeaf)
				if historyErr != nil || !reflect.DeepEqual(historyAfter, history) {
					t.Fatalf("second operation changed historical receipt: err=%v", historyErr)
				}
				oldPinAfter, pinErr := readHubMcpStateFile(snapshotLeaf)
				if pinErr != nil || string(oldPinAfter) != string(oldPin) {
					t.Fatalf("second operation changed historical snapshot: err=%v", pinErr)
				}
			}
		} else {
			var failure *AttachClientApplyError
			if rolled != nil || !errors.As(rollbackErr, &failure) || !failure.RecoveryRequired {
				t.Fatalf("conflicting rollback falsely succeeded: result=%+v err=%v", rolled, rollbackErr)
			}
			if lockScenario == "rollback-cas-race" {
				if failure.Code != "ATTACH_ROLLBACK_CONFLICT" || !failure.Invoked {
					t.Fatalf("CAS race had wrong refusal: %+v", failure)
				}
				current, readErr := os.ReadFile(selected)
				if readErr != nil || string(current) != string(racedBytes) {
					t.Fatalf("CAS race overwrote changed target: err=%v", readErr)
				}
			}
		}
		afterReceipt, err := attachReadReceipt(receiptLeaf)
		if err != nil || ((lockScenario == "rollback" || lockScenario == "rollback-history" || lockScenario == "rollback-archive-fail") && afterReceipt.State != "rolled-back") || ((lockScenario == "rollback-conflict" || lockScenario == "rollback-symlink" || lockScenario == "rollback-cas-race") && afterReceipt.State != "recovery-required") {
			t.Fatalf("rollback receipt state incorrect: receipt=%+v err=%v", afterReceipt, err)
		}
		for p, prior := range before {
			if p == selected {
				continue
			}
			actual, err := os.ReadFile(p)
			if err != nil || string(actual) != string(prior) {
				t.Fatalf("rollback changed excluded or daemon/manifest/intent %s: err=%v", filepath.Base(p), err)
			}
		}
		if pid, found, err := LoopbackPortOwnerPIDContext(context.Background(), port); err != nil || !found || pid != os.Getpid() {
			t.Fatalf("rollback changed occupied listener: pid=%d found=%v err=%v", pid, found, err)
		}
	}
}

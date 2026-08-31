package api

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"mcp-local-hub/internal/config"
	"mcp-local-hub/internal/mcpcompat/readinesswire"
	"mcp-local-hub/internal/scheduler"
)

const (
	stdioBridgeAdmissionHelperEnv     = "MCPHUB_STDIO_BRIDGE_ADMISSION_HELPER"
	stdioBridgeAdmissionHelperPortEnv = "MCPHUB_STDIO_BRIDGE_ADMISSION_PORT"
)

func TestFrozenStdioBridgeAdmissionHelper(t *testing.T) {
	mode := os.Getenv(stdioBridgeAdmissionHelperEnv)
	if mode == "" {
		return
	}
	port, err := strconv.Atoi(os.Getenv(stdioBridgeAdmissionHelperPortEnv))
	if err != nil || port <= 0 {
		os.Exit(2)
	}
	listener, err := net.Listen("tcp", "127.0.0.1:"+strconv.Itoa(port))
	if err != nil {
		os.Exit(3)
	}
	server := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if mode == "hang" {
			<-r.Context().Done()
			return
		}
		if mode == "legacy" && strings.Contains(string(body), `"method":"initialize"`) {
			_ = readinesswire.WriteFailure(w, readinesswire.Failure{
				FailureID: readinesswire.FailureBackingProtocolUnsupported, Stage: readinesswire.StageInitialize,
				HTTPStatus: http.StatusBadGateway, RequestedProtocol: "2025-03-26", NegotiatedProtocol: "2024-11-05", SupportedFloor: "2025-03-26",
			})
			return
		}
		var request struct {
			Method string `json:"method"`
		}
		_ = json.Unmarshal(body, &request)
		w.Header().Set("Content-Type", "application/json")
		switch request.Method {
		case "initialize":
			_, _ = io.WriteString(w, `{"jsonrpc":"2.0","id":1,"result":{"protocolVersion":"2025-03-26","capabilities":{"tools":{}}}}`)
		case "tools/list":
			_, _ = io.WriteString(w, `{"jsonrpc":"2.0","id":2,"result":{"tools":[]}}`)
		default:
			http.Error(w, "unexpected method", http.StatusBadRequest)
		}
	})}
	_ = server.Serve(listener)
}

func TestFrozenStdioBridgeAdmissionRejectsUnsupportedBeforeInstallMutation(t *testing.T) {
	original := frozenStdioBridgeAdmissionFn
	originalScheduler := schedulerFactoryFn
	stateDir := t.TempDir()
	t.Cleanup(func() {
		frozenStdioBridgeAdmissionFn = original
		schedulerFactoryFn = originalScheduler
	})
	t.Cleanup(SetDaemonStateRootForTest(stateDir))

	called := 0
	frozenStdioBridgeAdmissionFn = func(_ context.Context, requests []FrozenStdioBridgeAdmissionRequest) error {
		called++
		if len(requests) != 1 {
			t.Fatalf("requests = %#v, want one selected stdio bridge", requests)
		}
		if got := requests[0]; got.ManifestName != "probe" || got.DaemonName != "default" || got.Port != 19304 || got.Command != "frozen-mcphub" || got.WorkingDir != "C:/frozen" {
			t.Fatalf("request = %#v, want frozen descriptor fields", got)
		}
		return &MCPReadinessError{Result: MCPReadinessResult{
			SchemaVersion: "mcp-readiness-v1", State: MCPReadinessUnready,
			Stage: MCPReadinessStageInitialize, FailureID: "MCP_BACKING_PROTOCOL_UNSUPPORTED",
			RequestedProtocol: "2025-03-26", NegotiatedProtocol: "2024-11-05", SupportedFloor: "2025-03-26",
		}}
	}
	schedulerCalls := 0
	schedulerFactoryFn = func() (scheduler.Scheduler, error) {
		schedulerCalls++
		return nil, errors.New("scheduler must not run after failed admission")
	}

	plan := &Plan{SupervisorIntent: []SupervisorIntentEntry{{
		Name: "mcp-local-hub-probe-default", Command: "frozen-mcphub", Args: []string{"daemon", "--server", "probe", "--daemon", "default"}, WorkingDir: "C:/frozen", StartupBindDeadlineSeconds: 1,
	}}}
	m := &config.ServerManifest{
		Name: "probe", Kind: config.KindGlobal, Transport: config.TransportStdioBridge,
		Daemons: []config.DaemonSpec{{Name: "default", Port: 19304}},
	}

	err := NewAPI().installFrozenPlanCore(context.Background(), m, plan, InstallOpts{Server: "probe", Writer: io.Discard}, CanonicalAtAdmission(), io.Discard, nil)
	var readinessErr *MCPReadinessError
	if !errors.As(err, &readinessErr) || readinessErr.Result.FailureID != "MCP_BACKING_PROTOCOL_UNSUPPORTED" {
		t.Fatalf("error = %v, want typed unsupported protocol", err)
	}
	if called != 1 {
		t.Fatalf("admission calls = %d, want 1", called)
	}
	if schedulerCalls != 0 {
		t.Fatalf("scheduler calls = %d, want 0 after admission failure", schedulerCalls)
	}
	if _, statErr := os.Stat(filepath.Join(stateDir, supervisorIntentFileLeaf)); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("failed admission wrote supervisor intent: %v", statErr)
	}
}

func TestFrozenStdioBridgeAdmissionSkipsDryRunAndNonStdioPlans(t *testing.T) {
	original := frozenStdioBridgeAdmissionFn
	t.Cleanup(func() { frozenStdioBridgeAdmissionFn = original })

	called := 0
	frozenStdioBridgeAdmissionFn = func(context.Context, []FrozenStdioBridgeAdmissionRequest) error {
		called++
		return errors.New("admission must not run")
	}
	plan := &Plan{supervisorIntentHashesBound: true, SupervisorIntent: []SupervisorIntentEntry{{Name: "mcp-local-hub-probe-default", Command: "frozen", Args: []string{"daemon"}, WorkingDir: "C:/frozen", StartupBindDeadlineSeconds: 1, manifestHash: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}}}
	base := &config.ServerManifest{Name: "probe", Kind: config.KindGlobal, Transport: config.TransportStdioBridge, Daemons: []config.DaemonSpec{{Name: "default", Port: 19305}}}

	if err := NewAPI().installFrozenPlanCore(context.Background(), base, plan, InstallOpts{Server: "probe", DryRun: true, Writer: io.Discard}, CanonicalAtAdmission(), io.Discard, nil); err != nil {
		t.Fatalf("dry-run = %v", err)
	}
	native := *base
	native.Transport = config.TransportNativeHTTP
	if err := admitFrozenStdioBridgeBeforeMutation(context.Background(), &native, plan, InstallOpts{Server: "probe"}); err != nil {
		t.Fatalf("native admission gate = %v", err)
	}
	if called != 0 {
		t.Fatalf("admission calls = %d, want 0", called)
	}
}

func TestFrozenStdioBridgeAdmissionProbeReapsSuccessfulProvisionalTree(t *testing.T) {
	port := reserveAdmissionPort(t)
	t.Setenv(stdioBridgeAdmissionHelperEnv, "ready")
	t.Setenv(stdioBridgeAdmissionHelperPortEnv, strconv.Itoa(port))
	t.Cleanup(SetDaemonStateRootForTest(t.TempDir()))

	request := FrozenStdioBridgeAdmissionRequest{
		ManifestName: "probe", DaemonName: "default", Command: os.Args[0],
		Args: []string{"-test.run=^TestFrozenStdioBridgeAdmissionHelper$"}, WorkingDir: t.TempDir(),
		Port: port, StartDeadline: 2 * time.Second,
	}
	if err := admitFrozenStdioBridgeRequest(context.Background(), request); err != nil {
		t.Fatalf("admit = %v", err)
	}
	if portInUse(port) {
		t.Fatalf("provisional admission left port %d in use", port)
	}
}

func TestFrozenStdioBridgeAdmissionProbeReturnsTypedLegacyProtocolFailureAndReaps(t *testing.T) {
	port := reserveAdmissionPort(t)
	t.Setenv(stdioBridgeAdmissionHelperEnv, "legacy")
	t.Setenv(stdioBridgeAdmissionHelperPortEnv, strconv.Itoa(port))
	t.Cleanup(SetDaemonStateRootForTest(t.TempDir()))

	err := admitFrozenStdioBridgeRequest(context.Background(), FrozenStdioBridgeAdmissionRequest{
		ManifestName: "probe", DaemonName: "default", Command: os.Args[0],
		Args: []string{"-test.run=^TestFrozenStdioBridgeAdmissionHelper$"}, WorkingDir: t.TempDir(),
		Port: port, StartDeadline: 2 * time.Second,
	})
	var readinessErr *MCPReadinessError
	if !errors.As(err, &readinessErr) || readinessErr.Result.FailureID != readinesswire.FailureBackingProtocolUnsupported || readinessErr.Result.RequestedProtocol != "2025-03-26" || readinessErr.Result.NegotiatedProtocol != "2024-11-05" {
		t.Fatalf("error = %#v, want typed legacy protocol failure", err)
	}
	if portInUse(port) {
		t.Fatalf("failed provisional admission left port %d in use", port)
	}
}

func TestFrozenStdioBridgeAdmissionCancellationReapsProvisionalTree(t *testing.T) {
	port := reserveAdmissionPort(t)
	t.Setenv(stdioBridgeAdmissionHelperEnv, "hang")
	t.Setenv(stdioBridgeAdmissionHelperPortEnv, strconv.Itoa(port))
	t.Cleanup(SetDaemonStateRootForTest(t.TempDir()))
	ctx, cancel := context.WithTimeout(context.Background(), 75*time.Millisecond)
	defer cancel()

	started := time.Now()
	err := admitFrozenStdioBridgeRequest(ctx, FrozenStdioBridgeAdmissionRequest{
		ManifestName: "probe", DaemonName: "default", Command: os.Args[0],
		Args: []string{"-test.run=^TestFrozenStdioBridgeAdmissionHelper$"}, WorkingDir: t.TempDir(),
		Port: port, StartDeadline: 2 * time.Second,
	})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("admit canceled = %v, want deadline exceeded", err)
	}
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("canceled admission returned after %s, want bounded cleanup", elapsed)
	}
	if portInUse(port) {
		t.Fatalf("canceled provisional admission left port %d in use", port)
	}
}

func TestFrozenStdioBridgeAdmissionProbesExactExistingGenerationWithoutSpawningDuplicate(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		if strings.Contains(string(body), `"method":"initialize"`) {
			_, _ = io.WriteString(w, `{"jsonrpc":"2.0","id":1,"result":{"protocolVersion":"2025-03-26","capabilities":{"tools":{}}}}`)
			return
		}
		_, _ = io.WriteString(w, `{"jsonrpc":"2.0","id":2,"result":{"tools":[]}}`)
	}))
	defer server.Close()
	port := admissionServerPort(t, server.URL)

	stateDir := hardenedTempDir(t)
	t.Cleanup(SetDaemonStateRootForTest(stateDir))
	const hash = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	request := FrozenStdioBridgeAdmissionRequest{
		ManifestName: "probe", DaemonName: "default", ManifestHash: hash,
		// A duplicate-spawn regression would try this and fail; a matching
		// committed generation must probe the existing listener instead.
		Command: "definitely-not-a-real-admission-command", Args: []string{"daemon"}, WorkingDir: t.TempDir(),
		Port: port, StartDeadline: time.Second,
	}
	if err := WriteSupervisorIntent(filepath.Join(stateDir, supervisorIntentFileLeaf), &SupervisorIntentFile{Version: 1, Daemons: []SupervisorDaemon{{
		Server: request.ManifestName, Daemon: request.DaemonName, ManifestHash: request.ManifestHash,
		Command: request.Command, Args: request.Args, WorkingDir: request.WorkingDir, StartupBindDeadlineSeconds: 1, Port: request.Port,
	}}}); err != nil {
		t.Fatal(err)
	}
	if err := admitFrozenStdioBridgeRequest(context.Background(), request); err != nil {
		t.Fatalf("admit existing = %v", err)
	}
}

func TestFrozenStdioBridgeAdmissionRejectsExistingGenerationWorkingDirAndDeadlineDrift(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(*SupervisorDaemon)
	}{
		{name: "working directory", mutate: func(d *SupervisorDaemon) { d.WorkingDir = t.TempDir() }},
		{name: "legacy empty working directory", mutate: func(d *SupervisorDaemon) { d.WorkingDir = "" }},
		{name: "start deadline", mutate: func(d *SupervisorDaemon) { d.StartupBindDeadlineSeconds = 2 }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			stateDir := hardenedTempDir(t)
			t.Cleanup(SetDaemonStateRootForTest(stateDir))
			request := FrozenStdioBridgeAdmissionRequest{
				ManifestName: "probe", DaemonName: "default", ManifestHash: "eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee",
				Command: "definitely-not-a-real-admission-command", Args: []string{"daemon"}, WorkingDir: t.TempDir(), Port: 19307, StartDeadline: time.Second,
			}
			existing := SupervisorDaemon{
				Server: request.ManifestName, Daemon: request.DaemonName, ManifestHash: request.ManifestHash,
				Command: request.Command, Args: request.Args, WorkingDir: request.WorkingDir, StartupBindDeadlineSeconds: 1, Port: request.Port,
			}
			tc.mutate(&existing)
			if err := WriteSupervisorIntent(filepath.Join(stateDir, supervisorIntentFileLeaf), &SupervisorIntentFile{Version: 1, Daemons: []SupervisorDaemon{existing}}); err != nil {
				t.Fatal(err)
			}
			err := admitFrozenStdioBridgeRequest(context.Background(), request)
			if err == nil || !strings.Contains(err.Error(), "does not exactly match") {
				t.Fatalf("admit drifted generation = %v, want exact-generation refusal", err)
			}
		})
	}
}

func TestSameFrozenWorkingDirPreservesBothEmptyLegacyDescriptors(t *testing.T) {
	if !sameFrozenWorkingDir("", "") {
		t.Fatal("two legacy inherited working directories must remain compatible")
	}
	if sameFrozenWorkingDir("", "/frozen") || sameFrozenWorkingDir("/current", "") {
		t.Fatal("a missing legacy working directory must not prove an exact nonempty frozen descriptor")
	}
}

func TestFrozenStdioBridgeAdmissionExistingProbeHonorsFrozenDeadline(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		// Deliberately outlast the frozen one-second deadline but remain
		// finite so the test server itself always settles.
		time.Sleep(1500 * time.Millisecond)
	}))
	defer server.Close()
	port := admissionServerPort(t, server.URL)
	stateDir := hardenedTempDir(t)
	t.Cleanup(SetDaemonStateRootForTest(stateDir))
	request := FrozenStdioBridgeAdmissionRequest{
		ManifestName: "probe", DaemonName: "default", ManifestHash: "ffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff",
		Command: "definitely-not-a-real-admission-command", Args: []string{"daemon"}, WorkingDir: t.TempDir(), Port: port, StartDeadline: time.Second,
	}
	if err := WriteSupervisorIntent(filepath.Join(stateDir, supervisorIntentFileLeaf), &SupervisorIntentFile{Version: 1, Daemons: []SupervisorDaemon{{
		Server: request.ManifestName, Daemon: request.DaemonName, ManifestHash: request.ManifestHash,
		Command: request.Command, Args: request.Args, WorkingDir: request.WorkingDir, StartupBindDeadlineSeconds: 1, Port: request.Port,
	}}}); err != nil {
		t.Fatal(err)
	}
	started := time.Now()
	err := admitFrozenStdioBridgeRequest(context.Background(), request)
	if elapsed := time.Since(started); elapsed > 1500*time.Millisecond {
		t.Fatalf("existing probe elapsed %s, want frozen one-second deadline instead of generic health timeout", elapsed)
	}
	if err == nil {
		t.Fatal("existing hanging probe succeeded")
	}
}

func TestFrozenStdioBridgeAdmissionRejectsMismatchedExistingGenerationBeforeSpawn(t *testing.T) {
	stateDir := hardenedTempDir(t)
	t.Cleanup(SetDaemonStateRootForTest(stateDir))
	request := FrozenStdioBridgeAdmissionRequest{
		ManifestName: "probe", DaemonName: "default", ManifestHash: "cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc",
		Command: "definitely-not-a-real-admission-command", Args: []string{"daemon"}, WorkingDir: t.TempDir(), Port: 19306, StartDeadline: time.Second,
	}
	if err := WriteSupervisorIntent(filepath.Join(stateDir, supervisorIntentFileLeaf), &SupervisorIntentFile{Version: 1, Daemons: []SupervisorDaemon{{
		Server: request.ManifestName, Daemon: request.DaemonName, ManifestHash: "dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd",
		Command: request.Command, Args: request.Args, Port: request.Port,
	}}}); err != nil {
		t.Fatal(err)
	}
	err := admitFrozenStdioBridgeRequest(context.Background(), request)
	if err == nil || !strings.Contains(err.Error(), "does not exactly match") {
		t.Fatalf("admit mismatched generation = %v, want exact-generation refusal", err)
	}
}

func admissionServerPort(t *testing.T, rawURL string) int {
	t.Helper()
	u, err := url.Parse(rawURL)
	if err != nil {
		t.Fatal(err)
	}
	_, rawPort, err := net.SplitHostPort(u.Host)
	if err != nil {
		t.Fatal(err)
	}
	port, err := strconv.Atoi(rawPort)
	if err != nil {
		t.Fatal(err)
	}
	return port
}

func reserveAdmissionPort(t *testing.T) int {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := listener.Addr().(*net.TCPAddr).Port
	if err := listener.Close(); err != nil {
		t.Fatal(err)
	}
	return port
}

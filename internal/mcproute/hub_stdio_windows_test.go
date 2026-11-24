package mcproute

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"mcp-local-hub/internal/api"
	"mcp-local-hub/internal/api/apitest"
	"mcp-local-hub/internal/config"
	"mcp-local-hub/internal/daemon"
)

const hubStdioFixture = "MCPHUB_TEST_HUB_PRIVATE_FIXTURE"
const hubStdioEOFURL = "MCPHUB_TEST_HUB_PRIVATE_EOF_URL"

// A real child with process-local state. The EOF acknowledgement is a separate
// test-owned channel; waiting for it does not infer cleanup from an HTTP 204.
func TestHubStdioProviderFixture(t *testing.T) {
	if os.Getenv(hubStdioFixture) != "1" {
		return
	}
	count, branch := 0, ""
	scan := bufio.NewScanner(os.Stdin)
	for scan.Scan() {
		var request struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
			Params struct {
				Cursor    string `json:"cursor"`
				Arguments struct {
					Branch string `json:"branch"`
				} `json:"arguments"`
			} `json:"params"`
		}
		if json.Unmarshal(scan.Bytes(), &request) != nil {
			os.Exit(21)
		}
		var result string
		switch request.Method {
		case "initialize":
			result = `{"protocolVersion":"2025-11-25","capabilities":{"tools":{}}}`
		case "tools/list":
			if request.Params.Cursor == "" {
				result = `{"tools":[{"name":"advance","inputSchema":{"type":"object"}}],"nextCursor":"page-two"}`
			} else if request.Params.Cursor == "page-two" {
				result = `{"tools":[{"name":"inspect","inputSchema":{"type":"object"}}]}`
			} else {
				os.Exit(25)
			}
		case "tools/call":
			count++
			if request.Params.Arguments.Branch != "" {
				branch = request.Params.Arguments.Branch
			}
			encoded, _ := json.Marshal(branch)
			result = fmt.Sprintf(`{"content":[],"structuredContent":{"count":%d,"branch":%s,"pid":%d}}`, count, encoded, os.Getpid())
		}
		if result != "" {
			if _, err := fmt.Fprintf(os.Stdout, "{\"jsonrpc\":\"2.0\",\"id\":%s,\"result\":%s}\n", request.ID, result); err != nil {
				os.Exit(22)
			}
		}
	}
	if scan.Err() != nil {
		os.Exit(23)
	}
	client := &http.Client{Timeout: 2 * time.Second}
	response, err := client.Post(os.Getenv(hubStdioEOFURL), "text/plain", strings.NewReader(strconv.Itoa(os.Getpid())))
	if err != nil {
		os.Exit(24)
	}
	response.Body.Close()
	os.Exit(0)
}

func TestRegisteredHubStdioSessionsIsolateAndRetireOriginalProviders(t *testing.T) {
	stateDir := apitest.HardenedTempDir(t)
	t.Setenv("MCPHUB_STATE_DIR_OVERRIDE", stateDir)
	t.Cleanup(api.SetDaemonStateRootForTest(stateDir))
	manifestRoot := t.TempDir()
	t.Setenv("MCPHUB_MANIFEST_DIR_OVERRIDE", manifestRoot)
	t.Setenv("HOME", t.TempDir())
	t.Setenv("USERPROFILE", t.TempDir())
	t.Setenv("APPDATA", t.TempDir())
	t.Setenv("LOCALAPPDATA", t.TempDir())
	prior := api.LoadResolverSnapshot()
	t.Cleanup(func() { api.PublishResolverSnapshot(prior) })
	eof := make(chan int, 4)
	ack := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, err := io.ReadAll(io.LimitReader(r.Body, 32))
		pid, parseErr := strconv.Atoi(string(raw))
		if err != nil || parseErr != nil {
			w.WriteHeader(400)
			return
		}
		eof <- pid
		w.WriteHeader(http.StatusNoContent)
	}))
	defer ack.Close()
	var networkCalls atomic.Int32
	ordinary := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		networkCalls.Add(1)
		w.WriteHeader(http.StatusTeapot)
	}))
	defer ordinary.Close()
	_, portText, err := net.SplitHostPort(strings.TrimPrefix(ordinary.URL, "http://"))
	if err != nil {
		t.Fatal(err)
	}
	providerPort, err := strconv.Atoi(portText)
	if err != nil {
		t.Fatal(err)
	}
	manifest := fmt.Sprintf("name: isolation-fixture\nkind: global\ntransport: stdio-bridge\ncommand: fixture\ndaemons:\n  - name: default\n    port: %d\n    mcp_stdio_state: session-local\nclient_bindings:\n  - client: claude-code\n    daemon: default\n    url_path: /mcp\n  - client: cursor\n    daemon: default\n    url_path: /mcp\n", providerPort)
	manifestDir := filepath.Join(manifestRoot, "isolation-fixture")
	if err := os.MkdirAll(manifestDir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(manifestDir, "manifest.yaml"), []byte(manifest), 0600); err != nil {
		t.Fatal(err)
	}
	tokens, err := api.EnsureHubTokens([]string{"claude-code", "cursor"})
	if err != nil {
		t.Fatal(err)
	}
	probe, err := api.NewListenerWithSOExclusiveContext(t.Context(), "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := probe.Addr().(*net.TCPAddr).Port
	if err := probe.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := api.EnsureHubEndpoint(port, os.Getpid()); err != nil {
		t.Fatal(err)
	}
	identity, err := api.ObserveHubRouteStateDirectory(t.Context(), stateDir)
	if err != nil {
		t.Fatal(err)
	}
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	a := api.NewAPI()
	a.HealthStatusFn = func(api.StatusOpts) ([]api.DaemonStatus, error) { return nil, nil }
	// Scan uses Status independently of the health watcher. Supply its
	// existing observation seam; no installed supervisor belongs to this test.
	t.Cleanup(api.SetStatusInternalDialForTest(func(context.Context) ([]api.DaemonStatus, error) { return nil, nil }))
	lifetime, cancel := context.WithCancel(t.Context())
	defer cancel()
	listener, err := StartHubListener(t.Context(), a, HubListenerOptions{
		Lifetime: lifetime,
		PrepareStdio: func(ctx context.Context, _ *api.API, endpoint api.HubStdioEndpoint) (*daemon.StdioLaunchContext, daemon.HostConfig, error) {
			launch, err := daemon.CaptureStdioLaunchContext(ctx)
			if err != nil {
				return nil, daemon.HostConfig{}, err
			}
			parsed, err := launch.ParseManifest(strings.NewReader(manifest))
			if err != nil {
				return nil, daemon.HostConfig{}, err
			}
			if api.SessionLocalStdioEndpoint(parsed, &parsed.Daemons[0]) != endpoint {
				return nil, daemon.HostConfig{}, fmt.Errorf("fixture descriptor differs from registered manifest")
			}
			return launch, daemon.HostConfig{Command: exe, Args: []string{"-test.run=^TestHubStdioProviderFixture$"}, Env: map[string]string{hubStdioFixture: "1", hubStdioEOFURL: ack.URL}}, nil
		},
		BindExisting: func(ctx context.Context, validate func() error) (*api.HubMcpBindResult, error) {
			if err := validate(); err != nil {
				return nil, err
			}
			var result *api.HubMcpBindResult
			err := api.WithHubRouteState(ctx, stateDir, identity, func(h *api.HubRouteState) error {
				endpoint, err := h.ReadEndpoint()
				if err != nil {
					return err
				}
				row := api.HubRouteQualification{TransactionID: strings.Repeat("a", 32), SuccessorSHA256: strings.Repeat("b", 64), ScopeSHA256: strings.Repeat("c", 64), EndpointAuthoritySHA256: endpoint.AuthoritySHA256(), EndpointProvenanceSHA256: endpoint.ProvenanceSHA256()}
				if err := h.StageQualification("", endpoint, row); err != nil {
					return err
				}
				result, err = h.BindExistingHubMcpListener(endpoint, row)
				return err
			})
			return result, err
		},
	})
	if listener != nil {
		defer func() {
			ctx, release := context.WithTimeout(context.Background(), 10*time.Second)
			defer release()
			if outcome := listener.RetireOriginal(ctx); !outcome.Settled() || outcome.Err != nil {
				t.Errorf("original listener/provider retirement: %+v", outcome)
			}
		}()
	}
	if err != nil {
		t.Fatal(err)
	}
	endpoint := listener.Endpoint()
	client := &http.Client{Timeout: 10 * time.Second}
	request := func(scope, sid, method, body string) (int, string, []byte) {
		t.Helper()
		r, err := http.NewRequest(method, fmt.Sprintf("http://127.0.0.1:%d/clients/%s/mcp", endpoint.Port, scope), strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		r.Header.Set("Content-Type", "application/json")
		r.Header.Set("X-Mcphub-Hub-Token", tokens.Tokens[scope])
		r.Header.Set("X-Mcphub-Instance-Id", endpoint.InstanceID)
		if sid != "" {
			r.Header.Set("Mcp-Session-Id", sid)
			r.Header.Set("MCP-Protocol-Version", "2025-11-25")
		}
		response, err := client.Do(r)
		if err != nil {
			t.Fatal(err)
		}
		defer response.Body.Close()
		raw, err := io.ReadAll(io.LimitReader(response.Body, 1<<20))
		if err != nil {
			t.Fatal(err)
		}
		return response.StatusCode, response.Header.Get("Mcp-Session-Id"), raw
	}
	initialize := func(scope string) string {
		t.Helper()
		status, sid, body := request(scope, "", http.MethodPost, `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-11-25"}}`)
		if status != 200 || sid == "" {
			t.Fatalf("%s initialize: %d %s", scope, status, body)
		}
		for attempt := 0; attempt < 2; attempt++ {
			status, _, body = request(scope, sid, http.MethodPost, `{"jsonrpc":"2.0","id":2,"method":"tools/list"}`)
			if status != 200 || !bytes.Contains(body, []byte(`isolation-fixture__advance`)) || !bytes.Contains(body, []byte(`isolation-fixture__inspect`)) {
				t.Fatalf("%s registered paginated tools, attempt %d: %d %s", scope, attempt, status, body)
			}
		}
		return sid
	}
	aSID, bSID := initialize("claude-code"), initialize("cursor")
	if aSID == bSID {
		t.Fatal("front sessions reused one identity")
	}
	call := func(scope, sid, branch string, count int) int {
		t.Helper()
		body := fmt.Sprintf(`{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"isolation-fixture__advance","arguments":{"branch":%q}}}`, branch)
		status, _, raw := request(scope, sid, http.MethodPost, body)
		var result struct {
			Result struct {
				Structured struct {
					Count  int    `json:"count"`
					Branch string `json:"branch"`
					PID    int    `json:"pid"`
				} `json:"structuredContent"`
			} `json:"result"`
		}
		if err := json.Unmarshal(raw, &result); err != nil {
			t.Fatal(err)
		}
		if status != 200 || result.Result.Structured.Count != count || result.Result.Structured.Branch != branch || result.Result.Structured.PID <= 0 {
			t.Fatalf("%s private state: %d %s", scope, status, raw)
		}
		return result.Result.Structured.PID
	}
	aPID := call("claude-code", aSID, "branch-a", 1)
	bPID := call("cursor", bSID, "branch-b", 1)
	if aPID == bPID {
		t.Fatal("distinct front sessions shared a provider process")
	}
	var originals []*daemon.IsolatedStdioRetirement
	for _, provider := range listener.stdio.providers {
		originals = append(originals, provider.Retirements()...)
	}
	if len(originals) != 2 {
		t.Fatalf("original provider receipts=%d", len(originals))
	}
	type originalResult struct {
		receipt daemon.StdioHostSettlement
		err     error
	}
	receipts := make(chan originalResult, len(originals))
	waitCtx, stopWait := context.WithTimeout(context.Background(), 30*time.Second)
	var waiters sync.WaitGroup
	defer func() { stopWait(); waiters.Wait() }()
	for _, original := range originals {
		waiters.Add(1)
		go func() {
			defer waiters.Done()
			receipt, err := original.Await(waitCtx)
			receipts <- originalResult{receipt, err}
		}()
	}
	if got := call("claude-code", aSID, "branch-a", 2); got != aPID {
		t.Fatal("A continuation changed provider")
	}
	if status, _, _ := request("cursor", aSID, http.MethodPost, `{"jsonrpc":"2.0","id":4,"method":"ping"}`); status != 401 {
		t.Fatalf("cross-client SID status=%d", status)
	}
	if status, _, raw := request("claude-code", aSID, http.MethodDelete, ""); status != 204 {
		t.Fatalf("A DELETE: %d %s", status, raw)
	}
	select {
	case pid := <-eof:
		if pid != aPID {
			t.Fatalf("A retirement received another provider: %d", pid)
		}
	case <-time.After(8 * time.Second):
		t.Fatal("A original provider did not receive EOF")
	}
	select {
	case original := <-receipts:
		if original.err != nil || !original.receipt.Complete() || !original.receipt.RootWait.Known || !original.receipt.OriginalIdentity.Known || original.receipt.OriginalIdentity.PID != aPID {
			t.Fatalf("A original process/stream settlement: %+v / %v", original.receipt, original.err)
		}
	case <-waitCtx.Done():
		t.Fatal("A original native receipt was not received")
	}
	if got := call("cursor", bSID, "branch-b", 2); got != bPID {
		t.Fatal("B provider replaced after A teardown")
	}
	// The captured launch cannot be redirected after a manifest edit, even
	// though the server/daemon names and configured port remain unchanged.
	currentSnapshot := api.LoadResolverSnapshot()
	changed, err := config.ParseManifest(strings.NewReader(strings.Replace(manifest, "command: fixture", "command: changed-fixture", 1)))
	if err != nil {
		t.Fatal(err)
	}
	api.BumpResolverOnManifestChange([]config.ServerManifest{*changed})
	status, _, refused := request("cursor", bSID, http.MethodPost, `{"jsonrpc":"2.0","id":4,"method":"tools/call","params":{"name":"isolation-fixture__advance","arguments":{"branch":"forbidden"}}}`)
	if status != 200 || !bytes.Contains(refused, []byte(`"error"`)) {
		t.Fatalf("changed descriptor dispatched: %d %s", status, refused)
	}
	api.PublishResolverSnapshot(currentSnapshot)
	if got := call("cursor", bSID, "branch-b", 3); got != bPID {
		t.Fatal("descriptor refusal replaced B's original provider")
	}
	// A fresh token can authenticate the front, but cannot adopt the old
	// token's already-open private conversation. Observed revocation retires it.
	tokens, err = api.RotateHubToken("cursor")
	if err != nil {
		t.Fatal(err)
	}
	status, _, refused = request("cursor", bSID, http.MethodPost, `{"jsonrpc":"2.0","id":5,"method":"tools/call","params":{"name":"isolation-fixture__advance","arguments":{"branch":"forbidden"}}}`)
	if status != 200 || !bytes.Contains(refused, []byte(`"error"`)) {
		t.Fatalf("rotated credential adopted old private state: %d %s", status, refused)
	}
	select {
	case original := <-receipts:
		if original.err != nil || !original.receipt.Complete() || !original.receipt.RootWait.Known || !original.receipt.OriginalIdentity.Known || original.receipt.OriginalIdentity.PID != bPID {
			t.Fatalf("B credential revocation settlement: %+v / %v", original.receipt, original.err)
		}
	case <-waitCtx.Done():
		t.Fatal("B original receipt after credential revocation was not received")
	}
	ctx, release := context.WithTimeout(context.Background(), 10*time.Second)
	defer release()
	if outcome := listener.RetireOriginal(ctx); !outcome.Settled() || outcome.Err != nil {
		t.Fatalf("original children/streams not settled: %+v", outcome)
	}
	select {
	case pid := <-eof:
		if pid != bPID {
			t.Fatalf("B retirement received another provider: %d", pid)
		}
	default:
		t.Fatal("B native settlement preceded its original EOF acknowledgement")
	}
	if calls := networkCalls.Load(); calls != 0 {
		t.Fatalf("private route fell through to ordinary TCP: %d requests", calls)
	}
}

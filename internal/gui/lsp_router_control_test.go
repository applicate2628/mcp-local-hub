package gui

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"mcp-local-hub/internal/api"
)

type fakeLSPRouterControlAPI struct {
	disabled map[string]bool
	statuses []api.LSPRouterClientStatus

	setNames       []string
	rollbackClient string
	rollbackOpts   api.LSPClientRouterOpts
	ensureOpts     api.LSPClientRouterOpts
	disableClient  string
	disableOpts    api.LSPClientRouterOpts
	enableClient   string
	enableOpts     api.LSPClientRouterOpts
}

func (f *fakeLSPRouterControlAPI) LSPRouterDisabledClientSet() (map[string]bool, error) {
	out := map[string]bool{}
	for name, disabled := range f.disabled {
		out[name] = disabled
	}
	return out, nil
}

func (f *fakeLSPRouterControlAPI) SetLSPRouterDisabledClients(names []string) error {
	f.setNames = append([]string(nil), names...)
	return nil
}

func (f *fakeLSPRouterControlAPI) DisableLSPRouterClient(clientName string, opts api.LSPClientRouterOpts) (*api.LSPClientRouterReport, error) {
	f.disableClient = clientName
	f.disableOpts = opts
	return &api.LSPClientRouterReport{
		Removed: []api.LSPClientRouterChange{{Client: clientName, Language: "python", EntryName: "mcp-language-server-python"}},
	}, nil
}

func (f *fakeLSPRouterControlAPI) EnableLSPRouterClient(clientName string, opts api.LSPClientRouterOpts) (*api.LSPClientRouterReport, error) {
	f.enableClient = clientName
	f.enableOpts = opts
	return &api.LSPClientRouterReport{
		Applied: []api.LSPClientRouterChange{{Client: clientName, Language: "python", EntryName: "mcp-language-server-python"}},
	}, nil
}

func (f *fakeLSPRouterControlAPI) LSPRouterClientStatuses(opts api.LSPClientRouterOpts) ([]api.LSPRouterClientStatus, error) {
	f.ensureOpts = opts
	out := append([]api.LSPRouterClientStatus(nil), f.statuses...)
	return out, nil
}

func (f *fakeLSPRouterControlAPI) RollbackLSPRouterClientEntriesForClient(clientName string, opts api.LSPClientRouterOpts) (*api.LSPClientRouterReport, error) {
	f.rollbackClient = clientName
	f.rollbackOpts = opts
	return &api.LSPClientRouterReport{
		Removed: []api.LSPClientRouterChange{{Client: clientName, Language: "python", EntryName: "mcp-language-server-python"}},
	}, nil
}

func (f *fakeLSPRouterControlAPI) EnsureLSPRouterClientEntries(opts api.LSPClientRouterOpts) (*api.LSPClientRouterReport, error) {
	f.ensureOpts = opts
	return &api.LSPClientRouterReport{
		Applied: []api.LSPClientRouterChange{{Client: opts.ForceClientName, Language: "python", EntryName: "mcp-language-server-python"}},
	}, nil
}

func withFakeLSPRouterControlAPI(t *testing.T, fake *fakeLSPRouterControlAPI) *Server {
	t.Helper()
	// The control API itself is faked, but the handler still resolves the target
	// through lspRouterControlClientAdapter → clients.AllClients()
	// (lsp_router_control.go:102, :126), which constructs the whole registry.
	sandboxClientConfigHome(t)
	orig := lspRouterControlAPIFactory
	lspRouterControlAPIFactory = func() lspRouterControlAPI { return fake }
	t.Cleanup(func() { lspRouterControlAPIFactory = orig })
	return NewServer(Config{Port: 7777})
}

func postLSPRouterControl(t *testing.T, s *Server, path, client string) *httptest.ResponseRecorder {
	t.Helper()
	body, err := json.Marshal(map[string]string{"client": client})
	if err != nil {
		t.Fatalf("marshal request: %v", err)
	}
	req := httptest.NewRequest(http.MethodPost, path, bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	s.mux.ServeHTTP(rec, req)
	return rec
}

func TestLSPRouterDisableRoutePersistsOptOutAndRollsBackTargetClient(t *testing.T) {
	fake := &fakeLSPRouterControlAPI{disabled: map[string]bool{"claude-code": true}}
	s := withFakeLSPRouterControlAPI(t, fake)

	rec := postLSPRouterControl(t, s, "/api/lsp-router/disable", "codex-cli")

	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%q", rec.Code, rec.Body.String())
	}
	if fake.disableClient != "codex-cli" {
		t.Fatalf("disable client = %q, want codex-cli", fake.disableClient)
	}
	if fake.disableOpts.GUIPort != 0 {
		t.Fatalf("disable GUIPort = %d, want unstarted server port 0", fake.disableOpts.GUIPort)
	}
	var resp lspRouterControlResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v body=%q", err, rec.Body.String())
	}
	if resp.Client != "codex-cli" || resp.Enabled {
		t.Fatalf("response = %+v, want client codex-cli enabled=false", resp)
	}
	if resp.Report == nil || len(resp.Report.Removed) != 1 {
		t.Fatalf("response report = %+v, want one removed row", resp.Report)
	}
}

func TestLSPRouterEnableRouteClearsOptOutAndForcesEnsureForTargetClient(t *testing.T) {
	fake := &fakeLSPRouterControlAPI{disabled: map[string]bool{"claude-code": true, "codex-cli": true}}
	s := withFakeLSPRouterControlAPI(t, fake)

	rec := postLSPRouterControl(t, s, "/api/lsp-router/enable", "codex-cli")

	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%q", rec.Code, rec.Body.String())
	}
	if fake.enableClient != "codex-cli" {
		t.Fatalf("enable client = %q, want codex-cli", fake.enableClient)
	}
	if fake.enableOpts.GUIPort != 0 {
		t.Fatalf("enable GUIPort = %d, want unstarted server port 0", fake.enableOpts.GUIPort)
	}
	var resp lspRouterControlResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v body=%q", err, rec.Body.String())
	}
	if resp.Client != "codex-cli" || !resp.Enabled {
		t.Fatalf("response = %+v, want client codex-cli enabled=true", resp)
	}
	if resp.Report == nil || len(resp.Report.Applied) != 1 {
		t.Fatalf("response report = %+v, want one applied row", resp.Report)
	}
}

func TestLSPRouterStatusRouteReportsPersistedClientOptOut(t *testing.T) {
	fake := &fakeLSPRouterControlAPI{
		statuses: []api.LSPRouterClientStatus{
			{
				Client:          "codex-cli",
				ConfigPath:      "/tmp/codex/config.toml",
				Disabled:        true,
				ExistingEntries: nil,
				MissingEntries:  []string{"mcp-language-server-python"},
			},
		},
	}
	s := withFakeLSPRouterControlAPI(t, fake)

	req := httptest.NewRequest(http.MethodGet, "/api/lsp-router/status", nil)
	rec := httptest.NewRecorder()
	s.mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%q", rec.Code, rec.Body.String())
	}
	if fake.ensureOpts.GUIPort != 0 {
		t.Fatalf("status GUIPort = %d, want unstarted server port 0", fake.ensureOpts.GUIPort)
	}
	var resp lspRouterStatusResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v body=%q", err, rec.Body.String())
	}
	if len(resp.Clients) != 1 {
		t.Fatalf("clients=%+v, want one status", resp.Clients)
	}
	got := resp.Clients[0]
	if got.Client != "codex-cli" || !got.Disabled || got.ConfigPath != "/tmp/codex/config.toml" {
		t.Fatalf("client status=%+v, want codex-cli disabled with config path", got)
	}
	if len(got.MissingEntries) != 1 || got.MissingEntries[0] != "mcp-language-server-python" {
		t.Fatalf("missing entries=%+v", got.MissingEntries)
	}
}

package gui

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"mcp-local-hub/internal/api"
)

type fakeClientInstallPrefsAPI struct {
	view func() (api.ClientInstallToggleSnapshot, error)
	set  func([]string) error
}

func (f fakeClientInstallPrefsAPI) ClientInstallToggleView() (api.ClientInstallToggleSnapshot, error) {
	return f.view()
}

func (f fakeClientInstallPrefsAPI) SetDefaultInstallClientNames(names []string) error {
	return f.set(names)
}

// clientInstallPrefsTestServer retains production route registration and
// supplies only this Server's preference dependency. No real preferences
// file is touched, and other servers retain their own dependencies.
func clientInstallPrefsTestServer(
	t *testing.T,
	view func() (api.ClientInstallToggleSnapshot, error),
	set func([]string) error,
) *Server {
	t.Helper()
	s := newEphemeralServer(t, Config{})
	s.clientInstallPrefs = fakeClientInstallPrefsAPI{view: view, set: set}
	return s
}

func sampleSnapshot(overrideActive bool, selected map[string]bool) api.ClientInstallToggleSnapshot {
	return api.ClientInstallToggleSnapshot{
		OverrideActive: overrideActive,
		Rows: []api.ClientInstallToggleRow{
			{Name: "claude-code", CompileDefault: true, Selected: selected["claude-code"]},
			{Name: "codex-cli", CompileDefault: true, Selected: selected["codex-cli"]},
			{Name: "cursor", CompileDefault: false, Selected: selected["cursor"]},
			{Name: "vscode", CompileDefault: false, Selected: selected["vscode"]},
		},
	}
}

func decodeClientPrefsResp(t *testing.T, rec *httptest.ResponseRecorder) clientInstallPrefsResponse {
	t.Helper()
	var resp clientInstallPrefsResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response %q: %v", rec.Body.String(), err)
	}
	return resp
}

func TestClientInstallPrefs_UsesServerAPI(t *testing.T) {
	s := newEphemeralServer(t, Config{})
	if s.clientInstallPrefs != s.api {
		t.Fatal("client-install preferences must use the Server's API")
	}
}

func TestClientInstallPrefs_ServersKeepIndependentDependencies(t *testing.T) {
	newServer := func(selectedName string) *Server {
		selected := map[string]bool{selectedName: true}
		return clientInstallPrefsTestServer(t,
			func() (api.ClientInstallToggleSnapshot, error) {
				return sampleSnapshot(true, selected), nil
			},
			func(names []string) error {
				selected = map[string]bool{}
				for _, name := range names {
					selected[name] = true
				}
				return nil
			},
		)
	}
	first := newServer("cursor")
	second := newServer("vscode")

	// Interleave both servers after both dependencies have been installed.
	// Each POST must read back the same server's updated state, while the
	// other server retains its prior selection.
	steps := []struct {
		name     string
		server   *Server
		method   string
		body     string
		selected string
	}{
		{"first initial read", first, http.MethodGet, "", "cursor"},
		{"second initial read", second, http.MethodGet, "", "vscode"},
		{"first mutation", first, http.MethodPost, `{"clients":["claude-code"]}`, "claude-code"},
		{"second after first mutation", second, http.MethodGet, "", "vscode"},
		{"second mutation", second, http.MethodPost, `{"clients":["codex-cli"]}`, "codex-cli"},
		{"first after second mutation", first, http.MethodGet, "", "claude-code"},
	}
	for _, step := range steps {
		t.Run(step.name, func(t *testing.T) {
			req := httptest.NewRequest(step.method, "/api/client-install-prefs", bytes.NewBufferString(step.body))
			rec := httptest.NewRecorder()
			step.server.mux.ServeHTTP(rec, req)
			if rec.Code != http.StatusOK {
				t.Fatalf("status=%d body=%q", rec.Code, rec.Body.String())
			}
			resp := decodeClientPrefsResp(t, rec)
			if !resp.OverrideActive || len(resp.Clients) != 4 {
				t.Fatalf("response = %+v, want an active override with four client rows", resp)
			}
			selected := []string{}
			for _, client := range resp.Clients {
				if client.Selected {
					selected = append(selected, client.Name)
				}
			}
			if len(selected) != 1 || selected[0] != step.selected {
				t.Fatalf("selected=%v, want [%s]", selected, step.selected)
			}
		})
	}
}

func TestClientInstallPrefs_GetDefault(t *testing.T) {
	s := clientInstallPrefsTestServer(t,
		func() (api.ClientInstallToggleSnapshot, error) {
			return sampleSnapshot(false, map[string]bool{
				"claude-code": true, "codex-cli": true,
			}), nil
		},
		nil,
	)
	req := httptest.NewRequest(http.MethodGet, "/api/client-install-prefs", nil)
	rec := httptest.NewRecorder()
	s.mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%q", rec.Code, rec.Body.String())
	}
	resp := decodeClientPrefsResp(t, rec)
	if resp.Clients == nil {
		t.Fatal("clients must be a non-nil array")
	}
	if resp.OverrideActive {
		t.Fatal("override_active = true, want false for default snapshot")
	}
	if len(resp.Clients) != 4 {
		t.Fatalf("clients = %d, want 4", len(resp.Clients))
	}
	// claude-code is compile-default + selected; vscode is neither.
	byName := map[string]clientInstallPrefsClientDTO{}
	for _, c := range resp.Clients {
		byName[c.Name] = c
	}
	if !byName["claude-code"].CompileDefault || !byName["claude-code"].Selected {
		t.Fatalf("claude-code = %+v, want compile_default+selected", byName["claude-code"])
	}
	if byName["vscode"].CompileDefault || byName["vscode"].Selected {
		t.Fatalf("vscode = %+v, want neither compile_default nor selected", byName["vscode"])
	}
	if byName["cursor"].CompileDefault || byName["cursor"].Selected {
		t.Fatalf("cursor = %+v, want neither compile_default nor selected", byName["cursor"])
	}
}

func TestClientInstallPrefs_PostPersistsAndReturnsSnapshot(t *testing.T) {
	var gotNames []string
	persisted := map[string]bool{}
	s := clientInstallPrefsTestServer(t,
		func() (api.ClientInstallToggleSnapshot, error) {
			return sampleSnapshot(len(persisted) > 0, persisted), nil
		},
		func(names []string) error {
			gotNames = names
			persisted = map[string]bool{}
			for _, n := range names {
				persisted[n] = true
			}
			return nil
		},
	)
	body, _ := json.Marshal(map[string]any{"clients": []string{"claude-code", "vscode"}})
	req := httptest.NewRequest(http.MethodPost, "/api/client-install-prefs", bytes.NewReader(body))
	rec := httptest.NewRecorder()
	s.mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%q", rec.Code, rec.Body.String())
	}
	if len(gotNames) != 2 || gotNames[0] != "claude-code" || gotNames[1] != "vscode" {
		t.Fatalf("Set received %v, want [claude-code vscode]", gotNames)
	}
	resp := decodeClientPrefsResp(t, rec)
	if !resp.OverrideActive {
		t.Fatal("override_active = false, want true after a successful POST")
	}
	byName := map[string]clientInstallPrefsClientDTO{}
	for _, c := range resp.Clients {
		byName[c.Name] = c
	}
	if !byName["vscode"].Selected || !byName["claude-code"].Selected {
		t.Fatalf("post-save selection wrong: %+v", resp.Clients)
	}
	if byName["cursor"].Selected {
		t.Fatalf("cursor should be deselected after override [claude-code vscode]: %+v", byName["cursor"])
	}
}

func TestClientInstallPrefs_PostUnknownClientIs400(t *testing.T) {
	s := clientInstallPrefsTestServer(t,
		nil,
		func(names []string) error {
			return stubUnknownClientErr()
		},
	)
	body, _ := json.Marshal(map[string]any{"clients": []string{"ghost"}})
	req := httptest.NewRequest(http.MethodPost, "/api/client-install-prefs", bytes.NewReader(body))
	rec := httptest.NewRecorder()
	s.mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status=%d body=%q, want 400 for unknown client", rec.Code, rec.Body.String())
	}
}

func TestClientInstallPrefs_PostEmptySetIs400(t *testing.T) {
	s := clientInstallPrefsTestServer(t,
		nil,
		func(names []string) error {
			return stubAtLeastOneClientErr()
		},
	)
	body, _ := json.Marshal(map[string]any{"clients": []string{}})
	req := httptest.NewRequest(http.MethodPost, "/api/client-install-prefs", bytes.NewReader(body))
	rec := httptest.NewRecorder()
	s.mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status=%d body=%q, want 400 for empty set", rec.Code, rec.Body.String())
	}
}

func TestClientInstallPrefs_MethodNotAllowed(t *testing.T) {
	s := clientInstallPrefsTestServer(t,
		func() (api.ClientInstallToggleSnapshot, error) { return sampleSnapshot(false, nil), nil },
		nil,
	)
	req := httptest.NewRequest(http.MethodDelete, "/api/client-install-prefs", nil)
	rec := httptest.NewRecorder()
	s.mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status=%d, want 405 for DELETE", rec.Code)
	}
	if allow := rec.Header().Get("Allow"); allow != "GET, POST" {
		t.Fatalf("Allow=%q, want 'GET, POST'", allow)
	}
}

func TestClientInstallPrefs_CrossOriginRejected(t *testing.T) {
	s := clientInstallPrefsTestServer(t,
		func() (api.ClientInstallToggleSnapshot, error) { return sampleSnapshot(false, nil), nil },
		nil,
	)
	req := httptest.NewRequest(http.MethodGet, "/api/client-install-prefs", nil)
	req.Header.Set("Sec-Fetch-Site", "cross-site")
	rec := httptest.NewRecorder()
	s.mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status=%d, want 403 for cross-site request", rec.Code)
	}
}

// stubUnknownClientErr / stubAtLeastOneClientErr produce error strings whose
// substrings the handler matches to map to 400 — kept here so the test pins
// the exact wording contract the handler depends on.
func stubUnknownClientErr() error { return &stubErr{"unknown client \"ghost\" (expected ...)"} }
func stubAtLeastOneClientErr() error {
	return &stubErr{"default-install client set must name at least one supported client"}
}

type stubErr struct{ msg string }

func (e *stubErr) Error() string { return e.msg }

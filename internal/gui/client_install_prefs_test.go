package gui

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"mcp-local-hub/internal/api"
)

// clientInstallPrefsTestServer wires a fresh Server with the
// /api/client-install-prefs routes registered (production registration is
// wired centrally; this test registers them directly on the test mux) and
// stubs the api round-trip so no real gui-preferences.yaml is touched. The
// returned cleanup restores the package-level seams.
func clientInstallPrefsTestServer(
	t *testing.T,
	view func() (api.ClientInstallToggleSnapshot, error),
	set func([]string) error,
) *Server {
	t.Helper()
	origView, origSet := clientInstallPrefsViewFn, clientInstallPrefsSetFn
	clientInstallPrefsViewFn = view
	clientInstallPrefsSetFn = set
	t.Cleanup(func() {
		clientInstallPrefsViewFn = origView
		clientInstallPrefsSetFn = origSet
	})
	s := newEphemeralServer(t, Config{})
	return s
}

func sampleSnapshot(overrideActive bool, selected map[string]bool) api.ClientInstallToggleSnapshot {
	return api.ClientInstallToggleSnapshot{
		OverrideActive: overrideActive,
		Rows: []api.ClientInstallToggleRow{
			{Name: "claude-code", CompileDefault: true, Selected: selected["claude-code"]},
			{Name: "codex-cli", CompileDefault: true, Selected: selected["codex-cli"]},
			{Name: "cursor", CompileDefault: true, Selected: selected["cursor"]},
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

func TestClientInstallPrefs_GetDefault(t *testing.T) {
	s := clientInstallPrefsTestServer(t,
		func() (api.ClientInstallToggleSnapshot, error) {
			return sampleSnapshot(false, map[string]bool{
				"claude-code": true, "codex-cli": true, "cursor": true,
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

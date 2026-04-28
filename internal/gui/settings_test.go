package gui

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"mcp-local-hub/internal/api"
)

// fakeSettings is the test seam.
type fakeSettings struct {
	values     map[string]string
	setErr     error
	openErr    error
	openCalled string
	path       string // configurable for first-run path tests; defaults to /tmp/test/gui-preferences.yaml
}

func (f *fakeSettings) List() (map[string]string, error) {
	if f.values == nil {
		f.values = map[string]string{}
	}
	out := map[string]string{}
	for _, def := range api.SettingsRegistry {
		if def.Type == api.TypeAction {
			continue
		}
		if v, ok := f.values[def.Key]; ok {
			out[def.Key] = v
		} else {
			out[def.Key] = def.Default
		}
	}
	return out, nil
}

func (f *fakeSettings) Set(key, value string) error {
	if f.setErr != nil {
		return f.setErr
	}
	if f.values == nil {
		f.values = map[string]string{}
	}
	f.values[key] = value
	return nil
}

func (f *fakeSettings) SettingsPath() string {
	if f.path != "" {
		return f.path
	}
	return "/tmp/test/gui-preferences.yaml"
}

func (f *fakeSettings) OpenPath(path string) error {
	f.openCalled = path
	return f.openErr
}

func newTestServer(t *testing.T) (*Server, *fakeSettings) {
	t.Helper()
	s := NewServer(Config{Port: 9125, Version: "test", PID: 1})
	// Seed the live port atomic so s.Port() returns a non-zero value in
	// tests that don't call Start (which is where the atomic is normally set).
	s.port.Store(9125)
	fake := &fakeSettings{}
	s.settings = fake
	return s, fake
}

func sameOriginHeaders() http.Header {
	h := http.Header{}
	h.Set("Sec-Fetch-Site", "same-origin")
	return h
}

func TestSettings_GET_Snapshot(t *testing.T) {
	s, _ := newTestServer(t)
	req := httptest.NewRequest("GET", "/api/settings", nil)
	req.Header = sameOriginHeaders()
	rr := httptest.NewRecorder()
	s.mux.ServeHTTP(rr, req)
	if rr.Code != 200 {
		t.Fatalf("got %d: %s", rr.Code, rr.Body.String())
	}
	var resp struct {
		Settings   []map[string]any `json:"settings"`
		ActualPort int              `json:"actual_port"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp.ActualPort == 0 {
		t.Error("actual_port must be set (memo §6.1, Codex r1 P2.4)")
	}
	if len(resp.Settings) != len(api.SettingsRegistry) {
		t.Errorf("expected %d entries, got %d", len(api.SettingsRegistry), len(resp.Settings))
	}
}

func TestSettings_GET_ConfigEntriesAlwaysIncludeDefaultAndValue(t *testing.T) {
	// Codex r7 P2: regression for r6 — config entries with empty default/value
	// (most importantly appearance.default_home with Default:"", Optional:true)
	// MUST include both `default` and `value` keys in the JSON, otherwise the
	// frontend ConfigSettingDTO contract breaks.
	s, _ := newTestServer(t)
	req := httptest.NewRequest("GET", "/api/settings", nil)
	req.Header = sameOriginHeaders()
	rr := httptest.NewRecorder()
	s.mux.ServeHTTP(rr, req)
	var resp struct {
		Settings []map[string]any `json:"settings"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	var entry map[string]any
	for _, e := range resp.Settings {
		if e["key"] == "appearance.default_home" {
			entry = e
			break
		}
	}
	if entry == nil {
		t.Fatal("appearance.default_home missing from snapshot")
	}
	if entry["type"] != "path" {
		t.Errorf("expected type=path, got %v", entry["type"])
	}
	if _, has := entry["default"]; !has {
		t.Error("default_home must include 'default' key in JSON (Codex r7 P2)")
	}
	if _, has := entry["value"]; !has {
		t.Error("default_home must include 'value' key in JSON (Codex r7 P2)")
	}
	// And both should be empty strings (since Optional:true and no user value).
	if entry["default"] != "" {
		t.Errorf("expected default=\"\", got %v", entry["default"])
	}
	if entry["value"] != "" {
		t.Errorf("expected value=\"\", got %v", entry["value"])
	}
}

func TestSettings_GET_ActionEntriesOmitValueDefault(t *testing.T) {
	// Codex r1 P2.2: action entries MUST not include value or default
	// in the JSON wire shape.
	s, _ := newTestServer(t)
	req := httptest.NewRequest("GET", "/api/settings", nil)
	req.Header = sameOriginHeaders()
	rr := httptest.NewRecorder()
	s.mux.ServeHTTP(rr, req)
	var resp struct {
		Settings []map[string]any `json:"settings"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	for _, e := range resp.Settings {
		if e["type"] == "action" {
			if _, has := e["value"]; has {
				t.Errorf("action %q must not have 'value' in JSON", e["key"])
			}
			if _, has := e["default"]; has {
				t.Errorf("action %q must not have 'default' in JSON", e["key"])
			}
		}
	}
}

func TestSettings_PUT_ValidWrite(t *testing.T) {
	s, fake := newTestServer(t)
	body := strings.NewReader(`{"value":"dark"}`)
	req := httptest.NewRequest("PUT", "/api/settings/appearance.theme", body)
	req.Header = sameOriginHeaders()
	rr := httptest.NewRecorder()
	s.mux.ServeHTTP(rr, req)
	if rr.Code != 200 {
		t.Fatalf("got %d: %s", rr.Code, rr.Body.String())
	}
	if fake.values["appearance.theme"] != "dark" {
		t.Errorf("expected dark stored, got %q", fake.values["appearance.theme"])
	}
}

func TestSettings_PUT_InvalidEnum(t *testing.T) {
	s, fake := newTestServer(t)
	// Simulate the real api.SettingsSet error message format.
	fake.setErr = errString("invalid value for appearance.theme: not in enum [light dark system]")
	body := strings.NewReader(`{"value":"puce"}`)
	req := httptest.NewRequest("PUT", "/api/settings/appearance.theme", body)
	req.Header = sameOriginHeaders()
	rr := httptest.NewRecorder()
	s.mux.ServeHTTP(rr, req)
	if rr.Code != 400 {
		t.Fatalf("expected 400, got %d", rr.Code)
	}
	var resp map[string]any
	json.Unmarshal(rr.Body.Bytes(), &resp)
	if resp["error"] != "validation_failed" {
		t.Errorf("expected validation_failed, got %v", resp["error"])
	}
}

func TestSettings_PUT_UnknownKey(t *testing.T) {
	s, _ := newTestServer(t)
	req := httptest.NewRequest("PUT", "/api/settings/no.such.key", strings.NewReader(`{"value":"x"}`))
	req.Header = sameOriginHeaders()
	rr := httptest.NewRecorder()
	s.mux.ServeHTTP(rr, req)
	if rr.Code != 404 {
		t.Fatalf("expected 404, got %d", rr.Code)
	}
}

func TestSettings_PUT_ToActionKey_Returns405(t *testing.T) {
	s, _ := newTestServer(t)
	req := httptest.NewRequest("PUT", "/api/settings/advanced.open_app_data_folder", strings.NewReader(`{"value":"x"}`))
	req.Header = sameOriginHeaders()
	rr := httptest.NewRecorder()
	s.mux.ServeHTTP(rr, req)
	if rr.Code != 405 {
		t.Fatalf("expected 405, got %d", rr.Code)
	}
	if rr.Header().Get("Allow") != "POST" {
		t.Errorf("expected Allow: POST, got %q", rr.Header().Get("Allow"))
	}
}

func TestSettings_POST_ToConfigKey_Returns405(t *testing.T) {
	s, _ := newTestServer(t)
	req := httptest.NewRequest("POST", "/api/settings/appearance.theme", bytes.NewReader(nil))
	req.Header = sameOriginHeaders()
	rr := httptest.NewRecorder()
	s.mux.ServeHTTP(rr, req)
	if rr.Code != 405 {
		t.Fatalf("expected 405, got %d", rr.Code)
	}
	if rr.Header().Get("Allow") != "PUT" {
		t.Errorf("expected Allow: PUT, got %q", rr.Header().Get("Allow"))
	}
}

func TestSettings_POST_OpenAppDataFolder_Success(t *testing.T) {
	s, fake := newTestServer(t)
	req := httptest.NewRequest("POST", "/api/settings/advanced.open_app_data_folder", nil)
	req.Header = sameOriginHeaders()
	rr := httptest.NewRecorder()
	s.mux.ServeHTTP(rr, req)
	if rr.Code != 200 {
		t.Fatalf("got %d: %s", rr.Code, rr.Body.String())
	}
	if fake.openCalled == "" {
		t.Error("OpenPath not called")
	}
}

func TestSettings_POST_DeferredAction_Returns404(t *testing.T) {
	s, _ := newTestServer(t)
	req := httptest.NewRequest("POST", "/api/settings/backups.clean_now", nil)
	req.Header = sameOriginHeaders()
	rr := httptest.NewRecorder()
	s.mux.ServeHTTP(rr, req)
	if rr.Code != 404 {
		t.Fatalf("expected 404 deferred, got %d", rr.Code)
	}
	var resp map[string]any
	json.Unmarshal(rr.Body.Bytes(), &resp)
	if resp["error"] != "deferred_action_not_implemented" {
		t.Errorf("expected deferred_action_not_implemented, got %v", resp["error"])
	}
}

func TestSettings_AllRoutes_RejectCrossOrigin(t *testing.T) {
	// Codex r1 P2.5 + r2 P2.2: every settings route (read AND mutating)
	// must reject cross-origin browser requests.
	s, _ := newTestServer(t)
	cases := []struct{ method, path, body string }{
		{"GET", "/api/settings", ""},
		{"PUT", "/api/settings/appearance.theme", `{"value":"dark"}`},
		{"POST", "/api/settings/advanced.open_app_data_folder", ""},
	}
	for _, c := range cases {
		var body *strings.Reader
		if c.body != "" {
			body = strings.NewReader(c.body)
		} else {
			body = strings.NewReader("")
		}
		req := httptest.NewRequest(c.method, c.path, body)
		req.Header.Set("Sec-Fetch-Site", "cross-site")
		rr := httptest.NewRecorder()
		s.mux.ServeHTTP(rr, req)
		if rr.Code != 403 {
			t.Errorf("%s %s: expected 403 cross-origin, got %d", c.method, c.path, rr.Code)
		}
	}
}

func TestSettings_POST_OpenAppDataFolder_CreatesDirIfMissing(t *testing.T) {
	// Codex PR #20 r3 P2: first-run path where the app-data directory
	// doesn't exist yet must succeed (not 500). MkdirAll runs before
	// OpenPath so explorer.exe / xdg-open never sees a non-existent path.
	s, fake := newTestServer(t)
	realDir := t.TempDir()
	fake.path = filepath.Join(realDir, "subdir", "gui-preferences.yaml")
	// Sanity: subdir must not exist yet.
	if _, err := os.Stat(filepath.Join(realDir, "subdir")); !os.IsNotExist(err) {
		t.Fatal("test setup: subdir should not exist yet")
	}
	req := httptest.NewRequest("POST", "/api/settings/advanced.open_app_data_folder", nil)
	req.Header = sameOriginHeaders()
	rr := httptest.NewRecorder()
	s.mux.ServeHTTP(rr, req)
	if rr.Code != 200 {
		t.Fatalf("expected 200, got %d: %s", rr.Code, rr.Body.String())
	}
	// MkdirAll must have created the directory.
	if _, err := os.Stat(filepath.Join(realDir, "subdir")); err != nil {
		t.Errorf("expected MkdirAll to have created subdir, got %v", err)
	}
}

// errString turns a string into an error for fake.setErr.
type errString string

func (e errString) Error() string { return string(e) }

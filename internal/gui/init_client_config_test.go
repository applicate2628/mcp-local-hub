// internal/gui/init_client_config_test.go
package gui

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"mcp-local-hub/internal/clients"
)

type fakeClientInitializer struct {
	called string
	result *InitClientConfigResult
	err    error
}

func (f *fakeClientInitializer) Init(client string) (*InitClientConfigResult, error) {
	f.called = client
	return f.result, f.err
}

// TestInitClientConfig_HappyPath pins the success contract: a POST
// naming a known client whose parent directory exists returns 200 +
// the structured result with `created: true` when the stub was just
// written.
func TestInitClientConfig_HappyPath(t *testing.T) {
	fi := &fakeClientInitializer{
		result: &InitClientConfigResult{
			Client:  "vscode",
			Path:    `C:\Users\dima_\AppData\Roaming\Code\User\mcp.json`,
			Created: true,
		},
	}
	s := NewServer(Config{})
	s.clientInit = fi

	req := httptest.NewRequest(http.MethodPost, "/api/init-client-config",
		bytes.NewReader([]byte(`{"client":"vscode"}`)))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	s.mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%q", rec.Code, rec.Body.String())
	}
	if fi.called != "vscode" {
		t.Errorf("Init called with %q, want %q", fi.called, "vscode")
	}
	var resp InitClientConfigResult
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if resp.Client != "vscode" || !resp.Created {
		t.Errorf("response=%+v, want client=vscode created=true", resp)
	}
}

// TestInitClientConfig_IdempotentNoOp pins the second-click case:
// when the file already existed by the time InitEmpty saw it, the
// handler still returns 200 but Created=false so the frontend can
// suppress a duplicate "we initialized your config" toast.
func TestInitClientConfig_IdempotentNoOp(t *testing.T) {
	fi := &fakeClientInitializer{
		result: &InitClientConfigResult{
			Client:  "vscode",
			Path:    `dummy`,
			Created: false,
		},
	}
	s := NewServer(Config{})
	s.clientInit = fi

	req := httptest.NewRequest(http.MethodPost, "/api/init-client-config",
		bytes.NewReader([]byte(`{"client":"vscode"}`)))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	s.mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%q", rec.Code, rec.Body.String())
	}
	var resp InitClientConfigResult
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if resp.Created {
		t.Errorf("Created=true for idempotent retry, want false")
	}
}

// TestInitClientConfig_UnknownClient pins the 404 mapping. A typo or
// adversarial client name must not fan into arbitrary filesystem
// writes — the handler rejects before touching disk.
func TestInitClientConfig_UnknownClient(t *testing.T) {
	fi := &fakeClientInitializer{
		err: fmt.Errorf("%w: %q", errUnknownClient, "bogus"),
	}
	s := NewServer(Config{})
	s.clientInit = fi

	req := httptest.NewRequest(http.MethodPost, "/api/init-client-config",
		bytes.NewReader([]byte(`{"client":"bogus"}`)))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	s.mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404; body=%q", rec.Code, rec.Body.String())
	}
	var resp map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if resp["code"] != "UNKNOWN_CLIENT" {
		t.Errorf("code=%q, want UNKNOWN_CLIENT", resp["code"])
	}
}

// TestInitClientConfig_ParentMissing pins the 412 mapping. The
// frontend depends on this exact status to flip its scan state
// without seeding a stray `~/.cursor/` tree on a host where the
// client is genuinely not installed.
func TestInitClientConfig_ParentMissing(t *testing.T) {
	fi := &fakeClientInitializer{
		err: fmt.Errorf("%w: %s", errParentMissing, `C:\Nonexistent\Path\file.json`),
	}
	s := NewServer(Config{})
	s.clientInit = fi

	req := httptest.NewRequest(http.MethodPost, "/api/init-client-config",
		bytes.NewReader([]byte(`{"client":"vscode"}`)))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	s.mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusPreconditionFailed {
		t.Fatalf("status = %d, want 412; body=%q", rec.Code, rec.Body.String())
	}
	var resp map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if resp["code"] != "PARENT_MISSING" {
		t.Errorf("code=%q, want PARENT_MISSING", resp["code"])
	}
}

// TestInitClientConfig_GenericFailure pins the 500 fallthrough.
// Disk-write errors and other unanticipated failures must NOT be
// reported as 404/412 (which would suggest the operator can fix
// them by adjusting client install state).
func TestInitClientConfig_GenericFailure(t *testing.T) {
	fi := &fakeClientInitializer{
		err: errors.New("disk full"),
	}
	s := NewServer(Config{})
	s.clientInit = fi

	req := httptest.NewRequest(http.MethodPost, "/api/init-client-config",
		bytes.NewReader([]byte(`{"client":"vscode"}`)))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	s.mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", rec.Code)
	}
	var resp map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if resp["code"] != "INIT_FAILED" {
		t.Errorf("code=%q, want INIT_FAILED", resp["code"])
	}
}

// TestInitClientConfig_RejectsGET pins the method allowlist. CSRF
// hardening relies on POST-only + requireSameOrigin — a GET on this
// route must return 405 even when the client name is valid.
func TestInitClientConfig_RejectsGET(t *testing.T) {
	s := NewServer(Config{})
	s.clientInit = &fakeClientInitializer{}

	req := httptest.NewRequest(http.MethodGet, "/api/init-client-config?client=vscode", nil)
	rec := httptest.NewRecorder()
	s.mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("status = %d, want 405", rec.Code)
	}
}

// TestInitClientConfig_EmptyClientField pins the 400 mapping for an
// empty client field. The handler must reject before reaching the
// initializer — otherwise an empty-string lookup in
// `clients.AllClients()` returns nil and the path is ambiguous.
func TestInitClientConfig_EmptyClientField(t *testing.T) {
	s := NewServer(Config{})
	s.clientInit = &fakeClientInitializer{}

	req := httptest.NewRequest(http.MethodPost, "/api/init-client-config",
		bytes.NewReader([]byte(`{"client":""}`)))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	s.mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", rec.Code)
	}
}

// TestInitClientConfig_MalformedBody pins the 400 mapping for invalid
// JSON. The handler must not panic on a malformed body.
func TestInitClientConfig_MalformedBody(t *testing.T) {
	s := NewServer(Config{})
	s.clientInit = &fakeClientInitializer{}

	req := httptest.NewRequest(http.MethodPost, "/api/init-client-config",
		bytes.NewReader([]byte(`not json`)))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	s.mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", rec.Code)
	}
}

// TestRealClientInitializer_ParentMissingError exercises the real
// adapter against a path whose parent directory does not exist on
// disk. The Init call must return errParentMissing without touching
// the filesystem (no stub file is created).
//
// We can't easily redirect clients.AllClients() to a temp dir, so
// this test uses a custom Server that wraps realClientInitializer
// with a path override. Instead, we directly probe the predicate
// via the real adapter selected through clients.AllClients() and
// verify a missing parent is detected — using a sub-directory
// path under t.TempDir() that we know does not exist.
//
// Approach: we cannot mutate the adapter's path without exporting
// new test seams, so this test focuses on the OUTPUT contract of
// the real initializer by stubbing the clients.AllClients() lookup
// at the OS-level — HOME=t.TempDir() so vscode adapter binds to
// %APPDATA% / $HOME-derived paths that do not exist.
func TestRealClientInitializer_ParentMissingError(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)
	t.Setenv("USERPROFILE", tmp)
	t.Setenv("APPDATA", filepath.Join(tmp, "AppData", "Roaming"))
	t.Setenv("LOCALAPPDATA", filepath.Join(tmp, "AppData", "Local"))

	all := clients.AllClients()
	if _, ok := all["vscode"]; !ok {
		t.Skipf("vscode adapter not constructible in test env; got clients=%v", keys(all))
	}

	init := realClientInitializer{}
	_, err := init.Init("vscode")
	if !errors.Is(err, errParentMissing) {
		t.Errorf("err=%v, want wraps errParentMissing", err)
	}

	// Verify no stub file was written.
	vscodePath := all["vscode"].ConfigPath()
	if _, statErr := os.Stat(vscodePath); statErr == nil {
		t.Errorf("vscode config %s was created despite parent missing", vscodePath)
	}
}

// TestRealClientInitializer_RefusesStrictMode pins the v0.4.5
// deep-sec Lane C #1 closure: when MCPHUB_REQUIRE_SINGLE_USER_HOME=1
// is set, the Init endpoint refuses to seed an empty stub because
// the atomic create-new-only path bypasses the SecureWriteClientConfig
// pipeline that strict-mode operators explicitly opted into. The
// refusal maps to 412 STRICT_MODE_REFUSED at the HTTP layer; the
// underlying Init returns errStrictModeRefused so the handler
// switch statement can route it.
func TestRealClientInitializer_RefusesStrictMode(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)
	t.Setenv("USERPROFILE", tmp)
	t.Setenv("APPDATA", filepath.Join(tmp, "AppData", "Roaming"))
	t.Setenv("LOCALAPPDATA", filepath.Join(tmp, "AppData", "Local"))
	t.Setenv("MCPHUB_REQUIRE_SINGLE_USER_HOME", "1")

	all := clients.AllClients()
	vscode, ok := all["vscode"]
	if !ok {
		t.Skipf("vscode adapter not constructible in test env")
	}
	// Pre-create the parent so the parent-stat check passes; the
	// strict-mode refusal should fire BEFORE the parent stat to make
	// the gap diagnostic clearer.
	if err := os.MkdirAll(filepath.Dir(vscode.ConfigPath()), 0o755); err != nil {
		t.Fatalf("mkdir parent: %v", err)
	}

	init := realClientInitializer{}
	_, err := init.Init("vscode")
	if !errors.Is(err, errStrictModeRefused) {
		t.Errorf("err=%v, want wraps errStrictModeRefused", err)
	}

	// Verify NO stub was created — refusal must be honored before
	// any disk write.
	if _, statErr := os.Stat(vscode.ConfigPath()); statErr == nil {
		t.Errorf("strict-mode refusal still created %s; init must refuse before write", vscode.ConfigPath())
	}
}

// TestInitClientConfig_StrictModeMapsTo412 verifies the handler-level
// status-code routing for the strict-mode refusal.
func TestInitClientConfig_StrictModeMapsTo412(t *testing.T) {
	fi := &fakeClientInitializer{
		err: fmt.Errorf("%w: strict mode active", errStrictModeRefused),
	}
	s := NewServer(Config{})
	s.clientInit = fi

	req := httptest.NewRequest(http.MethodPost, "/api/init-client-config",
		bytes.NewReader([]byte(`{"client":"vscode"}`)))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	s.mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusPreconditionFailed {
		t.Fatalf("status = %d, want 412; body=%q", rec.Code, rec.Body.String())
	}
	var resp map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if resp["code"] != "STRICT_MODE_REFUSED" {
		t.Errorf("code=%q, want STRICT_MODE_REFUSED", resp["code"])
	}
}

// TestRealClientInitializer_HappyPath drives the real initializer
// end-to-end: with a hand-prepared parent directory, Init creates
// the empty stub and reports Created=true. A second call returns
// Created=false (idempotent no-op) without rewriting the file.
func TestRealClientInitializer_HappyPath(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)
	t.Setenv("USERPROFILE", tmp)
	t.Setenv("APPDATA", filepath.Join(tmp, "AppData", "Roaming"))
	t.Setenv("LOCALAPPDATA", filepath.Join(tmp, "AppData", "Local"))

	all := clients.AllClients()
	vscode, ok := all["vscode"]
	if !ok {
		t.Skipf("vscode adapter not constructible in test env")
	}
	// Pre-create the parent so Init's parent-dir gate passes.
	if err := os.MkdirAll(filepath.Dir(vscode.ConfigPath()), 0o755); err != nil {
		t.Fatalf("mkdir parent: %v", err)
	}

	init := realClientInitializer{}
	res, err := init.Init("vscode")
	if err != nil {
		t.Fatalf("first Init: %v", err)
	}
	if !res.Created {
		t.Errorf("first Init Created=false, want true")
	}
	if res.Client != "vscode" {
		t.Errorf("Client=%q, want vscode", res.Client)
	}

	// Verify the stub bytes match the vscode adapter's expected schema.
	body, err := os.ReadFile(vscode.ConfigPath())
	if err != nil {
		t.Fatalf("read stub: %v", err)
	}
	if string(body) != "{\n  \"servers\": {}\n}\n" {
		t.Errorf("stub body=%q, want vscode `servers: {}` shape", body)
	}

	// Second call: idempotent no-op.
	res2, err := init.Init("vscode")
	if err != nil {
		t.Fatalf("second Init: %v", err)
	}
	if res2.Created {
		t.Errorf("second Init Created=true, want false (idempotent)")
	}
}

func keys(m map[string]clients.Client) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

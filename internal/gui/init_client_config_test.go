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
	"strings"
	"testing"

	"mcp-local-hub/internal/api"
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
			Path:    `C:\Users\alice\AppData\Roaming\Code\User\mcp.json`,
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

// TestRealClientInitializer_StrictModeProceedsToHardenedPipeline
// pins the v0.4.5 PR #208 codex r1 F1 closure (replacing the prior
// TestRealClientInitializer_RefusesStrictMode contract):
//
// When MCPHUB_REQUIRE_SINGLE_USER_HOME=1 is set, the Init endpoint
// no longer short-circuits with an unconditional STRICT_MODE_REFUSED.
// Instead, it proceeds to adapter.InitEmpty() which routes through
// the hardened SecureCreateClientConfigIfMissing pipeline (wired via
// internal/api/client_write_init.go::secureCreateClientConfigIfMissingWithOperatorOpt).
// That pipeline ALREADY enforces the parent-DACL gate in strict mode,
// so strict-mode operators with owner-only parents get the empty
// stub seeded correctly; broadened parents are rejected by the
// underlying pipeline with `ErrSecureWriteParentInsecure`.
//
// Verification on most CI / dev workstations: t.TempDir is under
// %TEMP% with Authenticated Users in its DACL, so the strict gate
// rejects with a "parent directory not single-user safe" error AND
// the canonical "MCPHUB_REQUIRE_SINGLE_USER_HOME is set" suffix from
// secureCreateClientConfigIfMissingWithOperatorOpt. That outcome
// (a) proves the strict gate is now reachable from Init (the prior
// unconditional refusal hid it), and (b) confirms the error message
// is the actionable one operators need.
//
// If a future tmpfs ACL ever happens to be owner-only, the success
// branch instead verifies that the stub gets written through the
// hardened pipeline under strict mode.
func TestRealClientInitializer_StrictModeProceedsToHardenedPipeline(t *testing.T) {
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
	// Pre-create the parent so the parent-stat check passes.
	if err := os.MkdirAll(filepath.Dir(vscode.ConfigPath()), 0o700); err != nil {
		t.Fatalf("mkdir parent: %v", err)
	}

	init := realClientInitializer{}
	res, err := init.Init("vscode")
	if err == nil {
		// Owner-only-parent path: stub must be written.
		if !res.Created {
			t.Errorf("Init under strict mode Created=false on owner-only parent, want true")
		}
		if _, statErr := os.Stat(vscode.ConfigPath()); statErr != nil {
			t.Errorf("strict-mode Init did not create %s: %v", vscode.ConfigPath(), statErr)
		}
		return
	}
	// Broadened-parent path: the strict gate must reject with the
	// canonical message. F1 pre-fix this branch was unreachable
	// because the GUI handler short-circuited every strict-mode
	// call. The presence of this error proves strict mode is now
	// enforced by the hardened pipeline, not by an early endpoint
	// refusal.
	if !strings.Contains(err.Error(), "MCPHUB_REQUIRE_SINGLE_USER_HOME") {
		t.Fatalf("strict-mode error missing canonical message; got %v", err)
	}
	if !strings.Contains(err.Error(), "parent") {
		t.Fatalf("strict-mode error not about parent dir; got %v", err)
	}
	// Verify NO stub leaked into the broadened parent.
	if _, statErr := os.Stat(vscode.ConfigPath()); statErr == nil {
		t.Errorf("strict-mode rejection still created %s; pipeline must refuse before write", vscode.ConfigPath())
	}
}

// TestInitClientConfig_StrictModeErrorMapsTo500 verifies that the
// strict-mode error (now surfaced from the hardened pipeline, not
// the prior endpoint short-circuit) maps to 500 INIT_FAILED through
// the default branch of the handler switch. The frontend reads the
// error body (which includes the canonical strict-mode message) to
// surface an actionable diagnostic.
//
// v0.4.5 PR #208 codex r1 F1 closure: pre-fix this mapped to 412
// STRICT_MODE_REFUSED at the handler layer; post-fix the handler is
// free of strict-mode-specific routing because the strict refusal
// is now an INIT_FAILED with the underlying pipeline's error body.
func TestInitClientConfig_StrictModeErrorMapsTo500(t *testing.T) {
	fi := &fakeClientInitializer{
		err: fmt.Errorf("init vscode: secure write: parent directory not single-user safe: SID S-1-5-11 grants access; MCPHUB_REQUIRE_SINGLE_USER_HOME is set, so the strict parent-dir gate is enforced for init-stub creation"),
	}
	s := NewServer(Config{})
	s.clientInit = fi

	req := httptest.NewRequest(http.MethodPost, "/api/init-client-config",
		bytes.NewReader([]byte(`{"client":"vscode"}`)))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	s.mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500; body=%q", rec.Code, rec.Body.String())
	}
	var resp map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if resp["code"] != "INIT_FAILED" {
		t.Errorf("code=%q, want INIT_FAILED", resp["code"])
	}
	// Error message must propagate so the GUI can render actionable
	// guidance (mentions strict-mode env var + parent-dir gate).
	if !strings.Contains(resp["error"], "MCPHUB_REQUIRE_SINGLE_USER_HOME") {
		t.Errorf("error body missing strict-mode hint: %q", resp["error"])
	}
}

// TestRealClientInitializer_HappyPath drives the real initializer
// end-to-end: with a hand-prepared parent directory, Init creates
// the empty stub and reports Created=true. A second call returns
// Created=false (idempotent no-op) without rewriting the file.
func TestRealClientInitializer_HappyPath(t *testing.T) {
	// Isolate the daemon state root FIRST. realClientInitializer.Init →
	// adapter.InitEmpty → clients.CreateConfigFileIfMissing resolves to
	// the api-side secure writer (swapped in by api.init()), which on the
	// relax lane emits a client-write-unhardened-fallback audit row via
	// LogHubMcpEvent → DaemonStateDir(). Without this redirect that row
	// lands in the operator's REAL %LOCALAPPDATA%\mcp-local-hub\hub-mcp.log
	// (test-hygiene leak; api-side sibling fixed in PR #264 via
	// isolateStateDir). SetDaemonStateRootForTest is the exported,
	// production-guarded (panics outside a test binary) cross-package
	// equivalent of the api package's in-package daemonStateRootOverride.
	t.Cleanup(api.SetDaemonStateRootForTest(t.TempDir()))

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

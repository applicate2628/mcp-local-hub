package gui

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"mcp-local-hub/internal/api"
)

// trustedRootsHandlerTestServer wires a fresh Server and redirects the
// api-layer daemon state dir to a per-test temp tree so the handler's
// api.BlessDefaultTrustedRoot / api.RemoveDefaultTrustedRoot /
// api.LoadLSPTrustedRoots calls touch ONLY the temp store, never the
// developer's real %LOCALAPPDATA%\mcp-local-hub\lsp-trusted-roots.json.
// Returns the server and the resolved store path.
func trustedRootsHandlerTestServer(t *testing.T) (*Server, string) {
	t.Helper()
	root := t.TempDir() // 0700 / single-user — passes the parent-DACL read gate
	restore := api.SetDaemonStateRootForTest(root)
	t.Cleanup(restore)
	path, err := api.DefaultLSPTrustedRootsPath()
	if err != nil {
		t.Fatalf("resolve trusted-roots path: %v", err)
	}
	return NewServer(Config{}), path
}

// mkTrustedDir makes a real directory under base so canonicalization
// (EvalSymlinks) resolves it the way a production workspace root would.
func mkTrustedDir(t *testing.T, base, name string) string {
	t.Helper()
	p := filepath.Join(base, name)
	if err := os.MkdirAll(p, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", p, err)
	}
	return p
}

func decodeTrustedRootsResp(t *testing.T, rec *httptest.ResponseRecorder) lspTrustedRootsResponse {
	t.Helper()
	var resp lspTrustedRootsResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response %q: %v", rec.Body.String(), err)
	}
	return resp
}

func TestTrustedRoots_GetEmpty(t *testing.T) {
	s, path := trustedRootsHandlerTestServer(t)

	req := httptest.NewRequest(http.MethodGet, "/api/lsp/trusted-roots", nil)
	rec := httptest.NewRecorder()
	s.mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%q", rec.Code, rec.Body.String())
	}
	resp := decodeTrustedRootsResp(t, rec)
	if resp.Roots == nil {
		t.Fatal("roots must be a non-nil JSON array even when the store is absent")
	}
	if len(resp.Roots) != 0 {
		t.Fatalf("absent store should yield zero roots, got %v", resp.Roots)
	}
	if resp.Path != path {
		t.Fatalf("path=%q want %q", resp.Path, path)
	}
}

func TestTrustedRoots_PostAddAppearsInGetAndOnDisk(t *testing.T) {
	s, path := trustedRootsHandlerTestServer(t)
	base := t.TempDir()
	root := mkTrustedDir(t, base, "proj")

	body, _ := json.Marshal(map[string]string{"root": root})
	req := httptest.NewRequest(http.MethodPost, "/api/lsp/trusted-roots", bytes.NewReader(body))
	rec := httptest.NewRecorder()
	s.mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("POST status=%d body=%q", rec.Code, rec.Body.String())
	}
	resp := decodeTrustedRootsResp(t, rec)
	if len(resp.Roots) != 1 {
		t.Fatalf("POST response should carry the added root, got %v", resp.Roots)
	}

	// It also appears in a fresh GET.
	getReq := httptest.NewRequest(http.MethodGet, "/api/lsp/trusted-roots", nil)
	getRec := httptest.NewRecorder()
	s.mux.ServeHTTP(getRec, getReq)
	getResp := decodeTrustedRootsResp(t, getRec)
	if len(getResp.Roots) != 1 {
		t.Fatalf("GET after add should list 1 root, got %v", getResp.Roots)
	}

	// And it is persisted on disk through the api store.
	f, err := api.LoadLSPTrustedRoots(path)
	if err != nil {
		t.Fatalf("LoadLSPTrustedRoots: %v", err)
	}
	if len(f.Roots) != 1 {
		t.Fatalf("on-disk store should hold 1 root, got %v", f.Roots)
	}
	if !f.LSPWorkspaceRootTrusted(root) {
		t.Fatalf("added root %q must be trusted by the on-disk store", root)
	}
}

func TestTrustedRoots_DeleteRemoves(t *testing.T) {
	s, path := trustedRootsHandlerTestServer(t)
	base := t.TempDir()
	root := mkTrustedDir(t, base, "proj")

	// Seed via the api store directly.
	if err := api.BlessTrustedRoot(path, root); err != nil {
		t.Fatalf("seed bless: %v", err)
	}

	body, _ := json.Marshal(map[string]string{"root": root})
	req := httptest.NewRequest(http.MethodDelete, "/api/lsp/trusted-roots", bytes.NewReader(body))
	rec := httptest.NewRecorder()
	s.mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("DELETE status=%d body=%q", rec.Code, rec.Body.String())
	}
	resp := decodeTrustedRootsResp(t, rec)
	if len(resp.Roots) != 0 {
		t.Fatalf("DELETE response should reflect the now-empty store, got %v", resp.Roots)
	}

	f, err := api.LoadLSPTrustedRoots(path)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if f.LSPWorkspaceRootTrusted(root) {
		t.Fatalf("removed root %q must no longer be trusted on disk", root)
	}
}

func TestTrustedRoots_DeleteAbsentIsNoopSuccess(t *testing.T) {
	s, _ := trustedRootsHandlerTestServer(t)
	base := t.TempDir()
	root := mkTrustedDir(t, base, "neverBlessed")

	body, _ := json.Marshal(map[string]string{"root": root})
	req := httptest.NewRequest(http.MethodDelete, "/api/lsp/trusted-roots", bytes.NewReader(body))
	rec := httptest.NewRecorder()
	s.mux.ServeHTTP(rec, req)

	// Removing an absent root is an idempotent no-op success.
	if rec.Code != http.StatusOK {
		t.Fatalf("DELETE of absent root should be 200 no-op, got %d body=%q", rec.Code, rec.Body.String())
	}
}

func TestTrustedRoots_PostRelativePathRejected(t *testing.T) {
	s, _ := trustedRootsHandlerTestServer(t)

	body, _ := json.Marshal(map[string]string{"root": "relative/path"})
	req := httptest.NewRequest(http.MethodPost, "/api/lsp/trusted-roots", bytes.NewReader(body))
	rec := httptest.NewRecorder()
	s.mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("relative path must be 400, got %d body=%q", rec.Code, rec.Body.String())
	}
	var env map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatalf("decode error envelope: %v", err)
	}
	if env["code"] != "LSP_TRUSTED_ROOTS_NOT_ABSOLUTE" {
		t.Fatalf("code=%q want LSP_TRUSTED_ROOTS_NOT_ABSOLUTE", env["code"])
	}
}

func TestTrustedRoots_PostEmptyRootRejected(t *testing.T) {
	s, _ := trustedRootsHandlerTestServer(t)

	body, _ := json.Marshal(map[string]string{"root": "   "})
	req := httptest.NewRequest(http.MethodPost, "/api/lsp/trusted-roots", bytes.NewReader(body))
	rec := httptest.NewRecorder()
	s.mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("empty/whitespace root must be 400, got %d body=%q", rec.Code, rec.Body.String())
	}
	var env map[string]string
	_ = json.Unmarshal(rec.Body.Bytes(), &env)
	if env["code"] != "LSP_TRUSTED_ROOTS_INVALID" {
		t.Fatalf("code=%q want LSP_TRUSTED_ROOTS_INVALID", env["code"])
	}
}

func TestTrustedRoots_PostInvalidJSONRejected(t *testing.T) {
	s, _ := trustedRootsHandlerTestServer(t)

	req := httptest.NewRequest(http.MethodPost, "/api/lsp/trusted-roots", bytes.NewReader([]byte("{not json")))
	rec := httptest.NewRecorder()
	s.mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("malformed JSON must be 400, got %d body=%q", rec.Code, rec.Body.String())
	}
	var env map[string]string
	_ = json.Unmarshal(rec.Body.Bytes(), &env)
	if env["code"] != "LSP_TRUSTED_ROOTS_INVALID_JSON" {
		t.Fatalf("code=%q want LSP_TRUSTED_ROOTS_INVALID_JSON", env["code"])
	}
}

func TestTrustedRoots_MethodNotAllowed(t *testing.T) {
	s, _ := trustedRootsHandlerTestServer(t)

	req := httptest.NewRequest(http.MethodPut, "/api/lsp/trusted-roots", nil)
	rec := httptest.NewRecorder()
	s.mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("PUT must be 405, got %d", rec.Code)
	}
	if allow := rec.Header().Get("Allow"); allow != "GET, POST, DELETE" {
		t.Fatalf("Allow=%q want \"GET, POST, DELETE\"", allow)
	}
}

// TestTrustedRoots_CrossOriginRejected confirms the route is wrapped in
// requireSameOrigin: a browser-driven cross-origin POST is rejected with
// 403 CROSS_ORIGIN before any store mutation.
func TestTrustedRoots_CrossOriginRejected(t *testing.T) {
	s, path := trustedRootsHandlerTestServer(t)
	base := t.TempDir()
	root := mkTrustedDir(t, base, "proj")

	body, _ := json.Marshal(map[string]string{"root": root})
	req := httptest.NewRequest(http.MethodPost, "/api/lsp/trusted-roots", bytes.NewReader(body))
	req.Header.Set("Origin", "http://evil.example.com")
	rec := httptest.NewRecorder()
	s.mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("cross-origin POST must be 403, got %d body=%q", rec.Code, rec.Body.String())
	}
	var env map[string]string
	_ = json.Unmarshal(rec.Body.Bytes(), &env)
	if env["code"] != "CROSS_ORIGIN" {
		t.Fatalf("code=%q want CROSS_ORIGIN", env["code"])
	}
	// The rejected request must NOT have mutated the store.
	f, err := api.LoadLSPTrustedRoots(path)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if len(f.Roots) != 0 {
		t.Fatalf("cross-origin POST must not mutate the store, got %v", f.Roots)
	}
}

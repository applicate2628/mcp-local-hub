// internal/gui/resolve_symlink_write_test.go
package gui

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"mcp-local-hub/internal/api"
	"mcp-local-hub/internal/clients"
)

type fakeSymlinkResolveWriter struct {
	resolveCalled  string
	writeClient    string
	writePinned    string
	writeHash      string
	resolveResult  *ResolveSymlinkResult
	resolveErr     error
	writeResult    *WriteSymlinkResult
	writeErr       error
}

func (f *fakeSymlinkResolveWriter) Resolve(client string) (*ResolveSymlinkResult, error) {
	f.resolveCalled = client
	return f.resolveResult, f.resolveErr
}

func (f *fakeSymlinkResolveWriter) Write(client, pinnedRealPath, contentHash string) (*WriteSymlinkResult, error) {
	f.writeClient = client
	f.writePinned = pinnedRealPath
	f.writeHash = contentHash
	return f.writeResult, f.writeErr
}

func postResolveSymlink(t *testing.T, s *Server, body string, hdr map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/api/resolve-symlink-and-write", bytes.NewReader([]byte(body)))
	req.Header.Set("Content-Type", "application/json")
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	s.mux.ServeHTTP(rec, req)
	return rec
}

// TestResolveSymlinkWrite_ResolvePhase pins the confirm=false phase: returns
// the pinned real path + content hash for the confirm modal, no write.
func TestResolveSymlinkWrite_ResolvePhase(t *testing.T) {
	fw := &fakeSymlinkResolveWriter{
		resolveResult: &ResolveSymlinkResult{
			Client:         "codex-cli",
			OriginalPath:   "/home/u/.codex/config.toml",
			ResolvedTarget: "/e/env/Agents/.codex/config.toml",
			PinnedRealPath: "/e/env/Agents/.codex",
			ContentHash:    "abc123",
			IsSymlink:      true,
		},
	}
	s := NewServer(Config{})
	s.symlinkWriter = fw

	rec := postResolveSymlink(t, s, `{"client":"codex-cli","confirm":false}`, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d, body=%q", rec.Code, rec.Body.String())
	}
	if fw.resolveCalled != "codex-cli" {
		t.Errorf("Resolve called with %q, want codex-cli", fw.resolveCalled)
	}
	var resp ResolveSymlinkResult
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.PinnedRealPath != "/e/env/Agents/.codex" || resp.ContentHash != "abc123" || !resp.IsSymlink {
		t.Errorf("resolve resp=%+v, want pinned path + hash + isSymlink", resp)
	}
}

// TestResolveSymlinkWrite_WritePhase pins the confirm=true phase: carries the
// pinned path + hash into Write, returns the structured write result.
func TestResolveSymlinkWrite_WritePhase(t *testing.T) {
	fw := &fakeSymlinkResolveWriter{
		writeResult: &WriteSymlinkResult{
			Client:       "codex-cli",
			OriginalPath: "/home/u/.codex/config.toml",
			WrittenPath:  "/e/env/Agents/.codex/config.toml",
			Written:      true,
		},
	}
	s := NewServer(Config{})
	s.symlinkWriter = fw

	rec := postResolveSymlink(t, s,
		`{"client":"codex-cli","confirm":true,"pinned_real_path":"/e/env/Agents/.codex","content_hash":"abc123"}`, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d, body=%q", rec.Code, rec.Body.String())
	}
	if fw.writeClient != "codex-cli" || fw.writePinned != "/e/env/Agents/.codex" || fw.writeHash != "abc123" {
		t.Errorf("Write got (%q,%q,%q), want (codex-cli, /e/env/Agents/.codex, abc123)", fw.writeClient, fw.writePinned, fw.writeHash)
	}
	var resp WriteSymlinkResult
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !resp.Written {
		t.Errorf("Written=false, want true")
	}
}

// TestResolveSymlinkWrite_WriteRequiresPinnedPath pins the 400 when confirm=true
// without pinned_real_path — the anti-TOCTOU pin is mandatory; the handler must
// NOT reach Write.
func TestResolveSymlinkWrite_WriteRequiresPinnedPath(t *testing.T) {
	fw := &fakeSymlinkResolveWriter{}
	s := NewServer(Config{})
	s.symlinkWriter = fw

	rec := postResolveSymlink(t, s, `{"client":"codex-cli","confirm":true}`, nil)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status=%d, want 400", rec.Code)
	}
	if fw.writeClient != "" {
		t.Errorf("Write was reached despite missing pinned_real_path")
	}
}

// TestResolveSymlinkWrite_WriteRequiresContentHash pins the 400 when confirm=true
// has pinned_real_path but OMITS content_hash (F4 defense-in-depth): the
// concurrent-edit drift token is mandatory, so a hand-crafted request cannot
// drop it to skip the drift guard. The handler must NOT reach Write.
func TestResolveSymlinkWrite_WriteRequiresContentHash(t *testing.T) {
	fw := &fakeSymlinkResolveWriter{}
	s := NewServer(Config{})
	s.symlinkWriter = fw

	rec := postResolveSymlink(t, s,
		`{"client":"codex-cli","confirm":true,"pinned_real_path":"/e/env/Agents/.codex"}`, nil)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status=%d, want 400; body=%q", rec.Code, rec.Body.String())
	}
	if code := decodeCode(t, rec); code != "BAD_REQUEST" {
		t.Errorf("code=%q, want BAD_REQUEST", code)
	}
	if fw.writeClient != "" {
		t.Errorf("Write was reached despite missing content_hash")
	}
}

// TestResolveSymlinkWrite_NotSymlinkMaps412 pins the NOT_SYMLINK 412 mapping
// (the GUI refreshes its scan instead of offering a follow).
func TestResolveSymlinkWrite_NotSymlinkMaps412(t *testing.T) {
	fw := &fakeSymlinkResolveWriter{
		resolveErr: fmt.Errorf("%w: /home/u/.codex/config.toml", errSymlinkNotApplicable),
	}
	s := NewServer(Config{})
	s.symlinkWriter = fw

	rec := postResolveSymlink(t, s, `{"client":"codex-cli","confirm":false}`, nil)
	if rec.Code != http.StatusPreconditionFailed {
		t.Fatalf("status=%d, want 412; body=%q", rec.Code, rec.Body.String())
	}
	if code := decodeCode(t, rec); code != "NOT_SYMLINK" {
		t.Errorf("code=%q, want NOT_SYMLINK", code)
	}
}

// TestResolveSymlinkWrite_RepointedMaps409 pins the SYMLINK_REPOINTED 409.
func TestResolveSymlinkWrite_RepointedMaps409(t *testing.T) {
	fw := &fakeSymlinkResolveWriter{
		writeErr: fmt.Errorf("%w: confirmed X, now resolves to Y", errSymlinkRepointed),
	}
	s := NewServer(Config{})
	s.symlinkWriter = fw

	rec := postResolveSymlink(t, s,
		`{"client":"codex-cli","confirm":true,"pinned_real_path":"/x","content_hash":"abc123"}`, nil)
	if rec.Code != http.StatusConflict {
		t.Fatalf("status=%d, want 409", rec.Code)
	}
	if code := decodeCode(t, rec); code != "SYMLINK_REPOINTED" {
		t.Errorf("code=%q, want SYMLINK_REPOINTED", code)
	}
}

// TestResolveSymlinkWrite_ConfigChangedMaps409 pins CONFIG_CHANGED 409.
func TestResolveSymlinkWrite_ConfigChangedMaps409(t *testing.T) {
	fw := &fakeSymlinkResolveWriter{
		writeErr: fmt.Errorf("%w: content of Y changed", errConfigChanged),
	}
	s := NewServer(Config{})
	s.symlinkWriter = fw

	rec := postResolveSymlink(t, s,
		`{"client":"codex-cli","confirm":true,"pinned_real_path":"/x","content_hash":"abc123"}`, nil)
	if rec.Code != http.StatusConflict {
		t.Fatalf("status=%d, want 409", rec.Code)
	}
	if code := decodeCode(t, rec); code != "CONFIG_CHANGED" {
		t.Errorf("code=%q, want CONFIG_CHANGED", code)
	}
}

// TestResolveSymlinkWrite_UnknownClientMaps404 pins the client-allowlist 404.
func TestResolveSymlinkWrite_UnknownClientMaps404(t *testing.T) {
	fw := &fakeSymlinkResolveWriter{
		resolveErr: fmt.Errorf("%w: %q", errUnknownClient, "bogus"),
	}
	s := NewServer(Config{})
	s.symlinkWriter = fw

	rec := postResolveSymlink(t, s, `{"client":"bogus","confirm":false}`, nil)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status=%d, want 404", rec.Code)
	}
	if code := decodeCode(t, rec); code != "UNKNOWN_CLIENT" {
		t.Errorf("code=%q, want UNKNOWN_CLIENT", code)
	}
}

// TestResolveSymlinkWrite_StrictRefusalMaps500 pins that the strict-mode
// (or any secure-write) refusal maps to 500 WRITE_REFUSED with the underlying
// message propagated for the GUI to render.
func TestResolveSymlinkWrite_StrictRefusalMaps500(t *testing.T) {
	fw := &fakeSymlinkResolveWriter{
		writeErr: fmt.Errorf("write codex-cli via consent: secure write: strict mode is active (via MCPHUB_REQUIRE_SINGLE_USER_HOME=1)"),
	}
	s := NewServer(Config{})
	s.symlinkWriter = fw

	rec := postResolveSymlink(t, s,
		`{"client":"codex-cli","confirm":true,"pinned_real_path":"/x","content_hash":"abc123"}`, nil)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status=%d, want 500", rec.Code)
	}
	var m map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &m); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if m["code"] != "WRITE_REFUSED" {
		t.Errorf("code=%q, want WRITE_REFUSED", m["code"])
	}
	if !bytes.Contains([]byte(m["error"]), []byte("MCPHUB_REQUIRE_SINGLE_USER_HOME")) {
		t.Errorf("error body missing strict-mode hint: %q", m["error"])
	}
}

// TestResolveSymlinkWrite_RejectsGET pins POST-only.
func TestResolveSymlinkWrite_RejectsGET(t *testing.T) {
	s := NewServer(Config{})
	s.symlinkWriter = &fakeSymlinkResolveWriter{}
	req := httptest.NewRequest(http.MethodGet, "/api/resolve-symlink-and-write?client=codex-cli", nil)
	rec := httptest.NewRecorder()
	s.mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("status=%d, want 405", rec.Code)
	}
}

// TestResolveSymlinkWrite_RejectsCrossOrigin pins the same-origin guard:
// a cross-site Sec-Fetch-Site request is refused 403 before reaching the
// writer.
func TestResolveSymlinkWrite_RejectsCrossOrigin(t *testing.T) {
	fw := &fakeSymlinkResolveWriter{}
	s := NewServer(Config{})
	s.symlinkWriter = fw
	rec := postResolveSymlink(t, s, `{"client":"codex-cli","confirm":false}`, map[string]string{
		"Sec-Fetch-Site": "cross-site",
	})
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status=%d, want 403", rec.Code)
	}
	if fw.resolveCalled != "" {
		t.Errorf("writer reached despite cross-origin rejection")
	}
}

// TestResolveSymlinkWrite_EmptyClient pins the 400 for an empty client field.
func TestResolveSymlinkWrite_EmptyClient(t *testing.T) {
	s := NewServer(Config{})
	s.symlinkWriter = &fakeSymlinkResolveWriter{}
	rec := postResolveSymlink(t, s, `{"client":"","confirm":false}`, nil)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status=%d, want 400", rec.Code)
	}
}

func decodeCode(t *testing.T, rec *httptest.ResponseRecorder) string {
	t.Helper()
	var m map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &m); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	return m["code"]
}

// ---------------------------------------------------------------------------
// Real adapter (realSymlinkResolveWriter) end-to-end — POSIX only (symlink
// creation needs elevation on Windows; the cross-platform path is identical).
// ---------------------------------------------------------------------------

// realSymlinkClient builds a real symlinked client config under a tmp home and
// returns the client name + the real target path. It picks codex-cli, whose
// config path is ~/.codex/config.toml — a single-file path easy to symlink.
func realSymlinkClient(t *testing.T) (client, realTarget string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("symlink creation requires elevation on Windows")
	}
	t.Cleanup(api.SetDaemonStateRootForTest(t.TempDir()))
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)
	t.Setenv("USERPROFILE", tmp)
	t.Setenv("APPDATA", filepath.Join(tmp, "AppData", "Roaming"))
	t.Setenv("LOCALAPPDATA", filepath.Join(tmp, "AppData", "Local"))
	t.Setenv("MCPHUB_REQUIRE_SINGLE_USER_HOME", "")
	t.Setenv("MCPHUB_ALLOW_CLIENT_CONFIG_SYMLINK", "")

	all := clients.AllClients()
	codex, ok := all["codex-cli"]
	if !ok {
		t.Skip("codex-cli adapter not constructible in test env")
	}
	cfg := codex.ConfigPath()
	// Real target lives in a separate tree; symlink the client config to it.
	targetDir := filepath.Join(tmp, "dotfiles", ".codex")
	if err := os.MkdirAll(targetDir, 0o700); err != nil {
		t.Fatalf("mkdir target: %v", err)
	}
	realTarget = filepath.Join(targetDir, "config.toml")
	if err := os.WriteFile(realTarget, []byte("# codex config\n"), 0o600); err != nil {
		t.Fatalf("seed target: %v", err)
	}
	if err := os.MkdirAll(filepath.Dir(cfg), 0o700); err != nil {
		t.Fatalf("mkdir client dir: %v", err)
	}
	if err := os.Symlink(realTarget, cfg); err != nil {
		t.Skipf("symlink unsupported: %v", err)
	}
	return "codex-cli", realTarget
}

// TestRealSymlinkResolveWriter_ResolveThenWrite drives the real adapter: the
// RESOLVE phase pins the FULL real target path + content hash, then the WRITE
// phase round-trips the SAME bytes through the consent pipeline. The original
// symlink stays intact and the resolved target is unchanged content-wise.
func TestRealSymlinkResolveWriter_ResolveThenWrite(t *testing.T) {
	client, realTarget := realSymlinkClient(t)

	rw := realSymlinkResolveWriter{}
	res, err := rw.Resolve(client)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if !res.IsSymlink {
		t.Fatalf("Resolve IsSymlink=false, want true")
	}
	// The pin is the FULL resolved target path (parent + basename), equal to
	// ResolvedTarget — shown == pinned.
	if res.PinnedRealPath != filepath.Clean(realTarget) {
		t.Errorf("PinnedRealPath=%q, want full target %q", res.PinnedRealPath, filepath.Clean(realTarget))
	}
	if res.PinnedRealPath != filepath.Clean(res.ResolvedTarget) {
		t.Errorf("PinnedRealPath=%q != ResolvedTarget=%q (shown must equal pinned)", res.PinnedRealPath, res.ResolvedTarget)
	}
	// Hash must match the seeded content.
	want := sha256.Sum256([]byte("# codex config\n"))
	if res.ContentHash != hex.EncodeToString(want[:]) {
		t.Errorf("ContentHash=%q, want hash of seeded content", res.ContentHash)
	}

	wres, err := rw.Write(client, res.PinnedRealPath, res.ContentHash)
	if err != nil {
		t.Fatalf("Write: %v", err)
	}
	if !wres.Written {
		t.Errorf("Written=false, want true")
	}
	// Content unchanged (byte-exact round-trip).
	if b, _ := os.ReadFile(realTarget); string(b) != "# codex config\n" {
		t.Errorf("round-trip changed content: %q", b)
	}
	// Original client config still a symlink (not rewritten to a regular file).
	cfg := clients.AllClients()[client].ConfigPath()
	if lst, lerr := os.Lstat(cfg); lerr != nil {
		t.Fatalf("lstat client config: %v", lerr)
	} else if lst.Mode()&os.ModeSymlink == 0 {
		t.Errorf("client config symlink was rewritten to a regular file")
	}
}

// TestRealSymlinkResolveWriter_ContentDrift_Refused pins the concurrent-edit
// guard: a stale content hash (the target was edited after the modal) refuses
// the write with CONFIG_CHANGED and does NOT write.
func TestRealSymlinkResolveWriter_ContentDrift_Refused(t *testing.T) {
	client, realTarget := realSymlinkClient(t)
	rw := realSymlinkResolveWriter{}
	res, err := rw.Resolve(client)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	// External edit AFTER the modal.
	if err := os.WriteFile(realTarget, []byte("# edited externally\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err = rw.Write(client, res.PinnedRealPath, res.ContentHash)
	if !errors.Is(err, errConfigChanged) {
		t.Fatalf("Write err=%v, want errConfigChanged", err)
	}
	// The external edit must be preserved (no clobber).
	if b, _ := os.ReadFile(realTarget); string(b) != "# edited externally\n" {
		t.Errorf("external edit clobbered: %q", b)
	}
}

// TestRealSymlinkResolveWriter_Repointed_Refused pins the anti-TOCTOU pin
// check: the symlink is repointed to a different parent after Resolve, and the
// Write refuses with SYMLINK_REPOINTED before any byte is written.
func TestRealSymlinkResolveWriter_Repointed_Refused(t *testing.T) {
	client, _ := realSymlinkClient(t)
	rw := realSymlinkResolveWriter{}
	res, err := rw.Resolve(client)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	// Repoint the symlink to a different real target in a different parent.
	cfg := clients.AllClients()[client].ConfigPath()
	tmp := os.Getenv("HOME")
	otherDir := filepath.Join(tmp, "elsewhere", ".codex")
	if err := os.MkdirAll(otherDir, 0o700); err != nil {
		t.Fatal(err)
	}
	other := filepath.Join(otherDir, "config.toml")
	if err := os.WriteFile(other, []byte("# other\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	_ = os.Remove(cfg)
	if err := os.Symlink(other, cfg); err != nil {
		t.Fatalf("repoint: %v", err)
	}
	_, err = rw.Write(client, res.PinnedRealPath, res.ContentHash)
	if !errors.Is(err, errSymlinkRepointed) {
		t.Fatalf("Write err=%v, want errSymlinkRepointed", err)
	}
	// The swap target must be untouched.
	if b, _ := os.ReadFile(other); string(b) != "# other\n" {
		t.Errorf("repoint target written despite refusal: %q", b)
	}
}

// TestRealSymlinkResolveWriter_SameParentRepointed_Refused is the F2 GUI-level
// guard: the symlink is repointed to a DIFFERENT file in the SAME parent
// directory after Resolve. Because the pin is now the FULL resolved target path
// (not just the parent), the Write refuses with SYMLINK_REPOINTED — a
// parent-only pin would have passed (same parent) and landed the write on the
// unapproved file. The endpoint maps errSymlinkRepointed to 409.
func TestRealSymlinkResolveWriter_SameParentRepointed_Refused(t *testing.T) {
	client, realTarget := realSymlinkClient(t)
	rw := realSymlinkResolveWriter{}
	res, err := rw.Resolve(client)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	// Repoint the symlink to a SIBLING file in the SAME parent dir.
	cfg := clients.AllClients()[client].ConfigPath()
	sibling := filepath.Join(filepath.Dir(realTarget), "config-other.toml")
	if err := os.WriteFile(sibling, []byte("# sibling\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	_ = os.Remove(cfg)
	if err := os.Symlink(sibling, cfg); err != nil {
		t.Fatalf("repoint to sibling: %v", err)
	}
	_, err = rw.Write(client, res.PinnedRealPath, res.ContentHash)
	if !errors.Is(err, errSymlinkRepointed) {
		t.Fatalf("Write err=%v, want errSymlinkRepointed (same-parent repoint must be caught by the full-path pin)", err)
	}
	// The unapproved sibling target must be untouched (the consent BYPASS).
	if b, _ := os.ReadFile(sibling); string(b) != "# sibling\n" {
		t.Errorf("same-parent swap target written despite refusal — consent BYPASS not closed: %q", b)
	}
}

// TestResolveSymlinkWrite_SameParentRepointMaps409 pins the HTTP mapping for the
// F2 same-parent repoint: errSymlinkRepointed surfaces as 409 SYMLINK_REPOINTED
// at the endpoint (the GUI then tells the operator to rescan + retry).
func TestResolveSymlinkWrite_SameParentRepointMaps409(t *testing.T) {
	fw := &fakeSymlinkResolveWriter{
		writeErr: fmt.Errorf("%w: confirmed /cfg/claude.json, now resolves to /cfg/other.json", errSymlinkRepointed),
	}
	s := NewServer(Config{})
	s.symlinkWriter = fw

	rec := postResolveSymlink(t, s,
		`{"client":"codex-cli","confirm":true,"pinned_real_path":"/cfg/claude.json","content_hash":"abc123"}`, nil)
	if rec.Code != http.StatusConflict {
		t.Fatalf("status=%d, want 409", rec.Code)
	}
	if code := decodeCode(t, rec); code != "SYMLINK_REPOINTED" {
		t.Errorf("code=%q, want SYMLINK_REPOINTED", code)
	}
}

// TestRealSymlinkResolveWriter_StrictMode_Refused pins that strict mode refuses
// the consent write — the affordance shows the refusal, never a follow.
func TestRealSymlinkResolveWriter_StrictMode_Refused(t *testing.T) {
	client, realTarget := realSymlinkClient(t)
	t.Setenv("MCPHUB_REQUIRE_SINGLE_USER_HOME", "1")
	t.Cleanup(func() { api.ResetStrictModeIntentCacheForTest() })

	rw := realSymlinkResolveWriter{}
	res, err := rw.Resolve(client)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	_, werr := rw.Write(client, res.PinnedRealPath, res.ContentHash)
	if werr == nil {
		t.Fatalf("strict mode must REFUSE the consent write")
	}
	// The target's content must be unchanged (refused before/at the write).
	if b, _ := os.ReadFile(realTarget); string(b) != "# codex config\n" {
		t.Errorf("strict refusal still mutated target: %q", b)
	}
}

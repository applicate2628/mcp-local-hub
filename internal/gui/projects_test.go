// internal/gui/projects_test.go
//
// Per-project-GUI P2a: GET /api/projects/scan (read-only, Model B). Tests:
//   - golden global-scan-unchanged (the scan-isolation proof: a project scan
//     does not perturb the global DefaultScanConfigPaths scan output),
//   - project scan round-trip (.mcp.json + .cursor/mcp.json + .vscode/mcp.json
//     seeded under a temp root → per-client entries; empty project → empty),
//   - handler path-safety (traversal / relative / nonexistent / non-dir / empty
//     → 400, leak-safe body),
//   - no-write (the endpoint creates/modifies no file under the project root).
//
// State-safety: the internal/gui TestMain already fences this binary off the
// live state dir + LOCALAPPDATA/XDG. Each test additionally t.Setenv's
// HOME/USERPROFILE/LOCALAPPDATA to a temp dir so the GLOBAL DefaultScanConfigPaths
// resolution can never read the operator's real ~/.claude.json / configs, and
// the project root is always a t.TempDir().
package gui

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"

	"mcp-local-hub/internal/api"
	"mcp-local-hub/internal/clients"
)

// isolateHome points every client-config path root at an empty temp dir so a
// global config-path resolution (DefaultScanConfigPaths) reads nothing real.
//
// The five vars set inline below still left the mimocode managed layer resolving
// the machine-global %ProgramData%\opencode, plus COPILOT_HOME / KIMI_CODE_HOME /
// the MIMOCODE_* profile overrides — so the global-scan baselines these tests
// take were reading real host state. neutralizeClientConfigPathEnv
// (client_config_env_isolation_test.go) is the single owner of that set.
func isolateHome(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("LOCALAPPDATA", filepath.Join(home, "AppData", "Local"))
	neutralizeClientConfigPathEnv(t, home)
	return home
}

// seedProject writes the three project-scope config files under root.
func seedProject(t *testing.T, root string) {
	t.Helper()
	// claude-code: <root>/.mcp.json with top-level mcpServers
	mustWrite(t, filepath.Join(root, ".mcp.json"),
		`{"mcpServers":{"alpha":{"command":"node","args":["a.js"]}}}`)
	// cursor: <root>/.cursor/mcp.json with top-level mcpServers
	mustWrite(t, filepath.Join(root, ".cursor", "mcp.json"),
		`{"mcpServers":{"beta":{"url":"http://127.0.0.1:9200/mcp"}}}`)
	// vscode: <root>/.vscode/mcp.json with top-level servers (NOT mcpServers)
	mustWrite(t, filepath.Join(root, ".vscode", "mcp.json"),
		`{"servers":{"gamma":{"command":"uvx","args":["g"]}}}`)
}

func mustWrite(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", filepath.Dir(path), err)
	}
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func getProjectScan(t *testing.T, s *Server, root string) *httptest.ResponseRecorder {
	t.Helper()
	target := "/api/projects/scan"
	if root != "__OMIT__" {
		target += "?root=" + url.QueryEscape(root)
	}
	req := httptest.NewRequest(http.MethodGet, target, nil)
	rec := httptest.NewRecorder()
	s.mux.ServeHTTP(rec, req)
	return rec
}

// TestProjectsScan_GlobalScanUnchanged is the scan-isolation proof: running a
// project scan does NOT change the output of the global DefaultScanConfigPaths
// scan. Two disjoint resolvers → leakage structurally impossible. (Source-level
// proof: scan.go has zero diff; this is the runtime confirmation.)
func TestProjectsScan_GlobalScanUnchanged(t *testing.T) {
	isolateHome(t)

	// Baseline global scan (before any project scan).
	before, err := api.NewAPI().ScanFrom(api.ScanOpts{ConfigPaths: api.DefaultScanConfigPaths()})
	if err != nil {
		t.Fatalf("baseline global scan: %v", err)
	}

	// Run a project scan against a seeded temp project.
	root := t.TempDir()
	seedProject(t, root)
	s := NewServer(Config{})
	rec := getProjectScan(t, s, root)
	if rec.Code != http.StatusOK {
		t.Fatalf("project scan status = %d, body=%s", rec.Code, rec.Body.String())
	}

	// Global scan AFTER the project scan must be byte-identical (modulo the
	// At timestamp, which is wall-clock and excluded).
	after, err := api.NewAPI().ScanFrom(api.ScanOpts{ConfigPaths: api.DefaultScanConfigPaths()})
	if err != nil {
		t.Fatalf("post global scan: %v", err)
	}

	before.At = after.At // exclude the wall-clock field from the comparison
	// ScanFrom builds Entries from a Go map, so the slice ORDER is
	// non-deterministic between any two calls (pre-existing, not introduced by
	// P2a). Sort both by Name so the comparison is on CONTENT, which is what the
	// isolation invariant is about.
	sortEntriesByName(before.Entries)
	sortEntriesByName(after.Entries)
	if !reflect.DeepEqual(before, after) {
		t.Errorf("global scan output changed across a project scan:\nbefore=%+v\nafter=%+v", before, after)
	}
}

func sortEntriesByName(es []api.ScanEntry) {
	sort.Slice(es, func(i, j int) bool { return es[i].Name < es[j].Name })
}

// TestProjectsScan_ResolverDisjointFromGlobal pins decision 3's structural
// claim: ProjectScanConfigPaths and DefaultScanConfigPaths never produce a
// shared path for the same client (the resolvers are disjoint by construction).
func TestProjectsScan_ResolverDisjointFromGlobal(t *testing.T) {
	isolateHome(t)
	root := t.TempDir()
	_, proj, err := clients.ProjectScanConfigPaths(root)
	if err != nil {
		t.Fatalf("ProjectScanConfigPaths: %v", err)
	}
	global := api.DefaultScanConfigPaths()
	for client, pp := range proj {
		if gp, ok := global[client]; ok && gp == pp {
			t.Errorf("client %q: project path == global path %q (resolvers not disjoint)", client, pp)
		}
	}
}

// TestProjectsScan_ReturnsPerClientEntries seeds the three project config files
// and asserts the per-client ClientPresence reflects them (alpha→claude-code,
// beta→cursor, gamma→vscode), with the vscode `servers` key parsed correctly.
func TestProjectsScan_ReturnsPerClientEntries(t *testing.T) {
	isolateHome(t)
	root := t.TempDir()
	seedProject(t, root)

	s := NewServer(Config{})
	rec := getProjectScan(t, s, root)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", rec.Code, rec.Body.String())
	}

	var out api.ScanResult
	if err := json.NewDecoder(rec.Body).Decode(&out); err != nil {
		t.Fatalf("decode: %v", err)
	}

	byName := map[string]api.ScanEntry{}
	for _, e := range out.Entries {
		byName[e.Name] = e
	}

	check := func(server, client string) {
		e, ok := byName[server]
		if !ok {
			t.Fatalf("server %q absent from project scan entries (names=%v)", server, entryNames(out))
		}
		if _, present := e.ClientPresence[client]; !present {
			t.Errorf("server %q has no %q client presence (presence=%v)", server, client, e.ClientPresence)
		}
	}
	check("alpha", "claude-code")
	check("beta", "cursor")
	check("gamma", "vscode") // proves the vscode `servers` key was parsed

	// Raw config must be stripped (sanitizeScanResult) — no config internals on
	// the wire.
	for _, e := range out.Entries {
		for client, ce := range e.ClientPresence {
			if ce.Raw != nil {
				t.Errorf("server %q client %q leaked Raw config on the wire", e.Name, client)
			}
		}
	}
}

func entryNames(r api.ScanResult) []string {
	names := make([]string, 0, len(r.Entries))
	for _, e := range r.Entries {
		names = append(names, e.Name)
	}
	sort.Strings(names)
	return names
}

// TestProjectsScan_EmptyProject returns a successful (200) empty result for a
// project with no client configs — NOT an error.
func TestProjectsScan_EmptyProject(t *testing.T) {
	isolateHome(t)
	root := t.TempDir() // empty: no .mcp.json / .cursor / .vscode

	s := NewServer(Config{})
	rec := getProjectScan(t, s, root)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", rec.Code, rec.Body.String())
	}
	var out api.ScanResult
	if err := json.NewDecoder(rec.Body).Decode(&out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	// No client-config-derived entries. (The manifest-only pass may add
	// embedded-manifest server rows with empty ClientPresence; assert no entry
	// carries presence for our three project clients.)
	for _, e := range out.Entries {
		for _, client := range []string{"claude-code", "cursor", "vscode"} {
			if _, ok := e.ClientPresence[client]; ok {
				t.Errorf("empty project unexpectedly reported %q presence for server %q", client, e.Name)
			}
		}
	}
}

// TestProjectsScan_PathSafety_Rejects asserts every hostile/degenerate root is
// rejected with 400 and a leak-safe body (no raw root echo, no internal path,
// stable code).
func TestProjectsScan_PathSafety_Rejects(t *testing.T) {
	isolateHome(t)
	existing := t.TempDir()
	regFile := filepath.Join(existing, "afile")
	if err := os.WriteFile(regFile, []byte("x"), 0o600); err != nil {
		t.Fatalf("seed file: %v", err)
	}

	cases := []struct {
		name string
		root string
	}{
		{"omitted-param", "__OMIT__"},
		{"empty", ""},
		{"relative", "some/rel/dir"},
		{"relative-traversal", "../../etc"},
		{"dot", "."},
		{"nonexistent-absolute", filepath.Join(existing, "nope-xyz")},
		{"non-directory-file", regFile},
	}
	s := NewServer(Config{})
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			rec := getProjectScan(t, s, c.root)
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("root=%q status = %d, want 400; body=%s", c.root, rec.Code, rec.Body.String())
			}
			body := rec.Body.String()
			// Leak-safety: the body must not echo the raw root, nor any
			// absolute filesystem path of the test temp tree.
			if c.root != "" && c.root != "__OMIT__" && c.root != "." &&
				strings.Contains(body, c.root) {
				t.Errorf("response body echoes raw root %q: %q", c.root, body)
			}
			if strings.Contains(body, existing) {
				t.Errorf("response body leaks internal temp path %q: %q", existing, body)
			}
			var env struct{ Error, Code string }
			if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
				t.Fatalf("decode envelope: %v (body=%s)", err, body)
			}
			if env.Code != "PROJECT_ROOT_INVALID" {
				t.Errorf("code = %q, want PROJECT_ROOT_INVALID", env.Code)
			}
			if env.Error != "internal error" {
				t.Errorf("error = %q, want generic redacted body", env.Error)
			}
		})
	}
}

// TestProjectsScan_NoWrite asserts the endpoint creates/modifies NOTHING under
// the project root (read-only invariant). It snapshots the tree before/after.
func TestProjectsScan_NoWrite(t *testing.T) {
	isolateHome(t)
	root := t.TempDir()
	seedProject(t, root)

	before := snapshotTree(t, root)

	s := NewServer(Config{})
	rec := getProjectScan(t, s, root)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", rec.Code, rec.Body.String())
	}

	after := snapshotTree(t, root)
	if !reflect.DeepEqual(before, after) {
		t.Errorf("project tree changed after a read-only scan:\nbefore=%v\nafter=%v", before, after)
	}
}

// TestProjectsScan_NoWrite_EmptyProjectCreatesNothing additionally guards the
// empty-project path (a careless InitEmpty/stub would create .mcp.json here).
func TestProjectsScan_NoWrite_EmptyProjectCreatesNothing(t *testing.T) {
	isolateHome(t)
	root := t.TempDir()

	before := snapshotTree(t, root)
	if len(before) != 0 {
		t.Fatalf("fresh temp project should be empty, has %v", before)
	}

	s := NewServer(Config{})
	rec := getProjectScan(t, s, root)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", rec.Code, rec.Body.String())
	}

	after := snapshotTree(t, root)
	if len(after) != 0 {
		t.Errorf("read-only scan created files under an empty project: %v", after)
	}
}

// TestProjectsScan_GUIPortThreaded_SerenaStaleVsLive is the finding-2 guard:
// the project scan must thread the live GUI port into ScanOpts.GUIPort so
// scan.go's classifier (IsLiveSerenaRouterURL) can tell a project config's
// serena /serena/mcp router URL on the LIVE port (→ "via-hub") from the SAME
// URL on a STALE old-GUI-port (→ "external"/re-migratable). With GUIPort
// unthreaded (0) the live-port check degrades to port-agnostic and the STALE
// entry is misclassified "via-hub".
//
// We pin the live GUI port via s.port.Store (the established test pattern, see
// install_test.go / daemons_test.go). A serena /serena/mcp entry whose port
// MATCHES the live port classifies "via-hub"; one on a stale port classifies
// "external". The contrast is what proves GUIPort was actually threaded — if it
// were dropped, BOTH would read "via-hub".
func TestProjectsScan_GUIPortThreaded_SerenaStaleVsLive(t *testing.T) {
	isolateHome(t)

	const livePort = 9125
	const stalePort = 9121 // legacy serena daemon port; not the live GUI port

	scanSerenaURL := func(t *testing.T, serenaURL string) string {
		t.Helper()
		root := t.TempDir()
		// A cursor project config whose serena entry points at serenaURL.
		mustWrite(t, filepath.Join(root, ".cursor", "mcp.json"),
			`{"mcpServers":{"serena":{"url":"`+serenaURL+`"}}}`)

		s := NewServer(Config{})
		s.port.Store(livePort) // pin the live GUI port the handler threads in

		rec := getProjectScan(t, s, root)
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, body=%s", rec.Code, rec.Body.String())
		}
		var out api.ScanResult
		if err := json.NewDecoder(rec.Body).Decode(&out); err != nil {
			t.Fatalf("decode: %v", err)
		}
		// The result must echo the threaded port (scan.go copies opts.GUIPort
		// into ScanResult.GUIPort) — a direct proof the handler passed it.
		if out.GUIPort != livePort {
			t.Errorf("ScanResult.GUIPort = %d, want threaded live port %d", out.GUIPort, livePort)
		}
		for _, e := range out.Entries {
			if e.Name == "serena" {
				return e.Status
			}
		}
		t.Fatalf("serena entry absent from project scan (names=%v)", entryNames(out))
		return ""
	}

	t.Run("live-port-via-hub", func(t *testing.T) {
		got := scanSerenaURL(t, "http://127.0.0.1:9125/serena/mcp")
		if got != "via-hub" {
			t.Errorf("serena at LIVE port: status = %q, want \"via-hub\"", got)
		}
	})

	t.Run("stale-port-external", func(t *testing.T) {
		got := scanSerenaURL(t, "http://127.0.0.1:9121/serena/mcp")
		if got != "external" {
			t.Errorf("serena at STALE port %d (live %d): status = %q, want \"external\" (GUIPort not threaded → would read \"via-hub\")", stalePort, livePort, got)
		}
	})
}

// snapshotTree returns a map of relative-path → (size, modtime-unixnano) for
// every regular file under root, so the before/after comparison catches a
// created OR modified file.
func snapshotTree(t *testing.T, root string) map[string][2]int64 {
	t.Helper()
	out := map[string][2]int64{}
	err := filepath.Walk(root, func(p string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			return nil
		}
		rel, rerr := filepath.Rel(root, p)
		if rerr != nil {
			return rerr
		}
		out[rel] = [2]int64{info.Size(), info.ModTime().UnixNano()}
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", root, err)
	}
	return out
}

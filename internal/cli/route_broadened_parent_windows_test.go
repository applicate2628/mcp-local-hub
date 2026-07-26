//go:build windows

// internal/cli/route_broadened_parent_windows_test.go
//
// Finding 1 (adversarial cross-family review round 3,
// mcphub-front-daemon): the standalone `mcphub route` front daemon's
// documented READ-ONLY invariant (never writes the GUI-owned shared
// hub-mcp.log) was still false on a genuinely BROADENED-parent host —
// every other route-readonly regression test (route_i6_readonly_test.go's
// TestBuildRouteServer_RegisteredWorkspaceUnreachableBackend_
// NoSharedStateFileWrite, and internal/gui/route_readonly_test.go's
// companion) deliberately uses apitest.HardenedTempDir, which keeps the
// shared inode-anchored state reader's parent-DACL gate on its STRICT path
// and never exercises the default-relax fallback at all. On a REAL
// broadened-parent host (a documented, by-design, non-rare condition — see
// CLAUDE.md's "Hardened client-config writes + corp-policy posture"), that
// fallback called api.LogHubMcpEvent directly from
// readStateFileInodeAnchoredWithOptions — a DIFFERENT, lower-layer emit
// site than the AuditFn seam P1-1 fixed in serena_router.go/lsp_router.go —
// so a registry Load() or trusted-root read still wrote hub-mcp.log +
// hub-mcp.log.lock regardless of the router's own AuditFn wiring.
//
// These two tests use broadenDirAuthenticatedUsers (already defined in
// secrets_edit_temp_windows_test.go, same package) to apply a REAL,
// non-allowlisted (Authenticated Users) ACE directly to the state directory
// itself — the parent of both workspaces.yaml/lsp-trusted-roots.json and
// hub-mcp.log — so the parent-DACL gate's default-relax fallback actually
// fires, exactly as the reviewer's own falsifier (a real Serena request on
// a broadened-parent host) reproduced.
//
// Mutation-proven (see the finding-1 fix report / commit): reverting either
// reg.SetAuditSink(api.RouteReadOnlyStderrSink) call in buildRouteServer
// (route.go), or the TrustedRootCheckFn closure wiring in
// gui.SetLSPRouterReadOnly, makes the corresponding test below fail —
// hub-mcp.log appears under the broadened state dir after the request.
package cli

import (
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"mcp-local-hub/internal/api"

	"github.com/spf13/cobra"
)

// TestBuildRouteServer_BroadenedParentRegistryRead_NoHubMcpLogWrite proves
// the serena/registry side of finding 1: a real, registered-workspace
// Serena tools/call whose upstream forward fails (dead port — mirrors
// route_i6_readonly_test.go's fixture) must never create hub-mcp.log even
// when the state directory's OWN DACL is broadened to Authenticated Users,
// which makes Registry.Load() (called from
// serena_routing.WorkspaceResolver.refresh()) hit the parent-gate
// default-relax fallback on every request.
func TestBuildRouteServer_BroadenedParentRegistryRead_NoHubMcpLogWrite(t *testing.T) {
	tmp := t.TempDir()
	broadenDirAuthenticatedUsers(t, tmp)
	t.Setenv("LOCALAPPDATA", tmp)
	restoreState := api.SetDaemonStateRootForTest(tmp)
	t.Cleanup(restoreState)

	root := filepath.Join(tmp, "workspace-root")
	ws := i6MakeSerenaWorkspace(t, root, "Trusted")
	toolFile := i6WriteWorkspaceFile(t, ws, "src", "main.go")

	// Reserve then immediately close a TCP port so the upstream forward hits
	// a real connection-refused failure — the request must still complete
	// (not panic) regardless of the read-path fix under test.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserve port: %v", err)
	}
	deadPort := ln.Addr().(*net.TCPAddr).Port
	if err := ln.Close(); err != nil {
		t.Fatalf("close reserved port: %v", err)
	}

	registryPath, rerr := api.DefaultRegistryPath()
	if rerr != nil {
		t.Fatalf("resolve registry path: %v", rerr)
	}
	reg := api.NewRegistry(registryPath)
	if err := reg.PutSerena(api.WorkspaceEntry{
		WorkspaceKey:  api.WorkspaceKey(ws),
		WorkspacePath: ws,
		Language:      api.SerenaLanguageSentinel,
		Backend:       "serena",
		Port:          deadPort,
		TaskName:      "mcp-local-hub-serena-trusted",
	}); err != nil {
		t.Fatalf("PutSerena: %v", err)
	}
	if err := reg.Save(); err != nil {
		t.Fatalf("Save registry: %v", err)
	}

	cmd := &cobra.Command{}
	s, err := buildRouteServer(cmd, mcpFrontI6TestPort)
	if err != nil {
		t.Fatalf("buildRouteServer: %v", err)
	}

	// Full state-directory inventory (P2-6-style decisive criterion, not
	// merely "hub-mcp.log is absent"): a snapshot-diff catches ANY new or
	// changed file under the broadened state dir — including a lock file
	// left behind by a would-be writer — not just the one path this test
	// happens to name. i6SnapshotTree walks the whole tree recording
	// size+mtime per relative path, so any create/append/rename is caught
	// regardless of which leaf it touches.
	before := i6SnapshotTree(t, tmp)

	body := i6BuildToolCallBody(t, "find_symbol", map[string]any{"relative_path": toolFile})
	req := httptest.NewRequest(http.MethodPost, "/serena/mcp", strings.NewReader(body))
	req.Host = "127.0.0.1:19321"
	req.Header.Set("Sec-Fetch-Site", "same-origin")
	req.Header.Set("Origin", "http://127.0.0.1:19321")
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	s.RouteHandler().ServeHTTP(rr, req)

	if rr.Code == http.StatusOK {
		t.Fatalf("status = 200; fixture expected a forward failure against a closed port, got success body=%s", rr.Body.String())
	}

	after := i6SnapshotTree(t, tmp)
	if !reflect.DeepEqual(before, after) {
		t.Fatalf("state directory %s changed after a registered-workspace request via `mcphub route` on a BROADENED-parent state dir (want ZERO shared-state writes):\nbefore=%v\nafter=%v", tmp, before, after)
	}

	assertNoHubMcpLogUnder(t, tmp, "registered-workspace request via `mcphub route` on a BROADENED-parent state dir")
}

// TestBuildRouteServer_BroadenedParentLSPTrustedRootRead_NoHubMcpLogWrite
// proves the LSP/trusted-root side of finding 1: an unregistered-workspace
// LSP first-touch tools/call — which, per SetLSPRouterReadOnly's own doc
// comment, ALWAYS performs the TrustedRootCheckFn read (a pure read of
// lsp-trusted-roots.json) regardless of AutoRegisterFn being nil — must
// never create hub-mcp.log even when the state directory's own DACL is
// broadened, which makes that read hit the parent-gate default-relax
// fallback (the store file need not even exist yet: the parent gate fires
// before the missing-file check).
func TestBuildRouteServer_BroadenedParentLSPTrustedRootRead_NoHubMcpLogWrite(t *testing.T) {
	tmp := t.TempDir()
	broadenDirAuthenticatedUsers(t, tmp)
	t.Setenv("LOCALAPPDATA", tmp)
	restoreState := api.SetDaemonStateRootForTest(tmp)
	t.Cleanup(restoreState)

	// "go" is a real language in the shipped mcp-language-server manifest
	// (servers/mcp-language-server/manifest.yaml), with project marker
	// go.mod — buildRouteServer loads that manifest via the embed FS, so no
	// extra manifest fixture is needed.
	root := filepath.Join(tmp, "workspace-root", "GoProject")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatalf("mkdir go workspace: %v", err)
	}
	if err := os.WriteFile(filepath.Join(root, "go.mod"), []byte("module example.com/goproject\n\ngo 1.22\n"), 0o644); err != nil {
		t.Fatalf("write go.mod marker: %v", err)
	}
	toolFile := filepath.Join(root, "main.go")
	if err := os.WriteFile(toolFile, []byte("package main\n"), 0o644); err != nil {
		t.Fatalf("write workspace file: %v", err)
	}

	// Deliberately NO registry row and NO lsp-trusted-roots.json: this is
	// the unregistered, untrusted first-touch path — the trusted-root read
	// still executes (see SetLSPRouterReadOnly's doc comment) and must
	// still land on the injected sink, not hub-mcp.log, even though the
	// store file does not exist yet (the parent-DACL gate fires before the
	// missing-file determination).

	cmd := &cobra.Command{}
	s, err := buildRouteServer(cmd, mcpFrontI6TestPort)
	if err != nil {
		t.Fatalf("buildRouteServer: %v", err)
	}

	// Full state-directory inventory (see the serena test's companion
	// comment above) — catches any new/changed file under the broadened
	// state dir, not just the one path this test happens to name.
	before := i6SnapshotTree(t, tmp)

	body := i6BuildToolCallBody(t, "diagnostics", map[string]any{"file": toolFile})
	req := httptest.NewRequest(http.MethodPost, "/lsp/go/mcp", strings.NewReader(body))
	req.Host = "127.0.0.1:19321"
	req.Header.Set("Sec-Fetch-Site", "same-origin")
	req.Header.Set("Origin", "http://127.0.0.1:19321")
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	s.RouteHandler().ServeHTTP(rr, req)

	// Canonical shape for an UNTRUSTED unregistered workspace (no
	// lsp-trusted-roots.json exists, so LSPWorkspaceRootTrusted resolves
	// false): a 200 JSON-RPC error body naming NEEDS_TRUST / "is not
	// registered" — this is the trust-check's own refusal, one step BEFORE
	// the AutoRegisterFn==nil guard SetLSPRouterReadOnly's doc comment
	// describes. Either refusal shape proves the trusted-root read ran (a
	// short-circuit that skipped the read entirely could not reach either
	// message), so assert on the read-time evidence directly instead of
	// pinning one specific downstream refusal wording.
	if !strings.Contains(rr.Body.String(), "NEEDS_TRUST") && !strings.Contains(rr.Body.String(), "is not registered") &&
		!strings.Contains(rr.Body.String(), "auto-register is not configured") {
		t.Fatalf("body must show a canonical unregistered/untrusted-workspace refusal (proves the trusted-root read path was exercised); status=%d body=%s", rr.Code, rr.Body.String())
	}

	after := i6SnapshotTree(t, tmp)
	if !reflect.DeepEqual(before, after) {
		t.Fatalf("state directory %s changed after an unregistered-workspace LSP first-touch request via `mcphub route` on a BROADENED-parent state dir (want ZERO shared-state writes):\nbefore=%v\nafter=%v", tmp, before, after)
	}

	assertNoHubMcpLogUnder(t, tmp, "unregistered-workspace LSP first-touch request via `mcphub route` on a BROADENED-parent state dir")
}

func assertNoHubMcpLogUnder(t *testing.T, stateDir, context string) {
	t.Helper()
	hubLog := filepath.Join(stateDir, "hub-mcp.log")
	if _, statErr := os.Stat(hubLog); statErr == nil {
		t.Fatalf("hub-mcp.log exists at %s after a %s — the route daemon must never write the GUI-owned shared event log", hubLog, context)
	}
	hubLogLock := hubLog + ".lock"
	if _, statErr := os.Stat(hubLogLock); statErr == nil {
		t.Fatalf("hub-mcp.log.lock exists at %s after a %s", hubLogLock, context)
	}
}

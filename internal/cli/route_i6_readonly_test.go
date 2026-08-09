// internal/cli/route_i6_readonly_test.go
//
// I6 regression guard (sub-increment 2a,
// work-items/decisions/2026-07-25-increment2-mcp-front-port-ownership.md):
// mirrors internal/gui/route_readonly_test.go's F1 falsifying test, but
// exercises the ACTUAL production construction path `mcphub route` ships
// (buildRouteServer, extracted from runRoute in this same increment) rather
// than gui.Server's helpers in isolation. This increment adds a NEW,
// additional way for a client to reach this same handler (the settings-owned
// mcp_front.port via --reconcile-mcp-front, instead of only the GUI's own
// port) — the handler itself (s.RouteHandler(), unchanged by this increment)
// does not distinguish which port a request arrived on, so one test of the
// read-only invariant against the real construction path covers every access
// path uniformly.
//
// Mutation-proven: temporarily changing buildRouteServer to call
// s.SetSerenaRouterProduction (the non-read-only wiring) instead of
// s.SetSerenaRouterReadOnly makes this test fail at the first assertion —
// the request either succeeds or 5xx's attempting a live upstream forward
// (there is no real serena daemon behind the freshly-registered-but-empty
// registry), never surfacing the canonical 503 "register workspace first"
// shape, and per gui's own F1 test doc comment, that path also reaches a
// real registry write via AutoRegisterSerenaWorkspace.
package cli

import (
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"mcp-local-hub/internal/api"
	"mcp-local-hub/internal/api/apitest"

	"github.com/spf13/cobra"
)

// mcpFrontI6TestPort is an arbitrary fixed port used only as the Config.Port
// value the same-origin guard checks against — buildRouteServer never binds
// a real listener in this test (httptest.NewRecorder drives the handler
// in-process), so no real port is ever opened.
const mcpFrontI6TestPort = 19321

func TestBuildRouteServer_UnregisteredWorkspace_Returns503WithNoRegistryOrSupervisorIntentMutation(t *testing.T) {
	tmp := t.TempDir()
	// Redirects api.DefaultRegistryPath() (buildRouteServer's production
	// registry-path resolution — see workspace_registry.go:112, which reads
	// LOCALAPPDATA directly with no build-tag gate) into a throwaway temp
	// dir, so this test can never touch a real operator's workspaces.yaml.
	t.Setenv("LOCALAPPDATA", tmp)

	root := filepath.Join(tmp, "workspace-root")
	ws := i6MakeSerenaWorkspace(t, root, "Trusted")
	toolFile := i6WriteWorkspaceFile(t, ws, "src", "main.go")

	cmd := &cobra.Command{}
	s, _, err := buildRouteServer(cmd, mcpFrontI6TestPort)
	if err != nil {
		t.Fatalf("buildRouteServer: %v", err)
	}

	body := i6BuildToolCallBody(t, "find_symbol", map[string]any{"relative_path": toolFile})
	req := httptest.NewRequest(http.MethodPost, "/serena/mcp", strings.NewReader(body))
	// httptest.NewRequest defaults req.Host to "example.com" for a relative
	// URL — requireAllowedHost (internal/gui/csrf.go) checks the ACTUAL Host
	// header, so it must be set explicitly to match Config.Port
	// (mcpFrontI6TestPort) or the request never reaches the router handler
	// at all (403 HOST_NOT_ALLOWED before the 503 this test asserts).
	req.Host = "127.0.0.1:19321"
	req.Header.Set("Sec-Fetch-Site", "same-origin")
	req.Header.Set("Origin", "http://127.0.0.1:19321")
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	s.RouteHandler().ServeHTTP(rr, req)

	// Assertion 1: the canonical unregistered-workspace 503 — not a nil-panic
	// and not a forward-attempt status (502/504/200).
	if rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503 (register workspace first); body=%s", rr.Code, rr.Body.String())
	}
	var resp struct {
		PhaseEStatus string `json:"phase_e_status"`
		HintCommand  string `json:"hint_command"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode 503 body: %v; body=%s", err, rr.Body.String())
	}
	if resp.PhaseEStatus != "deferred" {
		t.Errorf("phase_e_status = %q, want %q (canonical unregistered-workspace shape)", resp.PhaseEStatus, "deferred")
	}
	if resp.HintCommand == "" {
		t.Errorf("hint_command empty, want the register-workspace hint (canonical shape)")
	}

	// Assertion 2: reload the registry FRESH from disk (not any in-memory
	// handle buildRouteServer's resolver holds) and confirm it is still
	// empty — no row under any key.
	registryPath, rerr := api.DefaultRegistryPath()
	if rerr != nil {
		t.Fatalf("resolve registry path: %v", rerr)
	}
	freshReg := api.NewRegistry(registryPath)
	if err := freshReg.Load(); err != nil {
		t.Fatalf("reload registry after request: %v", err)
	}
	if entries := freshReg.SerenaEntries(); len(entries) != 0 {
		t.Fatalf("registry mutated: %d serena entries after an unregistered-workspace tool-call via `mcphub route`, want 0: %+v", len(entries), entries)
	}
	if entry, ok := freshReg.GetSerena(api.WorkspaceKey(ws)); ok {
		t.Fatalf("registry mutated: found a serena entry for %q after the read-only forward, want none: %+v", ws, entry)
	}

	// Assertion 3: supervisor-intent.json — the OTHER state F1 named as a
	// possible write target (AutoRegisterSerenaWorkspace / EnsureLSPRegistered
	// touch both registry AND supervisor-intent) — was never created. This is
	// a REGRESSION GUARD, not new proof: buildRouteServer wires
	// SetSerenaRouterReadOnly/SetLSPRouterReadOnly with nil AutoRegisterFn/
	// WakeIdleFn (asserted by assertion 1+2 above reaching the 503 via the
	// nil-guard, not a real auto-register attempt), so there is structurally
	// no code path here that could reach a supervisor-intent write; this
	// assertion catches a FUTURE regression that accidentally wires the
	// production (non-read-only) primitives back into buildRouteServer.
	stateDir, serr := api.DaemonStateDirReadOnly()
	if serr == nil {
		intentPath := filepath.Join(stateDir, "supervisor-intent.json")
		if _, statErr := os.Stat(intentPath); statErr == nil {
			t.Fatalf("supervisor-intent.json exists at %s after an unregistered-workspace tool-call via `mcphub route` — read-only wiring must never create it", intentPath)
		} else if !os.IsNotExist(statErr) {
			t.Fatalf("stat %s: %v", intentPath, statErr)
		}
	}
}

// TestBuildRouteServer_RegisteredWorkspaceUnreachableBackend_NoSharedStateFileWrite
// is the P1-1 + P2-6 falsifying test through the REAL production
// construction path (buildRouteServer), rather than gui.Server's helpers in
// isolation (internal/gui/route_readonly_test.go carries the companion
// test). Unlike the unregistered-workspace test above, this REGISTERS the
// workspace first so the request reaches the upstream-forward path — the
// "serena-upstream-unreachable" branch that used to write hub-mcp.log (P1-1)
// — and asserts a COMPLETE before/after state-directory inventory (P2-6: the
// pre-existing test above only ever exercised the early-503 path, which
// returns before any forward attempt or persist call could run, so it never
// falsified the actual P1-1 bug).
//
// Mutation-proven: temporarily reverting gui.SetSerenaRouterReadOnly to
// leave AuditFn nil (its pre-P1-1-fix shape) makes this test fail —
// hub-mcp.log appears under the isolated state dir after the request.
func TestBuildRouteServer_RegisteredWorkspaceUnreachableBackend_NoSharedStateFileWrite(t *testing.T) {
	tmp := apitest.HardenedTempDir(t)
	t.Setenv("LOCALAPPDATA", tmp)
	restoreState := api.SetDaemonStateRootForTest(tmp)
	t.Cleanup(restoreState)

	root := filepath.Join(tmp, "workspace-root")
	ws := i6MakeSerenaWorkspace(t, root, "Trusted")
	toolFile := i6WriteWorkspaceFile(t, ws, "src", "main.go")

	// Reserve then immediately close a TCP port so the upstream forward hits
	// a real connection-refused failure.
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
	s, _, err := buildRouteServer(cmd, mcpFrontI6TestPort)
	if err != nil {
		t.Fatalf("buildRouteServer: %v", err)
	}

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
		t.Fatalf("state directory %s changed after a registered-workspace, unreachable-backend request via `mcphub route`'s real construction path (want ZERO shared-state writes beyond the registry seeded above):\nbefore=%v\nafter=%v", tmp, before, after)
	}

	hubLog := filepath.Join(tmp, "hub-mcp.log")
	if _, statErr := os.Stat(hubLog); statErr == nil {
		t.Fatalf("hub-mcp.log exists at %s after a request via `mcphub route` — the route daemon must never write the GUI-owned shared event log", hubLog)
	}
}

func i6SnapshotTree(t *testing.T, root string) map[string][2]int64 {
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

func i6MakeSerenaWorkspace(t *testing.T, root, name string) string {
	t.Helper()
	wsPath := filepath.Join(root, name)
	if err := os.MkdirAll(filepath.Join(wsPath, ".serena"), 0o755); err != nil {
		t.Fatalf("mkdir serena workspace: %v", err)
	}
	if err := os.WriteFile(filepath.Join(wsPath, ".serena", "project.yml"), []byte("project_name: "+name+"\n"), 0o644); err != nil {
		t.Fatalf("write serena marker: %v", err)
	}
	canon, err := api.CanonicalWorkspacePath(wsPath)
	if err != nil {
		t.Fatalf("canonicalize workspace: %v", err)
	}
	return canon
}

func i6WriteWorkspaceFile(t *testing.T, wsPath string, elems ...string) string {
	t.Helper()
	path := filepath.Join(append([]string{wsPath}, elems...)...)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir workspace file dir: %v", err)
	}
	if err := os.WriteFile(path, []byte("// fixture\n"), 0o644); err != nil {
		t.Fatalf("write workspace file: %v", err)
	}
	return path
}

func i6BuildToolCallBody(t *testing.T, toolName string, arguments any) string {
	t.Helper()
	argsRaw, err := json.Marshal(arguments)
	if err != nil {
		t.Fatalf("marshal arguments: %v", err)
	}
	env := map[string]any{
		"jsonrpc": "2.0",
		"id":      1,
		"method":  "tools/call",
		"params": map[string]any{
			"name":      toolName,
			"arguments": json.RawMessage(argsRaw),
		},
	}
	body, err := json.Marshal(env)
	if err != nil {
		t.Fatalf("marshal envelope: %v", err)
	}
	return string(body)
}

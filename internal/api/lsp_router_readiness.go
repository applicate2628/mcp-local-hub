// internal/api/lsp_router_readiness.go
//
// Route-liveness proof for the shared LSP router path, plus the two generic
// router-probe primitives it shares with the serena readiness ping.
//
// WHY THIS EXISTS (codex bot PR #588 P1 closure). `mcphub install
// --reconcile-mcp-front` rewrites TWO client surfaces onto mcp_front.port:
// each client's serena entry (-> /serena/mcp) and each client's canonical
// `mcp-language-server-<language>` entry (-> /lsp/<language>/mcp). Before the
// fix it established liveness for exactly ONE of them: the serena reconcile's
// own port-liveness proof (defaultRouterReadinessPing) doubled as the whole
// command's gate, and the LSP leg then rewrote every LSP client without ever
// asking whether the route it was pointing them at answers.
//
// That is not a theoretical gap. The route daemon wires its two routers
// INDEPENDENTLY (internal/cli/route.go buildRouteServer): /serena/mcp is
// mounted unconditionally, while the LSP router is only wired when the
// `mcp-language-server` manifest loads AND parses — both failures are logged
// to stderr and the daemon keeps serving. A route daemon in exactly that state
// passes the serena probe perfectly and answers every /lsp/<language>/mcp
// request with "lsp router is not configured". The cutover would rewrite every
// LSP client onto it and report success.
//
// It is the same defect class as the supervisor-ownership finding this file's
// sibling (mcp_front_port_ownership.go) closes: a client is repointed at an
// endpoint whose liveness was never established. Proving one route says
// nothing about another route in the same process.
package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// ErrLSPRouterRouteNotLive is the fail-closed sentinel for "the LSP router
// route at this port did not prove itself live". Callers must write NO client
// config when it fires: an LSP entry pointing at a route that does not answer
// is strictly worse than the entry the operator already had.
var ErrLSPRouterRouteNotLive = errors.New("the /lsp/<language>/mcp router route is not live at mcp_front.port")

// routerProbeBudget bounds each individual probe request. Same budget the
// serena readiness ping has always used; both probes are single loopback
// round-trips against a synthetic (backend-free) handler.
const routerProbeBudget = 2 * time.Second

// AssertLSPRouterRouteLive proves the LSP router route at port answers the MCP
// session lifecycle, using the SAME two-step proof the serena route gets:
// a HEAD that must produce the router's 405 + `Allow: POST` signature, then a
// real `initialize` round-trip that must return a JSON-RPC result.
//
// The probe targets ONE language, and that is sufficient by construction: the
// route daemon wires the LSP router for the whole manifest language set in a
// single call (SetLSPRouterReadOnly(resolver, sessions, m.Languages)), so the
// mount is all-or-nothing. The language is drawn from loadLSPRouterLanguages —
// the same resolver the forward pass uses to decide which entries to write —
// so the probe can never test a language set the writer does not share.
//
// `initialize` is answered SYNTHETICALLY by the router (internal/gui/
// lsp_router.go handleLSPInitialize) with no backend touch, so this proves the
// route is mounted and serving without materializing a language backend or
// paying a cold-start. An unwired router fails it: the dispatcher's
// "lsp router is not configured" reply is a JSON-RPC error, not a result.
//
// Pure read: nothing is mutated on any path.
func AssertLSPRouterRouteLive(ctx context.Context, port int) error {
	if port <= 0 {
		return fmt.Errorf("%w: refusing to probe non-positive port %d", ErrLSPRouterRouteNotLive, port)
	}
	languages, err := loadLSPRouterLanguages(nil)
	if err != nil {
		return fmt.Errorf("%w: cannot resolve the LSP language set to probe: %v", ErrLSPRouterRouteNotLive, err)
	}
	if len(languages) == 0 {
		return fmt.Errorf("%w: the %s manifest declares no languages, so no LSP route exists to prove", ErrLSPRouterRouteNotLive, lspManifestServerName)
	}
	language := languages[0]
	path := fmt.Sprintf(lspRouterURLPathTemplate, language)
	if perr := routerRouteShapeProbe(ctx, port, path); perr != nil {
		return fmt.Errorf("%w: %v — the supervisor-managed `mcphub route` daemon wires /serena/mcp and /lsp/<language>/mcp independently, so a live serena route does not imply a live LSP route (a route daemon whose mcp-language-server manifest failed to load keeps serving serena and never mounts this one). Check the route daemon's stderr, then re-run", ErrLSPRouterRouteNotLive, perr)
	}
	if perr := routerInitializeLifecycleProbe(ctx, port, path); perr != nil {
		return fmt.Errorf("%w: %v — no LSP client config was written", ErrLSPRouterRouteNotLive, perr)
	}
	return nil
}

// routerRouteShapeProbe issues a loopback HEAD to path and returns nil only
// when the answer carries the mcphub MCP router's signature: 405 with `Allow`
// naming POST. Both the serena router (internal/gui/serena_router.go) and the
// LSP router (internal/gui/lsp_router.go, which answers `Allow: POST, DELETE`)
// produce it; an unrelated service that merely reused the port does not.
//
// Any transport error fails closed. Loopback-only, so a remote or spoofed
// endpoint can never satisfy it.
//
// This is the single owner of the "does this port speak mcphub-MCP-router at
// this path" decision — the serena readiness ping and the LSP route assertion
// must never drift on what that signature is.
func routerRouteShapeProbe(ctx context.Context, port int, path string) error {
	url := fmt.Sprintf("http://127.0.0.1:%d%s", port, path)
	reqCtx, cancel := context.WithTimeout(ctx, routerProbeBudget)
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, http.MethodHead, url, nil)
	if err != nil {
		return err
	}
	client := &http.Client{Timeout: routerProbeBudget}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusMethodNotAllowed || !strings.Contains(strings.ToUpper(resp.Header.Get("Allow")), "POST") {
		return fmt.Errorf("port %d responded but is not an mcphub MCP router at %s (HEAD -> status %d, Allow=%q; expected 405 with Allow: POST)", port, path, resp.StatusCode, resp.Header.Get("Allow"))
	}
	return nil
}

// routerInitializeLifecycleProbe POSTs a minimal MCP `initialize` to path and
// returns nil only when the router answers with a JSON-RPC RESULT — i.e. it
// actually serves the session lifecycle a real client opens with. A non-200
// status, a JSON-RPC error, or a missing result fails closed.
//
// Single-owned for the same reason as routerRouteShapeProbe: "the route serves
// the MCP lifecycle" must mean one thing across every route this codebase
// points a client config at.
func routerInitializeLifecycleProbe(ctx context.Context, port int, path string) error {
	url := fmt.Sprintf("http://127.0.0.1:%d%s", port, path)
	reqCtx, cancel := context.WithTimeout(ctx, routerProbeBudget)
	defer cancel()
	const initBody = `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2024-11-05","capabilities":{},"clientInfo":{"name":"mcphub-reconcile-probe","version":"0"}}}`
	req, err := http.NewRequestWithContext(reqCtx, http.MethodPost, url, strings.NewReader(initBody))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	client := &http.Client{Timeout: routerProbeBudget}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("router at port %d does not serve the MCP lifecycle at %s: initialize -> status %d (body=%.200s)", port, path, resp.StatusCode, string(raw))
	}
	var rpc struct {
		Result json.RawMessage `json:"result"`
		Error  json.RawMessage `json:"error"`
	}
	if jerr := json.Unmarshal(raw, &rpc); jerr != nil {
		return fmt.Errorf("router at port %d returned a non-JSON-RPC initialize response at %s: %w", port, path, jerr)
	}
	if len(rpc.Error) > 0 || len(rpc.Result) == 0 {
		return fmt.Errorf("router at port %d rejected MCP initialize at %s (no result; error=%s)", port, path, string(rpc.Error))
	}
	return nil
}

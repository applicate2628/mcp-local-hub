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
	"sync"
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
// Every language/backend route declared by the accepted embedded manifest is
// probed. The router dispatches by language and backend, so one healthy sibling
// cannot establish readiness for another. Probes run concurrently and their
// results are merged in canonical manifest order, keeping both the budget and
// the selected error deterministic.
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
		return newLSPRouterRouteLiveError("", "", MCPFrontProbeStageInput,
			fmt.Errorf("%w: refusing to probe non-positive port %d", ErrLSPRouterRouteNotLive, port))
	}
	specs, err := loadLSPRouterLanguageSpecs(nil)
	if err != nil {
		return newLSPRouterRouteLiveError("", "", MCPFrontProbeStageRouteSetLoad,
			fmt.Errorf("%w: cannot resolve the LSP language set to probe: %w", ErrLSPRouterRouteNotLive, err))
	}
	if len(specs) == 0 {
		return newLSPRouterRouteLiveError("", "", MCPFrontProbeStageRouteSetEmpty,
			fmt.Errorf("%w: the %s manifest declares no languages, so no LSP route exists to prove", ErrLSPRouterRouteNotLive, lspManifestServerName))
	}

	results := make([]error, len(specs))
	var probes sync.WaitGroup
	probes.Add(len(specs))
	for i, spec := range specs {
		i, spec := i, spec
		go func() {
			defer probes.Done()
			results[i] = assertLSPRouterLanguageRouteLive(ctx, port, spec.Name, spec.Backend)
		}()
	}
	probes.Wait()
	for _, err := range results {
		if err != nil {
			return err
		}
	}
	return nil
}

type lspRouterRouteLiveError struct {
	Language   string
	Backend    string
	ProbeStage MCPFrontProbeStage
	Cause      error
}

func (e *lspRouterRouteLiveError) Error() string { return e.Cause.Error() }
func (e *lspRouterRouteLiveError) Unwrap() error { return e.Cause }

func newLSPRouterRouteLiveError(language, backend string, stage MCPFrontProbeStage, cause error) *lspRouterRouteLiveError {
	return &lspRouterRouteLiveError{Language: language, Backend: backend, ProbeStage: stage, Cause: cause}
}

func assertLSPRouterLanguageRouteLive(ctx context.Context, port int, language, backend string) error {
	path := fmt.Sprintf(lspRouterURLPathTemplate, language)
	if perr := routerRouteShapeProbe(ctx, port, path); perr != nil {
		return newLSPRouterRouteLiveError(language, backend, probeStageFromError(perr),
			fmt.Errorf("%w: language %q backend %q: %w — the supervisor-managed `mcphub route` daemon wires and dispatches LSP routes independently; check the route daemon's stderr, then re-run", ErrLSPRouterRouteNotLive, language, backend, perr))
	}
	if perr := routerInitializeLifecycleProbe(ctx, port, path); perr != nil {
		return newLSPRouterRouteLiveError(language, backend, probeStageFromError(perr),
			fmt.Errorf("%w: language %q backend %q: %w — no LSP client config was written", ErrLSPRouterRouteNotLive, language, backend, perr))
	}
	return nil
}

type routerProbeError struct {
	Stage MCPFrontProbeStage
	Cause error
}

func (e *routerProbeError) Error() string { return e.Cause.Error() }
func (e *routerProbeError) Unwrap() error { return e.Cause }

func probeStageFromError(err error) MCPFrontProbeStage {
	var probeErr *routerProbeError
	if errors.As(err, &probeErr) {
		return probeErr.Stage
	}
	return MCPFrontProbeStageInput
}

func newRouterProbeError(ctx context.Context, stage MCPFrontProbeStage, cause error) *routerProbeError {
	if ctxErr := ctx.Err(); errors.Is(ctxErr, context.DeadlineExceeded) {
		stage = MCPFrontProbeStageParentDeadline
	} else if errors.Is(ctxErr, context.Canceled) {
		stage = MCPFrontProbeStageParentCanceled
	}
	return &routerProbeError{Stage: stage, Cause: cause}
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
		return newRouterProbeError(ctx, MCPFrontProbeStageShapeTransport, err)
	}
	client := &http.Client{Timeout: routerProbeBudget}
	resp, err := client.Do(req)
	if err != nil {
		return newRouterProbeError(ctx, MCPFrontProbeStageShapeTransport, err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusMethodNotAllowed || !strings.Contains(strings.ToUpper(resp.Header.Get("Allow")), "POST") {
		return newRouterProbeError(ctx, MCPFrontProbeStageShapeResponse,
			fmt.Errorf("port %d responded but is not an mcphub MCP router at %s (HEAD -> status %d, Allow=%q; expected 405 with Allow: POST)", port, path, resp.StatusCode, resp.Header.Get("Allow")))
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
		return newRouterProbeError(ctx, MCPFrontProbeStageInitializeTransport, err)
	}
	req.Header.Set("Content-Type", "application/json")
	client := &http.Client{Timeout: routerProbeBudget}
	resp, err := client.Do(req)
	if err != nil {
		return newRouterProbeError(ctx, MCPFrontProbeStageInitializeTransport, err)
	}
	defer func() { _ = resp.Body.Close() }()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusOK {
		return newRouterProbeError(ctx, MCPFrontProbeStageInitializeHTTPStatus,
			fmt.Errorf("router at port %d does not serve the MCP lifecycle at %s: initialize -> status %d (body=%.200s)", port, path, resp.StatusCode, string(raw)))
	}
	var rpc struct {
		Result json.RawMessage `json:"result"`
		Error  json.RawMessage `json:"error"`
	}
	if jerr := json.Unmarshal(raw, &rpc); jerr != nil {
		return newRouterProbeError(ctx, MCPFrontProbeStageInitializeJSONDecode,
			fmt.Errorf("router at port %d returned a non-JSON-RPC initialize response at %s: %w", port, path, jerr))
	}
	if len(rpc.Error) > 0 {
		return newRouterProbeError(ctx, MCPFrontProbeStageInitializeJSONRPCError,
			fmt.Errorf("router at port %d rejected MCP initialize at %s (error=%s)", port, path, string(rpc.Error)))
	}
	if len(rpc.Result) == 0 {
		return newRouterProbeError(ctx, MCPFrontProbeStageInitializeResultMissing,
			fmt.Errorf("router at port %d returned MCP initialize without a result at %s", port, path))
	}
	return nil
}

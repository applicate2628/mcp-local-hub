// internal/api/mcp_front_routes.go
//
// The single owner of the claim that a supervisor-managed MCP front is ready
// for client routing. A front is ready only when both independently-mounted
// route families answer their MCP lifecycle probes.
package api

import (
	"context"
	"errors"
	"fmt"
)

// mcpFrontRoutesLiveTimeout bounds the combined proof. Serena makes two
// sequential requests; every manifest-owned LSP route then makes its own pair
// concurrently. Five child budgets therefore cover Serena's pair, the longest
// LSP pair, and one budget of scheduling slack independent of language count.
const mcpFrontRoutesLiveTimeout = 5 * routerProbeBudget

// MCPFrontRouteNotReadyCode is the stable top-level discriminator for every
// fail-closed combined front-readiness rejection.
const MCPFrontRouteNotReadyCode = "MCP_FRONT_ROUTE_NOT_READY"

// MCPFrontRouteStage identifies the independently-mounted front route that
// failed readiness.
type MCPFrontRouteStage string

const (
	MCPFrontRouteStageFront  MCPFrontRouteStage = "front"
	MCPFrontRouteStageSerena MCPFrontRouteStage = "serena"
	MCPFrontRouteStageLSP    MCPFrontRouteStage = "lsp"
)

// MCPFrontProbeStage identifies the exact readiness substage that failed.
type MCPFrontProbeStage string

const (
	MCPFrontProbeStageInput                      MCPFrontProbeStage = "input"
	MCPFrontProbeStageRouteSetLoad               MCPFrontProbeStage = "route-set-load"
	MCPFrontProbeStageRouteSetEmpty              MCPFrontProbeStage = "route-set-empty"
	MCPFrontProbeStageShapeTransport             MCPFrontProbeStage = "shape-transport"
	MCPFrontProbeStageShapeResponse              MCPFrontProbeStage = "shape-response"
	MCPFrontProbeStageInitializeTransport        MCPFrontProbeStage = "initialize-transport"
	MCPFrontProbeStageInitializeHTTPStatus       MCPFrontProbeStage = "initialize-http-status"
	MCPFrontProbeStageInitializeJSONDecode       MCPFrontProbeStage = "initialize-json-decode"
	MCPFrontProbeStageInitializeJSONRPCError     MCPFrontProbeStage = "initialize-jsonrpc-error"
	MCPFrontProbeStageInitializeResultMissing    MCPFrontProbeStage = "initialize-result-missing"
	MCPFrontProbeStageInitializeSessionIDMissing MCPFrontProbeStage = "initialize-session-id-missing"
	MCPFrontProbeStageSessionCleanupTransport    MCPFrontProbeStage = "session-cleanup-transport"
	MCPFrontProbeStageSessionCleanupResponse     MCPFrontProbeStage = "session-cleanup-response"
	MCPFrontProbeStageParentDeadline             MCPFrontProbeStage = "parent-deadline"
	MCPFrontProbeStageParentCanceled             MCPFrontProbeStage = "parent-canceled"
)

// MCPFrontRoutesLiveError preserves both the failed route stage and its
// route-specific cause. Callers can use errors.As for the stage and errors.Is
// for ErrSerenaRouterRouteNotLive or ErrLSPRouterRouteNotLive.
type MCPFrontRoutesLiveError struct {
	Code       string
	Stage      MCPFrontRouteStage
	ProbeStage MCPFrontProbeStage
	Language   string
	Backend    string
	Cause      error
}

func (e *MCPFrontRoutesLiveError) Error() string {
	target := string(e.Stage)
	if e.Language != "" {
		target += fmt.Sprintf(" language=%q backend=%q", e.Language, e.Backend)
	}
	return fmt.Sprintf("%s: route=%s probe_stage=%s: %v", e.Code, target, e.ProbeStage, e.Cause)
}

func (e *MCPFrontRoutesLiveError) Unwrap() error {
	return e.Cause
}

// AssertMCPFrontRoutesLive proves that both routes a front cutover writes into
// client configuration are live: /serena/mcp and /lsp/<language>/mcp. It is
// read-only and fails closed at the first failed stage.
func AssertMCPFrontRoutesLive(ctx context.Context, port int) error {
	if port <= 0 {
		return newMCPFrontRoutesLiveError(ctx, MCPFrontRouteStageFront, "", "", MCPFrontProbeStageInput,
			fmt.Errorf("refusing to probe non-positive port %d", port))
	}
	ctx, cancel := context.WithTimeout(ctx, mcpFrontRoutesLiveTimeout)
	defer cancel()

	if err := AssertSerenaRouterRouteLive(ctx, port); err != nil {
		return newMCPFrontRoutesLiveError(ctx, MCPFrontRouteStageSerena, "", "", probeStageFromError(err), err)
	}
	if err := AssertLSPRouterRouteLive(ctx, port); err != nil {
		var routeErr *lspRouterRouteLiveError
		if errors.As(err, &routeErr) {
			return newMCPFrontRoutesLiveError(ctx, MCPFrontRouteStageLSP, routeErr.Language, routeErr.Backend, routeErr.ProbeStage, err)
		}
		return newMCPFrontRoutesLiveError(ctx, MCPFrontRouteStageLSP, "", "", probeStageFromError(err), err)
	}
	return nil
}

func newMCPFrontRoutesLiveError(ctx context.Context, route MCPFrontRouteStage, language, backend string, stage MCPFrontProbeStage, cause error) *MCPFrontRoutesLiveError {
	if ctxErr := ctx.Err(); errors.Is(ctxErr, context.DeadlineExceeded) {
		stage = MCPFrontProbeStageParentDeadline
	} else if errors.Is(ctxErr, context.Canceled) {
		stage = MCPFrontProbeStageParentCanceled
	}
	return &MCPFrontRoutesLiveError{
		Code:       MCPFrontRouteNotReadyCode,
		Stage:      route,
		ProbeStage: stage,
		Language:   language,
		Backend:    backend,
		Cause:      cause,
	}
}

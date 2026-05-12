// hub_mcp_aggregator.go — Phase 3 Task 3.4 (G4 unified hub MCP).
//
// Fan-out aggregator for the hub's three client-origin lifecycle
// methods: AggregateInitialize, AggregateToolsList, AggregateToolsCall.
// Plus ForwardCancellation for notifications/cancelled. Phase 4 wires
// these to the HTTP handler; Phase 3 exercises them via direct method
// calls in tests.
//
// Concurrency:
//
//   - FanOutConcurrency = 8: cap on simultaneous outbound HTTP calls
//     across all participating daemons within a single fan-out
//     invocation. Enforced with a buffered-channel semaphore.
//   - PerDaemonInitTimeout = 5s: each initialize sub-call has its own
//     context.WithTimeout. Independent of the wall-clock cap on the
//     fan-out as a whole.
//   - PerDaemonListTimeout = 10s: same shape for tools/list.
//   - PerCallWallClockCap = 60s: per-tools/call wall-clock cap that
//     also drives in-flight cleanup if the daemon hangs.
//
// SSE-or-JSON parsing reuses the pattern from health.go:687-805 —
// MCP daemons respond either as plain application/json or as a
// text/event-stream with a single `data: <json>` line. The aggregator
// accepts both shapes.
//
// Partial-failure surface:
//
//   - InitFailures append rows with stage="initialize".
//   - tools/list-time failures append rows with stage="tools/list".
//   - When ≥ 1 daemon succeeded, the failure rows surface in
//     result._meta.mcphub.partialFailures.
//   - When ALL participating daemons failed (no init successes AND
//     no list-time successes), the response is a JSON-RPC -32000
//     error envelope with data.mcphub.partialFailures.
//
// Tool-name namespacing:
//
//   - Exposed name = "<server>__<rawname>".
//   - The hub does NOT split on "__" — the route map is keyed by
//     the WHOLE exposed string.
//   - Hub rewrites params.name to RawName before forwarding tools/call.
//
// Resolver-snapshot revalidation on tools/call:
//
//   - Session captured a *ResolverSnapshot at initialize.
//   - tools/call loads the CURRENT snapshot via LoadResolverSnapshot.
//   - If the route's (Server, Daemon) tuple is NOT in the current
//     bindings for this client AND the snapshot pointer has changed,
//     refuse with -32601 "tool moved out of scope; reinitialize session".
//
// Spec: docs/superpowers/specs/2026-05-12-g4-unified-hub-mcp-design-v3.md
// §"Per-hub session model" + §"Partial-failure visibility" +
// §"Tool-name namespacing". Plan: Task 3.4.

package api

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sync"
	"time"

	"mcp-local-hub/internal/buildinfo"
)

// Concurrency + timing constants. Spec §"Concurrency + bounds".
const (
	FanOutConcurrency    = 8
	PerDaemonInitTimeout = 5 * time.Second
	PerDaemonListTimeout = 10 * time.Second
	PerCallWallClockCap  = 60 * time.Second

	// maxAggregatorResponseBytes caps a single per-daemon response.
	// Avoids OOM on a buggy or hostile daemon emitting unbounded
	// data. Mirrors maxHealthProbeResponseBytes scale.
	maxAggregatorResponseBytes = 4 * 1024 * 1024

	// hubProtocolVersionFallback is the MCP protocol version the hub
	// uses for outbound initialize when the session's
	// session.ProtocolVersion is empty (defensive — Phase 4 ensures
	// the session always has a version, but Phase 3 callers may
	// construct sessions without one).
	hubProtocolVersionFallback = "2025-11-25"
)

// AggregateInitialize fans out initialize to every daemon in
// sess.IntendedParticipants under FanOutConcurrency. Populates
// sess.InitSuccesses (per-daemon Mcp-Session-Id) and
// sess.InitFailures (rows with stage="initialize").
//
// Returns a synthetic hub-side initialize result body. The body shape
// reuses the daemon-facing protocol version + advertises tools as the
// hub's only capability (Phase 3 — prompts/resources are deferred).
//
// reqID is the CLIENT's JSON-RPC initialize-request id, echoed back
// verbatim in the synthetic response. Hardcoding it would break
// request/response correlation for clients sending string ids or
// non-1 number ids (codex bot r1 P1 closure on PR #157).
func AggregateInitialize(ctx context.Context, sess *hubSession, reqID json.RawMessage) ([]byte, error) {
	protoVer := sess.ProtocolVersion
	if protoVer == "" {
		protoVer = hubProtocolVersionFallback
	}

	type initResult struct {
		ref       canonicalDaemonRef
		sessionID string
		err       error
	}
	results := make([]initResult, len(sess.IntendedParticipants))

	sem := make(chan struct{}, FanOutConcurrency)
	var wg sync.WaitGroup
	for i, ref := range sess.IntendedParticipants {
		i, ref := i, ref
		wg.Add(1)
		sem <- struct{}{}
		go func() {
			defer wg.Done()
			defer func() { <-sem }()
			subCtx, cancel := context.WithTimeout(ctx, PerDaemonInitTimeout)
			defer cancel()
			sid, err := postInitialize(subCtx, ref, protoVer)
			// codex bot r4 P1 closure on PR #157: per MCP lifecycle,
			// the client (hub) MUST send notifications/initialized
			// after initialize succeeds. Strict daemons can reject or
			// ignore subsequent tools/list / tools/call until they
			// observe this notification. Best-effort: a failed
			// notification doesn't roll the init back — the per-call
			// HTTP error path will surface any downstream issue with
			// a clear stage label. Use a fresh subCtx with the init
			// timeout because the original subCtx is being torn down
			// via defer cancel() at goroutine exit.
			if err == nil {
				notifyCtx, notifyCancel := context.WithTimeout(ctx, PerDaemonInitTimeout)
				_ = postInitialized(notifyCtx, ref, sid, protoVer)
				notifyCancel()
			}
			results[i] = initResult{ref: ref, sessionID: sid, err: err}
		}()
	}
	wg.Wait()

	// Apply results to the session.
	sess.mu.Lock()
	defer sess.mu.Unlock()
	for _, r := range results {
		if r.err != nil {
			sess.InitFailures = append(sess.InitFailures, DaemonFailure{
				Server: r.ref.Server,
				Daemon: r.ref.Daemon,
				Stage:  "initialize",
				Err:    r.err.Error(),
			})
			continue
		}
		sess.InitSuccesses[r.ref] = r.sessionID
	}

	// Build a synthetic initialize result envelope.
	body, err := buildSyntheticInitResult(reqID, protoVer)
	if err != nil {
		return nil, err
	}
	return body, nil
}

// listResult is the per-daemon outcome of a tools/list fan-out call.
// On err == nil, tools + toolMap carry the namespaced contribution to
// the merged response. On err != nil, the row turns into a
// stage="tools/list" partialFailure.
type listResult struct {
	ref     canonicalDaemonRef
	tools   []json.RawMessage
	toolMap map[string]canonicalToolRef
	err     error
}

// AggregateToolsList fans out tools/list to every daemon in
// sess.InitSuccesses. Merges into a flat exposed-name route map keyed
// "<server>__<rawname>". The session's RouteMap (atomic.Pointer) is
// swapped to the freshly-built map.
//
// _meta.mcphub.partialFailures combines stored InitFailures with
// list-time failures (stage="tools/list"). If no list calls succeeded
// (whether because no init succeeded OR every list call failed), the
// response is a JSON-RPC -32000 error envelope with
// data.mcphub.partialFailures. Note: initialize succeeding but EVERY
// list call failing also lands here — surfacing the call as a failure
// is intentional, since the caller can't discover any tools.
func AggregateToolsList(ctx context.Context, sess *hubSession, reqID json.RawMessage) ([]byte, error) {
	// Snapshot the inputs under the session mu.
	sess.mu.Lock()
	successes := make(map[canonicalDaemonRef]string, len(sess.InitSuccesses))
	for ref, sid := range sess.InitSuccesses {
		successes[ref] = sid
	}
	initFailures := make([]DaemonFailure, len(sess.InitFailures))
	copy(initFailures, sess.InitFailures)
	sess.mu.Unlock()

	results := fanOutToolsList(ctx, successes, sess.ProtocolVersion)
	return assembleToolsListResponse(reqID, results, initFailures, sess)
}

// fanOutToolsList issues parallel tools/list calls to every daemon
// whose initialize succeeded. Returns one listResult per daemon. The
// caller (assembleToolsListResponse) merges + namespaces + publishes
// the route map.
//
// protoVer is the session's negotiated MCP protocol version, passed
// to postToolsList for the MCP-Protocol-Version header (codex bot r4
// P1 closure on PR #157).
func fanOutToolsList(ctx context.Context, successes map[canonicalDaemonRef]string, protoVer string) []listResult {
	results := make([]listResult, 0, len(successes))
	var resultsMu sync.Mutex

	sem := make(chan struct{}, FanOutConcurrency)
	var wg sync.WaitGroup
	for ref, sid := range successes {
		ref, sid := ref, sid
		wg.Add(1)
		sem <- struct{}{}
		go func() {
			defer wg.Done()
			defer func() { <-sem }()
			subCtx, cancel := context.WithTimeout(ctx, PerDaemonListTimeout)
			defer cancel()
			tools, err := postToolsList(subCtx, ref, sid, protoVer)
			r := listResult{ref: ref, err: err}
			if err == nil {
				r.tools, r.toolMap = nameSpaceTools(ref, tools)
			}
			resultsMu.Lock()
			results = append(results, r)
			resultsMu.Unlock()
		}()
	}
	wg.Wait()
	return results
}

// assembleToolsListResponse merges per-daemon list results into the
// final response envelope. Publishes the session's RouteMap atomically
// before returning so a concurrent tools/call sees the new routes.
//
// Decision: all-failed (-32000) vs success-with-partial-failures uses
// listSuccessCount > 0 as the sole criterion. An init success with
// zero list successes is treated as all-failed — the caller cannot
// discover any tools, so we surface the failures via the error
// envelope rather than returning result.tools=[].
func assembleToolsListResponse(reqID json.RawMessage, results []listResult, initFailures []DaemonFailure, sess *hubSession) ([]byte, error) {
	mergedTools := make([]json.RawMessage, 0)
	mergedRoutes := make(map[string]canonicalToolRef)
	listFailures := make([]DaemonFailure, 0)
	listSuccessCount := 0
	for _, r := range results {
		if r.err != nil {
			listFailures = append(listFailures, DaemonFailure{
				Server: r.ref.Server,
				Daemon: r.ref.Daemon,
				Stage:  "tools/list",
				Err:    r.err.Error(),
			})
			continue
		}
		listSuccessCount++
		mergedTools = append(mergedTools, r.tools...)
		for k, v := range r.toolMap {
			mergedRoutes[k] = v
		}
	}

	// Publish the route map to the session BEFORE returning so a
	// concurrent tools/call sees it.
	sess.RouteMap.Store(&mergedRoutes)

	allFailures := append([]DaemonFailure{}, initFailures...)
	allFailures = append(allFailures, listFailures...)

	if listSuccessCount == 0 {
		return buildAllFailedToolsListResponse(reqID, allFailures)
	}
	return buildToolsListResponse(reqID, mergedTools, allFailures)
}

// resolvedCallTarget is the output of route lookup + resolver
// revalidation. Either errBody is set (the caller returns it verbatim
// as the final response) or the ref/daemonSID fields drive the
// outbound HTTP forward.
type resolvedCallTarget struct {
	ref       canonicalToolRef
	daemonSID string
	errBody   []byte
}

// AggregateToolsCall looks up params.name in sess.RouteMap, revalidates
// (Client, Server, Daemon) against the CURRENT resolver snapshot, and
// forwards the call with params.name rewritten to RawName.
//
// On stale resolver → -32601 "tool moved out of scope".
// On unknown name → -32601 "Method not found: <name>".
// On daemon error → response body passed through verbatim.
func AggregateToolsCall(ctx context.Context, sess *hubSession, clientReqID json.RawMessage, paramsRaw json.RawMessage) ([]byte, error) {
	target, err := resolveToolsCallRoute(sess, clientReqID, paramsRaw)
	if err != nil {
		return nil, err
	}
	if target.errBody != nil {
		return target.errBody, nil
	}
	return dispatchToolsCall(ctx, sess, clientReqID, paramsRaw, target.ref, target.daemonSID)
}

// resolveToolsCallRoute parses params.name, looks it up in the
// session's RouteMap, revalidates against the current resolver
// snapshot, and resolves the daemon Mcp-Session-Id. On any client- or
// state-level rejection (-32602, -32601, -32603) returns the error
// envelope via target.errBody; the caller passes it through verbatim.
// Returns (nil-target, err) only on internal JSON-marshal failure
// inside buildJSONRPCError — never on routing rejection.
func resolveToolsCallRoute(sess *hubSession, clientReqID, paramsRaw json.RawMessage) (resolvedCallTarget, error) {
	// Parse params to extract name. Other fields (arguments, _meta,
	// extension attrs) survive verbatim through buildRewrittenParams's
	// generic-map round-trip; we only need name here to drive route
	// lookup.
	var p struct {
		Name string `json:"name"`
	}
	if err := json.Unmarshal(paramsRaw, &p); err != nil {
		body, mErr := buildJSONRPCError(clientReqID, -32602, "Invalid params: "+err.Error(), nil)
		return resolvedCallTarget{errBody: body}, mErr
	}
	if p.Name == "" {
		body, mErr := buildJSONRPCError(clientReqID, -32602, "Invalid params: missing name", nil)
		return resolvedCallTarget{errBody: body}, mErr
	}

	// Route lookup on the session's RouteMap.
	rmPtr := sess.RouteMap.Load()
	if rmPtr == nil {
		body, mErr := buildJSONRPCError(clientReqID, -32601, "Method not found: "+p.Name, nil)
		return resolvedCallTarget{errBody: body}, mErr
	}
	ref, ok := (*rmPtr)[p.Name]
	if !ok {
		body, mErr := buildJSONRPCError(clientReqID, -32601, "Method not found: "+p.Name, nil)
		return resolvedCallTarget{errBody: body}, mErr
	}

	// Resolver-snapshot revalidation: refuse if (Server, Daemon) is
	// not in the calling client's current bindings AND the snapshot
	// pointer has moved.
	current := LoadResolverSnapshot()
	if current != nil && current != sess.SnapshotAtInit {
		if !daemonStillBound(current, sess.Client, ref) {
			body, mErr := buildJSONRPCError(clientReqID, -32601, "tool moved out of scope; reinitialize session", nil)
			return resolvedCallTarget{errBody: body}, mErr
		}
	}

	// Look up the daemon's Mcp-Session-Id.
	sess.mu.Lock()
	daemonSID, hasSID := sess.InitSuccesses[canonicalDaemonRef{Server: ref.Server, Daemon: ref.Daemon, Port: ref.Port}]
	sess.mu.Unlock()
	if !hasSID {
		body, mErr := buildJSONRPCError(clientReqID, -32603, "Internal error: no daemon session id for target", nil)
		return resolvedCallTarget{errBody: body}, mErr
	}
	return resolvedCallTarget{ref: ref, daemonSID: daemonSID}, nil
}

// dispatchToolsCall builds the rewritten body, registers an in-flight
// row, forwards to the daemon under PerCallWallClockCap, and rewrites
// the daemon's response id back to the client's id. Removes the
// in-flight row via defer.
func dispatchToolsCall(ctx context.Context, sess *hubSession, clientReqID, paramsRaw json.RawMessage, ref canonicalToolRef, daemonSID string) ([]byte, error) {
	// Build the rewritten body. params.name → RawName.
	rewrittenParams, err := buildRewrittenParams(ref.RawName, paramsRaw)
	if err != nil {
		return buildJSONRPCError(clientReqID, -32603, "Internal error: "+err.Error(), nil)
	}

	// Hub-generated daemon request id. Use a hex prefix that callers
	// can grep on for diagnostics. The id MUST be a JSON value, not
	// a Go string — wrap with quotes when emitting.
	daemonReqID, err := generateDaemonRequestID()
	if err != nil {
		return buildJSONRPCError(clientReqID, -32603, "Internal error: "+err.Error(), nil)
	}

	// Insert in-flight row BEFORE issuing the HTTP call so a racing
	// cancel can find the entry.
	key, err := newRequestIDKey(clientReqID)
	if err != nil {
		// clientReqID failed validation (shouldn't reach this far —
		// handler validates earlier — but be defensive).
		return buildJSONRPCError(clientReqID, -32600, err.Error(), nil)
	}
	// Reject duplicate client request id (codex bot r1 P2 closure on
	// PR #157). InsertInFlight returns false on collision; we MUST
	// refuse the second call rather than overwrite the first entry,
	// otherwise cancellation/cleanup would target the wrong daemon
	// request and leave the original call untracked. JSON-RPC clients
	// MUST use distinct ids within a session — this is a protocol
	// contract violation, surfaced as -32600.
	inserted := sess.InsertInFlight(key, inflightEntry{
		DaemonRef:       canonicalDaemonRef{Server: ref.Server, Daemon: ref.Daemon, Port: ref.Port},
		DaemonSessionID: daemonSID,
		DaemonRequestID: daemonReqID,
		StartedAt:       time.Now(),
	})
	if !inserted {
		return buildJSONRPCError(clientReqID, -32600, "duplicate request id; original call still in flight", nil)
	}
	defer sess.RemoveInFlight(key)

	subCtx, cancel := context.WithTimeout(ctx, PerCallWallClockCap)
	defer cancel()

	body, err := postToolsCall(subCtx, ref, daemonSID, sess.ProtocolVersion, daemonReqID, rewrittenParams)
	if err != nil {
		return buildJSONRPCError(clientReqID, -32000, "tools/call failed: "+err.Error(), nil)
	}

	// Rewrite the daemon's response id back to the client's id.
	return rewriteResponseID(body, clientReqID)
}

// ForwardCancellation looks up clientReqID in sess.InFlightRequests,
// forwards notifications/cancelled to the daemon with the daemon's
// request id, and removes the in-flight row.
//
// Best-effort: if the daemon is unreachable, the in-flight row is
// still removed (the per-call wall-clock cap will clean it up
// regardless). Stdio-host daemons do NOT remap inbound requestId
// (per spec known limitation); the cancel may be dropped by the
// daemon, no harm done.
func ForwardCancellation(ctx context.Context, sess *hubSession, clientReqID json.RawMessage) {
	key, err := newRequestIDKey(clientReqID)
	if err != nil {
		// Malformed cancellation — ignore silently per spec
		// (notifications/cancelled with null/array/object/boolean
		// requestId is treated as no-op).
		return
	}
	entry, ok := sess.LookupInFlight(key)
	if !ok {
		return // already removed or never existed
	}
	defer sess.RemoveInFlight(key)

	subCtx, cancel := context.WithTimeout(ctx, PerDaemonInitTimeout)
	defer cancel()
	_ = postCancellation(subCtx, entry.DaemonRef, entry.DaemonSessionID, sess.ProtocolVersion, entry.DaemonRequestID)
}

// daemonStillBound reports whether the (Server, Daemon, Port) tuple
// is still in the calling client's bindings within the current
// snapshot.
func daemonStillBound(snap *ResolverSnapshot, client string, ref canonicalToolRef) bool {
	if snap == nil {
		return false
	}
	bindings, ok := snap.Bindings[client]
	if !ok {
		return false
	}
	for _, b := range bindings {
		if b.Server == ref.Server && b.Daemon == ref.Daemon && b.Port == ref.Port {
			return true
		}
	}
	return false
}

// ---------------- HTTP plumbing ----------------

// doDaemonPost issues a JSON-RPC POST to a daemon at 127.0.0.1:<port>
// /mcp and returns the response body (drained + capped at
// maxAggregatorResponseBytes) plus the response header. The caller
// parses the body and reads any header fields it needs (e.g.,
// Mcp-Session-Id on initialize). daemonSID is set as the
// Mcp-Session-Id request header when non-empty.
//
// HTTP >= 400 turns into an error. Body-read failure or oversize
// response also turns into an error. The body is fed through
// extractJSONPayload by callers that need to parse it — doDaemonPost
// itself is content-type-agnostic.
//
// Used by postInitialize, postToolsList, postToolsCall, and
// postCancellation. The single helper keeps the request-shape +
// retry policy + body-cap logic in one auditable place.
//
// protoVer is the MCP-Protocol-Version header value to send. Per MCP
// Streamable HTTP lifecycle, subsequent requests (after initialize)
// MUST carry this header; strict daemons may reject tools/list,
// tools/call, or notifications/cancelled despite a successful init
// (codex bot r4 P1 closure on PR #157). For the initialize call
// itself, callers pass "" — initialize is the negotiation step and
// the header is not yet known.
func doDaemonPost(ctx context.Context, port int, body []byte, daemonSID, protoVer string, timeout time.Duration) ([]byte, http.Header, error) {
	url := fmt.Sprintf("http://127.0.0.1:%d/mcp", port)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return nil, nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	if daemonSID != "" {
		req.Header.Set("Mcp-Session-Id", daemonSID)
	}
	if protoVer != "" {
		req.Header.Set("MCP-Protocol-Version", protoVer)
	}
	client := &http.Client{Timeout: timeout}
	resp, err := client.Do(req)
	if err != nil {
		return nil, nil, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxAggregatorResponseBytes+1))
	if err != nil {
		return nil, resp.Header, fmt.Errorf("read: %w", err)
	}
	if len(raw) > maxAggregatorResponseBytes {
		return nil, resp.Header, fmt.Errorf("response too large (> %d bytes)", maxAggregatorResponseBytes)
	}
	if resp.StatusCode >= 400 {
		return raw, resp.Header, fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	return raw, resp.Header, nil
}

// postInitialize sends an initialize call to a single daemon and
// returns the Mcp-Session-Id from the response header.
//
// The outbound JSON-RPC id is hard-coded to 1: a fresh HTTP connection
// is opened per call (no multiplexed responses), and the daemon's
// response id is not validated — we extract the Mcp-Session-Id header
// only. Phase 4 keeps the same shape; downstream callers see the
// hub-generated client_session_id, not this internal id.
func postInitialize(ctx context.Context, ref canonicalDaemonRef, protoVer string) (string, error) {
	hubVer, _, _ := buildinfo.Get()
	body := fmt.Sprintf(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"%s","capabilities":{},"clientInfo":{"name":"mcphub-hub","version":"%s"}}}`,
		protoVer, hubVer)
	raw, hdr, err := doDaemonPost(ctx, ref.Port, []byte(body), "", "", PerDaemonInitTimeout)
	if err != nil {
		return "", err
	}
	// codex bot r3 P1 closure on PR #157: inspect the JSON-RPC envelope
	// for an `error` object. Daemons returning HTTP 200 + JSON-RPC
	// error (protocol-version rejection, capability mismatch, etc.)
	// would otherwise be recorded as init successes with an empty
	// session id — misrouting follow-up tools/list / tools/call and
	// surfacing the wrong stage in partialFailures.
	payload := extractJSONPayload(raw)
	var env struct {
		Error *struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if uerr := json.Unmarshal(payload, &env); uerr != nil {
		return "", fmt.Errorf("parse initialize response: %w", uerr)
	}
	if env.Error != nil {
		return "", fmt.Errorf("daemon initialize error code=%d: %s", env.Error.Code, env.Error.Message)
	}
	return hdr.Get("Mcp-Session-Id"), nil
}

// postToolsList sends a tools/list call to a single daemon and
// returns the tool entries from result.tools. Each entry is raw JSON
// so we can re-emit it verbatim in the merged list (preserves any
// extension fields).
//
// The outbound JSON-RPC id is hard-coded to 2: a fresh HTTP connection
// is opened per call and the daemon response id is not validated —
// the hub-generated id used downstream by the aggregator is the
// session-level client_session_id, not this internal id.
func postToolsList(ctx context.Context, ref canonicalDaemonRef, daemonSID, protoVer string) ([]json.RawMessage, error) {
	body := []byte(`{"jsonrpc":"2.0","id":2,"method":"tools/list"}`)
	raw, _, err := doDaemonPost(ctx, ref.Port, body, daemonSID, protoVer, PerDaemonListTimeout)
	if err != nil {
		return nil, err
	}
	payload := extractJSONPayload(raw)
	var env struct {
		Error *struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
		Result struct {
			Tools []json.RawMessage `json:"tools"`
		} `json:"result"`
	}
	if err := json.Unmarshal(payload, &env); err != nil {
		return nil, fmt.Errorf("parse: %w", err)
	}
	if env.Error != nil {
		return nil, fmt.Errorf("daemon error code=%d: %s", env.Error.Code, env.Error.Message)
	}
	return env.Result.Tools, nil
}

// postToolsCall forwards a tools/call to a single daemon with the
// hub-generated daemon request id. Returns the full daemon response
// body (the caller rewrites the response id back to the client's
// id; we keep the raw form here so partial-failure surfaces have
// access to any structured error payload).
func postToolsCall(ctx context.Context, ref canonicalToolRef, daemonSID, protoVer string, daemonReqID, params json.RawMessage) ([]byte, error) {
	body, err := buildToolsCallEnvelope(daemonReqID, params)
	if err != nil {
		return nil, err
	}
	raw, _, err := doDaemonPost(ctx, ref.Port, body, daemonSID, protoVer, PerCallWallClockCap)
	if err != nil {
		return nil, err
	}
	return extractJSONPayload(raw), nil
}

// postCancellation sends a notifications/cancelled to a daemon.
// Best-effort: errors are swallowed by the caller.
func postCancellation(ctx context.Context, ref canonicalDaemonRef, daemonSID, protoVer string, daemonReqID json.RawMessage) error {
	envelope := struct {
		JSONRPC string `json:"jsonrpc"`
		Method  string `json:"method"`
		Params  struct {
			RequestID json.RawMessage `json:"requestId"`
		} `json:"params"`
	}{
		JSONRPC: "2.0",
		Method:  "notifications/cancelled",
	}
	envelope.Params.RequestID = daemonReqID
	body, err := json.Marshal(envelope)
	if err != nil {
		return err
	}
	if _, _, err := doDaemonPost(ctx, ref.Port, body, daemonSID, protoVer, PerDaemonInitTimeout); err != nil {
		return err
	}
	return nil
}

// postInitialized sends notifications/initialized to a daemon after
// a successful initialize. Per MCP lifecycle the client MUST send
// this notification before issuing any other method calls. Best-
// effort: callers ignore the returned error (codex bot r4 P1 closure
// on PR #157).
func postInitialized(ctx context.Context, ref canonicalDaemonRef, daemonSID, protoVer string) error {
	body := []byte(`{"jsonrpc":"2.0","method":"notifications/initialized"}`)
	if _, _, err := doDaemonPost(ctx, ref.Port, body, daemonSID, protoVer, PerDaemonInitTimeout); err != nil {
		return err
	}
	return nil
}

// extractJSONPayload returns the JSON body from an MCP daemon
// response. MCP daemons may emit either plain application/json OR
// a text/event-stream with one `data: <json>` line. This helper
// handles both shapes (mirrors health.go:727-734).
func extractJSONPayload(raw []byte) []byte {
	for _, line := range bytes.Split(raw, []byte("\n")) {
		if bytes.HasPrefix(line, []byte("data: ")) {
			return bytes.TrimPrefix(line, []byte("data: "))
		}
	}
	return raw
}

// ---------------- Response builders ----------------

// buildSyntheticInitResult assembles the hub-side initialize response.
// reqID echoes back the CLIENT's JSON-RPC id (codex bot r1 P1 closure
// on Phase 3 PR #157 — hardcoded id:1 broke request/response
// correlation for clients sending string ids or non-1 numbers). The
// id is passed through as raw JSON (json.RawMessage) so the
// discriminator between number and string ids is preserved verbatim.
func buildSyntheticInitResult(reqID json.RawMessage, protoVer string) ([]byte, error) {
	hubVer, _, _ := buildinfo.Get()
	// Default to JSON null if reqID is empty (notification-shaped
	// initialize would be invalid per MCP, but we never want to emit
	// invalid JSON).
	idField := reqID
	if len(idField) == 0 {
		idField = json.RawMessage(`null`)
	}
	body := map[string]any{
		"jsonrpc": "2.0",
		"id":      idField,
		"result": map[string]any{
			"protocolVersion": protoVer,
			"capabilities": map[string]any{
				"tools": map[string]any{},
			},
			"serverInfo": map[string]any{
				"name":    "mcphub-hub",
				"version": hubVer,
			},
		},
	}
	return json.Marshal(body)
}

// buildToolsListResponse assembles a successful (≥1 daemon ok)
// tools/list response with the merged tool list and partialFailures.
func buildToolsListResponse(reqID json.RawMessage, tools []json.RawMessage, failures []DaemonFailure) ([]byte, error) {
	meta := map[string]any{
		"mcphub": map[string]any{
			"partialFailures": failuresOrEmpty(failures),
			"instance_id":     currentInstanceIDOrEmpty(),
		},
	}
	result := map[string]any{
		"tools": tools,
		"_meta": meta,
	}
	envelope := map[string]any{
		"jsonrpc": "2.0",
		"id":      reqID,
		"result":  result,
	}
	return json.Marshal(envelope)
}

// buildAllFailedToolsListResponse assembles a JSON-RPC -32000 error
// envelope when every participating daemon failed.
func buildAllFailedToolsListResponse(reqID json.RawMessage, failures []DaemonFailure) ([]byte, error) {
	data := map[string]any{
		"mcphub": map[string]any{
			"partialFailures": failuresOrEmpty(failures),
			"instance_id":     currentInstanceIDOrEmpty(),
		},
	}
	envelope := map[string]any{
		"jsonrpc": "2.0",
		"id":      reqID,
		"error": map[string]any{
			"code":    -32000,
			"message": "all participating daemons failed",
			"data":    data,
		},
	}
	return json.Marshal(envelope)
}

// buildJSONRPCError emits a generic error envelope. dataField may be
// nil; when present it populates error.data.
func buildJSONRPCError(reqID json.RawMessage, code int, message string, dataField any) ([]byte, error) {
	errObj := map[string]any{
		"code":    code,
		"message": message,
	}
	if dataField != nil {
		errObj["data"] = dataField
	}
	envelope := map[string]any{
		"jsonrpc": "2.0",
		"id":      reqID,
		"error":   errObj,
	}
	return json.Marshal(envelope)
}

// buildToolsCallEnvelope assembles a tools/call request body with the
// hub-generated daemon request id.
func buildToolsCallEnvelope(daemonReqID, params json.RawMessage) ([]byte, error) {
	envelope := map[string]any{
		"jsonrpc": "2.0",
		"id":      daemonReqID,
		"method":  "tools/call",
		"params":  params,
	}
	return json.Marshal(envelope)
}

// buildRewrittenParams returns paramsRaw with `name` replaced by
// rawName. Other fields (arguments, _meta, etc.) survive VERBATIM as
// raw JSON — preserves number precision for large integers (>2^53)
// inside arguments. Decoding into `map[string]any` would force every
// number through float64 and silently round (codex bot r3 P1 closure
// on PR #157 — tools could execute against the wrong resource id).
//
// Pattern: decode into `map[string]json.RawMessage`; rewrite ONLY the
// `name` field as a fresh JSON string; re-marshal. The other fields'
// RawMessage values pass through unchanged.
func buildRewrittenParams(rawName string, paramsRaw json.RawMessage) (json.RawMessage, error) {
	var m map[string]json.RawMessage
	if err := json.Unmarshal(paramsRaw, &m); err != nil {
		return nil, err
	}
	nameRaw, err := json.Marshal(rawName)
	if err != nil {
		return nil, err
	}
	m["name"] = nameRaw
	out, err := json.Marshal(m)
	if err != nil {
		return nil, err
	}
	return out, nil
}

// rewriteResponseID swaps the `id` field in a daemon response with
// clientReqID. Used after a tools/call to hand the client back its
// original id (daemon-side id is hub-generated and the client must
// not see it).
func rewriteResponseID(body []byte, clientReqID json.RawMessage) ([]byte, error) {
	var m map[string]json.RawMessage
	if err := json.Unmarshal(body, &m); err != nil {
		return nil, err
	}
	m["id"] = clientReqID
	out, err := json.Marshal(m)
	if err != nil {
		return nil, err
	}
	return out, nil
}

// nameSpaceTools rewrites each tool's `name` field to
// "<server>__<rawname>" and builds the per-daemon contribution to the
// session's route map.
//
// codex bot r4 P2 closure on PR #157: rewrite ONLY the name field;
// every other field passes through as raw JSON bytes. Earlier
// `map[string]any` round-trip forced numeric fields in tool
// schemas (default values, enum members, min/max constraints in
// inputSchema) through float64, silently rounding integers > 2^53
// in tool metadata. Clients would receive corrupted definitions
// even though only `name` should change.
func nameSpaceTools(ref canonicalDaemonRef, tools []json.RawMessage) ([]json.RawMessage, map[string]canonicalToolRef) {
	out := make([]json.RawMessage, 0, len(tools))
	rm := make(map[string]canonicalToolRef, len(tools))
	for _, t := range tools {
		var m map[string]json.RawMessage
		if err := json.Unmarshal(t, &m); err != nil {
			continue
		}
		var rawName string
		if nameField, ok := m["name"]; ok {
			if uerr := json.Unmarshal(nameField, &rawName); uerr != nil {
				continue
			}
		}
		if rawName == "" {
			continue
		}
		exposed := ref.Server + "__" + rawName
		exposedJSON, err := json.Marshal(exposed)
		if err != nil {
			continue
		}
		m["name"] = exposedJSON
		nb, err := json.Marshal(m)
		if err != nil {
			continue
		}
		out = append(out, nb)
		rm[exposed] = canonicalToolRef{
			Server:  ref.Server,
			Daemon:  ref.Daemon,
			Port:    ref.Port,
			RawName: rawName,
		}
	}
	return out, rm
}

// failuresOrEmpty returns failures if non-nil, otherwise an empty
// slice (so JSON marshal emits `[]` rather than `null`).
func failuresOrEmpty(f []DaemonFailure) []DaemonFailure {
	if f == nil {
		return []DaemonFailure{}
	}
	return f
}

// currentInstanceIDOrEmpty returns the persistent hub instance_id
// from the loaded endpoint state, or "" if not loaded. Phase 4 fills
// this from the established endpoint state at handler startup; in
// tests it returns "" because the endpoint state isn't populated.
func currentInstanceIDOrEmpty() string {
	ep, err := LoadHubEndpoint()
	if err != nil {
		return ""
	}
	return ep.InstanceID
}

// generateDaemonRequestID returns a hex-encoded request id wrapped
// in JSON-string form (e.g. `"hub-abc123"`).
func generateDaemonRequestID() (json.RawMessage, error) {
	tok, err := generateHexToken()
	if err != nil {
		return nil, err
	}
	// Truncate to keep daemon-side logs readable; 16 hex chars = 64
	// bits of entropy, plenty for in-process correlation.
	if len(tok) > 16 {
		tok = tok[:16]
	}
	return json.RawMessage(`"hub-` + tok + `"`), nil
}

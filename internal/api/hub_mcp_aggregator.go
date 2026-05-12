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
	"strings"
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
func AggregateInitialize(ctx context.Context, sess *hubSession) ([]byte, error) {
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
	body, err := buildSyntheticInitResult(protoVer)
	if err != nil {
		return nil, err
	}
	return body, nil
}

// AggregateToolsList fans out tools/list to every daemon in
// sess.InitSuccesses. Merges into a flat exposed-name route map keyed
// "<server>__<rawname>". The session's RouteMap (atomic.Pointer) is
// swapped to the freshly-built map.
//
// _meta.mcphub.partialFailures combines stored InitFailures with
// list-time failures (stage="tools/list"). If len(InitSuccesses)==0
// AND no list-time successes, the response is a JSON-RPC -32000
// error envelope with data.mcphub.partialFailures.
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

	type listResult struct {
		ref     canonicalDaemonRef
		tools   []json.RawMessage
		toolMap map[string]canonicalToolRef
		err     error
	}
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
			tools, err := postToolsList(subCtx, ref, sid)
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

	// Build merged tools list, route map, and list-time failure rows.
	mergedTools := make([]json.RawMessage, 0)
	mergedRoutes := make(map[string]canonicalToolRef)
	listFailures := make([]DaemonFailure, 0)
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
		mergedTools = append(mergedTools, r.tools...)
		for k, v := range r.toolMap {
			mergedRoutes[k] = v
		}
	}

	// Publish the route map to the session BEFORE returning so a
	// concurrent tools/call sees it.
	sess.RouteMap.Store(&mergedRoutes)

	// All-failed path: no successes AND every fan-out failed.
	hadAnySuccess := len(mergedRoutes) > 0 || (len(initFailures) == 0 && len(listFailures) == 0 && len(successes) == 0)
	// Refine: "had any success" = at least one list result without err
	// OR no list calls were made AND init had successes (which means
	// initialize succeeded but tools/list found nothing — that's a
	// legitimate empty tool list, not a failure).
	listSuccessCount := 0
	for _, r := range results {
		if r.err == nil {
			listSuccessCount++
		}
	}
	hadAnySuccess = listSuccessCount > 0

	allFailures := append([]DaemonFailure{}, initFailures...)
	allFailures = append(allFailures, listFailures...)

	if !hadAnySuccess {
		return buildAllFailedToolsListResponse(reqID, allFailures)
	}
	return buildToolsListResponse(reqID, mergedTools, allFailures)
}

// AggregateToolsCall looks up params.name in sess.RouteMap, revalidates
// (Client, Server, Daemon) against the CURRENT resolver snapshot, and
// forwards the call with params.name rewritten to RawName.
//
// On stale resolver → -32601 "tool moved out of scope".
// On unknown name → -32601 "Method not found: <name>".
// On daemon error → response body passed through verbatim.
func AggregateToolsCall(ctx context.Context, sess *hubSession, clientReqID json.RawMessage, paramsRaw json.RawMessage) ([]byte, error) {
	// Parse params to extract name + arguments.
	var p struct {
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments,omitempty"`
	}
	if err := json.Unmarshal(paramsRaw, &p); err != nil {
		return buildJSONRPCError(clientReqID, -32602, "Invalid params: "+err.Error(), nil)
	}
	if p.Name == "" {
		return buildJSONRPCError(clientReqID, -32602, "Invalid params: missing name", nil)
	}

	// Route lookup on the session's RouteMap.
	rmPtr := sess.RouteMap.Load()
	if rmPtr == nil {
		return buildJSONRPCError(clientReqID, -32601, "Method not found: "+p.Name, nil)
	}
	ref, ok := (*rmPtr)[p.Name]
	if !ok {
		return buildJSONRPCError(clientReqID, -32601, "Method not found: "+p.Name, nil)
	}

	// Resolver-snapshot revalidation: refuse if (Server, Daemon) is
	// not in the calling client's current bindings AND the snapshot
	// pointer has moved.
	current := LoadResolverSnapshot()
	if current != nil && current != sess.SnapshotAtInit {
		if !daemonStillBound(current, sess.Client, ref) {
			return buildJSONRPCError(clientReqID, -32601, "tool moved out of scope; reinitialize session", nil)
		}
	}

	// Look up the daemon's Mcp-Session-Id.
	sess.mu.Lock()
	daemonSID, hasSID := sess.InitSuccesses[canonicalDaemonRef{Server: ref.Server, Daemon: ref.Daemon, Port: ref.Port}]
	sess.mu.Unlock()
	if !hasSID {
		return buildJSONRPCError(clientReqID, -32603, "Internal error: no daemon session id for target", nil)
	}

	// Build the rewritten body. params.name → RawName.
	rewrittenParams, err := buildRewrittenParams(p.Name, ref.RawName, p.Arguments, paramsRaw)
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
	sess.InsertInFlight(key, inflightEntry{
		DaemonRef:       canonicalDaemonRef{Server: ref.Server, Daemon: ref.Daemon, Port: ref.Port},
		DaemonSessionID: daemonSID,
		DaemonRequestID: daemonReqID,
		StartedAt:       time.Now(),
	})
	defer sess.RemoveInFlight(key)

	subCtx, cancel := context.WithTimeout(ctx, PerCallWallClockCap)
	defer cancel()

	body, err := postToolsCall(subCtx, ref, daemonSID, daemonReqID, rewrittenParams)
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
	_ = postCancellation(subCtx, entry.DaemonRef, entry.DaemonSessionID, entry.DaemonRequestID)
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

// postInitialize sends an initialize call to a single daemon and
// returns the Mcp-Session-Id from the response header.
func postInitialize(ctx context.Context, ref canonicalDaemonRef, protoVer string) (string, error) {
	url := fmt.Sprintf("http://127.0.0.1:%d/mcp", ref.Port)
	hubVer, _, _ := buildinfo.Get()
	body := fmt.Sprintf(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"%s","capabilities":{},"clientInfo":{"name":"mcphub-hub","version":"%s"}}}`,
		protoVer, hubVer)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, strings.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	client := &http.Client{Timeout: PerDaemonInitTimeout}
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	// Drain to avoid leaking the connection.
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxAggregatorResponseBytes+1))
	if resp.StatusCode >= 400 {
		return "", fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	return resp.Header.Get("Mcp-Session-Id"), nil
}

// postToolsList sends a tools/list call to a single daemon and
// returns the tool entries from result.tools. Each entry is raw JSON
// so we can re-emit it verbatim in the merged list (preserves any
// extension fields).
func postToolsList(ctx context.Context, ref canonicalDaemonRef, daemonSID string) ([]json.RawMessage, error) {
	url := fmt.Sprintf("http://127.0.0.1:%d/mcp", ref.Port)
	body := `{"jsonrpc":"2.0","id":2,"method":"tools/list"}`
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, strings.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	if daemonSID != "" {
		req.Header.Set("Mcp-Session-Id", daemonSID)
	}
	client := &http.Client{Timeout: PerDaemonListTimeout}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxAggregatorResponseBytes+1))
	if err != nil {
		return nil, fmt.Errorf("read: %w", err)
	}
	if len(raw) > maxAggregatorResponseBytes {
		return nil, fmt.Errorf("response too large (> %d bytes)", maxAggregatorResponseBytes)
	}
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("HTTP %d", resp.StatusCode)
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
func postToolsCall(ctx context.Context, ref canonicalToolRef, daemonSID string, daemonReqID, params json.RawMessage) ([]byte, error) {
	url := fmt.Sprintf("http://127.0.0.1:%d/mcp", ref.Port)
	body, err := buildToolsCallEnvelope(daemonReqID, params)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	if daemonSID != "" {
		req.Header.Set("Mcp-Session-Id", daemonSID)
	}
	client := &http.Client{Timeout: PerCallWallClockCap}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxAggregatorResponseBytes+1))
	if err != nil {
		return nil, fmt.Errorf("read: %w", err)
	}
	if len(raw) > maxAggregatorResponseBytes {
		return nil, fmt.Errorf("response too large (> %d bytes)", maxAggregatorResponseBytes)
	}
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	return extractJSONPayload(raw), nil
}

// postCancellation sends a notifications/cancelled to a daemon.
// Best-effort: errors are swallowed by the caller.
func postCancellation(ctx context.Context, ref canonicalDaemonRef, daemonSID string, daemonReqID json.RawMessage) error {
	url := fmt.Sprintf("http://127.0.0.1:%d/mcp", ref.Port)
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
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	if daemonSID != "" {
		req.Header.Set("Mcp-Session-Id", daemonSID)
	}
	client := &http.Client{Timeout: PerDaemonInitTimeout}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxAggregatorResponseBytes+1))
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
func buildSyntheticInitResult(protoVer string) ([]byte, error) {
	hubVer, _, _ := buildinfo.Get()
	body := map[string]any{
		"jsonrpc": "2.0",
		"id":      1,
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

// buildRewrittenParams returns paramsRaw with `name` replaced by raw.
// Other fields (arguments, _meta, etc.) survive verbatim. Falls back
// to a full re-marshal when the original params shape can't be
// rewritten with simple substitution.
func buildRewrittenParams(_, raw string, _ json.RawMessage, paramsRaw json.RawMessage) (json.RawMessage, error) {
	// Decode into a generic map, replace name, re-marshal. Preserves
	// every extension field a daemon might care about.
	var m map[string]any
	if err := json.Unmarshal(paramsRaw, &m); err != nil {
		return nil, err
	}
	m["name"] = raw
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
func nameSpaceTools(ref canonicalDaemonRef, tools []json.RawMessage) ([]json.RawMessage, map[string]canonicalToolRef) {
	out := make([]json.RawMessage, 0, len(tools))
	rm := make(map[string]canonicalToolRef, len(tools))
	for _, t := range tools {
		var m map[string]any
		if err := json.Unmarshal(t, &m); err != nil {
			continue
		}
		rawName, _ := m["name"].(string)
		if rawName == "" {
			continue
		}
		exposed := ref.Server + "__" + rawName
		m["name"] = exposed
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

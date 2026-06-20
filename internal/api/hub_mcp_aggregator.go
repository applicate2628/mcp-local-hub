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
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"

	"mcp-local-hub/internal/buildinfo"
)

// daemonHTTPError is the error doDaemonPost returns when the daemon RECEIVED the
// request but answered HTTP>=400 (as opposed to a transport failure where the
// request never landed). The distinction is load-bearing for the hot-swap (a)
// self-heal retry: an HTTP-level rejection means the daemon got the request and
// a non-idempotent tool's side effect may already have run, so it must NOT be
// retried (MCP tools/call has no idempotency key). Error() preserves the prior
// "HTTP %d" string so existing error messages are unchanged.
type daemonHTTPError struct {
	code int
	body []byte
}

func (e *daemonHTTPError) Error() string { return fmt.Sprintf("HTTP %d", e.code) }

// isRetriableTransportFailure reports whether err is a TRANSPORT-level failure
// where the request demonstrably never reached the daemon — connection refused
// or connection reset — and is therefore SAFE to retry against a freshly-
// restarted daemon. It deliberately returns false for:
//   - *daemonHTTPError (the daemon received the request; a side effect may have run),
//   - timeouts / context deadline (ambiguous — the request may have landed),
//   - any other error (conservative: never retry an unclassified failure).
func isRetriableTransportFailure(err error) bool {
	if err == nil {
		return false
	}
	var httpErr *daemonHTTPError
	if errors.As(err, &httpErr) {
		return false
	}
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
		return false
	}
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		return false
	}
	// A DIAL-phase failure means the TCP connection was never established, so the
	// request bytes were never sent — unambiguously safe to retry. This is the
	// daemon-restarted / connection-refused case, detected PORTABLY via the dial
	// OpError (errors.Is(err, syscall.ECONNREFUSED) is unreliable across Windows
	// winsock WSAECONNREFUSED vs POSIX errno). A mid-stream reset is deliberately
	// NOT retried: the request may have partially landed (double-exec risk).
	var opErr *net.OpError
	if errors.As(err, &opErr) && opErr.Op == "dial" {
		return true
	}
	return errors.Is(err, syscall.ECONNREFUSED)
}

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
	// codex bot r6 P2 closure on PR #157: persist the fallback onto
	// sess.ProtocolVersion when the session was allocated without a
	// negotiated version. Earlier code took a local copy, so fanOutToolsList
	// + postToolsCall (which re-read sess.ProtocolVersion at
	// hub_mcp_aggregator.go:210 + :429) would later send an EMPTY
	// MCP-Protocol-Version header — initialize used the fallback, but
	// tools/list and tools/call did not. The header mismatch is exactly
	// what the r4 protocol-version closure tried to prevent. Holding
	// sess.mu here matches the lock guarding session-state mutations.
	sess.mu.Lock()
	if sess.ProtocolVersion == "" {
		sess.ProtocolVersion = hubProtocolVersionFallback
	}
	protoVer := sess.ProtocolVersion
	sess.mu.Unlock()

	type initResult struct {
		ref             canonicalDaemonRef
		sessionID       string
		negotiatedProto string // codex bot r17 P1 closure on PR #157
		err             error
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
			sid, negotiated, err := initializeDaemonSession(ctx, ref, protoVer)
			results[i] = initResult{ref: ref, sessionID: sid, negotiatedProto: negotiated, err: err}
		}()
	}
	wg.Wait()

	// Apply results to the session.
	sess.mu.Lock()
	defer sess.mu.Unlock()
	// Defensive lazy-init: tests + future direct constructors may
	// allocate a hubSession without the DaemonProtoVer map.
	if sess.DaemonProtoVer == nil {
		sess.DaemonProtoVer = make(map[canonicalDaemonRef]string)
	}
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
		sess.DaemonProtoVer[r.ref] = r.negotiatedProto
	}

	// Build a synthetic initialize result envelope.
	body, err := buildSyntheticInitResult(reqID, protoVer)
	if err != nil {
		return nil, err
	}
	return body, nil
}

func initializeDaemonSession(ctx context.Context, ref canonicalDaemonRef, protoVer string) (sid, negotiated string, err error) {
	initCtx, initCancel := context.WithTimeout(ctx, PerDaemonInitTimeout)
	sid, negotiated, err = postInitialize(initCtx, ref, protoVer)
	initCancel()
	if err != nil {
		return "", "", err
	}

	// MCP lifecycle: notifications/initialized is mandatory after a
	// successful initialize. If it fails, the daemon allocated a session
	// that cannot be used; delete it with the negotiated protocol header and
	// report initialize-stage failure instead of caching a half-initialized SID.
	notifyCtx, notifyCancel := context.WithTimeout(ctx, PerDaemonInitTimeout)
	notifyErr := postInitialized(notifyCtx, ref, sid, negotiated)
	notifyCancel()
	if notifyErr == nil {
		return sid, negotiated, nil
	}

	cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), PerDaemonInitTimeout)
	_ = bestEffortDeleteDaemonSession(cleanupCtx, ref, sid, negotiated)
	cleanupCancel()
	return "", "", fmt.Errorf("notifications/initialized: %w", notifyErr)
}

// namespacedTool is one daemon's contribution to the merged tools/list
// response. Body is the original JSON blob with `name` rewritten to
// the exposed `<server>__<rawname>` form; Exposed and Ref let the
// merge step (assembleToolsListResponse) collision-detect by exact
// key match without re-parsing Body. codex bot r9 P2 closure on PR
// #157.
type namespacedTool struct {
	Exposed string
	Body    json.RawMessage
	Ref     canonicalToolRef
}

// listResult is the per-daemon outcome of a tools/list fan-out call.
// On err == nil, tools carries the daemon's namespaced contribution.
// On err != nil, the row turns into a stage="tools/list" partialFailure.
type listResult struct {
	ref   canonicalDaemonRef
	tools []namespacedTool
	err   error
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
func AggregateToolsList(ctx context.Context, sess *hubSession, reqID json.RawMessage, instanceID string) ([]byte, error) {
	// Snapshot the inputs under the session mu. codex bot r17 P1
	// closure on PR #157: capture each daemon's NEGOTIATED protocol
	// version alongside its Mcp-Session-Id so the fan-out uses
	// per-daemon headers instead of the session-level one.
	sess.mu.Lock()
	successes := make(map[canonicalDaemonRef]daemonInitState, len(sess.InitSuccesses))
	for ref, sid := range sess.InitSuccesses {
		proto := sess.DaemonProtoVer[ref]
		if proto == "" {
			proto = sess.ProtocolVersion
		}
		successes[ref] = daemonInitState{SessionID: sid, ProtocolVersion: proto}
	}
	initFailures := make([]DaemonFailure, len(sess.InitFailures))
	copy(initFailures, sess.InitFailures)
	sess.mu.Unlock()

	allowEmptySuccess := false
	if current := LoadResolverSnapshot(); sess.SnapshotAtInit != nil && current != nil && current != sess.SnapshotAtInit {
		originalCount := len(successes)
		if originalCount > 0 {
			filtered := make(map[canonicalDaemonRef]daemonInitState, originalCount)
			remapped := make(map[canonicalDaemonRef]daemonInitState)
			for ref, state := range successes {
				if liveRef, ok := liveDaemonBinding(current, sess.ScopeKey, ref); ok {
					filtered[liveRef] = state
					if liveRef != ref {
						remapped[liveRef] = state
					}
				}
			}
			successes = filtered
			if len(remapped) > 0 {
				sess.mu.Lock()
				for ref, state := range remapped {
					sess.InitSuccesses[ref] = state.SessionID
					sess.DaemonProtoVer[ref] = state.ProtocolVersion
				}
				sess.mu.Unlock()
			}
		}
		initFailures = filterInitFailuresByLiveBindings(initFailures, current, sess.ScopeKey, sess.IntendedParticipants)
		allowEmptySuccess = originalCount > 0 && len(successes) == 0 && len(initFailures) == 0
	}

	results := fanOutToolsList(ctx, successes)
	return assembleToolsListResponse(reqID, results, initFailures, sess, instanceID, allowEmptySuccess)
}

func filterInitFailuresByLiveBindings(failures []DaemonFailure, current *ResolverSnapshot, scopeKey string, intended []canonicalDaemonRef) []DaemonFailure {
	if len(failures) == 0 {
		return failures
	}
	filtered := make([]DaemonFailure, 0, len(failures))
	for _, f := range failures {
		if initFailureDaemonStillBound(current, scopeKey, intended, f) {
			filtered = append(filtered, f)
		}
	}
	return filtered
}

func initFailureDaemonStillBound(current *ResolverSnapshot, scopeKey string, intended []canonicalDaemonRef, failure DaemonFailure) bool {
	for _, ref := range intended {
		if ref.Server != failure.Server || ref.Daemon != failure.Daemon {
			continue
		}
		if daemonStillBound(current, scopeKey, canonicalToolRef{Server: ref.Server, Daemon: ref.Daemon, Port: ref.Port}) {
			return true
		}
	}
	return false
}

// daemonInitState bundles a daemon's per-daemon Mcp-Session-Id and
// negotiated MCP protocol version. AggregateToolsList snapshots these
// under sess.mu and passes the snapshot to fanOutToolsList so each
// fan-out call carries the daemon's own negotiated header value
// (codex bot r17 P1 closure on PR #157).
type daemonInitState struct {
	SessionID       string
	ProtocolVersion string
}

// fanOutToolsList issues parallel tools/list calls to every daemon
// whose initialize succeeded. Returns one listResult per daemon. The
// caller (assembleToolsListResponse) merges + namespaces + publishes
// the route map.
//
// Each successes entry carries the daemon's negotiated MCP protocol
// version (codex bot r17 P1 closure on PR #157); postToolsList uses
// it as the MCP-Protocol-Version header.
func fanOutToolsList(ctx context.Context, successes map[canonicalDaemonRef]daemonInitState) []listResult {
	results := make([]listResult, 0, len(successes))
	var resultsMu sync.Mutex

	sem := make(chan struct{}, FanOutConcurrency)
	var wg sync.WaitGroup
	for ref, state := range successes {
		ref, state := ref, state
		wg.Add(1)
		sem <- struct{}{}
		go func() {
			defer wg.Done()
			defer func() { <-sem }()
			subCtx, cancel := context.WithTimeout(ctx, PerDaemonListTimeout)
			defer cancel()
			tools, err := postToolsList(subCtx, ref, state.SessionID, state.ProtocolVersion)
			r := listResult{ref: ref, err: err}
			if err == nil {
				r.tools = nameSpaceTools(ref, tools)
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
//
// Namespace-collision handling: when two daemons under the same
// server expose the same raw tool name, the resulting exposed name
// "<server>__<rawname>" claims the same canonical route. Silent
// last-writer-wins routing produces non-deterministic tools/call
// targets across runs (codex bot r9 P2 closure on PR #157). Detect
// such collisions in a first pass, drop the colliding tool from
// BOTH the merged response AND the route map, and emit one
// stage="tools/list" partialFailure row per colliding daemon. The
// failure message names every daemon that claimed the key so
// operators can resolve the duplicate at the manifest layer.
func assembleToolsListResponse(reqID json.RawMessage, results []listResult, initFailures []DaemonFailure, sess *hubSession, instanceID string, allowEmptySuccess bool) ([]byte, error) {
	// codex bot r14 P2 closure on PR #157: fanOutToolsList appends
	// results in goroutine-completion order, so identical inputs
	// produced different mergedTools / partialFailures order across
	// requests. Sort results by ref tuple here so all downstream
	// loops (collision-row emission, HTTP-failure-row emission,
	// flat-tools collection) become deterministic. Tool-level sort
	// happens at the end of pass 2 once flatTools is assembled.
	sorted := append([]listResult{}, results...)
	sort.Slice(sorted, func(i, j int) bool {
		a, b := sorted[i].ref, sorted[j].ref
		if a.Server != b.Server {
			return a.Server < b.Server
		}
		if a.Daemon != b.Daemon {
			return a.Daemon < b.Daemon
		}
		return a.Port < b.Port
	})
	results = sorted

	// Resolve the session's FINE-GRAINED per-tool visibility filter
	// (groups/namespaces Phase 5a) BEFORE collision detection. For a GROUP
	// scope key this is the live group's tools_hidden map (server → hidden raw
	// names) carried on the current resolver snapshot; for a CLIENT scope key it
	// is nil → NO filtering (the byte-identical fence). Reading it from the live
	// snapshot mirrors tools/call revalidation: a tools_hidden republish revokes
	// the listing surface on the next tools/list without a reconnect. Precomputed
	// into a (server → set-of-raw-names) lookup ONCE so each per-tool check is
	// O(1); nil/empty filter → nil set → hides() short-circuits to false,
	// preserving the client fence.
	//
	// CRITICAL (codex bot r2 — leak via diagnostics): this MUST be computed
	// before Pass 1 so a HIDDEN tool is excluded from collision detection
	// too. Otherwise a hidden tool that ALSO collides between two same-server
	// daemons is dropped from result.tools (Pass 2) yet still named in a
	// `partialFailures` collision row — leaking the hidden tool's existence.
	hiddenSet := buildHiddenToolSet(sess.hiddenToolsForScope())

	// Pass 1 — for each successful daemon, record the SET of unique
	// daemons that produced each exposed key. Keys claimed by more
	// than one DISTINCT daemon become collisions. codex bot r13 P2
	// closure on PR #157: a single daemon returning a duplicate tool
	// name (`tools=[{name:"read"},{name:"read"}]`) must NOT be treated
	// as a cross-daemon collision — routing is still unambiguous, and
	// dedupe happens in pass 2.
	keyDaemons := make(map[string]map[canonicalDaemonRef]bool)
	for _, r := range results {
		if r.err != nil {
			continue
		}
		for _, t := range r.tools {
			if hiddenSet.hides(t.Ref.Server, t.Ref.RawName) {
				continue // hidden tools never enter collision detection → never leak via partialFailures
			}
			set, ok := keyDaemons[t.Exposed]
			if !ok {
				set = make(map[canonicalDaemonRef]bool)
				keyDaemons[t.Exposed] = set
			}
			set[r.ref] = true
		}
	}
	collisions := make(map[string]bool)
	collisionFailures := make([]DaemonFailure, 0)
	collisionKeys := make([]string, 0)
	for k, set := range keyDaemons {
		if len(set) > 1 {
			collisions[k] = true
			collisionKeys = append(collisionKeys, k)
		}
	}
	// Sort collision keys + per-key daemon lists for deterministic
	// emission order — assembleToolsListResponse is the ONLY surface
	// that puts these in the response, so output stability matters
	// for test golden files and for operator-facing diagnostics.
	sort.Strings(collisionKeys)
	for _, k := range collisionKeys {
		// Drain the set into a slice and sort for deterministic
		// emission AND to dedupe daemon-ref rows (one row per unique
		// daemon, never two rows for the same daemon even if it
		// happened to claim the key via multiple tool occurrences).
		refs := make([]canonicalDaemonRef, 0, len(keyDaemons[k]))
		for ref := range keyDaemons[k] {
			refs = append(refs, ref)
		}
		sort.Slice(refs, func(i, j int) bool {
			if refs[i].Server != refs[j].Server {
				return refs[i].Server < refs[j].Server
			}
			return refs[i].Daemon < refs[j].Daemon
		})
		daemonNames := make([]string, 0, len(refs))
		for _, ref := range refs {
			daemonNames = append(daemonNames, ref.Server+"/"+ref.Daemon)
		}
		joined := strings.Join(daemonNames, ", ")
		for _, ref := range refs {
			collisionFailures = append(collisionFailures, DaemonFailure{
				Server: ref.Server,
				Daemon: ref.Daemon,
				Stage:  "tools/list",
				Err:    fmt.Sprintf("namespace collision on exposed tool name %q claimed by daemons: %s", k, joined),
			})
		}
	}

	// Pass 2 — collect surviving tools into a flat slice, then sort
	// by exposed name for deterministic emission (codex bot r14 P2
	// closure on PR #157). A daemon's tools/list call counts as a
	// success even when every one of its tools is collided; the call
	// itself returned cleanly, and dropping the daemon from
	// listSuccessCount would mis-trigger the all-failed -32000
	// envelope path.
	//
	// `seenExposed` deduplicates within and across daemons even for
	// NON-collision keys: a daemon returning the same tool name twice
	// (intra-daemon duplicate — non-conformant but observed in the
	// wild) yields one mergedTools entry, not two. codex bot r13 P2
	// closure on PR #157.
	flatTools := make([]namespacedTool, 0)
	seenExposed := make(map[string]bool)
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
		for _, t := range r.tools {
			if collisions[t.Exposed] {
				continue
			}
			if seenExposed[t.Exposed] {
				continue
			}
			// Per-tool hide: drop a tool the group hides for its server.
			// Matched on the daemon-side (Server, RawName) — the same
			// (server → raw tool name) shape groups.yaml authors. A
			// dropped tool enters NEITHER the merged response NOR
			// mergedRoutes below, so a later tools/call for it returns
			// the existing -32601 (the dispatch path is untouched).
			if hiddenSet.hides(t.Ref.Server, t.Ref.RawName) {
				continue
			}
			seenExposed[t.Exposed] = true
			flatTools = append(flatTools, t)
		}
	}
	// Sort tools by exposed name so identical daemon/tool sets
	// produce byte-identical result.tools across requests regardless
	// of which goroutine completed first.
	sort.Slice(flatTools, func(i, j int) bool {
		return flatTools[i].Exposed < flatTools[j].Exposed
	})
	mergedTools := make([]json.RawMessage, 0, len(flatTools))
	mergedRoutes := make(map[string]canonicalToolRef, len(flatTools))
	for _, t := range flatTools {
		mergedTools = append(mergedTools, t.Body)
		mergedRoutes[t.Exposed] = t.Ref
	}
	listFailures = append(listFailures, collisionFailures...)

	// Publish the route map to the session BEFORE returning so a
	// concurrent tools/call sees it. codex bot r6 P1 closure on PR
	// #157: skip the Store when every fan-out failed — otherwise a
	// single transient outage (DNS hiccup, daemon restart, host paged
	// out) wipes the entire previously-known routes table and any
	// follow-up tools/call returns -32601 "tool moved out of scope"
	// even though nothing structural changed. Preserve the last-good
	// map and surface the list failures via the error envelope only.
	allFailures := append([]DaemonFailure{}, initFailures...)
	allFailures = append(allFailures, listFailures...)

	if listSuccessCount == 0 {
		// Zero INTENDED participants (a declared-but-empty group — decision
		// claim 5) is NOT the all-failed case: nothing was attempted, so the
		// route exposes an empty tool surface, not a -32000 "all daemons
		// failed" envelope. Distinguish on IntendedParticipants (empty ⇒
		// nothing intended) vs the genuine all-failed case (≥1 intended, all
		// failed).
		//
		// B2 (bot R3): the empty-success branch is restricted to GROUP scopes
		// (ScopeKey carries the GroupScopeKeyPrefix "g:"). A /clients/ session
		// with zero IntendedParticipants is NOT a declared-but-empty group —
		// it is a client with no bindings (a startup-publish failure, or a
		// client absent from the resolver snapshot). Returning empty-success
		// there would MASK a broken hub config AND change the byte-identical
		// client contract (pre-groups, a zero-binding client got the -32000
		// all-failed envelope). So a non-group scope keeps the -32000 envelope;
		// only a group reaches empty-success.
		if allowEmptySuccess || (len(sess.IntendedParticipants) == 0 && strings.HasPrefix(sess.ScopeKey, GroupScopeKeyPrefix)) {
			sess.RouteMap.Store(&mergedRoutes)                                         // empty map — no tools to route
			return buildToolsListResponse(reqID, mergedTools, allFailures, instanceID) // mergedTools is empty → result.tools=[]
		}
		return buildAllFailedToolsListResponse(reqID, allFailures, instanceID)
	}
	sess.RouteMap.Store(&mergedRoutes)
	return buildToolsListResponse(reqID, mergedTools, allFailures, instanceID)
}

// resolvedCallTarget is the output of route lookup + resolver
// revalidation. Either errBody is set (the caller returns it verbatim
// as the final response) or the ref/daemonSID fields drive the
// outbound HTTP forward. daemonProto is the per-daemon NEGOTIATED MCP
// protocol version (codex bot r17 P1 closure on PR #157), used as
// the MCP-Protocol-Version header on the daemon-facing tools/call.
type resolvedCallTarget struct {
	ref         canonicalToolRef
	daemonSID   string
	daemonProto string
	errBody     []byte
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
	return dispatchToolsCall(ctx, sess, clientReqID, paramsRaw, target.ref, target.daemonSID, target.daemonProto)
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
	// not in the calling client's current bindings, or if the current
	// group visibility filter now hides the target tool. The session
	// RouteMap is intentionally session-local, so a long-lived group
	// session may still contain a route that was visible at tools/list
	// time. Re-check the live immutable snapshot after a republish so a
	// tools_hidden edit revokes existing sessions without requiring the
	// daemon/server binding to change.
	current := LoadResolverSnapshot()
	if current != nil && current != sess.SnapshotAtInit {
		if !daemonStillBound(current, sess.ScopeKey, ref) {
			body, mErr := buildJSONRPCError(clientReqID, -32601, "tool moved out of scope; reinitialize session", nil)
			return resolvedCallTarget{errBody: body}, mErr
		}
		if snapshotHidesTool(current, sess.ScopeKey, ref) {
			body, mErr := buildJSONRPCError(clientReqID, -32601, "tool moved out of scope; reinitialize session", nil)
			return resolvedCallTarget{errBody: body}, mErr
		}
	}

	// Look up the daemon's Mcp-Session-Id and negotiated protocol
	// version. codex bot r17 P1 closure on PR #157: every daemon-
	// facing header must use the version returned by the daemon's
	// initialize, not the session-level requested version.
	daemonKey := canonicalDaemonRef{Server: ref.Server, Daemon: ref.Daemon, Port: ref.Port}
	sess.mu.Lock()
	daemonSID, hasSID := sess.InitSuccesses[daemonKey]
	daemonProto := sess.DaemonProtoVer[daemonKey]
	sessProto := sess.ProtocolVersion
	sess.mu.Unlock()
	if !hasSID {
		body, mErr := buildJSONRPCError(clientReqID, -32603, "Internal error: no daemon session id for target", nil)
		return resolvedCallTarget{errBody: body}, mErr
	}
	if daemonProto == "" {
		daemonProto = sessProto
	}
	return resolvedCallTarget{ref: ref, daemonSID: daemonSID, daemonProto: daemonProto}, nil
}

// dispatchToolsCall builds the rewritten body, registers an in-flight
// row, forwards to the daemon under PerCallWallClockCap, and rewrites
// the daemon's response id back to the client's id. Removes the
// in-flight row via defer.
func dispatchToolsCall(ctx context.Context, sess *hubSession, clientReqID, paramsRaw json.RawMessage, ref canonicalToolRef, daemonSID, daemonProto string) ([]byte, error) {
	daemonSID, daemonProto = sess.refreshStalePortBeforeDispatch(ctx, ref, daemonSID, daemonProto)

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
		DaemonProtocol:  daemonProto,
		DaemonRequestID: daemonReqID,
		StartedAt:       time.Now(),
	})
	if !inserted {
		return buildJSONRPCError(clientReqID, -32600, "duplicate request id; original call still in flight", nil)
	}
	defer sess.RemoveInFlight(key)

	subCtx, cancel := context.WithTimeout(ctx, PerCallWallClockCap)
	defer cancel()

	// codex bot r17 P1 closure on PR #157: tools/call uses the
	// DAEMON-NEGOTIATED protocol version, not the session-level
	// requested one.
	body, err := postToolsCall(subCtx, ref, daemonSID, daemonProto, daemonReqID, rewrittenParams)
	if err != nil {
		// Hot-swap (a) self-heal: ONLY on a transport failure (the daemon was
		// restarted out from under the cached session — the request never
		// landed, so it is safe to retry). An HTTP-level rejection is NOT
		// retried (the daemon received it; a non-idempotent side effect may
		// have run). No timer: the trigger is this failure, not a backoff.
		if isRetriableTransportFailure(err) {
			if retryBody, ok := sess.selfHealRetry(subCtx, ref, key, rewrittenParams); ok {
				return rewriteResponseID(retryBody, clientReqID)
			}
		}
		return buildJSONRPCError(clientReqID, -32000, "tools/call failed: "+err.Error(), nil)
	}

	// Rewrite the daemon's response id back to the client's id.
	return rewriteResponseID(body, clientReqID)
}

// selfHealRetry is the hot-swap (a) failure-driven self-heal backstop. It runs
// ONLY after a tools/call failed with a TRANSPORT error. It re-resolves the
// daemon's current port from the resolver snapshot, re-initializes the daemon
// session under per-daemonKey singleflight (so a mass restart cannot trigger an
// init-storm), refreshes the cached session id under the same mu AggregateInitialize
// uses, and retries the call ONCE in-place. NO timer: the trigger is the call
// failure; the cadence is the client's own retries.
//
// Returns (retryBody, true) when the single retry completed with a daemon
// response; (nil, false) when the re-init or the retry itself failed (the caller
// then returns the original -32000). Hard-capped at one attempt; never recurses.
func (s *hubSession) selfHealRetry(ctx context.Context, ref canonicalToolRef, inflightKey requestIDKey, params json.RawMessage) ([]byte, bool) {
	sid, proto, port, ok := s.reinitDaemonSession(ctx, ref)
	if !ok {
		return nil, false // daemon still down → caller returns the original -32000
	}

	retryReqID, err := generateDaemonRequestID()
	if err != nil {
		return nil, false
	}
	// Update the in-flight row so a racing cancel targets the live retry, not
	// the never-landed first attempt. Net count change is zero (the dispatch's
	// defer RemoveInFlight still balances the original Insert).
	s.RemoveInFlight(inflightKey)
	s.InsertInFlight(inflightKey, inflightEntry{
		DaemonRef:       canonicalDaemonRef{Server: ref.Server, Daemon: ref.Daemon, Port: port},
		DaemonSessionID: sid,
		DaemonProtocol:  proto,
		DaemonRequestID: retryReqID,
		StartedAt:       time.Now(),
	})

	retryRef := ref
	retryRef.Port = port
	body, callErr := postToolsCall(ctx, retryRef, sid, proto, retryReqID, params)
	if callErr != nil {
		return nil, false // hard count 1 — never retry again
	}
	return body, true
}

// refreshStalePortBeforeDispatch is the hot-swap (b) event-driven proactive
// re-init path. The DaemonRestartWatcher marks a daemon's port stale when it
// observes a per-port current_pid change (the daemon was restarted). If this
// port is stale, the first caller re-initializes the cached session BEFORE
// dispatching so the client never sees the stale-session failure that (a)
// recovers only reactively (one-call lag).
//
// Concurrent callers may have already resolved the old daemonSID before the
// first caller refreshed it. They all serialize on the retained per-port state:
// the first caller performs the refresh and clears state.stale; followers then
// re-read InitSuccesses/DaemonProtoVer under sess.mu and dispatch with the fresh
// daemon session id instead of their stale local copy.
func (s *hubSession) refreshStalePortBeforeDispatch(ctx context.Context, ref canonicalToolRef, daemonSID, daemonProto string) (string, string) {
	state, ok := s.stalePortStateFor(ref.Port)
	if !ok {
		return daemonSID, daemonProto
	}

	state.mu.Lock()
	defer state.mu.Unlock()

	if state.stale {
		if sid, proto, _, ok := s.reinitDaemonSession(ctx, ref); ok {
			state.stale = false
			return sid, proto
		}
		_ = LogHubMcpEvent("debug", "proactive-reinit-failed", map[string]any{
			"server": ref.Server, "daemon": ref.Daemon, "port": ref.Port,
		})
		return daemonSID, daemonProto
	}

	daemonKey := canonicalDaemonRef{Server: ref.Server, Daemon: ref.Daemon, Port: ref.Port}
	s.mu.Lock()
	freshSID, hasSID := s.InitSuccesses[daemonKey]
	freshProto := s.DaemonProtoVer[daemonKey]
	sessProto := s.ProtocolVersion
	s.mu.Unlock()
	if !hasSID {
		// Follower woke after the first caller cleared stale, but no fresh sid
		// is cached for this daemonKey (e.g. reinitDaemonSession hit a nil/empty
		// InitSuccesses and skipped the write). Fall back to the caller's sid —
		// (a)'s reactive self-heal backstops a stale dispatch — but log it so the
		// silent degradation is observable (mirrors the reinit-failed debug above).
		_ = LogHubMcpEvent("warn", "proactive-reinit-follower-no-fresh-sid", map[string]any{
			"server": ref.Server, "daemon": ref.Daemon, "port": ref.Port,
		})
		return daemonSID, daemonProto
	}
	if freshProto == "" {
		freshProto = sessProto
	}
	return freshSID, freshProto
}

// reinitDaemonSession re-initializes the hub's MCP session to a daemon that was
// restarted, refreshing the cached session id + negotiated proto under the
// session mu (the same writer discipline AggregateInitialize uses). The
// initialize is coalesced per (Server,Daemon) via singleflight so a mass restart
// affecting many sessions/calls cannot trigger an init-storm. Returns the fresh
// (sid, proto, port, true), or ("","",0,false) if the daemon is still
// unreachable. SHARED by the (a) failure-driven self-heal (selfHealRetry) and the
// (b) event-driven proactive re-init (the supervisor-state restart watcher).
func (s *hubSession) reinitDaemonSession(ctx context.Context, ref canonicalToolRef) (string, string, int, bool) {
	port := s.currentDaemonPort(ref)
	daemonRef := canonicalDaemonRef{Server: ref.Server, Daemon: ref.Daemon, Port: port}
	// daemonKey MUST match resolveToolsCallRoute's key ({Server,Daemon,ref.Port})
	// so the refreshed session id is found by the NEXT call's route lookup.
	daemonKey := canonicalDaemonRef{Server: ref.Server, Daemon: ref.Daemon, Port: ref.Port}

	type initResult struct{ sid, proto string }
	v, initErr, _ := s.reinitGroup.Do(ref.Server+"\x00"+ref.Daemon, func() (any, error) {
		sid, negotiated, err := initializeDaemonSession(ctx, daemonRef, s.ProtocolVersion)
		if err != nil {
			return nil, err
		}
		return initResult{sid: sid, proto: negotiated}, nil
	})
	if initErr != nil {
		return "", "", 0, false
	}
	res := v.(initResult)
	proto := res.proto
	if proto == "" {
		proto = s.ProtocolVersion
	}

	s.mu.Lock()
	if s.InitSuccesses != nil {
		s.InitSuccesses[daemonKey] = res.sid
	}
	if s.DaemonProtoVer == nil {
		s.DaemonProtoVer = map[canonicalDaemonRef]string{}
	}
	s.DaemonProtoVer[daemonKey] = proto
	s.mu.Unlock()
	return res.sid, proto, port, true
}

// currentDaemonPort re-resolves the daemon's CURRENT port for (ref.Server,
// ref.Daemon) from the live resolver snapshot — so a self-heal retry reaches a
// daemon that came back on a NEW port. Falls back to the route's own port (the
// common same-port restart case).
func (s *hubSession) currentDaemonPort(ref canonicalToolRef) int {
	snap := LoadResolverSnapshot()
	if snap != nil {
		for _, b := range snap.Bindings[s.ScopeKey] {
			if b.Server == ref.Server && b.Daemon == ref.Daemon {
				return b.Port
			}
		}
	}
	return ref.Port
}

// ForwardCancellation looks up clientReqID in sess.InFlightRequests
// and forwards notifications/cancelled to the daemon with the daemon's
// request id. The in-flight row is NOT removed here.
//
// codex bot r15 P2 closure on PR #157: MCP cancellation is best-
// effort — the daemon may still finish the original call later. The
// original tools/call goroutine in dispatchToolsCall owns the
// in-flight row's lifecycle via its own `defer sess.RemoveInFlight`,
// and PerCallWallClockCap guarantees the goroutine returns within
// bounded time. Removing the row here would create a window in
// which a second tools/call reusing the same client request id
// would PASS InsertInFlight's duplicate-detection (the row is gone)
// even though the first call is still mid-flight on the daemon.
// Response correlation would then route both daemon responses to the
// same client id, and any subsequent cancellation would target the
// wrong daemon request. Hold the row until the original goroutine
// cleans up.
//
// Stdio-host daemons do NOT remap inbound requestId (per spec known
// limitation); the cancel may be dropped by the daemon — no harm
// done. If the daemon is unreachable, postCancellation's error is
// swallowed; the in-flight row still gets cleaned up by the
// original call's wall-clock cap.
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

	// codex bot r17 P1 closure on PR #157: notifications/cancelled
	// uses the DAEMON-NEGOTIATED protocol version stored on the
	// in-flight row at dispatch time, NOT the session-level requested
	// version. Strict daemons reject mismatched headers with 400.
	proto := entry.DaemonProtocol
	if proto == "" {
		proto = sess.ProtocolVersion
	}
	subCtx, cancel := context.WithTimeout(ctx, PerDaemonInitTimeout)
	defer cancel()
	_ = postCancellation(subCtx, entry.DaemonRef, entry.DaemonSessionID, proto, entry.DaemonRequestID)
}

// liveDaemonBinding returns the current binding for the stable
// (Server, Daemon) identity in the calling client's bindings.
func liveDaemonBinding(snap *ResolverSnapshot, client string, ref canonicalDaemonRef) (canonicalDaemonRef, bool) {
	if snap == nil {
		return canonicalDaemonRef{}, false
	}
	bindings, ok := snap.Bindings[client]
	if !ok {
		return canonicalDaemonRef{}, false
	}
	for _, b := range bindings {
		if b.Server == ref.Server && b.Daemon == ref.Daemon {
			return b, true
		}
	}
	return canonicalDaemonRef{}, false
}

// daemonStillBound reports whether the stable (Server, Daemon) identity
// is still in the calling client's bindings within the current snapshot.
func daemonStillBound(snap *ResolverSnapshot, client string, ref canonicalToolRef) bool {
	_, ok := liveDaemonBinding(snap, client, canonicalDaemonRef{Server: ref.Server, Daemon: ref.Daemon, Port: ref.Port})
	return ok
}

// ---------------- HTTP plumbing ----------------

// doDaemonPost issues a JSON-RPC POST to a daemon at 127.0.0.1:<port>
// /mcp and returns the JSON-RPC envelope bytes plus the response
// header. The envelope is ready to feed into `json.Unmarshal`
// directly — no SSE framing remains in the returned slice.
//
// `expectResponse` controls how the response body is processed:
//   - true (postInitialize, postToolsList, postToolsCall): the caller
//     sent a JSON-RPC method call and expects a response envelope
//     with a matching `id`. SSE bodies route through readSSEResponse
//     to find the response event; plain JSON returns the raw bytes.
//   - false (postInitialized, postCancellation): the caller sent a
//     JSON-RPC NOTIFICATION which by spec has no response. The body
//     (if any — daemons routinely return 200/202 with an empty body,
//     an SSE keepalive comment, or even a future event we don't
//     care about) is drained and discarded. Returning success on
//     2xx is the contract; trying to extract a JSON-RPC response
//     event from a stream that legitimately has none was the
//     codex bot r16 P1 bug — successful notifications got mis-
//     reported as init/cancel failures.
//
// Content-Type dispatch (codex bot r12 P1 closures on PR #157):
//   - `application/json` (or default): read body to EOF up to
//     maxAggregatorResponseBytes. Return raw bytes (or nil when
//     !expectResponse).
//   - `text/event-stream`: when expectResponse, stream-parse SSE
//     events incrementally via readSSEResponse and return the FIRST
//     event whose data is a JSON-RPC response (has an `id` field).
//     When !expectResponse, drain + discard. In the response-
//     expected case the connection is closed as soon as the
//     response event arrives, so a daemon that keeps the SSE stream
//     open for additional notifications never blocks us until the
//     timeout.
//
// HTTP >= 400 turns into an error with the raw body returned for
// the caller's error message. Body-read failure or oversize response
// also turns into an error.
//
// Used by postInitialize, postToolsList, postToolsCall (response
// callers), postCancellation, and postInitialized (notification
// callers). The single helper keeps the request-shape + retry policy
// + body-cap logic in one auditable place.
//
// protoVer is the MCP-Protocol-Version header value to send. Per MCP
// Streamable HTTP lifecycle, subsequent requests (after initialize)
// MUST carry this header; strict daemons may reject tools/list,
// tools/call, or notifications/cancelled despite a successful init
// (codex bot r4 P1 closure on PR #157). For the initialize call
// itself, callers pass "" — initialize is the negotiation step and
// the header is not yet known.
func doDaemonPost(ctx context.Context, port int, body []byte, daemonSID, protoVer string, timeout time.Duration, expectResponse bool) ([]byte, http.Header, error) {
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

	if resp.StatusCode >= 400 {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, maxAggregatorResponseBytes+1))
		if len(raw) > maxAggregatorResponseBytes {
			raw = raw[:maxAggregatorResponseBytes]
		}
		// Typed error so callers (hot-swap (a) self-heal) can tell a
		// received-but-rejected HTTP failure from a never-landed transport
		// failure. Error() still renders "HTTP %d", so messages are unchanged.
		return raw, resp.Header, &daemonHTTPError{code: resp.StatusCode, body: raw}
	}

	// Notification path: return IMMEDIATELY on 2xx without reading
	// the body. codex bot r18 P1 closure on PR #157: io.Copy-then-
	// discard would block until EOF for daemons that keep
	// text/event-stream connections open after accepting a
	// notification, turning successful notifications into ~5s stalls
	// (and cascading into initialize-timeout failures under tighter
	// parent contexts). The deferred resp.Body.Close() above tears
	// down the connection without waiting for EOF, which is correct:
	// JSON-RPC notifications by spec have no response payload so the
	// body bytes are not load-bearing. The trade-off is a lost
	// keep-alive slot (Go's transport pool won't reuse a connection
	// whose body wasn't fully drained), but we open a fresh
	// connection per call regardless.
	if !expectResponse {
		return nil, resp.Header, nil
	}

	if isSSEContentType(resp.Header.Get("Content-Type")) {
		payload, err := readSSEResponse(resp.Body, maxAggregatorResponseBytes)
		if err != nil {
			return nil, resp.Header, err
		}
		return payload, resp.Header, nil
	}

	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxAggregatorResponseBytes+1))
	if err != nil {
		return nil, resp.Header, fmt.Errorf("read: %w", err)
	}
	if len(raw) > maxAggregatorResponseBytes {
		return nil, resp.Header, fmt.Errorf("response too large (> %d bytes)", maxAggregatorResponseBytes)
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
//
// The envelope is built with json.Marshal — never fmt.Sprintf into a
// JSON template literal — so a protocolVersion containing `"` or `\`
// (Phase 4 will receive this from the client handshake) cannot
// corrupt the outbound JSON. codex bot r8 P2 closure on PR #157.
// Same shape as postCancellation in this file.
func postInitialize(ctx context.Context, ref canonicalDaemonRef, protoVer string) (sid string, negotiated string, err error) {
	hubVer, _, _ := buildinfo.Get()
	envelope := struct {
		JSONRPC string `json:"jsonrpc"`
		ID      int    `json:"id"`
		Method  string `json:"method"`
		Params  struct {
			ProtocolVersion string         `json:"protocolVersion"`
			Capabilities    map[string]any `json:"capabilities"`
			ClientInfo      struct {
				Name    string `json:"name"`
				Version string `json:"version"`
			} `json:"clientInfo"`
		} `json:"params"`
	}{
		JSONRPC: "2.0",
		ID:      1,
		Method:  "initialize",
	}
	envelope.Params.ProtocolVersion = protoVer
	envelope.Params.Capabilities = map[string]any{}
	envelope.Params.ClientInfo.Name = "mcphub-hub"
	envelope.Params.ClientInfo.Version = hubVer
	body, err := json.Marshal(envelope)
	if err != nil {
		return "", "", fmt.Errorf("marshal initialize envelope: %w", err)
	}
	raw, hdr, err := doDaemonPost(ctx, ref.Port, body, "", "", PerDaemonInitTimeout, true)
	if err != nil {
		return "", "", err
	}
	// codex bot r3 P1 closure on PR #157: inspect the JSON-RPC envelope
	// for an `error` object. Daemons returning HTTP 200 + JSON-RPC
	// error (protocol-version rejection, capability mismatch, etc.)
	// would otherwise be recorded as init successes with an empty
	// session id — misrouting follow-up tools/list / tools/call and
	// surfacing the wrong stage in partialFailures.
	//
	// `raw` is the JSON-RPC envelope: doDaemonPost already peeled off
	// any SSE framing in the text/event-stream path (codex bot r12 P1
	// closures on PR #157).
	//
	// codex bot r17 P1 closure on PR #157: parse `result.protocolVersion`
	// from the daemon's response. MCP version negotiation lets the
	// daemon return a SUPPORTED version different from the requested
	// `protoVer`; every subsequent header (notifications/initialized,
	// tools/list, tools/call, notifications/cancelled) MUST use the
	// daemon-negotiated value or strict daemons reject with 400.
	// codex bot r18 P2 closure on PR #157: use a pointer for `result`
	// so the absent case is distinguishable from an empty object. A
	// malformed daemon returning `{"jsonrpc":"2.0","id":1}` (no error,
	// no result) plus the Mcp-Session-Id header would otherwise be
	// treated as a successful initialize and recorded in InitSuccesses
	// — masking the real protocol violation until a follow-up call.
	var env struct {
		Error *struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
		Result *struct {
			ProtocolVersion string `json:"protocolVersion"`
		} `json:"result"`
	}
	if uerr := json.Unmarshal(raw, &env); uerr != nil {
		return "", "", fmt.Errorf("parse initialize response: %w", uerr)
	}
	if env.Error != nil {
		return "", "", fmt.Errorf("daemon initialize error code=%d: %s", env.Error.Code, env.Error.Message)
	}
	if env.Result == nil {
		return "", "", fmt.Errorf("daemon initialize response missing `result` field")
	}
	// codex bot r6 P1 closure on PR #157: reject HTTP 200 + no
	// JSON-RPC error + empty Mcp-Session-Id. The header is mandatory
	// per the MCP Streamable HTTP spec — every follow-up tools/list /
	// tools/call / cancellation MUST echo it back. Treating the empty
	// case as success would record the daemon in InitSuccesses with
	// daemonSID="", and subsequent calls would post without the
	// session header. Surface this as an init-stage failure instead.
	sid = hdr.Get("Mcp-Session-Id")
	if sid == "" {
		return "", "", fmt.Errorf("daemon initialize succeeded with empty Mcp-Session-Id header")
	}
	// Fall back to the requested protoVer when the daemon doesn't
	// emit a result.protocolVersion (older MCP daemons predating the
	// version-negotiation field).
	negotiated = env.Result.ProtocolVersion
	if negotiated == "" {
		negotiated = protoVer
	}
	return sid, negotiated, nil
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
	raw, _, err := doDaemonPost(ctx, ref.Port, body, daemonSID, protoVer, PerDaemonListTimeout, true)
	if err != nil {
		return nil, err
	}
	// `raw` is the JSON-RPC envelope; doDaemonPost peeled off any SSE
	// framing (codex bot r12 P1 closures on PR #157).
	//
	// codex bot r17 P1 closure on PR #157: pointer types let us
	// distinguish "result absent / result.tools absent" from "valid
	// empty tools list". A daemon returning {"jsonrpc":"2.0","id":2}
	// (no result, no error) must be surfaced as a list-stage failure
	// — counting it as success would publish an empty route map and
	// silently wipe routes from a previous good list, then later
	// tools/call would 404 with -32601 instead of the operator
	// seeing the real malformed-response error.
	var env struct {
		Error *struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
		Result *struct {
			Tools *[]json.RawMessage `json:"tools"`
		} `json:"result"`
	}
	if err := json.Unmarshal(raw, &env); err != nil {
		return nil, fmt.Errorf("parse: %w", err)
	}
	if env.Error != nil {
		return nil, fmt.Errorf("daemon error code=%d: %s", env.Error.Code, env.Error.Message)
	}
	if env.Result == nil {
		return nil, fmt.Errorf("daemon tools/list response missing `result` field")
	}
	if env.Result.Tools == nil {
		return nil, fmt.Errorf("daemon tools/list response missing `result.tools` field")
	}
	return *env.Result.Tools, nil
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
	raw, _, err := doDaemonPost(ctx, ref.Port, body, daemonSID, protoVer, PerCallWallClockCap, true)
	if err != nil {
		return nil, err
	}
	// `raw` is the JSON-RPC envelope; doDaemonPost peeled off any SSE
	// framing (codex bot r12 P1 closures on PR #157).
	return raw, nil
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
	// expectResponse=false: notifications/cancelled has no response
	// per JSON-RPC spec. doDaemonPost drains+discards the body so a
	// daemon emitting text/event-stream by default (common with
	// streamable endpoints) doesn't trip the readSSEResponse error
	// path. codex bot r16 P1 closure on PR #157.
	if _, _, err := doDaemonPost(ctx, ref.Port, body, daemonSID, protoVer, PerDaemonInitTimeout, false); err != nil {
		return err
	}
	return nil
}

// postInitialized sends notifications/initialized to a daemon after
// a successful initialize. Per MCP lifecycle the client MUST send
// this notification before issuing any other method calls; callers
// decide whether failure is best-effort or initialize-fatal.
func postInitialized(ctx context.Context, ref canonicalDaemonRef, daemonSID, protoVer string) error {
	body := []byte(`{"jsonrpc":"2.0","method":"notifications/initialized"}`)
	// expectResponse=false: notifications/initialized has no response
	// per JSON-RPC spec. Same rationale as postCancellation above
	// (codex bot r16 P1 closure on PR #157).
	if _, _, err := doDaemonPost(ctx, ref.Port, body, daemonSID, protoVer, PerDaemonInitTimeout, false); err != nil {
		return err
	}
	return nil
}

// isSSEContentType reports whether the Content-Type header value
// denotes Server-Sent Events. Accepts media-type parameters and
// case-insensitive match per RFC 7231.
func isSSEContentType(ct string) bool {
	base, _, _ := strings.Cut(ct, ";")
	return strings.EqualFold(strings.TrimSpace(base), "text/event-stream")
}

// readSSEResponse parses a text/event-stream body INCREMENTALLY,
// returning as soon as it sees the first event whose data is a
// JSON-RPC response envelope (jsonrpc=="2.0" with a non-empty `id`
// and no `method` field). codex bot r12 P1 closures on PR #157:
//
//   - Stops at the response event without waiting for stream EOF —
//     compliant Streamable HTTP daemons may keep the connection open
//     to send post-response notifications. Earlier `io.ReadAll`
//     blocked until timeout in that case.
//   - Respects SSE event boundaries (empty line terminates an event).
//     Earlier code flattened every `data:` line across the whole
//     stream into one blob, producing invalid concatenated JSON when
//     a daemon sent a notification before the response.
//   - Skips non-data SSE fields (`event:`, `id:`, `retry:`) and
//     comment lines (`:` prefix), matching HTML5 EventSource spec.
//   - Strips at most one leading space from the data value per spec.
//   - Joins multiple `data:` lines within a single event with `\n`.
//
// maxBytes caps total bytes read; exceeding it returns an error so
// a runaway daemon can't OOM the hub.
func readSSEResponse(r io.Reader, maxBytes int) ([]byte, error) {
	return readSSESelectedResponse(r, maxBytes, selectJSONRPCResponse, "JSON-RPC response event")
}

func readSSESelectedResponse(r io.Reader, maxBytes int, selectResponse func([][]byte) ([]byte, bool), responseDescription string) ([]byte, error) {
	scanner := bufio.NewScanner(r)
	// Allow a single line up to maxBytes — some daemons emit one
	// large `data:` line containing the full JSON envelope.
	scanner.Buffer(make([]byte, 64*1024), maxBytes+1)

	var dataLines [][]byte
	totalBytes := 0
	for scanner.Scan() {
		raw := scanner.Bytes()
		totalBytes += len(raw) + 1 // +1 for the LF the scanner consumed
		if totalBytes > maxBytes {
			return nil, fmt.Errorf("SSE response too large (> %d bytes)", maxBytes)
		}
		line := bytes.TrimSuffix(raw, []byte("\r"))

		if len(line) == 0 {
			// Empty line: dispatch the accumulated event.
			if len(dataLines) > 0 {
				if payload, ok := selectResponse(dataLines); ok {
					return payload, nil
				}
				dataLines = nil
			}
			continue
		}
		if line[0] == ':' {
			// SSE comment line — ignored.
			continue
		}
		if !bytes.HasPrefix(line, []byte("data:")) {
			// Other SSE field (`event:`, `id:`, `retry:`, unknown).
			continue
		}
		value := line[len("data:"):]
		if len(value) > 0 && value[0] == ' ' {
			value = value[1:]
		}
		// Copy: scanner reuses its buffer on the next Scan call, so
		// retaining the slice across iterations would corrupt the
		// accumulated event.
		valueCopy := make([]byte, len(value))
		copy(valueCopy, value)
		dataLines = append(dataLines, valueCopy)
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("read SSE: %w", err)
	}
	// Stream ended without a trailing blank line — dispatch any
	// final partial event.
	if len(dataLines) > 0 {
		if payload, ok := selectResponse(dataLines); ok {
			return payload, nil
		}
	}
	return nil, fmt.Errorf("SSE stream ended without a %s", responseDescription)
}

// selectJSONRPCResponse joins the accumulated `data:` lines of an SSE
// event with `\n`, parses the result, and returns it as the response
// payload IFF it is a JSON-RPC response envelope (jsonrpc=="2.0"
// with non-empty `id` and no `method`). Notifications (id absent,
// method present) and unrelated events are rejected so the caller
// keeps reading for the actual response.
func selectJSONRPCResponse(dataLines [][]byte) ([]byte, bool) {
	payload := bytes.Join(dataLines, []byte("\n"))
	var env struct {
		Jsonrpc string          `json:"jsonrpc"`
		ID      json.RawMessage `json:"id"`
		Method  string          `json:"method"`
	}
	if err := json.Unmarshal(payload, &env); err != nil {
		return nil, false
	}
	if env.Jsonrpc != "2.0" {
		return nil, false
	}
	// JSON-RPC responses ALWAYS have an `id`. Notifications carry
	// `method` and never `id`. Reject anything without an id or with
	// a method field.
	if len(env.ID) == 0 || env.Method != "" {
		return nil, false
	}
	return payload, true
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
func buildToolsListResponse(reqID json.RawMessage, tools []json.RawMessage, failures []DaemonFailure, instanceID string) ([]byte, error) {
	meta := map[string]any{
		"mcphub": map[string]any{
			"partialFailures": failuresOrEmpty(failures),
			"instance_id":     instanceID,
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
func buildAllFailedToolsListResponse(reqID json.RawMessage, failures []DaemonFailure, instanceID string) ([]byte, error) {
	data := map[string]any{
		"mcphub": map[string]any{
			"partialFailures": failuresOrEmpty(failures),
			"instance_id":     instanceID,
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
	// codex bot r5 P1 closure on PR #157: guard against null/non-object
	// daemon body. json.Unmarshal of `null` into a map type succeeds
	// but leaves the map nil; the subsequent `m["id"] = ...` would panic
	// with "assignment to entry in nil map". A buggy or non-conformant
	// daemon returning HTTP 200 + body=`null` would crash the request
	// path instead of surfacing a controlled JSON-RPC error.
	if m == nil {
		return nil, fmt.Errorf("daemon response body is not a JSON object")
	}
	m["id"] = clientReqID
	out, err := json.Marshal(m)
	if err != nil {
		return nil, err
	}
	return out, nil
}

// nameSpaceTools rewrites each tool's `name` field to
// "<server>__<rawname>" and returns one namespacedTool per accepted
// input. The caller (assembleToolsListResponse) does collision
// detection across daemons via the Exposed field — silent overwrites
// in a map would route tools/call non-deterministically when two
// daemons under the same server expose the same raw tool name (codex
// bot r9 P2 closure on PR #157).
//
// codex bot r4 P2 closure on PR #157: rewrite ONLY the name field;
// every other field passes through as raw JSON bytes. Earlier
// `map[string]any` round-trip forced numeric fields in tool
// schemas (default values, enum members, min/max constraints in
// inputSchema) through float64, silently rounding integers > 2^53
// in tool metadata. Clients would receive corrupted definitions
// even though only `name` should change.
func nameSpaceTools(ref canonicalDaemonRef, tools []json.RawMessage) []namespacedTool {
	out := make([]namespacedTool, 0, len(tools))
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
		out = append(out, namespacedTool{
			Exposed: exposed,
			Body:    nb,
			Ref: canonicalToolRef{
				Server:  ref.Server,
				Daemon:  ref.Daemon,
				Port:    ref.Port,
				RawName: rawName,
			},
		})
	}
	return out
}

// hiddenToolsForScope returns the session's FINE-GRAINED per-tool
// visibility filter (server name → hidden raw tool names) for its scope
// key, read from the currently published resolver snapshot (groups/namespaces
// Phase 5a). Returns nil for a CLIENT scope key (no entry → no filtering,
// the byte-identical fence) and for any live snapshot with no ToolsHidden.
// The binding set remains session-scoped; only per-tool visibility is live so
// operator tools_hidden changes revoke tools/list without a reconnect.
func (s *hubSession) hiddenToolsForScope() map[string][]string {
	snap := LoadResolverSnapshot()
	if snap == nil || snap.ToolsHidden == nil {
		return nil
	}
	return snap.ToolsHidden[s.ScopeKey]
}

// hiddenToolSet is the precomputed O(1)-lookup form of a group's per-tool
// visibility filter: server name → set of hidden raw tool names. Built ONCE
// per tools/list assembly (buildHiddenToolSet) so the per-tool check in the
// merge loop is a map lookup, not a linear scan over the slice form. nil for
// a CLIENT scope key (no filter) or any group with no hidden tools.
type hiddenToolSet map[string]map[string]bool

// buildHiddenToolSet converts the snapshot's (server → []rawName) slice
// filter into the set form. nil/empty filter → nil set (the byte-identical
// client fence: hides() short-circuits to false). The conversion is a pure
// reshape — the same (server, rawName) pairs, just indexed for O(1) lookup.
func buildHiddenToolSet(filter map[string][]string) hiddenToolSet {
	if len(filter) == 0 {
		return nil
	}
	set := make(hiddenToolSet, len(filter))
	for server, names := range filter {
		if len(names) == 0 {
			continue
		}
		m := make(map[string]bool, len(names))
		for _, n := range names {
			m[n] = true
		}
		set[server] = m
	}
	if len(set) == 0 {
		return nil
	}
	return set
}

// hides reports whether the filter hides the raw tool name for the given
// server. nil/empty set → never hidden (the fence). The match is on
// (server, rawName) — the exact (server → raw tool name) shape groups.yaml's
// tools_hidden authors. A filter entry naming a server not in the session's
// fan-out, or a tool the server never advertises, simply matches nothing — a
// harmless no-op (decision claim 5: a stale/bad filter never faults, only
// narrows). O(1) per call (vs the prior linear scan over the slice form).
func (s hiddenToolSet) hides(server, rawName string) bool {
	if len(s) == 0 {
		return false
	}
	return s[server][rawName]
}

// snapshotHidesTool reports whether snap's live per-tool visibility filter
// currently hides ref for scopeKey. It is used on tools/call revalidation to
// close stale group sessions whose RouteMap was built before a tools_hidden
// republish. Client scope keys have no ToolsHidden entry by invariant, so
// this remains a no-op for /clients/ sessions.
//
// This runs on the tools/call hot path (once per call), so it does a DIRECT
// scan of the one server's hidden-name slice rather than calling
// buildHiddenToolSet — that helper allocates a whole map-of-maps over EVERY
// hidden tool in the scope just to answer a single (server, rawName) lookup,
// which is wasted work per call. The per-server hidden list is tiny
// (operator-authored), so a linear scan is both faster and allocation-free.
func snapshotHidesTool(snap *ResolverSnapshot, scopeKey string, ref canonicalToolRef) bool {
	if snap == nil || snap.ToolsHidden == nil {
		return false
	}
	for _, hidden := range snap.ToolsHidden[scopeKey][ref.Server] {
		if hidden == ref.RawName {
			return true
		}
	}
	return false
}

// failuresOrEmpty returns failures if non-nil, otherwise an empty
// slice (so JSON marshal emits `[]` rather than `null`).
func failuresOrEmpty(f []DaemonFailure) []DaemonFailure {
	if f == nil {
		return []DaemonFailure{}
	}
	return f
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

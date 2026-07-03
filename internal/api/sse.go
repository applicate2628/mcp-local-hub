// sse.go — the G3 health/capability probes' single adapter for pulling a
// JSON-RPC response out of a fully-buffered Streamable-HTTP body that may be
// plain application/json or a text/event-stream frame.
//
// MCP daemons answer either as plain application/json or as SSE. The probes
// (singleHealthProbe, sendForceMaterializeTools, capabilityListSubSection)
// io.ReadAll the WHOLE body (under their own size cap) before parsing, so they
// need a []byte adapter. Before this they each carried an independent,
// incomplete inline SSE parser (mandatory space after `data:`, first-`data:`-
// line-only, no CRLF strip). This collapses all three onto ONE owner.
//
// The owner is the package's existing event-aware readSSEResponse — NOT a new
// whole-buffer parser. A previous whole-buffer extractSSEPayload joined every
// `data:` line across event boundaries into one blob, so a multi-event reply
// (a JSON-RPC response event interleaved with a progress/notification event)
// produced two concatenated JSON objects and broke json.Unmarshal (codex bot
// P2 on PR #263). readSSEResponse respects SSE event boundaries and selects
// the JSON-RPC *response* event (jsonrpc 2.0 + id, no method), skipping
// notifications — the exact semantics the streaming hub aggregator already
// relies on (TestReadSSEResponseSkipsNotifications). manifest_test_remote.go's
// consumeSSE stays separate (it owns the G6 remote-manifest streaming path).
//
// Reference: HTML5 EventSource spec
// https://html.spec.whatwg.org/multipage/server-sent-events.html
package api

import "bytes"

// extractSSEPayload returns the JSON-RPC response body from a fully-buffered
// HTTP response that may be plain application/json or a text/event-stream
// frame. SSE bodies route through the event-aware readSSEResponse, which
// respects event boundaries and selects the JSON-RPC response event (skipping
// interleaved notifications). A body with no JSON-RPC response event — a plain
// application/json reply, or a stream carrying only notifications — is returned
// unchanged for the caller's json.Unmarshal to handle exactly as before.
func extractSSEPayload(raw []byte) []byte {
	// len(raw)+1 as the bound: the callers already size-cap the body when they
	// buffer it, so readSSEResponse's own size guard must not re-trip here.
	if payload, err := readSSEResponse(bytes.NewReader(raw), len(raw)+1); err == nil {
		return payload
	}
	return raw
}

// sse.go — single spec-compliant owner for extracting a JSON payload from
// a fully-buffered Server-Sent Events response body.
//
// MCP daemons answer over Streamable HTTP either as plain application/json
// or as a text/event-stream. The G3 health/capability probes
// (singleHealthProbe, sendForceMaterializeTools, liveCapabilitySubSection)
// each io.ReadAll the WHOLE body before parsing, so they need a byte-slice
// helper that turns the buffered SSE frame back into the embedded JSON
// envelope. Before this helper they carried three independent, incomplete
// inline copies of the same logic that:
//   - required a mandatory space after `data:` (compliant SSE may emit
//     `data:foo` with no space), and
//   - captured only the FIRST `data:` line (multi-line `data:` events are
//     legitimate per the HTML5 EventSource spec), and
//   - left a trailing `\r` on each line after splitting on `\n` (CRLF
//     framing then broke json.Unmarshal).
//
// extractSSEPayload fixes all three and is the ONE owner the three health-
// path probes call, per the AGENTS.md "no logic duplication" rule and the
// bug doc work-items/bugs/2026-05-12-health-go-sse-parser-incomplete.md.
//
// Scope note: the hub MCP aggregator (hub_mcp_aggregator.go::readSSEResponse)
// and the remote-manifest smoke check (manifest_test_remote.go::consumeSSE)
// are deliberately NOT collapsed onto this helper. Those parse the stream
// INCREMENTALLY off an io.Reader so they can early-exit at the JSON-RPC
// response event, skip pre-response progress notifications, and respect SSE
// event boundaries — semantics a whole-buffer []byte helper cannot provide.
// Folding them onto extractSSEPayload would re-introduce the
// "concatenate every data: line across the whole stream into one blob" bug
// that codex bot r12 fixed (see TestReadSSEResponseSkipsNotifications).
//
// Reference: HTML5 EventSource spec
// https://html.spec.whatwg.org/multipage/server-sent-events.html
package api

import "bytes"

// extractSSEPayload pulls the JSON payload out of a fully-buffered SSE
// response body. It concatenates the values of every `data:` line (in
// order, joined with `\n`, matching the EventSource "data buffer" rule),
// trimming an optional single leading space after the colon and a trailing
// `\r` from CRLF framing on each line.
//
// When the body contains no `data:` line at all it is returned unchanged —
// the plain application/json fallback path. An empty input therefore also
// returns empty (no `data:` line present), and the caller's json.Unmarshal
// surfaces the parse error exactly as it did before.
func extractSSEPayload(raw []byte) []byte {
	var parts [][]byte
	seenData := false
	for _, line := range bytes.Split(raw, []byte("\n")) {
		line = bytes.TrimSuffix(line, []byte("\r"))
		if !bytes.HasPrefix(line, []byte("data:")) {
			continue
		}
		value := line[len("data:"):]
		if len(value) > 0 && value[0] == ' ' {
			value = value[1:]
		}
		parts = append(parts, value)
		seenData = true
	}
	if !seenData {
		return raw
	}
	return bytes.Join(parts, []byte("\n"))
}

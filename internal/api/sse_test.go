package api

import "testing"

// TestExtractSSEPayload pins the adapter the three G3 health-path probes
// (singleHealthProbe, sendForceMaterializeTools, capabilityListSubSection)
// use to pull a JSON-RPC response out of a fully-buffered Streamable-HTTP
// body. extractSSEPayload routes SSE bodies through the package's event-aware
// readSSEResponse, so the matrix here asserts EVENT-BOUNDARY-aware,
// JSON-RPC-response-selecting behavior — not whole-buffer concatenation.
//
// The three shapes the old inline copies got wrong are each still pinned
// (data: with/without space; multi-line data: joined with \n; CRLF leaves no
// trailing \r), PLUS the codex bot PR #263 P2 cases: a multi-event reply where
// the JSON-RPC response is interleaved with a notification event must select
// the RESPONSE, in either order.
func TestExtractSSEPayload(t *testing.T) {
	// A JSON-RPC RESPONSE (jsonrpc 2.0, has id, no method) — selected.
	const response = `{"jsonrpc":"2.0","id":1,"result":{}}`
	// A JSON-RPC NOTIFICATION (method present, no id) — skipped.
	const notification = `{"jsonrpc":"2.0","method":"notifications/progress"}`

	cases := map[string]struct {
		raw  string
		want string
	}{
		// data: with a mandatory space (the only shape the old parsers handled).
		"data-with-space": {"data: " + response + "\n\n", response},
		// data: with NO space — compliant SSE; the old parsers left this as raw.
		"data-without-space": {"data:" + response + "\n\n", response},
		// Multi-line data: WITHIN one event — joined with \n per the EventSource
		// data-buffer rule; the joined text is still valid JSON. Old parsers
		// captured only the FIRST line.
		"data-multi-line-joined": {
			"data: {\"jsonrpc\":\"2.0\",\ndata: \"id\":1,\"result\":{}}\n\n",
			"{\"jsonrpc\":\"2.0\",\n\"id\":1,\"result\":{}}",
		},
		// CRLF framing — old strings.Split on \n left a trailing \r that broke
		// json.Unmarshal.
		"crlf-terminated": {"data: " + response + "\r\n\r\n", response},
		// Plain JSON, no data: prefix — no SSE response event, returned unchanged.
		"plain-json-fallback": {response, response},
		// Empty input — no data: line, returned unchanged (empty).
		"empty-input": {"", ""},
		// Leading + trailing blank lines around the event are ignored.
		"leading-trailing-blank-lines": {"\n\ndata: " + response + "\n\n", response},
		// event:/comment lines are non-data fields and are skipped within an event.
		"event-field-skipped":  {"event: message\ndata: " + response + "\n\n", response},
		"comment-line-skipped": {": keepalive\ndata: " + response + "\n\n", response},
		// Final event with no trailing blank line still yields its payload.
		"final-event-no-trailing-blank": {"data: " + response, response},
		// PR #263 P2 — response event then a notification event: select the
		// response, do NOT concatenate the two JSON objects.
		"response-then-notification": {
			"data: " + response + "\n\ndata: " + notification + "\n\n",
			response,
		},
		// PR #263 P2 — notification event FIRST, then the response: skip the
		// notification (method, no id) and keep reading to the response.
		"notification-then-response": {
			"data: " + notification + "\n\ndata: " + response + "\n\n",
			response,
		},
		// A stream carrying ONLY a notification (no JSON-RPC response) has no
		// response event → returned unchanged; the caller's json.Unmarshal then
		// fails, correctly classifying the daemon's reply as malformed.
		"only-notification-fallback": {
			"data: " + notification + "\n\n",
			"data: " + notification + "\n\n",
		},
		// A daemon emitting only non-data fields (no data: at all) falls back to
		// the raw buffer verbatim.
		"only-non-data-fields": {"event: ping\n: comment\n\n", "event: ping\n: comment\n\n"},
		// A bare data: line whose value is not a JSON-RPC response → no response
		// event → raw buffer returned unchanged.
		"bare-data-no-response": {"data:\n\n", "data:\n\n"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			got := string(extractSSEPayload([]byte(tc.raw)))
			if got != tc.want {
				t.Errorf("extractSSEPayload(%q):\n got  %q\n want %q", tc.raw, got, tc.want)
			}
		})
	}
}

package api

import "testing"

// TestExtractSSEPayload pins the shared spec-compliant SSE-frame parser
// used by the three G3 health-path probes (singleHealthProbe,
// sendForceMaterializeTools, liveCapabilitySubSection). It covers the
// matrix named in the bug doc's "Verification target":
// work-items/bugs/2026-05-12-health-go-sse-parser-incomplete.md.
//
// The three properties the old inline copies got wrong are each pinned:
//   - data: WITH a space and data: WITHOUT a space both parse.
//   - multiple data: lines concatenate with \n (EventSource data buffer).
//   - CRLF framing leaves no trailing \r on the payload.
//
// extractSSEPayload is a WHOLE-BUFFER helper (the callers io.ReadAll the
// body first), so — unlike the streaming readSSEResponse in
// hub_mcp_aggregator.go — it has no SSE event-boundary awareness and no
// JSON-RPC-response selection: it returns the joined value of EVERY data:
// line in the buffer. The multiple-events case below asserts exactly that
// whole-buffer concatenation rather than readSSEResponse's first-response
// selection. Mirrors TestReadSSEResponseHandlesCompliantFrames where the
// semantics overlap.
func TestExtractSSEPayload(t *testing.T) {
	const json = `{"jsonrpc":"2.0","id":1,"result":{}}`
	cases := map[string]struct {
		raw  string
		want string
	}{
		// data: with a mandatory space (the only shape the old parsers handled).
		"data-with-space": {"data: " + json + "\n\n", json},
		// data: with NO space — compliant SSE; the old parsers left this as
		// raw and json.Unmarshal then failed.
		"data-without-space": {"data:" + json + "\n\n", json},
		// Multi-line data: event — joined with \n per the EventSource data
		// buffer rule. The old parsers captured only the FIRST line.
		"data-multi-line": {
			"data: {\"jsonrpc\":\"2.0\",\ndata: \"id\":1,\"result\":{}}\n\n",
			"{\"jsonrpc\":\"2.0\",\n\"id\":1,\"result\":{}}",
		},
		// CRLF framing — the old `strings.Split` on \n left a trailing \r on
		// the payload that broke json.Unmarshal.
		"crlf-terminated": {"data: " + json + "\r\n\r\n", json},
		// Plain JSON, no data: prefix — returned unchanged (fallback path).
		"plain-json-fallback": {json, json},
		// Empty input — no data: line, returned unchanged (empty).
		"empty-input": {"", ""},
		// Leading + trailing blank lines around the data: event are ignored.
		"leading-trailing-blank-lines": {"\n\ndata: " + json + "\n\n", json},
		// event:/comment lines are not data: lines and are skipped.
		"event-prefix-skipped": {"event: message\ndata: " + json + "\n\n", json},
		"comment-line-skipped": {": keepalive\ndata: " + json + "\n\n", json},
		// Final event with no trailing blank line still yields its payload.
		"final-event-no-trailing-blank-line": {"data: " + json, json},
		// Multiple events: a whole-buffer helper concatenates EVERY data:
		// value across event boundaries (no boundary awareness). Two events
		// each carrying one data: line → two joined parts.
		"multiple-events-concatenate": {
			"data: " + json + "\n\ndata: " + json + "\n\n",
			json + "\n" + json,
		},
		// A daemon emitting only non-data fields (no data: at all) falls back
		// to returning the raw buffer verbatim.
		"only-non-data-fields": {"event: ping\n: comment\n\n", "event: ping\n: comment\n\n"},
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

// TestExtractSSEPayloadEmptyDataLine pins the edge case of a bare `data:`
// line with no value: it contributes an empty part, not a skip. A single
// empty data line yields an empty payload (distinct from the no-data:
// fallback, which returns the raw buffer).
func TestExtractSSEPayloadEmptyDataLine(t *testing.T) {
	got := string(extractSSEPayload([]byte("data:\n\n")))
	if got != "" {
		t.Errorf("bare data: line should yield empty payload, got %q", got)
	}
}

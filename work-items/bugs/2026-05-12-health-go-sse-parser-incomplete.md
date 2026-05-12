---
title: health.go SSE parser has the same incomplete-spec bug as hub_mcp_aggregator.go (pre-r10)
severity: medium
found-by: codex bot r10 inline review of PR #157
found-on: 2026-05-12
project: mcp-local-hub
context: adjacent-finding
status: open
related-pr: pending Phase 3 (fixed in hub_mcp_aggregator.go but health.go left in narrow scope)
---

# health.go SSE parser misclassifies compliant SSE daemon responses

## Symptom

`singleHealthProbe` / `probeMcpMethod` at `internal/api/health.go:727-734`
contains an inline SSE parser identical to the one the codex bot r10
flagged in `hub_mcp_aggregator.go:extractJSONPayload`:

```go
payload := raw
for _, line := range strings.Split(string(raw), "\n") {
    if strings.HasPrefix(line, "data: ") {
        payload = []byte(strings.TrimPrefix(line, "data: "))
        break
    }
}
```

The bugs are the same:
- Mandatory space after `data:` — compliant SSE may emit `data:foo` with
  no space, leaving `payload = raw` and JSON parsing fails.
- Only the FIRST `data:` line is captured — multi-line `data:` events
  (legitimate per HTML5 EventSource spec) produce truncated payloads.
- No CRLF handling — but `strings.Split` on `\n` leaves a trailing `\r`
  on each line; `HasPrefix("data: ")` then succeeds, but the trailing
  `\r` is preserved on the payload and breaks `json.Unmarshal`.

## Impact

The capability/health probe (G3 surface) misclassifies SSE-emitting
MCP daemons as errored. Operators see "error" state in the G3
capability display for daemons that are actually healthy but speak SSE
instead of plain `application/json`.

## Why filing as adjacent finding

The Phase 3 PR (#157) is the G4 unified hub MCP endpoint. `health.go`
is Phase G3 territory, and the bot specifically flagged only
`hub_mcp_aggregator.go:extractJSONPayload`. Per AGENTS.md "Bug-fix
scope: keep bug fixes narrowly scoped to the defect", I patched only
the aggregator in PR #157 and filed this as a follow-up.

The fix is a verbatim port of `extractJSONPayload`'s new
implementation (parses multiple `data:` lines, handles CRLF, accepts
both `data: foo` and `data:foo`):

```go
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
```

Ideal: extract the parser as a shared helper (e.g.,
`internal/api/sse.go` or a small package) and have both
`hub_mcp_aggregator.go` and `health.go` call it. That collapses the
two inline copies and prevents the next refactor from drifting again.

## Related code

- `internal/api/health.go:727-734` — current incomplete parser
- `internal/api/hub_mcp_aggregator.go:extractJSONPayload` (post-r10) —
  reference implementation
- HTML5 EventSource spec — https://html.spec.whatwg.org/multipage/server-sent-events.html

## Verification target

- New test analogous to `TestExtractJSONPayloadSSECompliance` covering
  the same 10 cases (no-space, multi-line, CRLF, plain-json fallback,
  etc.) on whichever path/function ends up owning the parser.

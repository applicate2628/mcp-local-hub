# Research: HFSS/CST MCP feedback R1

## Accepted evidence

- External evaluation: `<VFEM_fort>/.scratch/hfss-cst-mcp-evaluation/MCP-FEEDBACK-2026-08-10.md` (read-only input).
- Live safe reproduction on `127.0.0.1:9139/mcp` and `127.0.0.1:9140/mcp`, using only `initialize`, nonexistent-job `status`, schema/list, missing-resource calls, and `confirm=false`. No solver or export process was launched.
- Normative source: [MCP Streamable HTTP transport 2025-11-25](https://modelcontextprotocol.io/specification/2025-11-25/basic/transports). It requires HTTP 400 for an explicit unsupported protocol version; emitted session IDs should be unique and cryptographically secure; requests after session termination must receive HTTP 404.

## Finding disposition

| Finding | Current reproduction | Owner evidence | Verdict |
| --- | --- | --- | --- |
| MCP-HTTP-001 | Both endpoints returned HTTP 200 and reached `unknown job_id` with `MCP-Protocol-Version: qa-invalid-version`. | `internal/daemon/host.go:915-952` parses and dispatches without a version gate. | CONFIRMED |
| MCP-SESSION-001 | Two initializations returned the same hashed ID; missing, bogus, and deleted IDs all reached the tool at HTTP 200; DELETE returned 204. | `internal/daemon/host.go:92-145` owns one process-lifetime `sessionID`; `:215-232` creates it once; `:928` emits it on every POST; `:1179-1181` only compares against that constant and DELETE never removes it. | CONFIRMED |
| MCP-SCHEMA-001 | `tools/list` says “inspect, retrieve”; action schema is unconstrained string, while runtime accepts only start/status/result/cancel. | `servers/electromagnetics-mcp/src/mcphub_em_mcp/hfss.py:220-249`; `cst.py:248-276`; `jobs.py:228-245`. | CONFIRMED |
| MCP-SCHEMA-002 | Unknown argument was ignored and request reached job lookup; `additionalProperties` is absent. | Installed MCP 1.29.0 builds function schemas on `ArgModelBase`, whose default Pydantic config ignores extras; the two tool functions expose flat parameters. | CONFIRMED |
| MCP-RESOURCE-001 | Direct daemon `resources/read` and `tools/call __read_resource__` both now return the same explicit top-level `Unknown resource` JSON-RPC error. | `internal/daemon/resource_bridge.go:55-88` transforms only responses carrying `result`; error envelopes pass through unchanged. | REFUTED on current restarted runtime; no code change admitted |
| MCP-SCHEMA-003 | Invalid bounds with `confirm=false` stop at confirmation; published schemas lack bounds and CST array cardinality. | HFSS validation occurs after confirmation at `hfss.py:250-278`; CST validation occurs after confirmation at `cst.py:277-325`. | CONFIRMED |

## Root causes

1. `StdioHost` models an HTTP session as one immutable host token rather than a bounded set of initialize-created sessions. This also leaves no place to bind the negotiated protocol version.
2. MCP 1.29.0's FastMCP function adapter generates useful field schemas from type annotations but uses a Pydantic argument model that ignores unknown fields by default.
3. Solve validation is ordered behind the launch-confirmation gate, so the only no-launch path cannot validate a complete request.

## Constraints

- The live hub and both current daemon processes remain running until a replacement build passes isolated verification.
- The shared transport fix applies to all `stdio-bridge` daemons; native-HTTP forwarding and the hub's client-facing session store are protected.
- No real HFSS/CST job or export is authorized in this correction stage.
- The external feedback file is immutable.

## Research gate

PASS — five defects are reproduced and mapped to single owners; one reported defect is explicitly refuted by current runtime plus source.

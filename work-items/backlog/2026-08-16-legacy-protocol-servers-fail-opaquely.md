# Backlog: shipped manifests back servers the hub cannot talk to, and the refusal is opaque

Filed 2026-08-16 by $lead from a VFEM session that needed the `wolfram` server and lost time to a
misleading failure mode. Diagnosis is complete and verified; no hub code was changed. The operator
elected to defer the fix, so this is a record, not a request for immediate work.

## Symptom as the user sees it

```
wolfram: http://127.0.0.1:9132/mcp (HTTP) - Failed to connect
  HTTP 502: Streamable HTTP error: Error POSTing to endpoint:
  initialize negotiated unsupported protocol version "2024-11-05"
lldb:    http://127.0.0.1:9130/mcp (HTTP) - Failed to connect  (identical)
```

`mcphub status` shows both daemons **Running** with a healthy PID, port and uptime. `mcphub restart
--server wolfram` succeeds and changes nothing. A direct `curl` POST of a well-formed `initialize`
returns the same string as bare text regardless of the `protocolVersion` the client offers.

On the same hub, at the same time, `paper-search-mcp` (9127), `godbolt` (9126), `serena` and
`matlab` all connect normally — so the hub is fine and the failure is per-server.

## Root cause

The backing server is too old, and the hub is right to refuse it. Two facts establish this:

1. **The refusal is ours.** The literal format string `initialize negotiated unsupported protocol
   version %q` is present in the installed `mcphub` binary. The protocol-version literals in that
   binary are `2024-11-05, 2025-03-26, 2025-06-18, 2025-11-25, 2026-01-04, 2026-04-17, 2026-04-18,
   2026-04-20, 2026-05-06` — so `2024-11-05` is recognised but sits below the accepted floor of
   `2025-03-26`.
2. **The backing server speaks only that version.** `mcphub manifest show wolfram` gives
   `transport: stdio-bridge`, `command: node`,
   `base_args: ${HOME}/.local/mcp-servers/wolframalpha-llm-mcp/build/index.js`. That third-party
   clone pins `@modelcontextprotocol/sdk` at **0.6.0**, both declared in `package.json` and
   installed under `node_modules`. SDK 0.6.0 negotiates `2024-11-05` and nothing newer.

So the version incompatibility is real and the refusal is correct behaviour. What is wrong is
everything around it.

## P2 — the shipped manifest set is inconsistent with the hub's own protocol floor

`wolfram` and `lldb` are shipped manifests. Installing them produces a daemon that starts, stays
Running, binds its port, and can never serve a single request. Nothing in install or in `status`
notices.

The manifest for `wolfram` already documents that the user must clone and build a third-party Node
project — so the hub knows this server is externally sourced and unversioned, and knows it cannot
vouch for its SDK. A manifest that pins an external clone with no minimum-version expectation is a
promise the hub cannot keep.

Options for whoever takes this, none of them chosen here:

- Record a minimum backing-server protocol version (or SDK version) in the manifest and check it at
  install time, failing loudly rather than installing something that cannot work.
- Add a post-install or first-start preflight that performs one `initialize` against the freshly
  started daemon and reports the negotiated version. A daemon that cannot complete a handshake
  should not report `Running`.
- If neither is practical for externally-cloned servers, mark such manifests explicitly as
  unverified-at-install and say so in the install output.

## P2 — `status` reports Running for a daemon that cannot serve

`mcp-local-hub-wolfram-default  Running  9132  8880  5MB  1h38m` is true at the process level and
actively misleading at the service level. The operator reasonably concluded the server was working
and that the problem lay elsewhere; the first hypothesis was a configuration or enablement issue,
and a restart was tried before the real cause was found.

A liveness column that reflected the last successful handshake — or a distinct state such as
`Running (handshake failed)` — would have made this a ten-second diagnosis instead of a
multi-step investigation across the binary's strings and a third-party `package.json`.

## P3 — the message is accurate but arrives in the wrong voice

The text the client receives reads as a hub fault: an `HTTP 502` wrapping a transport error wrapping
a protocol string. Every fact needed for a good message is already in the hub's hands at that
moment — the server name, the manifest, the backing command, the version offered, and the floor it
failed. A message of the shape

> server `wolfram` negotiates MCP protocol `2024-11-05`, below the supported floor `2025-03-26`;
> its backing command is `node .../wolframalpha-llm-mcp/build/index.js` — the backing server's SDK
> is too old for this hub

would name the responsible party and the remedy. The present message names neither, and its
`502` framing points the reader at the hub.

## Not done, deliberately

The obvious fix on the other side — bump `@modelcontextprotocol/sdk` in the third-party clone and
rebuild — was scoped and then dropped on the operator's instruction. It is not a version bump:
`0.6.0` to `1.x` changes server construction, transport wiring and request-handler registration, so
it is a migration of a third-party project, and it belongs to that project or to whoever maintains
that clone, not to this repository. `lldb` fails identically and was never diagnosed past the
symptom, so treat "same cause" as unverified for it.

No hub code, configuration, manifest or daemon state was modified while establishing any of the
above. The only actions taken against the hub were read-only queries plus one `restart --server
wolfram`, which changed nothing.

## Terms and Abbreviations

- MCP — Model Context Protocol.
- Protocol floor — the oldest protocol version this hub will accept during `initialize`.
- stdio-bridge — a hub daemon that speaks HTTP to clients and proxies to a stdio server process.
- Backing server — the third-party process a manifest launches behind the hub's port.

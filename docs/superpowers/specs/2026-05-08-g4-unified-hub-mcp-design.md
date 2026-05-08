# G4 — Unified Hub MCP Endpoint Design

**Status:** approved 2026-05-08 (post-brainstorm). Adds an opt-in **dedicated-port** MCP endpoint that aggregates tools across all installed daemons of a given client. Lives behind a per-client shared-secret token + loopback-guard; default-OFF; introduces no behavior change for existing per-daemon URLs.

## Goal

Give an MCP client (Claude / Codex / Cursor / etc.) **one URL** that exposes the union of tools from every server installed for that client, instead of the current 1-URL-per-daemon configuration. Operators who today run `n` servers × `m` clients see `n` daemon entries per client config — G4 collapses that to a single hub entry per client when enabled.

Tool **execution** is in scope. Prompts, resources, and server-initiated notifications are explicitly out of scope (see "Out of scope" below).

## Architecture

A new HTTP listener on `127.0.0.1:9120` ("hub-mcp port") owned by the running `mcphub gui` process. Separate `http.ServeMux` and `http.Server` from the GUI port (`9125`); both lifetimes attach to the GUI process.

**Per-client URL paths** carve namespace per client adapter:

```text
http://127.0.0.1:9120/clients/claude/mcp
http://127.0.0.1:9120/clients/codex/mcp
http://127.0.0.1:9120/clients/cursor/mcp
…
```

When a client connects to its path, the hub:
1. Validates the request (loopback-guard + per-client token — see "Auth").
2. Resolves the union of tools from every daemon installed for that client across all manifests.
3. For `initialize`, returns a synthetic capabilities reply.
4. For `tools/list`, fans out to all participating daemons (concurrent, bounded), aggregates results with namespaced names, returns the merged list.
5. For `tools/call`, decodes the namespaced name back to (server, daemon, raw-tool-name), forwards to that daemon's `/mcp` endpoint, streams the response back.

The hub-mcp port behaves as a **transparent proxy + aggregator**, not a stateful broker. Every request resolves daemons live from the in-memory manifest snapshot the GUI process already holds (`a.Status()`).

**Tech stack:** Go (existing `internal/api` + `internal/daemon` packages). No new deps. Reuses `JSONRPCRequest` / `JSONRPCResponse` from `internal/daemon/backend_lifecycle.go:15-50` and the same SSE-or-JSON parsing shape as the G2 capability prober (`internal/api/health.go:687-805`).

## Opt-in gate

Default is **OFF**. Operator opt-in via Settings:

- New registry key in `internal/api/settings_registry.go:84-93` (gui_server section):
  ```go
  {Key: "gui_server.hub_endpoint_enabled", Section: "gui_server", Type: TypeBool,
      Default: "false", Deferred: true,
      Help: "Expose a single aggregated hub URL per client instead of per-daemon URLs. Restart required. Per-client tokens regenerate on enable; client configs need re-install."},
  ```
- `Deferred: true` matches `gui_server.port` precedent — value persists; takes effect on next `mcphub gui` start.
- Until the operator enables and restarts, the hub-mcp listener does not bind, no token files are written, no client configs change.

Toggling OFF after enable: listener stops on next start; per-client token files remain on disk (operator-deletable). Re-installing clients reverts them to per-daemon URLs (covered by existing `mcphub install`).

## Auth — three-layer defense

1. **Loopback-guard** (existing `rejectUnsafeLoopbackRequest` from `internal/daemon/loopback_guard.go:12-67`). Rejects:
   - Non-loopback `Host:` header (DNS-rebind defense).
   - Non-loopback `Origin:` if Origin is set.
   - `Sec-Fetch-Site: cross-site` (browser-driven attack defense).

2. **Per-client shared-secret token** carried in `X-Mcphub-Hub-Token` request header. Cryptographically random, 32 bytes hex-encoded (64 chars). Token-per-client (not per-server, not global) — limits blast radius if any one client's config is exfiltrated.

3. **Constant-time comparison** at the hub side (`subtle.ConstantTimeCompare`) to defeat timing-oracle probes.

Token storage:

- Tokens live in `<state-dir>/hub-mcp-tokens.json` (0600). Same `<state-dir>` resolution as the watchdog (`%LOCALAPPDATA%\mcp-local-hub\` on Windows; XDG-style on POSIX, gated by `test_state_path_env` build tag).
- Schema:
  ```json
  {
    "schema_version": "1",
    "tokens": {
      "claude":  {"token": "<hex>", "created_at": "2026-05-08T11:30:00Z"},
      "codex":   {"token": "<hex>", "created_at": "2026-05-08T11:30:00Z"},
      "cursor":  {"token": "<hex>", "created_at": "2026-05-08T11:30:00Z"}
    }
  }
  ```
- File is generated lazily on first `mcphub gui` start with the gate ON (idempotent — does not regenerate if file already valid).
- A new CLI subcommand `mcphub hub-mcp regenerate-token --client <id>` rotates one client's token and prints a re-install reminder.

Token write into client configs at `mcphub install` time: when `hub_endpoint_enabled=true`, the install path writes the hub URL + adapter-appropriate header instead of the per-daemon URL. Adapters that do not support custom headers (relay-style entries, antigravity) fall back to per-daemon URL with an explicit warning — they remain on the legacy path until per-adapter support lands.

## Tool-name namespacing

MCP tool names are flat strings within a client view, but two servers might both expose `read_file`. The hub namespaces every aggregated tool name as:

```text
<server>__<original_tool_name>
```

Double-underscore separator is chosen for two reasons:
- The G2 capability ID format (`server/daemon/kind/name`) uses `/` which is not legal in MCP tool names per the JSON-RPC method-name conventions clients enforce.
- Most existing tool names use snake_case and rarely contain `__`. We document the convention and reject server names containing `__` at install time (additional manifest validation rule — separate task in plan).

Decode at `tools/call`:
1. Split on first `__`.
2. Left half = server name. Look up `(server, daemon)` for the calling client's binding.
3. Right half = raw tool name. Forward unchanged in the proxied JSON-RPC request body.

Tool collisions across servers (e.g. two servers both expose `filesys__read_file`) are surfaced as a manifest validation warning; the hub serves the first manifest's binding by deterministic order (alphabetical server name) and logs the conflict.

## Data flow — `tools/call` round-trip

```text
Claude IDE                             Hub (9120)                     Daemon (random port)
   │                                       │                              │
   │── POST /clients/claude/mcp ──────────►│                              │
   │   Body: {tools/call, name="filesys__read_file", args={…}}            │
   │   Headers: Mcp-Session-Id (per-client),                              │
   │            X-Mcphub-Hub-Token: <hex>                                 │
   │                                       │                              │
   │                                       │── decode name → filesys/claude
   │                                       │── lookup port from a.Status()
   │                                       │   (no port? → JSON-RPC error)
   │                                       │                              │
   │                                       │── POST /mcp ────────────────►│
   │                                       │   Body: {tools/call, name="read_file", args={…}}
   │                                       │   Mcp-Session-Id: forwarded  │
   │                                       │   (no token; daemon trusts loopback)
   │                                       │                              │
   │                                       │◄── JSON or SSE response ─────│
   │                                       │                              │
   │◄── streamed pass-through ─────────────│                              │
```

`tools/list` follows the same pattern but fans out concurrently to every daemon installed for the client (bounded concurrency = `min(8, len(daemons))`), aggregates with namespaced names, returns one merged JSON-RPC reply. Per-daemon failure is logged and the daemon's tools are dropped from that client's view; the rest of the list still goes through.

`initialize` does NOT fan out. The hub returns a synthetic `initialize` response with:
- `protocolVersion`: `"2025-03-26"` (matches the version the capability prober uses today).
- `capabilities`: `{"tools": {"listChanged": false}}` (no listChanged events in MVP).
- `serverInfo`: `{"name": "mcphub-aggregator", "version": <hub-version>}`.

Per-call init is the daemon's responsibility — daemons already cache an init exchange in their lazy-proxy / protocol bridge layer (`internal/daemon/protocol_bridge.go:91-154`). The hub never holds session state for clients.

## Multi-instance client safety

Multiple Claude windows or two Codex sessions all hit the same `/clients/<id>/mcp` URL with the same token. Per-process MCP session isolation is preserved by:

- The `Mcp-Session-Id` header — clients pick their own; the hub forwards it to daemons unchanged.
- StdioHost (`internal/daemon/host.go:749-777`) already multiplexes JSON-RPC ids across concurrent HTTP callers, so two parallel `tools/call` requests to the same daemon do not collide.

G4 inherits this — it does not introduce a new session model. The 2-daemons-per-server arrangement (claude.exe + codex.exe) is unchanged.

## Out of scope (MVP)

Explicitly **not** in G4:

| Feature | Why deferred |
|---|---|
| `prompts/list`, `prompts/get` | Most MCP clients don't yet exercise prompt routing; aggregation semantics under-specified. |
| `resources/list`, `resources/read`, `resources/subscribe` | Resource URIs need a namespacing scheme like tools but with URL-safe fragments; punted to a follow-up. |
| `notifications/*` (server-initiated) | Hub would need to demux fan-in notifications back to the right client — requires durable per-client connection state we explicitly avoid in MVP. |
| Tool-allowlist UI | Forward-all is the gate; allowlist needs UX design + persistence schema — separate task. |
| Per-tool rate limits | Out of MVP. |
| Multi-instance daemons | Existing 1-daemon-per-(server, client) is preserved. |
| Adapter-side header support for relay-style clients (antigravity) | Falls back to per-daemon URL with warning. |

## Threat model

| Vector | Mitigation |
|---|---|
| Browser-driven CSRF (malicious page → loopback) | `Sec-Fetch-Site` + `Origin` checks via existing `rejectUnsafeLoopbackRequest`. |
| DNS rebinding (`evil.com → 127.0.0.1`) | `Host:` header must be loopback. |
| Local privilege escalation (low-priv process reads token from another user's `%LOCALAPPDATA%`) | Token file at 0600 in per-user state dir. Same boundary as watchdog / intent-audit. |
| Token leak via process memory dump | Acknowledged residual risk. Tokens are per-client; rotate via `mcphub hub-mcp regenerate-token` if compromise suspected. Same boundary as existing daemon `Mcp-Session-Id` cookies. |
| Cross-client tool-call leakage (Claude calls a Codex-only tool) | Hub resolves daemons via the **calling client's** bindings only; Codex daemons are not in Claude's view. |
| Manifest-name injection (`server__name__more`) | Tool-name namespacing reserves `__` separator at hub side; server-name validation at install time rejects `__` in manifest server names (separate task in plan). |
| Token-comparison timing oracle | `subtle.ConstantTimeCompare` on the hub. |
| Hub becoming a privileged proxy that bypasses per-daemon checks | Hub forwards JSON-RPC body unchanged; daemons retain full authority over tool execution. Hub adds NO authority — it only adds aggregation + auth gating. |
| Multi-process race on token file write at `mcphub install` time | Atomic write via `os.WriteFile` to temp + rename; install lock already serializes per-server installs (`internal/api/install.go`). |

The hub-mcp port introduces no new authority that didn't already exist on per-daemon ports; it only adds a fan-out + gate.

## Settings + CLI surface

**Settings UI (Settings → gui_server section):**
- New row "Hub MCP endpoint" with toggle. Help text:
  > "When enabled, mcphub installs a single aggregated URL per client (instead of per-daemon URLs). Restart required. Per-client tokens are generated on first enable; rotating a token requires re-installing clients."
- Pending-restart badge on save (mirrors existing `gui_server.port` deferred-save behavior — see `internal/gui/frontend/src/screens/Settings.tsx`).

**New CLI subcommand `mcphub hub-mcp`:**
```text
mcphub hub-mcp status [--json]      # show endpoint state, port, per-client token presence
mcphub hub-mcp regenerate-token --client <id>
                                    # rotate one client's token; prints
                                    # re-install reminder; refuses if
                                    # endpoint is disabled
```

Status output includes:
- Listener bound? port? PID of owning gui process?
- For each client: token present (yes/no), created-at, last-used-at (if tracked).
- Tail of recent decisions (rejected loopback, bad token, bad path) — lives in a new `<state-dir>/hub-mcp.log` JSON Lines file; same 10 MB → `.log.1` rotation as watchdog logs.

## Data model — wire types

New file `internal/api/hub_mcp_types.go`:

```go
package api

// HubMCPTokens — schema for <state-dir>/hub-mcp-tokens.json. Mirrors the
// shape consumed by mcphub install + the hub-mcp listener.
type HubMCPTokens struct {
    SchemaVersion string                       `json:"schema_version"` // "1"
    Tokens        map[string]HubMCPClientToken `json:"tokens"`         // key = client id (claude, codex, …)
}

type HubMCPClientToken struct {
    Token     string `json:"token"`      // 64-char hex (32 random bytes)
    CreatedAt string `json:"created_at"` // RFC3339 UTC
}
```

Frontend Settings consumer needs no new types — the toggle goes through the existing `SettingDTO` registry path.

## Files to create / modify

| File | Kind | Purpose |
|---|---|---|
| `internal/api/hub_mcp_types.go` | new | wire types for the token store |
| `internal/api/hub_mcp_tokens.go` | new | load / generate / atomic-write `hub-mcp-tokens.json`; per-client lookup; rotate |
| `internal/api/hub_mcp_handler.go` | new | HTTP handler for `/clients/<id>/mcp`; loopback-guard + token check + JSON-RPC dispatch |
| `internal/api/hub_mcp_aggregator.go` | new | fan-out for `tools/list`; namespacing helpers; tools/call decode + forward |
| `internal/api/settings_registry.go` | modify | add `gui_server.hub_endpoint_enabled` row |
| `internal/api/install.go` | modify | when gate ON, write hub URL + token header into adapter-supporting client configs; fall back to per-daemon URL otherwise |
| `internal/gui/server.go` (or equivalent gui bootstrap) | modify | start the hub-mcp listener on `:9120` when gate ON |
| `cmd/mcphub/hubmcp.go` | new | `mcphub hub-mcp status / regenerate-token` subcommand |
| `internal/gui/frontend/src/screens/Settings.tsx` | modify | render the new toggle + pending-restart badge |
| `docs/superpowers/plans/phase-3b-ii-backlog.md` | modify | mark G4 in-progress; later mark complete |

## Test surface

**Go unit tests:**
- `internal/api/hub_mcp_tokens_test.go`: generate, load, atomic-write, missing-file → fresh state, corrupted JSON → quarantine.
- `internal/api/hub_mcp_handler_test.go`: loopback-guard + token check matrix (8 combinations: loopback Y/N × token Y/N × correct path Y/N), constant-time timing test (statistical, bounded variance).
- `internal/api/hub_mcp_aggregator_test.go`: namespace encode/decode, collision deterministic ordering, server-name with reserved `__` rejected.
- Fan-out: 3 fake daemon HTTP servers (1 ok, 1 timeout, 1 error) → aggregated list contains only the OK daemon's tools, partial-failure logged.

**Go integration tests:**
- `internal/api/hub_mcp_e2e_test.go`: spin up `mcphub gui` with gate ON, hit `/clients/claude/mcp` with valid + invalid tokens, observe daemon receives forwarded `tools/call`, response streams back.

**Frontend unit tests:**
- `Settings.test.tsx`: new toggle row renders, save dispatches PUT with the right key, pending-restart badge appears.

**Playwright E2E:**
- New test `internal/gui/e2e/tests/hub-mcp.spec.ts`: gate OFF → `/clients/claude/mcp` returns connection refused (no listener); gate ON after restart → returns 401 without token, 200 with token.

**Manual smoke (added to `docs/phase-3b-ii-verification.md`):**
- D2.7: enable gate, restart, verify token files at `<state-dir>/hub-mcp-tokens.json` (0600); install a server for Claude; open Claude; observe `<server>__<tool>` names in Claude's tool picker; invoke a tool; observe daemon log shows the call.

## Migration / rollback

- **Forward**: operator toggles `gui_server.hub_endpoint_enabled=true`, restarts gui. Re-runs `mcphub install` for each server they want exposed via the hub URL — installer prefers hub URL when gate is ON.
- **Rollback**: operator toggles OFF, restarts gui. Hub listener stops. Re-runs `mcphub install` to revert client configs to per-daemon URLs. Token files remain on disk (idempotent re-enable preserves them); operator may delete the token file manually if they want a clean state.

No migration of existing client configs is automatic. Operators stay on per-daemon URLs until they explicitly opt-in.

## Open questions for codex review

1. **Header-vs-bearer token transport.** Some adapters (codex-cli? cursor?) may not support arbitrary custom headers in MCP server config. If only `Authorization: Bearer …` is universally supported, switch the convention. Verify per-adapter at plan time.
2. **Token rotation UX.** Current design requires re-running `mcphub install` after `regenerate-token`. Alternative: hub accepts a 60s grace window where both old + new tokens are valid. Defer to plan unless codex flags it as a security concern.
3. **Concurrent fan-out cap.** Hard-coded `min(8, n)` may under-utilize on machines with many daemons. Make it a hidden setting? Probably not — premature.
4. **Per-call timeout.** Hub forwards with no timeout cap; daemon's own `/mcp` handler enforces tool-call timeouts. Should the hub add a wall-clock cap (say 60s) to defend against pathological daemon hangs? Likely yes; codex can advise.

## Acceptance criteria

- Gate default-OFF; existing per-daemon URLs unchanged.
- Settings toggle persists to `gui-preferences.yaml`; pending-restart badge appears on save.
- After restart with gate ON, the listener binds `127.0.0.1:9120` and serves `/clients/<id>/mcp` for every adapter id known to `mcphub install`.
- Per-client token file generated 0600 on first start with gate ON.
- Loopback-guard + token validation rejected requests log to `<state-dir>/hub-mcp.log` with redacted token field.
- `mcphub install` for a client config with hub-mode-supported adapter writes the hub URL + token header, NOT a per-daemon URL.
- `tools/list` returns the merged set with `<server>__<tool>` names, deterministic ordering on collisions.
- `tools/call` round-trips through the daemon and returns the daemon's exact response body.
- Partial daemon failure during `tools/list` logs but does not 500 the whole list.
- All existing per-daemon endpoints still work with their existing clients.

## Terms and Abbreviations

- `MCP`: Model Context Protocol; JSON-RPC over Streamable HTTP transport spec used by Claude / Codex / Cursor / etc.
- `JSON-RPC`: text-based RPC protocol; hub forwards request/response bodies between client and daemon unchanged.
- `Mcp-Session-Id`: HTTP header MCP uses for session multiplexing; hub forwards unchanged.
- `Sec-Fetch-Site`: browser-emitted Fetch Metadata header used by the loopback-guard to reject cross-site browser requests.
- `DNS rebinding`: attack where a malicious DNS response points to `127.0.0.1` to bypass same-origin restrictions.
- `Loopback-guard`: existing `rejectUnsafeLoopbackRequest` middleware that rejects non-loopback Host / non-loopback Origin / cross-site Sec-Fetch-Site.
- `gate`: the `gui_server.hub_endpoint_enabled` boolean setting.
- `hub-mcp port`: dedicated `127.0.0.1:9120` listener owned by the gui process; serves only the aggregated MCP endpoint.
- `per-daemon URL`: existing per-daemon `/mcp` endpoint at `http://localhost:<random-port>/mcp` — unchanged by G4.
- `state-dir`: per-user state directory; `%LOCALAPPDATA%\mcp-local-hub\` on Windows, `$XDG_STATE_HOME/mcp-local-hub` on POSIX.
- `client adapter`: per-IDE installer logic (claude-code, codex-cli, cursor, etc.) — owned by `internal/api/install.go`.
- `relay-style adapter`: adapter that persists a relay-tuple (e.g. antigravity) instead of a URL; falls back to per-daemon URL in G4 MVP.

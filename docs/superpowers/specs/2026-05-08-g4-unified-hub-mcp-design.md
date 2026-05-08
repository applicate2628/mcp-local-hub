# G4 — Unified Hub MCP Endpoint Design

**Status:** v2, 2026-05-08. Revised from v1 (commit `ff02f23`) after stage-0 codex general-lane review (`.reports/2026-05/report(architecture-reviewer)-2026-05-08_22-30_g4-spec-stage0-general.md`) and security-lane review (`.reports/2026-05/report(security-reviewer)-2026-05-08_22-30_g4-spec-stage0-security.md`). All 12 findings (1 HIGH, 4 MED, 1 LOW security; 5 P1, 1 P2 general) addressed below.

**Pre-gate**: rejecting `__` in manifest server names is a **blocking prerequisite** for G4 implementation, not deferred (security F-S4 + general F-G4). It lands in the first plan task.

## Goal

Give an MCP client (Claude / Codex / Cursor / etc.) **one URL** that exposes the union of tools from every server installed for that client, instead of the current 1-URL-per-daemon configuration. Default-OFF; opt-in via Settings. Tool **execution** in scope. Prompts, resources, server-initiated notifications: out of scope (see "Out of scope").

## Architecture

A new HTTP listener owned by the running `mcphub gui` process, bound to **`127.0.0.1:0` (per-user random port chosen at start)**. The actual port + PID + start time are persisted to `<state-dir>/hub-mcp.endpoint.json` (0600). Client configs read this file at install-time to learn the URL.

This replaces v1's fixed `9120` (security F-S1: a fixed loopback port is machine-global, so any local user / low-priv process can pre-bind it and capture victim tokens). Per-user random + 0600 state file plus token authentication makes pre-bind capture impractical: an attacker that did not obtain the state file does not know which port to bind, and a legit hub start with a missing port file fails closed before clients connect.

**Per-client URL paths** carve namespace per client adapter:

```text
http://127.0.0.1:<random-port>/clients/claude/mcp
http://127.0.0.1:<random-port>/clients/codex/mcp
http://127.0.0.1:<random-port>/clients/cursor/mcp
…
```

**Tech stack:** Go (existing `internal/api` + `internal/daemon` packages). No new third-party deps. Reuses `JSONRPCRequest` / `JSONRPCResponse` from `internal/daemon/backend_lifecycle.go:15-50`, the SSE-or-JSON parsing shape from `internal/api/health.go:687-805`, and the in-process daemon resolution from `internal/api/install.go:1037-1067`. Atomic file writes use `os.WriteFile` to `*.tmp` with `O_CREATE|O_EXCL|O_WRONLY|0600`, `Sync`, then `os.Rename`. Windows DACL verification uses `golang.org/x/sys/windows`.

## Per-hub session model

v1 declared the hub stateless. **That is wrong** (general F-G2): MCP backends require a successful `initialize` exchange before they accept `tools/list` / `tools/call` (`internal/daemon/backend_lifecycle.go:195`, `:321`); native HTTP daemons issue a distinct `Mcp-Session-Id` per `initialize` (`internal/daemon/http_host.go:371`); the existing capability prober already follows the init→capture-sid→reuse-sid pattern (`internal/api/health.go:693-712`). v2 introduces a small per-client-session map.

**Hub session state**:

```go
type hubSession struct {
    ClientSessionID string                       // hub-issued UUID, returned to client as Mcp-Session-Id
    Client          string                       // claude | codex | cursor | …
    DaemonSessions  map[string]string            // key = canonical "<server>/<daemon>"; value = daemon Mcp-Session-Id
    RouteMap        map[string]canonicalToolRef  // exposed-flat-name → canonical (server, daemon, kind, raw_name)
    InitAt          time.Time
    LastUsedAt      time.Time
}

type canonicalToolRef struct {
    Server  string
    Daemon  string
    Port    int
    RawName string
}
```

**Lifecycle:**

1. **Client `initialize`**: hub assigns a fresh `client_session_id` (UUID), eagerly fans out `initialize` to every daemon participating in the calling client's bindings, captures each daemon's `Mcp-Session-Id`, stores the map. Returns a synthetic `initialize` reply with hub `serverInfo`. Per-daemon init failure logs but does not 500 the client; the failed daemon is omitted from the route map.
2. **Client `tools/list`**: hub fans out `tools/list` using stored daemon sids, builds the per-session route map (exposed-name → canonical ref), returns the merged list. Partial daemon failure surfaces in `result._meta.mcphub.partialFailures`. All-failed → JSON-RPC `-32000` (general F-G6).
3. **Client `tools/call`**: hub looks up `params.name` in the per-session route map. If absent → `-32601 method not found` (raw tool name, not "tool not found", because clients distinguish). If present → forward to that daemon with the stored daemon sid; stream response back.
4. **Notifications + ping** — see "Lifecycle methods" below.
5. **Session expiry**: hub drops sessions after 30 min idle or on `notifications/cancelled` for the whole session id (best effort). Daemon shutdown is NOT triggered by hub session expiry; daemons live by their own lifecycle.

Two parallel Claude windows each run their own `initialize` and own a distinct hub session — no cross-window state. A hub session cannot move between clients (path `client_id` and stored `Client` field must match on every request).

## Tool-name namespacing — route map, not split-decode

v1 used `<server>__<raw>` and split on first `__`. **Two separate findings reject that approach**:

- General F-G4: Claude Code documents `mcp__<server>__<tool>` exposed names. Underscores in raw tool names are common; `__` is not reserved by manifest validation today (`internal/api/manifest.go:25`, `internal/config/manifest.go:153`). Split-on-first-`__` is locally not reversible.
- Security F-S4: deferred `__` rejection in server names breaks the namespace boundary; an attacker with a hostile manifest named `evil__realserver` can collide with `evil` + tool `realserver__name`.

**v2 takes both fixes:**

1. **Manifest validator amendment (BLOCKING prereq):**
   - `internal/api/manifest.go:25` — `validManifestName` regex unchanged, but a new `containsReservedSeparator(s string) bool` helper rejects any name containing `__`. Applied in `checkManifestName` and `internal/config/manifest.go:153` validation.
   - First-time enforcement only on add / edit / install paths; existing on-disk manifests with `__` in server names are surfaced as a manifest-validation warning at startup. The hub gate (`gui_server.hub_endpoint_enabled=true`) refuses to bind if any participating manifest fails the new validation.
   - Test in `internal/config/manifest_test.go` and `internal/api/manifest_test.go`: `__` rejected; single `_` accepted.

2. **Exposed name + route map** — exposed flat name is still `<server>__<raw>`, but the hub does NOT split it. At `tools/list` time the hub builds:

   ```text
   exposedName := <server> + "__" + <raw_tool_name>
   route_map[exposedName] = canonicalToolRef{Server: <server>, Daemon: <daemon>, Port: <port>, RawName: <raw_tool_name>}
   ```

   `tools/call` looks up `params.name` in `route_map`. No string-split, no parsing — exact match against the map key. Raw tool names containing `__` work transparently (e.g. server `foo` + tool `bar__baz` → exposed `foo__bar__baz` → key `foo__bar__baz` maps to `(foo, daemon, "bar__baz")`).

3. **Collision handling** — if two `(server, raw)` pairs produce the same exposed name (only possible if `__` validation is bypassed; defensive), hub fails closed for that client until resolved. v2 does NOT silently pick alphabetical first (security F-S4).

## Cross-client invariant — hard auth gate

Five-step gate, in order, before any business logic runs (security F-S5):

1. **Loopback-guard** via existing `rejectUnsafeLoopbackRequest` (`internal/daemon/loopback_guard.go:12-67`). Same checks as today's per-daemon endpoints: `Host:` loopback, `Origin:` loopback or absent, `Sec-Fetch-Site` not cross-site.
2. **Path → client_id**. URL pattern `/clients/{id}/mcp` only. Anything else → `404` with empty body. `id` looked up in `tokenTable`. Unknown id → `401` with identical empty body for every reject (no oracle).
3. **Token shape gate**. `X-Mcphub-Hub-Token` header MUST be present and exactly 64 lowercase hex chars. Anything else → `401` with same identical body. (`subtle.ConstantTimeCompare` returns immediately on length mismatch — so length-validate first.)
4. **`subtle.ConstantTimeCompare(headerToken, tokenTable[client_id].Token)`**. Mismatch → `401`.
5. **Per-session lookup**. If request carries `Mcp-Session-Id`, look up the session — its `Client` field MUST equal the path `client_id` of step 2; otherwise `401`. (Defends against session-id-stealing-from-other-client.)

After all 5 pass, the route map for the request is built from ONLY this `client_id`'s bindings. There is no path through which Claude's token could route a `tools/call` against a Codex-only daemon, nor where `client_a/mcp` could see daemons not bound to `client_a`.

Acceptance tests:

- Claude token + `/clients/codex/mcp` path → `401`.
- Claude token + `/clients/claude/mcp` + `name="codex_only_server__tool"` → `-32601` (route map for Claude does not contain that key).
- Claude session id + `/clients/codex/mcp` after both have valid tokens → `401`.

## Token lifecycle hardening

State files (security F-S2, F-S3):

```text
<state-dir>/hub-mcp.lock                   # flock; serializes generate / regenerate / install
<state-dir>/hub-mcp-tokens.json            # 0600; per-client tokens
<state-dir>/hub-mcp.endpoint.json          # 0600; {schema_version, port, pid, started_at}
<state-dir>/hub-mcp.log                    # 0600; JSON Lines, 10 MB → .log.1 rotation (matches watchdog pattern)
```

`<state-dir>` resolves identically to the watchdog: `%LOCALAPPDATA%\mcp-local-hub\` on Windows, `$XDG_STATE_HOME/mcp-local-hub` (or `~/.local/state/mcp-local-hub`) on POSIX. Same per-user 0700 boundary. Same POSIX sanity check (world-writable parent / non-owner uid → exit 8). Same `test_state_path_env` build tag governs the env-var fallback for tests.

**Token schema:**

```json
{
  "schema_version": "1",
  "tokens": {
    "claude": {"token": "<64-char-hex>", "created_at": "2026-05-08T11:30:00Z"},
    "codex":  {"token": "<64-char-hex>", "created_at": "2026-05-08T11:30:00Z"}
  }
}
```

**Endpoint schema:**

```json
{
  "schema_version": "1",
  "port": 51823,
  "pid": 12345,
  "started_at": "2026-05-08T11:30:00Z"
}
```

**Atomic write (security F-S3):**

```go
// pseudocode — actual implementation in internal/api/hub_mcp_state.go
func writeStateFile(path string, payload []byte) error {
    flock(<state-dir>/hub-mcp.lock)            // serialize concurrent writers
    defer funlock()
    tmp := path + ".tmp"
    f, err := os.OpenFile(tmp, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)  // O_EXCL defeats symlink races
    if err != nil { return err }
    if _, err := f.Write(payload); err != nil { f.Close(); os.Remove(tmp); return err }
    if err := f.Sync(); err != nil { ... }
    f.Close()
    return os.Rename(tmp, path)                // atomic on NTFS + POSIX
}
```

**Load-time validation (security F-S3):**

```go
// pseudocode — actual implementation in internal/api/hub_mcp_state.go
func loadStateFile(path string) ([]byte, error) {
    info, err := os.Lstat(path)                            // Lstat — does not follow symlinks
    if err != nil { return nil, err }
    if info.Mode().Type() & (os.ModeSymlink|os.ModeIrregular) != 0 {
        return nil, fmt.Errorf("not a regular file: %s", path)
    }
    if runtime.GOOS != "windows" {
        if info.Mode().Perm() & 0077 != 0 {
            return nil, fmt.Errorf("group/world readable: %s", path)
        }
        if uid := info.Sys().(*syscall.Stat_t).Uid; uid != uint32(os.Getuid()) {
            return nil, fmt.Errorf("not owned by current user: %s", path)
        }
    } else {
        if err := verifyWindowsDACL(path); err != nil { return nil, err }
    }
    return os.ReadFile(path)
}
```

**Bind ordering (security F-S3):**

```text
1. validate <state-dir> sanity (existing watchdog check)
2. flock(<state-dir>/hub-mcp.lock)
3. load OR generate hub-mcp-tokens.json (with above hardening)
4. resolve all manifests, validate no '__' in server names → exit 8 if any fail
5. listener := net.Listen("tcp", "127.0.0.1:0")  → capture random port
6. write hub-mcp.endpoint.json (atomic, with new port)
7. funlock
8. http.Serve(listener, mux)
```

If any of steps 1-6 fail, the listener is never created and no requests can hit half-initialized state.

**Windows DACL verification (security F-S2):**

`verifyWindowsDACL(path)` queries the file's owner SID via `golang.org/x/sys/windows.GetSecurityInfo`, confirms it equals the current process token's user SID via `windows.GetTokenInformation`, then iterates the DACL and rejects any non-owner ACE that grants `FILE_GENERIC_READ`. This catches the case where `%LOCALAPPDATA%` ACL inheritance was tampered with or the file was created via a write path that didn't tighten DACL.

The `<state-dir>/hub-mcp.lock` itself does NOT need DACL verify (the lock leaks no secret), but `hub-mcp-tokens.json` and `hub-mcp.endpoint.json` both do. Logs (`hub-mcp.log`) are checked too because logs may legitimately need to be redacted-but-preserved for support, and DACL validates we are reading our own log not an attacker's.

## Logging hygiene + golden test

Single redaction helper (security F-S2):

```go
// internal/api/hub_mcp_log_redact.go
var hexTokenRE = regexp.MustCompile(`[0-9a-f]{64}`)

func RedactToken(s string) string {
    return hexTokenRE.ReplaceAllString(s, "<token>")
}
```

Every log line, every status JSON, every `--json` install output that touches token-bearing context routes through `RedactToken`. The narrow regex (`[0-9a-f]{64}` — exactly 64 lowercase hex chars) is the same shape generated by `crypto/rand.Read(buf[:32])` + `hex.EncodeToString`, so false-positive collateral redaction is bounded.

Golden test `internal/api/hub_mcp_log_redaction_test.go`:

1. Generate a token via the same code path the runtime uses.
2. Inject the token through every log emitter the package exposes (status, install, regenerate, error path).
3. Read the full log buffer + status JSON + install stdout.
4. Assert the plain-token bytes appear nowhere; only `<token>` placeholders.

## Install reconciler — one entry per client when gate ON

v1 reused `internal/api/install.go:1037-1067` with URL-swap. **General F-G1 rejects this**: that loop emits one `ClientUpdate` per binding, so installing N servers writes N hub-URL entries to the same client config (all pointing at the same hub URL). Clients would see the aggregated tool list N times.

**v2 reconciler logic** (in `internal/api/install.go`):

When `gui_server.hub_endpoint_enabled=true` and the client adapter supports custom headers:

1. Compute the per-client union of (server, daemon) bindings across ALL manifests on disk (not just the manifest being installed). This is the "participating set" for that client.
2. Plan **one** `ClientUpdate` per client with:
   - `Action: "add/replace"`
   - `Name: "mcphub-hub"` (stable; documented in spec)
   - `URL: read from <state-dir>/hub-mcp.endpoint.json`
   - `Headers: {"X-Mcphub-Hub-Token": "<token-for-this-client>"}`
3. Plan a `ClientUpdate` with `Action: "remove"` for every existing per-(server, client) entry that the new hub entry obsoletes (entries detected via `RelayServer != "" || URL matches old per-daemon shape`).
4. On uninstall of a server: subtract that server's bindings from the participating set; if the set becomes empty for that client, plan removal of the `mcphub-hub` entry.

For adapters that do NOT support custom headers (relay-style — antigravity), the planner falls back to the v1-equivalent per-daemon URL plan and emits an `ECHO_WARN` line:

```text
WARN: client 'antigravity' does not support hub-mode (no custom headers); installing per-daemon URLs.
```

A new `internal/api/hub_mcp_resolver.go` owns the cross-cutting join (general F-G3): joins manifests + ClientBindings + DaemonStatus + ports → `map[string][]canonicalDaemonRef` keyed by client. Used by both install planner and hub handler — single source of truth.

Adapter capability table (verify in plan-time):

| Adapter | Custom-header support | Hub-mode? |
|---|---|---|
| claude-code | yes (`headers` map in MCP server config) | yes |
| codex-cli | yes | yes |
| cursor | yes | yes |
| gemini-cli | TBD — verify before implementation | TBD |
| qwen-cli | TBD | TBD |
| vscode | yes | yes |
| antigravity | no (relay-tuple persistence) | no — fall back to per-daemon URLs |

The TBDs are tracked as plan-task verification work. If `gemini-cli` / `qwen-cli` turn out to lack header support, they fall back to per-daemon URLs same as antigravity.

## Client-origin lifecycle methods (general F-G5)

| Method | Hub behavior |
|---|---|
| `initialize` (id) | hub-owned: assign client_session_id, fan-out init to participating daemons, capture per-daemon sids, return synthetic `initialize` reply with hub serverInfo. Existing precedent: lazy_proxy.go handles its own synthetic init. |
| `notifications/initialized` (no id) | 202 to client; fan-out 202 to participating daemons (`internal/daemon/lazy_proxy.go:258-262` precedent). |
| `notifications/cancelled` (no id) | 202 to client; if `params.requestId` is in flight on a specific daemon, forward `notifications/cancelled` to that daemon (best effort). Otherwise drop. |
| other `notifications/*` (no id) | 202 to client; do NOT fan out (per-daemon decides at its own surface). Matches `internal/daemon/lazy_proxy.go:282-285`. |
| `ping` (id) | hub-local echo: `{"jsonrpc":"2.0","id":<id>,"result":{}}`. Matches `internal/daemon/lazy_proxy.go:263-271`. No daemon fan-out. |
| `tools/list` | hub-owned fan-out + namespacing (see "Per-hub session model" + "Tool-name namespacing"). |
| `tools/call` | route via session route map (see "Per-hub session model"). |
| `prompts/list`, `prompts/get` | `-32601 Method not found`. v2 explicitly rejects. (Out of scope for MVP; deferring is honest, not silent.) |
| `resources/list`, `resources/read`, `resources/subscribe`, `resources/templates/list` | `-32601 Method not found`. |
| `logging/setLevel` | `-32601`. (Hub does not expose log surface to clients in MVP.) |
| anything else | `-32601 Method not found`. |

## Partial-failure visibility (general F-G6)

`tools/list` reply when at least one participating daemon returned tools and at least one failed:

```json
{
  "jsonrpc": "2.0",
  "id": <reqId>,
  "result": {
    "tools": [...merged list...],
    "_meta": {
      "mcphub": {
        "partialFailures": [
          {"server": "filesys", "daemon": "claude", "err": "initialize: HTTP 503"},
          {"server": "search",  "daemon": "claude", "err": "tools/list: read: i/o timeout"}
        ]
      }
    }
  }
}
```

`tools/list` reply when ALL participating daemons failed:

```json
{
  "jsonrpc": "2.0",
  "id": <reqId>,
  "error": {
    "code": -32000,
    "message": "all participating daemons failed",
    "data": {
      "mcphub": {
        "partialFailures": [...same shape...]
      }
    }
  }
}
```

`partialFailures[].err` strings carry NO token bytes (golden test enforces) but DO carry the underlying daemon error so operators can debug.

## Settings + opt-in gate

New registry row in `internal/api/settings_registry.go:84-93`:

```go
{Key: "gui_server.hub_endpoint_enabled", Section: "gui_server", Type: TypeBool,
    Default: "false", Deferred: true,
    Help: "Expose a single aggregated hub URL per client instead of per-daemon URLs. Restart required. Per-client tokens generate on first enable; rotating a token requires re-installing clients."},
```

Toggle ON → restart → next start binds the listener, generates tokens, writes endpoint state. Toggle OFF → restart → listener does not bind; token + endpoint files remain on disk for operator inspection (idempotent re-enable preserves them).

Settings UI surface: existing `gui_server` section row pattern; pending-restart badge mirrors `gui_server.port` (`internal/gui/frontend/src/screens/Settings.tsx`).

## CLI surface

```text
mcphub hub-mcp status [--json]
    Show endpoint state (port, pid, started-at, presence per client),
    redacted recent events. NEVER prints token values.

mcphub hub-mcp regenerate-token --client <id> [--yes]
    Rotate one client's token. Refuses non-TTY without --yes (exit 6,
    same family as 'mcphub watchdog uninstall'). On success, prints
    re-install instruction. No 'old + new accepted' grace window —
    grace would extend the lifetime of a stolen token (security F-S6).
```

**Exit codes** (mirrors watchdog):

```text
0 — success
1 — backend error (validation, file write, etc.)
6 — non-interactive shell with regenerate-token but no --yes
8 — state path sanity rejected
```

## Threat model

| Vector | Mitigation |
|---|---|
| **Fixed-port pre-bind** (security F-S1, HIGH) | Per-user random port at `127.0.0.1:0`; port written to 0600 + DACL-verified state file. Attacker that did not read the state file does not know what to bind. |
| Browser CSRF / DNS-rebind / cross-site-fetch | Existing `rejectUnsafeLoopbackRequest` (Host loopback + Origin loopback + Sec-Fetch-Site not cross-site). Token still required. Custom `X-*` headers are not CORS-safelisted, so a hostile page cannot send `X-Mcphub-Hub-Token` from a `no-cors` fetch. |
| Token leak via process memory dump | Acknowledged residual risk (low for desktop dev threat model). Tokens are per-client; rotate via CLI if compromise suspected. |
| Token leak via logs / status / install output (security F-S2) | Single `RedactToken` helper applied at every emit site; golden test asserts zero plain-token bytes across all surfaces. Windows DACL verify on every token-bearing file at load time. |
| Cross-client tool-call leakage (security F-S5) | Five-step auth gate; route map built ONLY from path-client's bindings; `Mcp-Session-Id` Client field must match path client. |
| Manifest namespace injection (security F-S4) | `__` rejection in server names is **blocking prerequisite**; route map (not split-decode) handles raw tool names containing `__` transparently. Collisions fail closed for that client. |
| Token-comparison timing oracle | `subtle.ConstantTimeCompare` after fixed-length 64-hex shape gate. All 401s return identical empty body. |
| Hub becoming a privileged proxy | Hub forwards JSON-RPC bodies unchanged to daemons. Daemons retain full authority. Hub adds aggregation + auth gate, NOT new authority. |
| Token-file race / TOCTOU (security F-S3) | flock on lock file; `O_CREATE|O_EXCL|O_WRONLY|0600` temp; fsync; atomic rename; on load: `Lstat` + symlink/regular-file check + owner check + mode check + DACL check. Generate/load BEFORE listener bind. |
| State-dir trust boundary | Same per-user 0600/0700 boundary as watchdog. Same POSIX sanity check (exit 8) on world-writable parent / non-owner uid. |
| `mcphub install` race (security F-S6) | Per-client config write held under client-config lock; under that lock, re-read gate state and token immediately before write; re-read after write to verify no partial state. |
| Malicious CLI invocation (security F-S6) | `regenerate-token` interactive confirm + `--yes` for non-TTY; status redacts; no grace window on rotation. |

## Files to create / modify

| File | Kind | Purpose |
|---|---|---|
| `internal/api/manifest.go` | modify | reject `__` in server names (blocking prereq) |
| `internal/config/manifest.go` | modify | same validation in config-loader path |
| `internal/api/hub_mcp_resolver.go` | new | join manifests + bindings + DaemonStatus + ports |
| `internal/api/hub_mcp_state.go` | new | atomic write + load + DACL-verify of token file + endpoint file; flock lock file |
| `internal/api/hub_mcp_tokens.go` | new | generate / lookup / rotate per-client tokens |
| `internal/api/hub_mcp_handler.go` | new | HTTP handler: 5-step auth gate + JSON-RPC dispatch |
| `internal/api/hub_mcp_session.go` | new | per-hub-session map; route-map builder; cleanup TTL |
| `internal/api/hub_mcp_aggregator.go` | new | tools/list fan-out + namespacing + partial-failure assembly |
| `internal/api/hub_mcp_log_redact.go` | new | `RedactToken` helper |
| `internal/api/settings_registry.go` | modify | add `gui_server.hub_endpoint_enabled` |
| `internal/api/install.go` | modify | hub-entry reconciler when gate ON; per-client union; obsolete-entry removal; client-config lock |
| `internal/gui/server.go` (or gui bootstrap) | modify | start hub-mcp listener after token+endpoint state ready; bind 127.0.0.1:0 |
| `cmd/mcphub/hubmcp.go` | new | `mcphub hub-mcp status` + `regenerate-token` |
| `internal/gui/frontend/src/screens/Settings.tsx` | modify | new toggle row + pending-restart badge |
| `docs/superpowers/plans/phase-3b-ii-backlog.md` | modify | mark G4 in-progress; later mark complete |

## Test surface

**Go unit tests:**
- `internal/api/manifest_test.go` (extend) — reject `__` in server names; allow `_`.
- `internal/api/hub_mcp_resolver_test.go` — join correctness across 0/1/N manifests, manifests with disjoint client sets, daemons missing port (filtered), unknown client.
- `internal/api/hub_mcp_state_test.go` — atomic write + load roundtrip; load rejects symlink, non-owner, mode 0644, missing file. Windows DACL: load rejects file with non-owner Read ACE.
- `internal/api/hub_mcp_tokens_test.go` — generate, persist, rotate, golden token-redaction test (across all log/status surfaces).
- `internal/api/hub_mcp_session_test.go` — session create + lookup + TTL expiry; route-map build for collision-free + colliding inputs (collision rejected).
- `internal/api/hub_mcp_handler_test.go` — 5-step auth gate matrix: bad path / unknown client / missing token / wrong-shape token / wrong token / wrong session-client → all 401 with identical body. Good path → 200.
- `internal/api/hub_mcp_aggregator_test.go` — fan-out: 3 fake daemons (1 ok, 1 timeout, 1 error) → aggregated list contains only OK daemon's tools, `partialFailures` populated with the 2 failures. All-failed → JSON-RPC `-32000`.
- `internal/api/install_test.go` (extend) — hub-mode planner: gate-OFF → existing per-daemon entries; gate-ON + 3 servers → ONE hub entry per client + 3 obsolete-entry-removals; gate-ON + adapter without header support → fall back per-daemon + WARN; uninstall path empties hub entry when last server removed for that client.

**Go integration tests:**
- `internal/api/hub_mcp_e2e_test.go` — spin up `mcphub gui` with gate ON via env; hit `/clients/claude/mcp` with valid + invalid tokens; observe daemons receive forwarded `tools/call` with their own daemon sid; response streams back; session expires after TTL; cross-client invariant tests (Claude token on Codex path, Claude token + Codex-only tool name).

**Frontend unit tests:**
- `Settings.test.tsx` (extend) — new toggle row; save dispatches PUT with right key; pending-restart badge appears.

**Playwright E2E:**
- `internal/gui/e2e/tests/hub-mcp.spec.ts` — gate OFF → no listener bound (loopback connect fails); gate ON after restart → 401 without token; 200 with token; tools/list returns merged list with namespaced names.

**Manual smoke** (added to `docs/phase-3b-ii-verification.md` D2.7): enable gate, restart, verify token + endpoint files at `<state-dir>` (0600 + correct DACL on Windows); install a server for Claude; open Claude; observe `<server>__<tool>` names; invoke a tool; observe daemon log shows the call; rotate token via CLI; observe Claude reconnect fails until re-install.

## Acceptance criteria

- Gate default-OFF; per-daemon URLs unchanged.
- Settings toggle persists to `gui-preferences.yaml`; pending-restart badge on save.
- Manifest validator rejects `__` in server names everywhere — add, edit, install, parse paths. Existing `__` manifests warn at startup; gate-ON refuses to bind if any participating manifest fails.
- Per-user random port in `127.0.0.1:0` written to `<state-dir>/hub-mcp.endpoint.json` (0600, DACL-verified on Windows).
- Per-client token file (0600, DACL-verified) generated lazily on first start with gate ON; load rejects symlink / non-owner / wrong mode / wrong DACL; bind never happens if load/generate fails.
- Listener bind happens AFTER state load/generate (load-before-bind invariant).
- 5-step auth gate; identical 401 body for every failure path; cross-client tests pass.
- `__` in raw tool names handled via route map (no string-split).
- `tools/list` partial failure: `result._meta.mcphub.partialFailures` populated; all-failed → JSON-RPC `-32000`.
- Lifecycle methods: `notifications/initialized` 202; `ping` echo; `prompts/*` + `resources/*` `-32601`.
- Install: gate-ON + adapter-with-header-support → ONE hub entry per client (obsolete entries removed in same plan). Adapter without header support → per-daemon URLs + WARN.
- `mcphub hub-mcp regenerate-token` requires confirm or `--yes`; status NEVER prints token values.
- Golden redaction test: zero plain-token bytes in any log / status / install / regenerate output.
- All existing per-daemon endpoints still work with their existing clients (gate-OFF path untouched).

## Migration / rollback

- **Forward**: operator toggles `gui_server.hub_endpoint_enabled=true`; restarts gui. Re-runs `mcphub install` for each server they want exposed via the hub URL — installer plans hub-entry reconcile when gate is ON.
- **Rollback**: operator toggles OFF; restarts gui. Hub listener does not bind. Re-runs `mcphub install` to revert client configs to per-daemon URLs. Token + endpoint files remain on disk; idempotent re-enable preserves them.
- **Token compromise**: operator runs `mcphub hub-mcp regenerate-token --client <id>`; re-runs `mcphub install` for affected servers. Old token rejected immediately (no grace window).

No automatic migration of existing client configs. Operators stay on per-daemon URLs until they explicitly opt-in.

## Out of scope (MVP)

| Feature | Why deferred |
|---|---|
| `prompts/list`, `prompts/get` | Aggregation semantics under-specified; defer to dedicated follow-up. |
| `resources/list`, `resources/read`, `resources/subscribe` | URI-namespacing scheme needs separate design. |
| `notifications/*` (server-initiated, hub→client) | Requires durable per-client connection state we explicitly avoid in MVP. |
| Tool-allowlist UI | Forward-all is the gate; allowlist needs UX design + persistence schema. |
| Per-tool rate limits | Out of MVP. |
| Multi-instance daemons (>1 daemon per (server, client)) | Existing 1-daemon-per-(server, client) is preserved. |
| Header support for relay-style adapters (antigravity) | Falls back to per-daemon URL with warning. |
| `gemini-cli` / `qwen-cli` hub-mode (TBD verify) | Plan-time decision based on adapter capability check. |

## Open questions for next codex round

1. **Adapter capability matrix verification.** Spec assumes claude-code, codex-cli, cursor, vscode support custom headers in MCP config. Verify each before plan-time. `gemini-cli` and `qwen-cli` are TBD.
2. **TTL for hub sessions.** 30 min idle is a guess. Should it match daemon idle timeout (`internal/api/health.go` daemons default ~10 min)? Or be operator-configurable?
3. **Concurrent fan-out cap.** Spec says `min(8, n)`. Defensible? Or hidden setting?
4. **Per-call wall-clock cap.** Hub forwards with no upper bound — relies on daemon's tool-call timeout. Add a 60s hub-side cap to defend against pathological hangs?
5. **`gemini-cli` / `qwen-cli` adapter check.** Their MCP server config schemas — do they accept arbitrary headers? Affects the capability matrix.
6. **Audit-log retention.** `hub-mcp.log` rotates at 10 MB → `.log.1` (matches watchdog). Two files × 10 MB = 20 MB ceiling. Sufficient for forensic needs of a token compromise?

## Terms and Abbreviations

- `MCP`: Model Context Protocol; JSON-RPC over Streamable HTTP.
- `JSON-RPC`: text-based RPC; hub forwards request/response bodies between client and daemon.
- `Mcp-Session-Id`: HTTP header MCP uses for session multiplexing.
- `Sec-Fetch-Site`: browser-emitted Fetch Metadata header used by loopback-guard.
- `DNS rebinding`: attack where a malicious DNS response points to `127.0.0.1` to bypass same-origin restrictions.
- `Loopback-guard`: existing `rejectUnsafeLoopbackRequest` middleware.
- `gate`: the `gui_server.hub_endpoint_enabled` boolean setting.
- `hub-mcp port`: per-user random `127.0.0.1:<random>` listener owned by gui process.
- `per-daemon URL`: existing per-daemon `/mcp` endpoint at `http://localhost:<random-port>/mcp` — unchanged by G4.
- `state-dir`: per-user state directory; `%LOCALAPPDATA%\mcp-local-hub\` on Windows, `$XDG_STATE_HOME/mcp-local-hub` on POSIX.
- `client adapter`: per-IDE installer logic (claude-code, codex-cli, cursor, etc.) — owned by `internal/api/install.go`.
- `relay-style adapter`: adapter that persists a relay-tuple (e.g. antigravity) instead of a URL; falls back to per-daemon URL in G4.
- `route map`: per-session map keyed by exposed flat tool name → canonical `(server, daemon, port, raw_name)`. Replaces split-decode parsing.
- `participating set`: per-client union of (server, daemon) bindings across all manifests; the set whose tools the client sees via the hub.
- `5-step auth gate`: loopback-guard + path-client lookup + token-shape gate + constant-time compare + session-client invariant; runs before any business logic.
- `DACL`: Windows Discretionary Access Control List; per-file ACL entries that grant / deny per-SID access.
- `O_EXCL`: file-open flag that fails if the path already exists; defeats symlink-races during atomic-write temp creation.
- `flock`: file-locking primitive (POSIX `flock`, Windows `LockFileEx`); serializes concurrent state-file writers.
- `partialFailures`: response-meta field carrying per-daemon error rows when fan-out is degraded but not fully failed.
- `route-map collision`: case where two `(server, raw)` tuples produce the same exposed name — fails closed for that client in v2.
- `golden redaction test`: test that injects a token through every emit surface and asserts zero plain-token bytes in output.

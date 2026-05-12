# G4 — Unified Hub MCP Endpoint Design (v3)

**Status:** active design, 2026-05-12. Replaces v2 (committed in `docs/superpowers/specs/2026-05-08-g4-unified-hub-mcp-design.md`, marked DEFERRED). All 24 codex-review findings from v1-r1 (12) and v2-r2 (12) are folded in as concrete design decisions. Implementation may begin once this v3 spec passes round-3 codex review.

**Scope decision:** G4 returns to v0.3.0 scope per user direction 2026-05-12 ("Phase 3 full includes all G-phases"). Realistic implementation effort: ~10-12 days carefully + bot/codex deep-sec PR cycles. Plan-time decomposition will split into ≥4 phased PRs to keep per-PR review surfaces manageable.

## Goal

Give an MCP client (Claude Code / Codex CLI / Cursor / VS Code / etc.) **one URL** that aggregates the tools of every daemon installed for that client, instead of the current 1-URL-per-daemon configuration. Default-OFF; opt-in via Settings + restart. Tool **execution** is in scope. Prompts, resources, server-initiated notifications: out of scope (deferred to future G-phases).

## Architecture

A new HTTP listener on `127.0.0.1:<persistent-random-port>` owned by the running `mcphub gui` process. **Persistent port**: chosen once on the first start with the gate ON, written to `<state-dir>/hub-mcp.endpoint.json` (0600 + DACL-verified), reused on every subsequent start. Operators install client configs ONCE; reinstall only when they regenerate tokens or move workspaces. This addresses v2 F-S1 (random-port stale-URL window) while keeping a per-user random port rather than fixed `9120` (which any local user can pre-bind).

**Hub instance ID + token binding** closes the residual replay surface: each hub start writes a fresh 32-byte hex `instance_id` to the endpoint file. The auth gate requires `X-Mcphub-Instance-Id` to match the current value; tokens captured from a previous hub instance fail at the gate because their stale instance id doesn't match. Operator workflow: hub restart invalidates client configs → operator runs `mcphub install` to refresh.

**Per-client URL paths** use canonical adapter IDs (F-G1 fix):

```text
http://127.0.0.1:<port>/clients/claude-code/mcp
http://127.0.0.1:<port>/clients/codex-cli/mcp
http://127.0.0.1:<port>/clients/cursor/mcp
http://127.0.0.1:<port>/clients/vscode/mcp
…
```

The path segment matches `clients.SupportedClientNames()`. No aliasing or normalization — the path-token-session triple must all reference the SAME canonical id at every check.

**Tech stack:** Go (existing `internal/api` + `internal/daemon` packages). No new third-party deps. Reuses `JSONRPCRequest`/`JSONRPCResponse` from `internal/daemon/backend_lifecycle.go:15-50`, the SSE-or-JSON parser from `internal/api/health.go:687-805`, the canonical capability resolver pattern from `internal/api/health.go:140`, and existing per-daemon install planning at `internal/api/install.go:1037-1067` extended for bidirectional reconciliation.

## Pre-gate (blocking prerequisite)

**Manifest validator amendment**, two modes (F-G6 fix):

- `ValidateModeStrict` — applied in add/edit/install paths AND any new hub-binding creation. Rejects `__` substring in server names. Used by manifest mutation surfaces.
- `ValidateModeCompat` — applied in startup inventory + manifest listing + GUI manifest reads. Warns when `__` is present but does NOT reject; legacy `__`-named manifests stay readable.
- Hub bind-time gate: when `gui_server.hub_endpoint_enabled=true`, hub refuses to bind if any participating manifest fails strict validation. Operator sees a clear error message naming the offending manifest(s); fix is rename via `mcphub manifest edit`.

This separation lets v0.3.0 ship without forcing every existing `__`-using manifest to break on first startup.

## Per-hub session model

Hub is **stateful** — necessary because MCP backends require `initialize` before `tools/list`/`tools/call` (`internal/daemon/backend_lifecycle.go:195,321`), native HTTP daemons issue a distinct `Mcp-Session-Id` per `initialize` (`internal/daemon/http_host.go:371`), and the existing capability prober already follows the init→capture-sid→reuse-sid pattern (`internal/api/health.go:693-712`).

```go
type hubSession struct {
    ClientSessionID  string                                  // hub-issued UUID (Mcp-Session-Id header value)
    Client           string                                  // canonical adapter id (claude-code, codex-cli, …)
    ResolverGen      int64                                   // resolver generation captured at session create (F-S4)
    IntendedParticipants []canonicalDaemonRef                // every daemon we tried to init (F-G3)
    InitSuccesses    map[canonicalDaemonRef]string           // value = daemon Mcp-Session-Id
    InitFailures     []DaemonFailure                          // surface in tools/list result._meta.mcphub.partialFailures
    RouteMap         map[string]canonicalToolRef             // exposed-flat-name → canonical
    InFlightRequests sync.Map                                 // clientReqID → daemon ref (F-G4 cancellation routing)
    InitAt           time.Time
    LastUsedAt       time.Time
    mu               sync.Mutex                              // protects RouteMap atomic swap + LastUsedAt
}

type canonicalDaemonRef struct {
    Server string
    Daemon string
    Port   int
}

type canonicalToolRef struct {
    Server  string
    Daemon  string
    Port    int
    RawName string  // sent to daemon as params.name (F-G2)
}
```

**Lifecycle:**

1. **`initialize`**: hub assigns `client_session_id` (UUID), captures current `ResolverGen`, fans out `initialize` to every daemon in the calling client's bindings (concurrency cap from F-S5). Successful initializations populate `InitSuccesses`; failed ones populate `InitFailures` so `tools/list` can surface them (F-G3 fix). Returns synthetic `initialize` reply with hub `serverInfo`. Per-daemon init timeout 5s (F-S5).

2. **`tools/list`**: hub fans out `tools/list` to the daemons in `InitSuccesses`. Concurrency cap = `FanOutConcurrency = 8`. Merges stored `InitFailures` with new list-time failures into `result._meta.mcphub.partialFailures`. If `len(InitSuccesses) == 0` after init phase OR all list-time fan-outs fail → JSON-RPC `-32000` with `data.mcphub.partialFailures` populated.

3. **`tools/call`**: hub looks up `params.name` in the route map. Then revalidates `(Client, Server, Daemon)` against the current resolver state (F-S4 fix): if `ResolverGen` advanced AND the daemon was removed from the calling client's bindings, refuse with `-32601` "tool moved out of scope; reinitialize session". **Hub rewrites `params.name` to the canonical `RawName`** before forwarding (F-G2 fix). Records the client/daemon request-id pair in `InFlightRequests` for cancellation routing.

4. **`notifications/cancelled` (with `requestId`)**: hub looks up the client request id in `InFlightRequests`, finds the daemon ref + daemon-side request id, forwards `notifications/cancelled` to that daemon with the daemon's request id. Then removes the in-flight row.

5. **`DELETE /clients/{client}/mcp` with `Mcp-Session-Id` header** (F-G4 fix): terminate the hub session. Fan out best-effort `DELETE /mcp` to each daemon session in `InitSuccesses`. Return 204. Idempotent.

6. **Session expiry**: idle-sweeper goroutine (ticks every 60s) removes sessions older than 30min idle. Sweeper acquires session mu, checks `InFlightRequests` is empty (skip if not), removes session. Hard cap `MaxSessionsPerClient = 16`, `MaxSessionsGlobal = 256` (F-S5 fix). LRU eviction at cap; client requesting a new session at cap → 429 with `Retry-After: 30`.

## Tool-name namespacing — route map + canonical rewrite

Exposed name = `<server>__<raw_tool_name>`. Hub does NOT split on `__` (F-G4 in v1 rejected this). Instead the route map keyed by the FULL exposed name maps to `canonicalToolRef{Server, Daemon, Port, RawName}`. `tools/call` looks up `params.name` in the map (exact key match), refuses on miss with `-32601`.

`__` substring in server names is rejected at manifest-mutation time (strict mode); no collisions possible in the participating set if validation gates are honored.

Raw tool names containing `__` are handled transparently — the map key is the WHOLE exposed string, no splitting.

## Cross-client invariant — five-step auth gate

In order, before any business logic runs:

1. **Loopback-guard** via existing `rejectUnsafeLoopbackRequest` (`internal/daemon/loopback_guard.go:12-67`). Rejects non-loopback `Host`, non-loopback `Origin`, cross-site `Sec-Fetch-Site`.

2. **Path → canonical client_id**. URL pattern `/clients/{adapter-id}/mcp` only. `adapter-id` must equal one of `clients.SupportedClientNames()` (F-G1 fix). Unknown → 404 with empty body. Known but no token entry → 401 with identical empty body for every reject path (no oracle).

3. **Token shape gate**. `X-Mcphub-Hub-Token` header MUST be present and exactly 64 lowercase hex chars. Anything else → 401 (identical body).

4. **`subtle.ConstantTimeCompare`** of header token vs `tokenTable[client_id].Token`. **AND** `X-Mcphub-Instance-Id` header MUST match the current hub instance id (F-S1 closure for replay defense). Mismatch on either → 401.

5. **Session-client binding**. If `Mcp-Session-Id` header is set, look up the session — its `Client` field must equal the path `client_id`. Mismatch → 401. This prevents cross-client session-id reuse.

All five steps execute before route-map construction, so a hostile `Claude-Code-token + Codex-session-id + Codex-only-tool-name` combination is rejected at step 5 before any business logic.

## Token + endpoint state hardening

State files at `<state-dir>`:

```text
hub-mcp.lock                       # flock; serializes generate/regenerate/install
hub-mcp-tokens.json                # 0600 + DACL-verified; per-client tokens
hub-mcp.endpoint.json              # 0600 + DACL-verified; {port, instance_id, pid, started_at}
hub-mcp.log                        # 0600; JSON Lines; 10MB rotation
```

Atomic write (security F-S3 closure):

```go
flock(<state-dir>/hub-mcp.lock)
tmp := path + ".tmp"
f := os.OpenFile(tmp, O_CREATE|O_EXCL|O_WRONLY, 0600)  // O_EXCL defeats symlink pre-creation
f.Write(payload); f.Sync(); f.Close()
os.Rename(tmp, path)                                    // atomic NTFS + POSIX
funlock()
```

Load-time validation:

```go
// Open with reparse-defeat flags on Windows
f := os.OpenFile(path, O_RDONLY|<windows: FILE_FLAG_NO_REPARSE>, 0)
defer f.Close()

// Stat from the OPEN handle so a swap between stat-and-read is impossible
info, _ := f.Stat()
if info.Mode().Type() & (os.ModeSymlink|os.ModeIrregular) != 0 {
    return ErrIrregularFile
}
if runtime.GOOS != "windows" {
    sysStat := info.Sys().(*syscall.Stat_t)
    if sysStat.Uid != uint32(os.Getuid()) { return ErrWrongOwner }
    if info.Mode().Perm() & 0077 != 0 { return ErrTooLoose }
} else {
    // Windows: handle-bound DACL verify
    if err := verifyWindowsDACLFromHandle(f.Fd()); err != nil { return err }
    // Parent dir DACL verify
    if err := verifyWindowsParentDACL(filepath.Dir(path)); err != nil { return err }
}
raw, _ := io.ReadAll(f)
```

Windows DACL verification rejects:
- Owner SID ≠ current process token's user SID.
- Any ACE granting `FILE_GENERIC_READ` (via mapped generic-rights) to broad SIDs: `Everyone (S-1-1-0)`, `Authenticated Users (S-1-5-11)`, `Users (S-1-5-32-545)`, `Domain Users`, `Guests`, including inherited ACEs.
- Generic rights are mapped to specific rights BEFORE the mask check.

**Bind ordering** (F-S3 closure for "no half-initialized state"):

```text
1. validate <state-dir> sanity (existing watchdog check)
2. flock(<state-dir>/hub-mcp.lock)
3. load OR generate hub-mcp-tokens.json (with above hardening)
4. resolve all manifests; validate no '__' in server names (strict mode for participating set) → exit 8 if any fail
5. generate fresh instance_id (32-byte hex from crypto/rand)
6. listener := net.ListenConfig{Control: setSO_EXCLUSIVEADDRUSE}.Listen("tcp", "127.0.0.1:<persistent-port>")
   (persistent-port = hub-mcp.endpoint.json.port; first start = ephemeral; subsequent starts reuse)
7. write hub-mcp.endpoint.json with {port, instance_id, pid, started_at} (atomic, under same lock)
8. funlock()
9. http.Serve(listener, mux)
```

If steps 1-7 fail, the listener is never created and no requests can hit half-initialized state.

## Logging hygiene + golden test (F-S2 closure)

Single redaction helper `RedactToken(s string) string`:

```go
var hexTokenRE = regexp.MustCompile(`[0-9a-f]{64}`)
func RedactToken(s string) string {
    return hexTokenRE.ReplaceAllString(s, "<token>")
}
```

Applied at every emit site:

- `hub-mcp.log` writes
- `mcphub hub-mcp status` stdout + stderr
- `mcphub install` stdout + stderr (token-bearing args may appear in error messages)
- `mcphub hub-mcp regenerate-token` stdout + stderr
- Syscall error wrappers (path arg may contain a basename of a token-bearing file)
- argv echo in command-not-found / unknown-flag paths

Pre-write DACL check on client config target file: before writing a token to e.g., `~/.claude.json`, verify the target's owner is the current user and no inherited ACE grants read to broad SIDs. If the check fails, refuse to write the token; fall back to per-daemon URL with a clear WARN telling the operator what's wrong with the config file's DACL.

Golden test (`hub_mcp_log_redaction_test.go`):

1. Generate a fresh token via the production code path.
2. Inject through every emit surface listed above (status/install/regenerate/error-paths).
3. Capture all stdout + stderr + log-file contents.
4. Assert plain-token bytes appear nowhere; only `<token>` placeholders.

## Bidirectional install reconciler (F-G5 closure)

`ClientUpdatePlan` extended:

```go
type ClientUpdatePlan struct {
    Client     string
    Path       string
    Action     ClientUpdateAction  // AddReplace | Remove
    EntryName  string               // F-G5: differentiates "mcphub-hub" from "<server>" entries
    URL        string               // empty for Remove
    Headers    map[string]string    // F-G5: token + instance id
    DaemonName string               // legacy field; only meaningful for per-daemon entries
}

type ClientUpdateAction string

const (
    ClientUpdateAddReplace ClientUpdateAction = "add/replace"
    ClientUpdateRemove     ClientUpdateAction = "remove"
)
```

Planner logic when `gui_server.hub_endpoint_enabled=true`:

1. Compute per-client union of `(server, daemon)` bindings across ALL manifests.
2. For each client with a non-empty participating set:
   - Plan ONE `AddReplace` with `EntryName="mcphub-hub"`, URL from endpoint file, headers `{X-Mcphub-Hub-Token, X-Mcphub-Instance-Id}`.
   - Plan `Remove` for every existing per-(server, client) entry (detected via `RelayServer != "" || URL matches the per-daemon shape`).
3. For each client whose participating set becomes empty: plan `Remove EntryName="mcphub-hub"`.

Planner logic when gate is OFF:

1. Existing per-binding planner runs unchanged.
2. PLUS: plan `Remove EntryName="mcphub-hub"` for every client that previously had it (detected by `EntryName=="mcphub-hub"` in the live config).

Result: toggling gate ON/OFF is round-trippable — gate-OFF cleans up after gate-ON, no stale `mcphub-hub` entries.

Adapter capability check (run at plan time):

| Adapter | Custom-header support | Hub-mode |
|---|---|---|
| claude-code | yes (`headers` map) | yes |
| codex-cli | yes | yes |
| cursor | yes | yes |
| vscode | yes | yes |
| gemini-cli | TBD — verify at plan time | TBD |
| qwen-cli | TBD — verify at plan time | TBD |
| antigravity | no (relay-tuple only) | no — fall back to per-daemon URLs + WARN |

The two TBDs are plan-time verification tasks; if unsupported, they join antigravity in the fall-back list.

## Client-origin lifecycle methods (F-G3, F-G4 closures)

| Method | Hub behavior |
|---|---|
| `initialize` (id) | hub-owned: assign client_session_id, fan-out init under concurrency cap, capture per-daemon sids, store init failures, return synthetic `initialize` reply. |
| `notifications/initialized` (no id) | 202 to client; fan-out 202 to participating daemons. |
| `notifications/cancelled` (no id, with requestId) | look up `requestId` in session's `InFlightRequests`, forward to identified daemon with the daemon's request id, then remove the in-flight row. Reply 202. |
| other `notifications/*` (no id) | 202 to client; do NOT fan-out (per-daemon decides at its own surface). |
| `ping` (id) | hub-local echo: `{"jsonrpc":"2.0","id":<id>,"result":{}}`. |
| `tools/list` (id) | hub-owned fan-out + namespacing + partial-failure assembly (above). |
| `tools/call` (id) | route via session map + canonical rewrite + resolver-gen revalidate + in-flight tracking (above). |
| `prompts/*`, `resources/*`, `logging/setLevel`, etc. | `-32601 Method not found`. (Out of scope for MVP; honest deferral per `Out of scope` below.) |
| `DELETE /clients/{id}/mcp` (HTTP method, with Mcp-Session-Id) | terminate session, best-effort fan-out DELETE to daemons, 204. |

## Concurrency + bounds (F-G7 closure)

- `hubSessionStore` uses `sync.RWMutex`. Lookup under RLock; insert/delete under Lock.
- Per-session `mu sync.Mutex` protects route-map atomic-swap + `LastUsedAt` updates.
- Route map updates use atomic pointer swap (`atomic.Pointer[map[string]canonicalToolRef]`) so concurrent `tools/call` lookups never see a half-built map.
- `InFlightRequests` uses `sync.Map` — high-frequency read/write of small entries, no enumeration required.
- Idle sweeper: dedicated goroutine, ticks every 60s. Per session: try-lock `mu` with `tryLock` semantics (or short timeout); skip-and-retry-next-tick if held. Only remove session if `LastUsedAt > 30min ago` AND `InFlightRequests` is empty.
- Hard caps: `MaxSessionsPerClient = 16`, `MaxSessionsGlobal = 256`. New `initialize` at cap → 429 with `Retry-After: 30`.

## Partial-failure visibility (closure for original F-G6 P2)

`tools/list` response when at least one daemon succeeded:

```json
{
  "jsonrpc": "2.0",
  "id": <reqId>,
  "result": {
    "tools": [...merged list...],
    "_meta": {
      "mcphub": {
        "partialFailures": [
          {"server": "filesys", "daemon": "claude-code", "stage": "initialize", "err": "HTTP 503"},
          {"server": "search",  "daemon": "claude-code", "stage": "tools/list", "err": "read: i/o timeout"}
        ],
        "instance_id": "<current-hub-instance-id>"
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
    "data": {"mcphub": {"partialFailures": [...same shape...], "instance_id": "..."}}
  }
}
```

`stage` field is new (vs v2): values `initialize` | `tools/list` | `tools/call`. Lets operators distinguish init-time vs list-time failures.

## Settings + CLI surface

Settings registry (extends `internal/api/settings_registry.go`):

```go
{Key: "gui_server.hub_endpoint_enabled", Section: "gui_server", Type: TypeBool,
    Default: "false", Deferred: true,
    Help: "Expose a single aggregated hub URL per client instead of per-daemon URLs. Restart required. Hub instance ID rotates on every gui restart — clients must be reinstalled (`mcphub install`) after each restart to pick up the new ID."},
```

New CLI subcommand `mcphub hub-mcp`:

```text
mcphub hub-mcp status [--json]
    Show endpoint state (port, instance_id PRESENCE only — never the value, pid, started-at,
    presence per client token), redacted recent events. Tokens NEVER printed.

mcphub hub-mcp regenerate-token --client <id> [--yes]
    Rotate one client's token. Refuses non-TTY without --yes (exit 6).
    Prints re-install instruction. No grace window (stolen token rejected immediately).
```

Exit codes: 0 success, 1 backend error, 6 non-TTY without --yes, 8 state path sanity rejected.

## Threat model

| Vector | Mitigation |
|---|---|
| Pre-bind on port (HIGH r1) | Persistent random port (per-user); SO_EXCLUSIVEADDRUSE bind; hub-instance-id challenge on every request. Captured token alone is useless if instance_id rotates on restart. |
| Stale URL after restart (HIGH r2 partial) | Hub instance_id mismatch → 401. Operator workflow surfaces re-install requirement. |
| Browser CSRF / DNS-rebind / cross-site-fetch | Existing `rejectUnsafeLoopbackRequest` (loopback Host, loopback Origin, Sec-Fetch-Site). Token + instance_id required. |
| Token leak via process memory dump | Acknowledged residual risk for desktop dev threat model. |
| Token leak via logs/status/install/stderr/argv (MED r2 partial) | Single `RedactToken` helper at every emit site; golden test asserts zero plain-token bytes across surfaces. |
| Token leak via client config + backups (MED r2 partial) | Pre-write DACL check on client config target; refuse + fall back if config DACL is loose. Backup files inherit ACL from the source config. |
| Cross-client tool-call leakage | 5-step auth gate; route map built only from path-client's bindings; session-client field MUST match path client. |
| Manifest namespace injection | Two-mode `__` validation; strict mode in mutation paths; bind-time refusal if participating set contains violators. |
| Token-comparison timing oracle | `subtle.ConstantTimeCompare` after fixed 64-hex shape gate. All 401s identical body. |
| Hub becoming privileged proxy | Hub forwards JSON-RPC bodies (with `params.name` rewritten); daemons retain full authority over tool execution. Hub adds aggregation + auth gate only. |
| Token-file race / TOCTOU (MED r2 partial) | flock + O_EXCL temp + fsync + atomic rename; load uses handle-bound DACL verify; parent-dir DACL check; reject inherited broad-SID ACEs; map generic rights before mask check. |
| State-dir trust boundary | Same per-user 0600/0700 boundary as watchdog. Same POSIX sanity check (exit 8). |
| Stale route map outliving authz (MED r2 NEW) | Resolver generation counter; per-`tools/call` revalidation of `(Client, Server, Daemon)` against current resolver state. |
| Init flood DoS (MED r2 NEW) | Hard caps: per-client 16 sessions, global 256; init rate limit; fan-out concurrency 8; per-daemon init timeout 5s; LRU eviction; 429 with Retry-After. |
| `mcphub install` race (LOW r1 partial) | Per-client config write held under flock; re-read gate state + token under same lock before write. |
| Malicious CLI invocation | `regenerate-token` interactive confirm + `--yes`; status redacts; no grace window. |

## Round-3 verification (must pass before implementation)

Codex review v3 must verify:

1. All 24 r1+r2 findings have explicit closure paths in this spec (cross-reference table).
2. No NEW issues introduced by v3 mechanisms (resolver-gen, instance-id, persistent port, idle sweeper).
3. Adapter capability matrix verified for gemini-cli + qwen-cli (TBDs resolved).
4. Concurrency model self-consistent (atomic-swap route map + RWMutex sessionStore + sync.Map InFlightRequests + idle sweeper with try-lock).
5. Bind ordering invariants honored (steps 1-7 sequential, no half-initialized state).

If round-3 returns REVISE, the v4 round is bounded — only NEW findings, not re-litigation of v1/v2 issues that have explicit closure paths here.

## Files to create / modify (impl-time outline)

| File | Kind | Purpose |
|---|---|---|
| `internal/api/manifest.go` | modify | strict-vs-compat `__` validation modes (F-G6) |
| `internal/config/manifest.go` | modify | same in config loader |
| `internal/api/hub_mcp_resolver.go` | new | canonical resolver + resolver-gen counter (F-G3, F-S4) |
| `internal/api/hub_mcp_state.go` | new | atomic write + handle-bound DACL verify (F-S3) |
| `internal/api/hub_mcp_tokens.go` | new | generate/lookup/rotate + RedactToken helper (F-S2) |
| `internal/api/hub_mcp_instance.go` | new | per-start instance_id generator + endpoint state (F-S1) |
| `internal/api/hub_mcp_session.go` | new | hubSessionStore + idle sweeper + caps (F-G7, F-S5) |
| `internal/api/hub_mcp_handler.go` | new | 5-step auth gate + JSON-RPC dispatch (F-G1) |
| `internal/api/hub_mcp_aggregator.go` | new | fan-out + namespacing + partial-failure + canonical rewrite (F-G2, F-G3) |
| `internal/api/hub_mcp_log_redact.go` | new | RedactToken + golden test helpers (F-S2) |
| `internal/api/settings_registry.go` | modify | add `gui_server.hub_endpoint_enabled` |
| `internal/api/install.go` | modify | bidirectional reconciler (F-G5); pre-write DACL on client configs (F-S2) |
| `internal/gui/server.go` | modify | start hub-mcp listener after state ready; SO_EXCLUSIVEADDRUSE bind |
| `cmd/mcphub/hubmcp.go` | new | `mcphub hub-mcp status` + `regenerate-token` |
| `internal/gui/frontend/src/screens/Settings.tsx` | modify | toggle row + pending-restart badge + instance_id display |

## Test surface

**Unit + integration:**

- `manifest_test.go` (extend): strict mode rejects `__`; compat mode warns.
- `hub_mcp_resolver_test.go`: join correctness; gen counter advances on manifest mutations; per-call revalidation rejects stale route entries.
- `hub_mcp_state_test.go`: atomic write + load roundtrip; load rejects symlink, non-owner, wrong mode, wrong DACL, reparse-point parent dir.
- `hub_mcp_tokens_test.go`: generate, persist, rotate; golden redaction across log/status/install/regenerate/stderr/argv emit surfaces.
- `hub_mcp_instance_test.go`: instance_id rotates on every start; client config with stale instance_id is rejected with 401.
- `hub_mcp_session_test.go`: session create + lookup + TTL expiry + max-sessions cap; idle sweeper respects InFlightRequests.
- `hub_mcp_handler_test.go`: 5-step auth gate matrix (path-unknown, token-shape, constant-time, instance-id, session-client) — every 401 returns identical empty body.
- `hub_mcp_aggregator_test.go`: fan-out partial-failure (init-failed daemon + tools/list-failed daemon → both surface in partialFailures); canonical rewrite (params.name rewritten to RawName); resolver-gen stale-route refusal.
- `install_test.go` (extend): bidirectional reconciler — gate ON adds mcphub-hub + removes per-daemon; gate OFF removes mcphub-hub + restores per-daemon; pre-write DACL check refuses + falls back.

**Integration:**

- `hub_mcp_e2e_test.go`: spin up `mcphub gui` with gate ON; hit `/clients/claude-code/mcp` with valid + invalid tokens / wrong instance ids; observe partial-failures, cancellation forwarding, DELETE session termination.

**Playwright E2E:**

- `internal/gui/e2e/tests/hub-mcp.spec.ts`: gate OFF → no listener; gate ON after restart → 401 without token / wrong instance id; tools/list returns merged list; restart → old token rejected, fresh install works.

**Manual smoke** (added to `docs/phase-3b-ii-verification.md` D2.7): full flow with claude-code + codex-cli.

## Acceptance criteria

- Gate default-OFF; per-daemon URLs unchanged.
- Settings toggle persists; pending-restart badge.
- Strict `__` validation in mutation paths; compat warn at startup.
- Persistent random port + hub-instance-id challenge defeats stale-URL replay.
- Per-client token + instance-id required on every request; 5-step auth gate; all 401s identical body.
- Atomic state writes; handle-bound DACL verify on Windows; reject inherited broad-SID ACEs.
- Bind happens AFTER all state validated; failures don't bind.
- Route map per-session + per-call resolver-gen revalidation.
- Partial-failure visibility with `stage` field; all-failed → JSON-RPC -32000.
- Lifecycle methods complete: notifications/initialized 202, ping echo, cancellation routed via InFlightRequests, DELETE session termination 204.
- Install bidirectional reconciler: gate ON/OFF round-trippable.
- Pre-write DACL check on client config target; fall back + WARN on loose ACL.
- Golden redaction test: zero plain-token bytes in any surface.
- Hard caps + 429 with Retry-After at session caps.
- All existing per-daemon endpoints still work (gate-OFF path untouched).

## Out of scope (MVP — preserve scope discipline)

- `prompts/*`, `resources/*` (separate G-phase later).
- Server-initiated notifications hub→client (durable per-client connection state out of MVP).
- Tool allowlist UI (forward-all is the gate).
- Per-tool rate limits.
- Multi-instance daemons (>1 daemon per server×client).
- Relay-style adapter header support (antigravity stays on per-daemon URLs with WARN).
- HTTP-API surface for `mcphub hub-mcp status` (CLI-only; future GUI integration).

## Terms and Abbreviations

- `MCP`: Model Context Protocol; JSON-RPC over Streamable HTTP.
- `JSON-RPC`: text-based RPC; hub forwards request/response bodies between client and daemon, rewriting only `params.name`.
- `Mcp-Session-Id`: HTTP header MCP uses for session multiplexing. Hub issues a new value per `initialize` and stores per-daemon session ids privately.
- `Sec-Fetch-Site`: browser-emitted Fetch Metadata header used by loopback-guard.
- `DNS rebinding`: attack where a malicious DNS response points to `127.0.0.1` to bypass same-origin restrictions.
- `Loopback-guard`: existing `rejectUnsafeLoopbackRequest` middleware.
- `gate`: the `gui_server.hub_endpoint_enabled` boolean setting.
- `hub-mcp port`: per-user persistent random `127.0.0.1:<port>` listener owned by gui process.
- `per-daemon URL`: existing per-daemon `/mcp` endpoint — unchanged by G4.
- `state-dir`: per-user state directory; `%LOCALAPPDATA%\mcp-local-hub\` on Windows, `$XDG_STATE_HOME/mcp-local-hub` on POSIX.
- `client adapter`: per-IDE installer logic (claude-code, codex-cli, cursor, etc.).
- `route map`: per-session map keyed by exposed flat tool name → canonical `(server, daemon, port, raw_name)`. Hub rewrites `params.name` to `raw_name` before forwarding `tools/call`.
- `5-step auth gate`: loopback-guard + path-client lookup + token-shape gate + constant-time compare + instance-id match + session-client invariant.
- `resolver generation`: monotonic counter bumped on every manifest add/edit/uninstall; sessions capture at create; per-`tools/call` revalidation refuses entries that became stale.
- `hub instance id`: 32-byte hex generated per `mcphub gui` start; required in every request via `X-Mcphub-Instance-Id`; rotates on restart so captured tokens from old instances are useless.
- `DACL`: Windows Discretionary Access Control List; per-file ACL entries that grant/deny per-SID access.
- `handle-bound DACL verify`: query security info from an open file handle (not by path), so a swap between stat and read cannot leak access.
- `golden redaction test`: enumerates every emit surface (log/status/install/regenerate/stderr/argv/syscall errors) and asserts zero plain-token bytes.
- `partialFailures`: response-meta field carrying per-daemon error rows with a `stage` discriminator (`initialize` | `tools/list` | `tools/call`).
- `InFlightRequests`: per-session map tracking client-request-id → daemon ref for cancellation routing.
- `idle sweeper`: dedicated goroutine that removes sessions older than 30min idle, respecting in-flight invariants.

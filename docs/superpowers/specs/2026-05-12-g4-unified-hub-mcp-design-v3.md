# G4 — Unified Hub MCP Endpoint Design (v3)

**Status:** active design, 2026-05-12. Replaces v2 (committed in `docs/superpowers/specs/2026-05-08-g4-unified-hub-mcp-design.md`, marked DEFERRED). All 24 codex-review findings from v1-r1 (12) and v2-r2 (12) are folded in as concrete design decisions. Implementation may begin once this v3 spec passes round-3 codex review.

**Scope decision:** G4 returns to v0.3.0 scope per user direction 2026-05-12 ("Phase 3 full includes all G-phases"). Realistic implementation effort: ~10-12 days carefully + bot/codex deep-sec PR cycles. Plan-time decomposition will split into ≥4 phased PRs to keep per-PR review surfaces manageable.

## Goal

Give an MCP client (Claude Code / Codex CLI / Cursor / VS Code / etc.) **one URL** that aggregates the tools of every daemon installed for that client, instead of the current 1-URL-per-daemon configuration. Default-OFF; opt-in via Settings + restart. Tool **execution** is in scope. Prompts, resources, server-initiated notifications: out of scope (deferred to future G-phases).

## Architecture

A new HTTP listener on `127.0.0.1:<persistent-random-port>` owned by the running `mcphub gui` process. **Persistent port**: chosen once on the first start with the gate ON, written to `<state-dir>/hub-mcp.endpoint.json` (0600 + DACL-verified, see "Token + endpoint state hardening" below), reused on every subsequent start. Operators install client configs ONCE; reinstall only when they regenerate tokens. This addresses v2 F-S1 (random-port stale-URL window) while keeping a per-user random port rather than fixed `9120` (which any local user can pre-bind).

**Pre-bind handling is DoS, not a security boundary** (codex r3 security F-S1 closure): `SO_EXCLUSIVEADDRUSE` protects only AFTER hub successfully binds; a local attacker that pre-binds the port BEFORE hub starts blocks the bind. On `bind: address in use`, hub exits with a non-zero status and a clear "port in use" diagnostic. Operator workflow: re-run `mcphub gui --reset-port` to pick a new ephemeral port + rewrite endpoint file; re-run `mcphub install` to refresh client configs with the new port. Confidentiality is preserved (auth gate still rejects the attacker's connections — see "Cross-client invariant" below); the attack is denial of service.

**Hub instance ID — persistent across restarts** (codex r3 security F-S1 closure): instance_id is generated ONCE on first start (when the endpoint file is created), persists across hub restarts, and is rotated only by explicit operator action via `mcphub hub-mcp regenerate-instance-id`. This resolves the v2 contradiction between "install once" and "every restart needs reinstall". Auth gate still rejects requests with mismatched `X-Mcphub-Instance-Id` — but the routine restart path doesn't require operator action. Operators only re-install after explicit rotation events (token compromise → `regenerate-token`, instance compromise → `regenerate-instance-id`).

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

**Resolver state is published via atomic snapshot** (codex r3 security F-S4 closure). Instead of a `ResolverGen` int64 that can drift from the underlying binding state under concurrent mutation, the hub keeps:

```go
type ResolverSnapshot struct {
    Gen      int64
    Bindings map[string][]canonicalDaemonRef // canonical client_id → daemons
    Routes   map[string]canonicalToolRef     // exposed name → canonical (per client, namespaced)
}

// Package-level pointer swapped atomically when any manifest mutation
// completes. Sessions capture a pointer (immutable snapshot) at create
// time; tools/call revalidates against the CURRENT snapshot pointer.
var resolverSnapshot atomic.Pointer[ResolverSnapshot]
```

Each manifest add/edit/uninstall builds a fresh `ResolverSnapshot` (gen bumped, bindings + routes rebuilt) and publishes via `atomic.Pointer.Store`. Sessions hold one snapshot pointer captured at `initialize`; `tools/call` loads the CURRENT snapshot pointer, revalidates `(Client, Server, Daemon)` against it, and refuses with `-32601 "tool moved out of scope"` if stale. The snapshot is immutable after publish — no torn reads.

```go
type hubSession struct {
    ClientSessionID  string                                       // hub-issued UUID (Mcp-Session-Id header value)
    Client           string                                       // canonical adapter id (claude-code, codex-cli, …)
    SnapshotAtInit   *ResolverSnapshot                            // captured at initialize; for diff vs current
    IntendedParticipants []canonicalDaemonRef                     // every daemon we tried to init (F-G3)
    InitSuccesses    map[canonicalDaemonRef]string                // value = daemon Mcp-Session-Id
    InitFailures     []DaemonFailure                              // surface in tools/list result._meta.mcphub.partialFailures
    RouteMap         atomic.Pointer[map[string]canonicalToolRef]  // session-local route map (atomic swap)
    InFlightRequests map[json.RawMessage]inflightEntry            // typed client req id → daemon-side info (F-S6 r3)
    inflightMu       sync.Mutex                                   // protects InFlightRequests
    InitAt           time.Time
    LastUsedAt       time.Time
    mu               sync.Mutex                                   // protects LastUsedAt + lifecycle
}

type inflightEntry struct {
    DaemonRef       canonicalDaemonRef
    DaemonRequestID json.RawMessage // hub-generated request id sent to the daemon
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

`InFlightRequests` is **per-session and typed** (codex r3 security F-6 closure): keyed by the client's exact JSON-RPC id bytes (`json.RawMessage` — preserves number/string discriminator), stores `{DaemonRef, DaemonRequestID}` because the request id we sent to the daemon is hub-generated (different from the client's). A forged `notifications/cancelled` from another session cannot collide because the lookup is scoped to `session.InFlightRequests`, not a global map; cross-session interference is impossible without bypassing the 5-step auth gate.

**Lifecycle:**

1. **`initialize`**: hub assigns `client_session_id` (UUID), captures current `ResolverGen`, fans out `initialize` to every daemon in the calling client's bindings (concurrency cap from F-S5). Successful initializations populate `InitSuccesses`; failed ones populate `InitFailures` so `tools/list` can surface them (F-G3 fix). Returns synthetic `initialize` reply with hub `serverInfo`. Per-daemon init timeout 5s (F-S5).

2. **`tools/list`**: hub fans out `tools/list` to the daemons in `InitSuccesses`. Concurrency cap = `FanOutConcurrency = 8`. Merges stored `InitFailures` with new list-time failures into `result._meta.mcphub.partialFailures`. If `len(InitSuccesses) == 0` after init phase OR all list-time fan-outs fail → JSON-RPC `-32000` with `data.mcphub.partialFailures` populated.

3. **`tools/call`**: hub looks up `params.name` in the route map. Then loads the CURRENT `resolverSnapshot` via `atomic.Pointer.Load` and revalidates `(Client, Server, Daemon)` against it (F-S4 atomic closure): if the daemon is no longer in the calling client's bindings (snapshot vs session-captured snapshot diff), refuse with `-32601` "tool moved out of scope; reinitialize session". **Hub rewrites `params.name` to the canonical `RawName`** before forwarding (F-G2 fix). Hub generates a new `daemonRequestID` for the daemon-side request, records `{daemonRef, daemonSessionID, daemonRequestID, startedAt}` in `InFlightRequests` keyed by the client's exact JSON-RPC id bytes (codex r3 general F1 + security F-6 closure). On daemon response/error/timeout, the in-flight row is removed; the per-call wall-clock cap (60s) guarantees cleanup even when the daemon hangs.

4. **`notifications/cancelled` (with `requestId`)**: hub looks up the client request id in `InFlightRequests` (per-session map; auth gate + session-client check have already run), finds the daemon ref + daemon-side request id, and forwards `notifications/cancelled` to that daemon with the daemon's request id. Then removes the in-flight row. **Stdio daemon caveat** (codex r3 general F1 sub-issue): the existing `internal/daemon/host.go:848` forwards no-id notifications unchanged — it does NOT remap inbound `requestId` for stdio backends, so hub cancellations through StdioHost are best-effort (daemon ignores unmatched ids without harm). HTTP-host daemons cancel correctly because their request-id space is hub-controlled. This caveat is documented as a known limitation; future task can extend StdioHost with a cancellation-id remap.

5. **`DELETE /clients/{client}/mcp` with `Mcp-Session-Id` header** (F-G4 fix): terminate the hub session. Fan out best-effort `DELETE /mcp` to each daemon session in `InitSuccesses`. Return 204. Idempotent.

6. **Session expiry**: idle-sweeper goroutine (ticks every 60s) removes sessions older than 30min idle. Sweeper holds session `mu`, atomically reads `len(InFlightRequests)` under `inflightMu` (skip session if not empty), then removes from `hubSessionStore`. Each session also keeps an `inFlightCount atomic.Int32` for fast "is there in-flight work" checks without taking the inflight mutex on every sweep tick. Hard cap `MaxSessionsPerClient = 16`, `MaxSessionsGlobal = 256` (F-S5). LRU eviction at cap; new `initialize` at cap → 429 with `Retry-After: 30` (codex r3 general F-G7 closure: explicit emptiness via atomic counter, not implicit).

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
// Open with reparse-defeat flags on Windows. The Go stdlib doesn't
// export FILE_FLAG_OPEN_REPARSE_POINT directly; the canonical path
// is golang.org/x/sys/windows constants + a build-tagged custom
// opener (windows: hub_mcp_state_windows.go; posix: hub_mcp_state_posix.go).
f := openStateFileNoReparse(path)
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
    // Parent dir DACL verify (per-handle of the parent dir)
    if err := verifyWindowsParentDACL(filepath.Dir(path)); err != nil { return err }
}
raw, _ := io.ReadAll(f)
```

Windows DACL verification is **allowlist-based** (codex r3 security F-S3 closure replaces the v3 draft's blacklist):

- Owner SID MUST equal current process token's user SID.
- DACL is canonically evaluated: process the ordered ACE list, applying ACE flags (INHERITED_ACE / OBJECT_INHERIT_ACE / NO_PROPAGATE_INHERIT_ACE), respecting ALLOW-vs-DENY ordering per Microsoft's documented DACL evaluation algorithm. Use `golang.org/x/sys/windows.GetSecurityInfo` to read the DACL and `golang.org/x/sys/windows.GetAce` to iterate.
- After generic-right mapping (`GENERIC_READ` → `FILE_GENERIC_READ` etc.), the set of SIDs allowed to read MUST be a subset of `{current-user-SID, LocalSystem (S-1-5-18), BuiltinAdministrators (S-1-5-32-544)}`. Any read-capable ALLOW ACE to a SID outside this allowlist → reject.
- DENY ACEs do NOT "rescue" an unsafe ALLOW unless the canonical evaluation proves no effective-access path through to the bad SID. Conservative: reject anyway if an unsafe ALLOW exists, regardless of DENY siblings.
- Inherited ACEs are validated against the same allowlist (no exemption for `INHERITED_ACE`).

**Handle-bound client config writer** (codex r3 security F-S3 closure for client configs): the existing `internal/clients/*.go` adapters use path-based `os.WriteFile` and `os.OpenFile` which are TOCTOU-vulnerable for token-bearing writes. A new helper `SecureWriteClientConfig(path, contents []byte)` opens the parent dir, creates a temp file with `O_CREATE|O_EXCL|O_WRONLY|0600` + handle-bound DACL verify, writes bytes, fsyncs, atomic-renames into place, then re-opens the destination and re-verifies handle DACL before declaring success. Failure on any check refuses the write (hard-fail when installing hub-mode tokens; fall back + WARN already documented for adapters without header support). Same helper used for backups in `BackupKeep`.

**Bind ordering** (F-S3 closure for "no half-initialized state", patched per codex r3 general F3):

```text
1. validate <state-dir> sanity (existing watchdog check)
2. flock(<state-dir>/hub-mcp.lock)
3. load OR generate hub-mcp-tokens.json (with above hardening)
4. load existing hub-mcp.endpoint.json if present → extract persistent port + instance_id;
   if absent (first start with gate ON), allocate port=0 (ephemeral) for step 6 and generate fresh instance_id.
   On parse/DACL failure: refuse to proceed (don't silently regenerate — operator must investigate first).
5. resolve all manifests; validate no '__' in server names (strict mode for participating set) → exit 8 if any fail
6. listener := newListenerWithSOExclusive("127.0.0.1:<port>")
   where newListenerWithSOExclusive is build-tagged:
   - windows: net.ListenConfig{Control: func(_, _ string, c syscall.RawConn) error {
         return c.Control(func(fd uintptr) {
             windows.SetsockoptInt(windows.Handle(fd), windows.SOL_SOCKET, windows.SO_EXCLUSIVEADDRUSE, 1)
         })
     }}.Listen
   - posix: net.ListenConfig{}.Listen (no analogue; loopback bind alone is sufficient on POSIX)
   Note: golang.org/x/sys/windows exposes SO_EXCLUSIVEADDRUSE (not in syscall stdlib).
   If port was 0 (first start), retrieve the assigned port via listener.Addr().(*net.TCPAddr).Port for step 7.
7. write hub-mcp.endpoint.json {port, instance_id, pid, started_at} (atomic, under same lock).
   If write fails after listener exists → defer listener.Close() in error path. The contract is
   "no listener accepts traffic until step 7 succeeds" — not "no listener exists".
8. funlock()
9. http.Serve(listener, mux)
```

If steps 1-5 fail, the listener was never created and no resource leaked. If step 6 fails (port in use → pre-bind DoS), exit cleanly with the "port in use" diagnostic. If step 7 fails after the listener exists, defer-close the listener so no connections accept traffic without a published endpoint file.

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

**Single-server install paths MUST NOT remove the `mcphub-hub` aggregate entry** (codex r3 general F2 closure). `mcphub install --server X` is a per-manifest path — it cannot determine whether OTHER manifests still need the hub entry. Removing `mcphub-hub` from a single-server install would deconfigure unrelated servers. Therefore:

- Gate-ON / Gate-OFF transitions go through a **dedicated full-reconcile pass** (`mcphub install --reconcile-hub-mode` or implicit on every gui startup): walks ALL manifests, computes the per-client participating set, and emits the aggregate add/remove plan AS A WHOLE.
- Per-manifest `mcphub install --server X` paths NEVER emit `Remove EntryName="mcphub-hub"`. They only emit the per-(server, client) entries the manifest demands.
- On gate-OFF, the gui startup full-reconcile (or explicit `--reconcile-hub-mode`) removes `mcphub-hub` for every client AND restores per-daemon entries for every manifest that was hub-routed.
- On gate-ON, the full-reconcile adds `mcphub-hub` for every client with at least one manifest needing it AND removes per-daemon entries that the hub now serves.

Planner logic when `gui_server.hub_endpoint_enabled=true` AND full-reconcile pass:

1. Compute per-client union of `(server, daemon)` bindings across ALL manifests.
2. For each client with a non-empty participating set:
   - Plan ONE `AddReplace` with `EntryName="mcphub-hub"`, URL from endpoint file, headers `{X-Mcphub-Hub-Token, X-Mcphub-Instance-Id}`.
   - Plan `Remove` for every existing per-(server, client) entry (detected via `RelayServer != "" || URL matches the per-daemon shape`).
3. For each client whose participating set becomes empty: plan `Remove EntryName="mcphub-hub"`.

Planner logic when `gui_server.hub_endpoint_enabled=true` AND single-server `mcphub install --server X`:

1. Existing per-binding planner runs unchanged for server X.
2. Skip `Remove EntryName="mcphub-hub"` (NOT a single-server concern; full-reconcile owns that).

Planner logic on gate-OFF full-reconcile pass:

1. Walk ALL manifests; emit per-binding plan for each (existing planner).
2. PLUS: plan `Remove EntryName="mcphub-hub"` for every client that previously had it (detected by `EntryName=="mcphub-hub"` in the live config).

Result: toggling gate ON/OFF via the full-reconcile pass is round-trippable. Single-server install paths are scope-coherent — they don't damage unrelated server entries.

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

## Concurrency + bounds (F-G7 closure, refined per codex r3 general F-G7 + security F-S4)

- `hubSessionStore` uses `sync.RWMutex`. Lookup under RLock; insert/delete under Lock.
- Per-session `mu sync.Mutex` protects `LastUsedAt` updates + lifecycle transitions.
- Route map updates use atomic pointer swap (`atomic.Pointer[map[string]canonicalToolRef]`) so concurrent `tools/call` lookups never see a half-built map.
- `InFlightRequests` is a regular `map[json.RawMessage]inflightEntry` protected by `inflightMu sync.Mutex`. Sessions also hold `inFlightCount atomic.Int32`, incremented before storing + decremented on cleanup. The idle sweeper checks `inFlightCount.Load() == 0` cheaply without taking `inflightMu`; if non-zero, skip-this-tick.
- Idle sweeper: dedicated goroutine, ticks every 60s. Per session: takes `mu` (lifecycle lock), checks `LastUsedAt > 30min ago` AND `inFlightCount.Load() == 0`; only then removes from `hubSessionStore`. Per-call cleanup invariants: response/error/timeout/cancel all decrement `inFlightCount`.
- **Resolver snapshot** is a package-level `atomic.Pointer[ResolverSnapshot]`. Mutations build a fresh snapshot off-line (gen bumped, bindings + routes rebuilt) and publish via `Store`. The pointer swap is atomic — readers either see the OLD snapshot or the NEW snapshot, never a torn read. Sessions capture the pointer at `initialize`; `tools/call` loads the CURRENT pointer and compares against the session's captured pointer to detect "binding was removed since session init" (codex r3 security F-S4 closure).
- Hard caps: `MaxSessionsPerClient = 16`, `MaxSessionsGlobal = 256`. New `initialize` at cap → 429 with `Retry-After: 30`.

**Token-table reload on rotation** (codex r3 security F1 HIGH closure): `mcphub hub-mcp regenerate-token --client X` is no longer a "rotate file, operator restarts hub" path. The CLI:

1. Acquires `<state-dir>/hub-mcp.lock`.
2. Reads + updates `hub-mcp-tokens.json` (atomic write).
3. Sends a SIGHUP-equivalent reload signal to the running hub process via a hub-internal control channel: `POST http://127.0.0.1:<port>/internal/reload-tokens` on the loopback listener, authenticated by an additional control token rotated per hub start (stored under `<state-dir>/hub-mcp-control.token`, 0600 + DACL-verified, never sent to clients).
4. Hub re-reads `hub-mcp-tokens.json` atomically (load → swap `tokenTable` under RWMutex Lock), responds 200.
5. CLI returns success only after the hub confirms the swap. Old tokens become unaccepted within milliseconds; no restart required.
6. Failure path: if the control endpoint is unreachable (hub stopped, crashed, etc.), the CLI returns with exit 1 + a message "rotate persisted to disk but live hub did not confirm; restart hub to apply or investigate". Worst case = old token still valid until next hub start, with operator surfaced clearly.

An e2e regression test (`hub_mcp_rotate_live_test.go`) asserts: after `regenerate-token`, a request bearing the OLD token returns 401 within 500ms — without restarting the hub.

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

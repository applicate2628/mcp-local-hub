# G4 — Unified Hub MCP Endpoint Design (v3)

**Status:** active design, 2026-05-12 (v3.2 amendment). Replaces v2 (committed in `docs/superpowers/specs/2026-05-08-g4-unified-hub-mcp-design.md`, marked DEFERRED). All 24 codex-review findings from v1-r1 (12) and v2-r2 (12) plus all 9 r3 findings (3 general + 6 security) are folded in. v3.2 layer addresses codex r4 follow-ups (F1 instance_id persistence scrub, F2 crash-safe reconcile ordering, F4 SO_EXCLUSIVEADDRUSE local constant + error capture, F5 `/internal/reload-tokens` contract, F6 SecureWriteClientConfig handle-relative semantics, F7 DACL enterprise stance, plus the F6-r3 requestIDKey wrapper for the non-comparable JSON-RPC id map key). Implementation may begin once this spec passes round-5 codex review.

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
    InFlightRequests map[requestIDKey]inflightEntry               // typed client req id → daemon-side info (F6-r3 closure)
    inflightMu       sync.Mutex                                   // protects InFlightRequests
    InitAt           time.Time
    LastUsedAt       time.Time
    mu               sync.Mutex                                   // protects LastUsedAt + lifecycle
}

// requestIDKey is a comparable wrapper for JSON-RPC request ids. The
// codex r4 review correctly flagged that `json.RawMessage` ([]byte)
// cannot be a Go map key (not comparable). requestIDKey is the
// validated, **losslessly canonicalized** form (codex r5 P1 closure
// — the v3.2 float64 path was rejected because float64 loses
// precision for integers > 2^53, collapsing distinct ids onto the
// same map key):
//   - JSON strings: `s:<unescaped-string>` (json.Unmarshal into string
//     then re-prefix; UTF-8 unmarshal handles escape sequences).
//   - JSON numbers: `n:<canonical-decimal-string>` — DO NOT go through
//     float64. Use `json.Decoder.UseNumber()` so the value is captured
//     as `json.Number` (its raw decimal string), then canonicalize via
//     pure string manipulation that preserves every digit:
//       1) reject leading `+` (json grammar forbids it);
//       2) strip leading zeros from the integer part (but keep a single
//          zero before the decimal point if the integer part is empty);
//       3) if there's an exponent, normalize the exponent sign and strip
//          leading zeros from the exponent;
//       4) if there's a fractional part, strip trailing zeros after the
//          decimal point; if the resulting fractional part is empty, drop
//          the decimal point entirely;
//       5) integers `1`, `1.0`, `1.00`, `1e0`, `1E+0` collapse to `n:1`;
//       6) `1.5`, `1.50`, `1.5e0` collapse to `n:1.5`;
//       7) very large integers like `9007199254740993` (= 2^53 + 1,
//          unrepresentable in float64) stay distinct from
//          `9007199254740992` — the canonical string preserves all
//          significant digits.
//     `math/big.Rat` from `(*big.Rat).SetString` is one off-the-shelf
//     option that yields a normalized rational form, but the string
//     manipulation above is sufficient and avoids the big-int allocation
//     on every request.
//   - JSON null: `null` is **accepted** (codex r5 P2 closure: JSON-RPC
//     2.0 permits string | number | null for id; rejecting null would
//     be non-compliant). Canonical form: `z:` (the empty-suffix
//     discriminator distinguishes it from an empty string `s:` and
//     from a numeric zero `n:0`). Cancellation by null-id is undefined
//     in MCP today; we accept the request but log a warning. If
//     multiple concurrent requests use null-id within one session,
//     the in-flight map collapses them; cancellation of "the null-id
//     request" cancels the most-recently-inserted entry (last-writer-
//     wins). Clients are responsible for using distinct ids if they
//     want individual cancellation.
//   - Arrays, objects, and booleans: rejected at parse time with -32600
//     "Invalid Request" (JSON-RPC grammar does not permit these).
type requestIDKey string

// newRequestIDKey validates + losslessly normalizes a raw JSON-RPC id
// field. Returns ("", err) on non-conforming input; caller surfaces
// -32600 for arrays/objects/booleans. Null ids return ("z:", nil).
// Fractional numeric IDs are accepted; their canonical decimal-string
// form disambiguates them from integer-equivalent strings and never
// loses precision.
func newRequestIDKey(raw json.RawMessage) (requestIDKey, error) { /* impl */ }

type inflightEntry struct {
    DaemonRef       canonicalDaemonRef
    DaemonSessionID string          // daemon-side Mcp-Session-Id
    DaemonRequestID json.RawMessage // hub-generated request id sent to the daemon (raw JSON form for re-emit)
    StartedAt       time.Time
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

`InFlightRequests` is **per-session and typed** (codex r3 F-S6 + r4 F6 closures): keyed by `requestIDKey` — a normalized comparable string built from the validated raw JSON-RPC id (string-vs-number discriminator preserved via the `s:` / `n:` prefix; numeric-equivalent forms like `1` / `1.0` / `1e0` collapse to one canonical bucket). Stores `{DaemonRef, DaemonSessionID, DaemonRequestID, StartedAt}` because the request id we send to the daemon is hub-generated (different from the client's). A forged `notifications/cancelled` from another session cannot collide because the lookup is scoped to `session.InFlightRequests` (per-session map), not a global structure; cross-session interference is impossible without bypassing the 5-step auth gate.

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

**Enterprise stance — Group Policy / MDM-managed ACLs** (codex r4 F7 closure): the allowlist (`current-user-SID, LocalSystem, BuiltinAdministrators`) is **intentionally narrow** and may reject configurations that an enterprise IT admin pushes via Group Policy, MDM, or AppLocker "managed application" templates that add inherited read ACEs for `Domain Users`, `Domain Admins`, a corporate management SID, or a backup-service SID. This is the correct behavior — `hub-mcp-tokens.json` carries bearer tokens for the local user's MCP clients, and exposing that read surface to "everyone in the domain" or "the IT support group" widens the trust boundary well beyond the desktop-dev threat model G4 assumes. The expected operator workflow when the allowlist rejects:

1. The hub refuses to start with a CLEAR diagnostic: `state-dir DACL not single-user-safe: ALLOW for <SID> (<resolved-name>) exceeds allowlist {current-user, LocalSystem, BuiltinAdministrators}. Either remove the policy ACL or run the hub from a profile location outside policy scope.`
2. Operator can either:
   a. Move `<state-dir>` outside the policy-managed path (e.g., to a per-user folder NOT covered by the inheriting OU/GPO scope). Set `MCPHUB_STATE_DIR` env var if the test-tag build is in use, OR (production) place the profile on a non-managed volume.
   b. Ask the IT admin to ADD the per-user `<state-dir>` (or its grandparent if `%LOCALAPPDATA%`) to the GPO's ACL exception list.
   c. Accept the loss and disable G4 hub-endpoint mode (`gui_server.hub_endpoint_enabled=false`); fall back to per-daemon URLs which write to their respective adapter-managed locations (different DACL constraints).
3. There is NO "trust this SID" config flag in v0.3.0. Adding one would be the natural escape hatch but every SID added widens the trust boundary; we keep the rejection hard until enterprise pilots tell us a specific shared-context scenario justifies the additional surface. Document this as a known enterprise constraint in `docs/phase-3b-ii-verification.md` D2.7 + `docs/operators-enterprise-notes.md` (new, light).
4. Domain-joined machines where the operator IS the IT admin and Group Policy CAN'T be edited (rare but real): document the explicit move-to-non-managed-profile recovery path. The hub is a desktop-dev tool; "policy ACL hands my token to the help-desk group" is the FAR more common scenario than "I trust the help-desk group to read my tokens".

**Tests** (`hub_mcp_state_dacl_test.go`): synthesize a state-dir with each of the above scenarios (vanilla single-user, allowlist-conforming, domain-user-add ACE present, Everyone-deny + Everyone-allow combo, deep INHERITED_ACE chain, etc.) and assert reject vs accept matches the allowlist semantics. Helper builds DACLs via `golang.org/x/sys/windows.ACL` + `BuildExplicitAccessWithName`.

**Handle-bound client config writer** (codex r3 security F-S3 closure for client configs; F6-r4 follow-up detail): the existing `internal/clients/*.go` adapters use path-based `os.WriteFile` and `os.OpenFile` which are TOCTOU-vulnerable for token-bearing writes. A new helper `SecureWriteClientConfig(path, contents []byte)` operates handle-relative throughout.

### SecureWriteClientConfig sequence (codex r4 F6 closure — TOCTOU-safe)

```text
1. parentDir, base := filepath.Split(path)
2. dirHandle = open(parentDir, O_DIRECTORY|O_RDONLY)        // POSIX
   dirHandle = CreateFile(parentDir, FILE_LIST_DIRECTORY,
                          FILE_SHARE_READ|FILE_SHARE_WRITE,
                          security=current-user-only,
                          OPEN_EXISTING,
                          FILE_FLAG_BACKUP_SEMANTICS|FILE_FLAG_OPEN_REPARSE_POINT,
                          0)                                  // Windows
   - REPARSE_POINT flag rejects symlinks/junctions in the parent path
   - defer dirHandle.Close()
3. verify dirHandle DACL (handle-bound): owner == current-user,
   only {current-user, LocalSystem, BuiltinAdministrators} allowlist
   (same canonical-ACE evaluation as the state-dir DACL verify above).
   On failure: reject with "<parent-dir>: DACL not single-user safe".
4. tempName := fmt.Sprintf(".%s.tmp.%d.%x", base, os.Getpid(),
                            crypto/rand 8 bytes)
   - Why crypto/rand: defeats predictable-name races on Windows where
     a competing process could pre-create the tempName under a
     DACL the attacker controls.
5. tempHandle = openat(dirHandle, tempName,
                       O_CREATE|O_EXCL|O_WRONLY|O_NOFOLLOW, 0600)
   - openat(2) (POSIX) / CreateFileEx-relative-to-dir-handle (Win32
     via NtCreateFile + RootDirectory) — guarantees the open resolves
     RELATIVE TO THE OPEN PARENT DIR HANDLE, not a re-walked path.
     Defeats "swap parent dir between step 2 and step 5" races.
   - O_EXCL: refuses if the tempName already exists.
   - O_NOFOLLOW: refuses if tempName is a pre-existing symlink.
   - On Windows there's no openat; use the same x/sys/windows trick
     as the watchdog state writer: NtCreateFile with RootDirectory set
     to the dirHandle, ObjectAttributes path = base only. Same TOCTOU
     guarantee as POSIX openat.
   - On failure: surface to caller, defer dirHandle.Close().
6. set DACL on tempHandle BEFORE writing bytes (Windows only):
   SetSecurityInfo(tempHandle, DACL_SECURITY_INFORMATION,
                    nil, nil, restrictiveDACL, nil)
   where restrictiveDACL = {Allow current-user GENERIC_ALL,
                             Allow LocalSystem GENERIC_ALL,
                             Allow BuiltinAdministrators GENERIC_ALL}.
   - Done by HANDLE not path, so swap-between-create-and-DACL is impossible.
   - On POSIX the O_CREATE used mode 0600; further fchmod is a no-op
     unless umask interfered, in which case fchmod(tempHandle, 0600)
     fixes it.
7. write(tempHandle, contents)
8. fsync(tempHandle)            // POSIX: durable to disk
   FlushFileBuffers(tempHandle) // Windows: equivalent durability
9. **DO NOT close tempHandle yet** (codex r5 MED — Windows rename
   semantics need the open file handle). On POSIX the renameat call
   takes a directory handle + name string and does not need the open
   file handle. On Windows `NtSetInformationFile` with
   `FileRenameInformationEx` is invoked ON the file being renamed
   (its handle), with `RootDirectory` specifying the destination
   parent dir handle and `FileName` specifying the destination
   basename. The file handle must remain open across the rename call.
10. POSIX: renameat(dirHandle, tempName, dirHandle, base)
    Windows: NtSetInformationFile(
                 tempHandle,                 // <-- the file being renamed
                 FileRenameInformationEx,
                 {ReplaceIfExists: true,
                  RootDirectory: dirHandle,  // <-- target parent dir handle
                  FileName: base,
                  Flags: REPLACE_IF_EXISTS | POSIX_SEMANTICS})
    - Atomic rename relative to dirHandle (root-directory-relative).
      Symlink/junction in the destination basename is impossible
      because the rename does not re-walk a string path from root.
    - POSIX_SEMANTICS flag on Windows 10+ makes the rename atomic
      (replaces unconditionally; no legacy "in-use" lock; concurrent
      handles to the old path become invalidated cleanly).
    - REPLACE_IF_EXISTS allows overwriting an existing destination
      (the client config's prior content).
11. tempHandle.Close()                                         // now safe to release
12. verifyHandle = openat(dirHandle, base, O_RDONLY|O_NOFOLLOW)
    - Re-open the destination via the SAME dirHandle to re-verify DACL.
      Path-based re-open here would be TOCTOU; the dirHandle anchor
      eliminates the window.
    - Sanity check: stat verifyHandle, assert (inode, size) match expectations.
13. verify verifyHandle DACL (handle-bound, same allowlist as step 3).
    - If the rename succeeded but the on-disk DACL is wrong (some
      Windows policy ACLs auto-apply on certain paths — see DACL
      enterprise stance below), surface a HARD ERROR; the caller
      MUST refuse to write the token and fall back to per-daemon URLs.
14. verifyHandle.Close(); dirHandle.Close()
```

**Why every step uses dirHandle-relative ops:**

- The classic TOCTOU is "path-based open / set-DACL / rename / re-open" — each path resolution is a fresh walk from root, giving an attacker with write to any ancestor a swap window.
- Anchoring every op to the parent-dir handle freezes the final-component ancestor resolution at step 2. Once dirHandle is held, the open/rename/re-open ops resolve their `base` name relative to that handle, not via a fresh walk from root. **The dirHandle freezes the FINAL ancestor (the immediate parent), not the entire ancestor chain** (codex r5 MED clarification — `FILE_FLAG_OPEN_REPARSE_POINT` only refuses to open the final reparse point we opened, it doesn't prove every intermediate parent component is symlink-free). The full ancestor-chain safety comes from the state-dir trust boundary: the per-user `<state-dir>` is 0600/0700 (POSIX) or single-user DACL (Windows), so an attacker without write to the user profile cannot swap an ancestor. Adapters that write OUTSIDE `<state-dir>` (e.g., client config files in `~/.claude.json`) inherit the same per-user trust boundary; if a different attacker holds write on those ancestors, they could already corrupt the client config independent of the hub.
- On Windows, `NtCreateFile`/`NtSetInformationFile` with `RootDirectory=dirHandle` give the same semantic; the wrapper in `internal/api/secure_write_windows.go` exposes `openatLikeWindows(dirHandle, name, flags) (windows.Handle, error)` and `renameWithRootDir(fileHandle, dirHandle, newBase) error`. Note the rename wrapper takes the FILE handle (the temp file being renamed) plus a SEPARATE target dir handle (`dirHandle`) — not two dir handles. The Windows API signature is `NtSetInformationFile(fileHandle, FileRenameInformationEx, { RootDirectory: dirHandle, FileName: newBase, ... })`.
- The `crypto/rand` tempName + O_EXCL + DACL-set-before-write sequence prevents an attacker with write-to-parent from racing into the temp slot.
- The final handle-bound re-verify is the actual safety net: even if some unanticipated policy ACL got applied between rename and re-open, we'd catch it at step 13 and refuse the write rather than leave a token in a file with leaked-read access.

Failure on any check refuses the write (hard-fail when installing hub-mode tokens; fall back + WARN already documented for adapters without header support). Same helper used for backups in `BackupKeep`.

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
   - windows (internal/api/hub_mcp_listener_windows.go): defines a local
     constant — `golang.org/x/sys/windows` does NOT export
     SO_EXCLUSIVEADDRUSE (codex r4 F4 closure; verified against
     x/sys@v0.26.0/windows/types_windows.go which only defines
     SO_REUSEADDR = 4). The Windows ws2def.h value is
     `((u_int)(~SO_REUSEADDR))` = -5. We use the bitwise-NOT form so
     intent reads at-a-glance and stays robust if x/sys ever updates
     SO_REUSEADDR:
       ```go
       // soExclusiveAddrUse is the Windows-specific SO_EXCLUSIVEADDRUSE
       // socket option. Defined locally because x/sys/windows does not
       // export it. Value matches ws2def.h's `((u_int)(~SO_REUSEADDR))`.
       const soExclusiveAddrUse = ^windows.SO_REUSEADDR // = -5 on Windows
       ```
     The listener factory is:
       ```go
       func newListenerWithSOExclusive(addr string) (net.Listener, error) {
           lc := net.ListenConfig{
               Control: func(_, _ string, c syscall.RawConn) error {
                   var setErr error
                   ctlErr := c.Control(func(fd uintptr) {
                       setErr = windows.SetsockoptInt(
                           windows.Handle(fd),
                           windows.SOL_SOCKET,
                           soExclusiveAddrUse,
                           1,
                       )
                   })
                   if ctlErr != nil {
                       return ctlErr
                   }
                   return setErr // F4: surface SetsockoptInt error to caller
               },
           }
           return lc.Listen(context.Background(), "tcp", addr)
       }
       ```
     The `setErr` capture is mandatory (codex r4 F4 followup): a silently-dropped Setsockopt error means the listener still binds with default `SO_REUSEADDR` semantics, defeating the pre-bind safeguard.
   - posix (internal/api/hub_mcp_listener_posix.go): plain
     `net.ListenConfig{}.Listen` — no analogue; loopback bind alone is
     sufficient on POSIX (the threat model treats pre-bind as DoS only).
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

### Crash-safe reconcile ordering (codex r4 F2 closure)

The full-reconcile pass executes its plans against each client config file in this fixed order:

1. **AddReplace entries are applied FIRST** (in any order — within one client, all AddReplace ops happen before any Remove op).
2. **Remove entries are applied LAST** (after every AddReplace succeeds for that client).
3. Each per-client mutation is one atomic config rewrite (`SecureWriteClientConfig`, see below) — partial-write torn states do not occur within a single client config.
4. Per-client mutations are independent: if one client write fails, the reconcile continues with the next client and surfaces the failure as a partial result; the operator reruns to converge.

**Why add-before-remove:** if `mcphub gui` crashes (or `mcphub install` is force-killed) BETWEEN clients or DURING a single client's mutation, the worst-case observable state is "two competing entries point to the same server" (one per-daemon + one `mcphub-hub`), NOT "no working entry". MCP clients tolerate duplicate entries — they may connect to one or both — but they do NOT tolerate a missing entry. The next reconcile pass converges to the intended state.

**Gate-ON transition** (per-daemon → hub):

1. For every client X with a non-empty participating set:
   1. AddReplace `EntryName="mcphub-hub"` first.
   2. THEN Remove every per-(server, X) entry the hub now serves.
2. For every client X with an EMPTY participating set:
   - Remove `EntryName="mcphub-hub"` (no AddReplace needed; there is no replacement entry).

**Gate-OFF transition** (hub → per-daemon):

1. For every client X with any previously-hub-routed manifest:
   1. AddReplace each per-(server, X) entry FIRST (every server that used to be hub-routed).
   2. THEN Remove `EntryName="mcphub-hub"`.

**Per-client write atomicity:** every `client-config.json` rewrite uses `SecureWriteClientConfig` (open parent dir → create handle-bound temp file with O_EXCL + DACL → write expanded contents → fsync → atomic rename → re-open final to verify DACL). The on-disk file is either the old contents or the fully-new contents — never a torn intermediate. See "Client config write hardening" below for the full TOCTOU-safe sequence (F-S2 / F6-r3 closure).

**Recovery on partial crash:** the next gui startup runs the full-reconcile pass implicitly (or operator explicitly via `mcphub install --reconcile-hub-mode`). The reconcile is **idempotent**: AddReplace is a no-op if the entry already matches; Remove is a no-op if the entry is absent. The "two competing entries" state from a partial crash converges to the intended state on the next pass — no operator intervention required, no data loss.

**Bind-failure rollback (gate-ON):** if the gui process binds the hub listener AFTER writing client configs and the bind fails (`address in use`), clients now have an `mcphub-hub` entry pointing to a non-listening port. To avoid this, the bind is sequenced BEFORE the reconcile (see "Bind ordering" — steps 1-7 below): full-reconcile only runs after the hub listener is up. If the bind fails, the reconcile is SKIPPED, the operator sees the bind error, and existing client configs (still per-daemon) keep working — gate-ON is effectively a no-op until the bind succeeds.

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
- `InFlightRequests` is a regular `map[requestIDKey]inflightEntry` protected by `inflightMu sync.Mutex` (codex r5 P1 closure — the v3.2 spec mistakenly carried over the old `map[json.RawMessage]inflightEntry` shape in this section; `json.RawMessage` is `[]byte` and not a valid Go map key, so an implementation following the unfixed version would not compile. The `requestIDKey` wrapper defined earlier — losslessly canonicalized comparable string form — is the correct key type for both insert and lookup). Sessions also hold `inFlightCount atomic.Int32`, incremented before storing + decremented on cleanup. The idle sweeper checks `inFlightCount.Load() == 0` cheaply without taking `inflightMu`; if non-zero, skip-this-tick.
- Idle sweeper: dedicated goroutine, ticks every 60s. Per session: takes `mu` (lifecycle lock), checks `LastUsedAt > 30min ago` AND `inFlightCount.Load() == 0`; only then removes from `hubSessionStore`. Per-call cleanup invariants: response/error/timeout/cancel all decrement `inFlightCount`.
- **Resolver snapshot** is a package-level `atomic.Pointer[ResolverSnapshot]`. Mutations build a fresh snapshot off-line (gen bumped, bindings + routes rebuilt) and publish via `Store`. The pointer swap is atomic — readers either see the OLD snapshot or the NEW snapshot, never a torn read. Sessions capture the pointer at `initialize`; `tools/call` loads the CURRENT pointer and compares against the session's captured pointer to detect "binding was removed since session init" (codex r3 security F-S4 closure).
- Hard caps: `MaxSessionsPerClient = 16`, `MaxSessionsGlobal = 256`. New `initialize` at cap → 429 with `Retry-After: 30`.

**Token-table reload on rotation** (codex r3 security F1 HIGH closure; CLI lock-scope tightened per codex r5 MED): `mcphub hub-mcp regenerate-token --client X` is no longer a "rotate file, operator restarts hub" path. The CLI:

1. Acquires `<state-dir>/hub-mcp.lock` (flock). **The CLI holds this lock continuously through steps 2-5; it MUST NOT release it before the POST response returns** (codex r5 MED — otherwise the control-token snapshot the CLI reads in step 3 could become stale between read and POST if the hub restarted under a parallel operator run, and a 2nd `regenerate-token` could interleave token-table updates).
2. Reads + updates `hub-mcp-tokens.json` (atomic write under flock).
3. Reads `<state-dir>/hub-mcp-control.token` (under the same flock).
4. Sends a SIGHUP-equivalent reload signal to the running hub process via a hub-internal control channel: `POST http://127.0.0.1:<port>/internal/reload-tokens` on the loopback listener, authenticated by the control token from step 3 (see "Control endpoint contract" below). **The hub-side handler MUST NOT acquire `hub-mcp.lock`** (would deadlock with the CLI's outstanding flock). The handler instead uses its own in-process `reloadMutex sync.Mutex` to serialize tokenTable swaps + rate-limit checks.
5. Hub re-reads `hub-mcp-tokens.json` atomically (load → swap `tokenTable` under RWMutex Lock), responds 204 No Content (no response body — see contract below).
6. CLI returns success only after the hub confirms the swap. Old tokens become unaccepted within milliseconds; no restart required.
7. CLI releases `hub-mcp.lock` (defer-funlock).
8. Failure path: if the control endpoint is unreachable (hub stopped, crashed, etc.), the CLI returns with exit 1 + a message "rotate persisted to disk but live hub did not confirm; restart hub to apply or investigate" — still under the flock; releases on return. Worst case = old token still valid until next hub start, with operator surfaced clearly.

An e2e regression test (`hub_mcp_rotate_live_test.go`) asserts: after `regenerate-token`, a request bearing the OLD token returns 401 within 500ms — without restarting the hub.

### Control endpoint contract `/internal/reload-tokens` (codex r4 F5 closure)

The control endpoint is a NEW attack surface introduced by the live-reload mechanism. Its contract is restrictive on purpose:

**Auth + access gates:**

- HTTP method: **POST only**. Any other method → 405 Method Not Allowed with empty body. Pre-flight OPTIONS rejected (no CORS opt-in).
- Listener: same socket as the per-client `/clients/{id}/mcp` endpoints (single bind; one less moving part). Routed at `/internal/reload-tokens` — the `/internal/` prefix is **the only one** the hub uses; any other path under `/internal/` returns 404 with empty body. The token-bearing `/clients/{id}/mcp` paths never enter this branch.
- **Loopback-guard runs first** — same `rejectUnsafeLoopbackRequest` middleware (`Host: 127.0.0.1[:port] | localhost[:port]`, Origin in the loopback set if present, `Sec-Fetch-Site` in `same-origin | none`). External hosts via DNS-rebind / cross-site fetch → 403.
- Auth header: `X-Mcphub-Control-Token: <64-hex>`. **No fallback to per-client `X-Mcphub-Hub-Token` or `X-Mcphub-Instance-Id`** — those headers are IGNORED on this path. The control token is a separate keyspace; a leaked client token cannot reach this endpoint.
- Comparison: `subtle.ConstantTimeCompare` after fixed-shape gate (64 hex bytes), identical to the per-client gate. Mismatch → 401 with empty body.
- Body: ignored. The endpoint takes no parameters; reload is global. (If the CLI later wants to rotate per-client incrementally, that's a separate endpoint with a separate token; keep this one parameterless.)

**Control token lifecycle:**

- Generated by `crypto/rand` on every hub start (rotates per hub-process lifetime, not per-machine-lifetime). The persisted-instance_id model has nothing to do with the control token — control token rotation is unrelated to client-config invalidation.
- Stored under `<state-dir>/hub-mcp-control.token` (file, NOT JSON; just the 64-hex value + newline). Mode 0600. Windows DACL via the same handle-bound writer used for `hub-mcp-tokens.json`. NEVER copied into client configs. NEVER printed by `mcphub hub-mcp status`. NEVER logged.
- On hub shutdown (graceful OR signal): the file is removed under the same `hub-mcp.lock` flock. A crashed hub may leave a stale file; the next hub start overwrites it (write-then-flock-release; load by the CLI is best-effort with a "file missing → hub not running" diagnostic).
- The CLI reads this file ONLY while holding `<state-dir>/hub-mcp.lock`. The lock guarantees the CLI's token snapshot was valid at read-time; the value goes straight into the HTTP header (in-memory) and is never logged or echoed.

**Status + observability minimization:**

- `mcphub hub-mcp status` does NOT print the control-token file path, mode, or presence. The token's existence is internal-only; even the most paranoid status output leaks only "control endpoint reachable: yes/no" via a probe round-trip with the operator-side CLI doing the auth.
- The internal control endpoint does NOT appear in `hub-mcp.log` route summaries. Successful reloads emit one log line `event=tokens-reloaded source=internal-reload` with NO token bytes, NO instance ids, NO source PID. Failed-auth attempts log `event=internal-reload-rejected reason=<unauth|method|loopback>` with no header content.

**Threat-model implications:**

| Vector | Mitigation |
|---|---|
| Attacker captures control token via process memory dump | Same trust boundary as the per-client token (state-dir 0600/DACL). Memory-dump access ⇒ already root/admin or the running user; full local compromise. Acknowledged residual desktop-dev risk. |
| Attacker captures control token via backup leak | `hub-mcp-control.token` is in `<state-dir>` (per-user) — same trust boundary as the watchdog state files. If state-dir is in a backup the operator already has bigger problems; document in `phase-3b-ii-verification.md` that backups of `<state-dir>` should be encrypted-at-rest or excluded. |
| Force-reload via captured control token | Worst case: attacker can repeatedly call `/internal/reload-tokens` causing the hub to re-read `hub-mcp-tokens.json` from disk. That file is also under `<state-dir>` (per-user); the attacker needs filesystem write to flip token bytes. Reload alone (with the file unchanged) is a no-op other than CPU. **Rate-limited server-side: minimum 5s between consecutive successful reloads, enforced via a single timestamp guarded by `reloadMutex sync.Mutex`** (codex r5 MED clarification — atomic CAS or mutex, NOT a 3-per-second bucket). A 2nd reload attempt within 5s of the previous SUCCESSFUL reload returns 429 with `Retry-After: 5` and does NOT trigger a tokenTable swap. Failed-auth attempts (401/403/405) do NOT count toward the cooldown (they couldn't have caused a swap anyway). Concurrent requests serialize on `reloadMutex`; the 2nd parallel request inherits the 1st's outcome and either returns 204 (if the 1st succeeded and the cooldown window opened) or 429 (if the 1st succeeded and we're still inside its cooldown window). |
| Replay across hub restarts | Control token rotates per hub start. A captured-and-replayed token from a previous hub-process is rejected by the constant-time compare (different keyspace). |

**Tests** (`hub_mcp_internal_reload_test.go`):

- POST with correct control token → 204; tokenTable swap observable on next per-client request.
- GET / PUT / DELETE / OPTIONS → 405, empty body.
- Wrong control token → 401, empty body, constant-time path exercised (hex-shape rejection vs compare-rejection both return identical bodies).
- Non-loopback Host → 403.
- Per-client `X-Mcphub-Hub-Token` header with control-token value → 401 (separate keyspace; per-client header NOT accepted for `/internal/*`).
- Rate-limit: 2 consecutive valid reloads within 5s → first 204, second 429 with `Retry-After: 5` (no tokenTable swap on the 429 path). After the 5s cooldown elapses, the next valid reload again returns 204. Concurrent parallel reloads serialize on `reloadMutex`; one wins and the other gets the consistent outcome (204 if window opened, 429 otherwise).
- Status + log surfaces: golden test asserts zero control-token bytes across `status`, `hub-mcp.log`, install error paths.

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
    Help: "Expose a single aggregated hub URL per client instead of per-daemon URLs. Restart required. Hub instance ID is generated once on first start and persists across restarts; clients re-install only on explicit operator-rotation events (`mcphub hub-mcp regenerate-instance-id` or `regenerate-token`)."},
```

New CLI subcommand `mcphub hub-mcp`:

```text
mcphub hub-mcp status [--json]
    Show endpoint state (port, instance_id PRESENCE only — never the value, pid, started-at,
    presence per client token), redacted recent events. Tokens NEVER printed.

mcphub hub-mcp regenerate-token --client <id> [--yes]
    Rotate one client's token. Refuses non-TTY without --yes (exit 6).
    Prints re-install instruction. No grace window (stolen token rejected immediately).

mcphub hub-mcp regenerate-instance-id [--yes]
    Rotate the persistent hub instance id (full burn-down: every client config
    becomes stale until reinstalled). For incident response — token leak that
    might have included instance_id exposure, or any concern that an old
    instance_id was captured by an attacker. Holds the same hub-mcp.lock as
    `regenerate-token`. Refuses non-TTY without --yes (exit 6). After
    rotation: every client must run `mcphub install` again. Stale-instance
    requests get 401 immediately; no grace window.

mcphub gui --reset-port [--yes]
    Discard the current persistent hub-mcp port; the next `mcphub gui` start
    picks a fresh ephemeral port. Use this when:
      a) a local-attacker pre-bind blocks the port (DoS recovery — see
         "Pre-bind handling" above),
      b) the port has been published externally (logs, screenshots) and you
         want to invalidate it pre-emptively,
      c) the endpoint state file is corrupted past the parse-failure handler.
    Implies a follow-up `mcphub install` to refresh every client config with
    the new port. Refuses non-TTY without --yes (exit 6). Does NOT touch
    instance_id (separate concern); use `regenerate-instance-id` for that.
```

Exit codes: 0 success, 1 backend error, 6 non-TTY without --yes, 8 state path sanity rejected.

## Threat model

| Vector | Mitigation |
|---|---|
| Pre-bind on port (HIGH r1) | Persistent random port (per-user); SO_EXCLUSIVEADDRUSE bind. A local-attacker pre-bind is **denial of service, not confidentiality** (auth gate still rejects the attacker's connections); operator recovers via `mcphub gui --reset-port` + reinstall. Captured-token replay defense: per-client token + persistent `X-Mcphub-Instance-Id`; both must match the current hub state. A stolen token is rejected after `regenerate-token`; a stolen token + stolen instance_id is rejected after `regenerate-instance-id` — operator-driven invalidation, not restart-driven. |
| Stale URL after explicit rotation event (HIGH r2 partial) | After `regenerate-token` or `regenerate-instance-id`, stale client configs fail with 401. Restart alone does NOT invalidate URLs by design (operator-installability tradeoff); only explicit operator action does. `regenerate-instance-id` is the burn-down for instance-compromise scenarios. |
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

## Round-5 verification (must pass before implementation)

Codex review v3.2 must verify:

1. All 33 cumulative findings (24 r1+r2 general/security + 9 r3 + 7 r4) have explicit closure paths in this spec (cross-reference table maintained in the commit message of `2b58b48` for r3 and the v3.2 commit for r4).
2. No NEW issues introduced by v3.2 mechanisms: persistent instance_id model with operator-driven rotation, crash-safe add-before-remove reconcile ordering, local `soExclusiveAddrUse` constant with SetsockoptInt error capture, `/internal/reload-tokens` control endpoint with per-start control token + loopback-guard + rate-limit, handle-relative SecureWriteClientConfig with `dirHandle`-anchored open/rename/re-verify, DACL enterprise-rejection stance with no per-SID escape hatch in v0.3.0.
3. Adapter capability matrix verified for gemini-cli + qwen-cli (TBDs resolved by impl phase).
4. Concurrency model self-consistent (atomic-swap resolver snapshot + RWMutex sessionStore + requestIDKey-keyed InFlightRequests + idle sweeper with try-lock + tokenTable RWMutex swap on live-reload).
5. Bind ordering invariants honored (steps 1-7 sequential, no half-initialized state); listener factory captures Setsockopt error.

If round-5 returns REVISE, the v3.3 round is bounded — only NEW findings, not re-litigation of issues that have explicit closure paths here.

## Files to create / modify (impl-time outline)

| File | Kind | Purpose |
|---|---|---|
| `internal/api/manifest.go` | modify | strict-vs-compat `__` validation modes (F-G6) |
| `internal/config/manifest.go` | modify | same in config loader |
| `internal/api/hub_mcp_resolver.go` | new | canonical resolver + resolver-gen counter (F-G3, F-S4) |
| `internal/api/hub_mcp_state.go` | new | atomic write + handle-bound DACL verify (F-S3) |
| `internal/api/hub_mcp_tokens.go` | new | generate/lookup/rotate + RedactToken helper (F-S2) |
| `internal/api/hub_mcp_instance.go` | new | persistent-across-restarts instance_id (generated once, rotated only by `regenerate-instance-id`) + endpoint state file (port + instance_id + pid + started_at) (F-S1) |
| `internal/api/hub_mcp_session.go` | new | hubSessionStore + idle sweeper + caps (F-G7, F-S5) |
| `internal/api/hub_mcp_handler.go` | new | 5-step auth gate + JSON-RPC dispatch (F-G1) |
| `internal/api/hub_mcp_aggregator.go` | new | fan-out + namespacing + partial-failure + canonical rewrite (F-G2, F-G3) |
| `internal/api/hub_mcp_log_redact.go` | new | RedactToken + golden test helpers (F-S2) |
| `internal/api/settings_registry.go` | modify | add `gui_server.hub_endpoint_enabled` |
| `internal/api/install.go` | modify | bidirectional reconciler (F-G5); pre-write DACL on client configs (F-S2) |
| `internal/gui/server.go` | modify | start hub-mcp listener after state ready; SO_EXCLUSIVEADDRUSE bind |
| `cmd/mcphub/hubmcp.go` | new | `mcphub hub-mcp status` + `regenerate-token` + `regenerate-instance-id` |
| `cmd/mcphub/gui.go` | modify | add `--reset-port` flag handler (rewrites endpoint file under hub-mcp.lock; clears persistent port; emits reinstall instruction) |
| `internal/gui/frontend/src/screens/Settings.tsx` | modify | toggle row + pending-restart badge + instance_id display |

## Test surface

**Unit + integration:**

- `manifest_test.go` (extend): strict mode rejects `__`; compat mode warns.
- `hub_mcp_resolver_test.go`: join correctness; gen counter advances on manifest mutations; per-call revalidation rejects stale route entries.
- `hub_mcp_state_test.go`: atomic write + load roundtrip; load rejects symlink, non-owner, wrong mode, wrong DACL, reparse-point parent dir.
- `hub_mcp_tokens_test.go`: generate, persist, rotate; golden redaction across log/status/install/regenerate/stderr/argv emit surfaces.
- `hub_mcp_instance_test.go`: instance_id generated once on first start, persisted, and unchanged across the next 10 simulated restarts; only `regenerate-instance-id` rotates it; post-rotation a stale-id request gets 401; ephemeral-port reset via `--reset-port` rewrites endpoint file under flock without touching instance_id.
- `hub_mcp_session_test.go`: session create + lookup + TTL expiry + max-sessions cap; idle sweeper respects InFlightRequests.
- `hub_mcp_handler_test.go`: 5-step auth gate matrix (path-unknown, token-shape, constant-time, instance-id, session-client) — every 401 returns identical empty body.
- `hub_mcp_aggregator_test.go`: fan-out partial-failure (init-failed daemon + tools/list-failed daemon → both surface in partialFailures); canonical rewrite (params.name rewritten to RawName); resolver-gen stale-route refusal.
- `install_test.go` (extend): bidirectional reconciler — gate ON adds mcphub-hub + removes per-daemon; gate OFF removes mcphub-hub + restores per-daemon; pre-write DACL check refuses + falls back.

**Integration:**

- `hub_mcp_e2e_test.go`: spin up `mcphub gui` with gate ON; hit `/clients/claude-code/mcp` with valid + invalid tokens / wrong instance ids; observe partial-failures, cancellation forwarding, DELETE session termination. Asserts restart preserves instance_id (token + URL still work without reinstall) and that `regenerate-instance-id` invalidates the stale config.

**Playwright E2E:**

- `internal/gui/e2e/tests/hub-mcp.spec.ts`: gate OFF → no listener; gate ON after restart → 401 without token / wrong instance id; tools/list returns merged list; restart → old token + URL still work (persistence positive); after `regenerate-token` → old token rejected, fresh install works; after `regenerate-instance-id` → both token-only and URL-only stale configs rejected, fresh install works.

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
- `hub instance id`: 32-byte hex generated ONCE on the first start with the gate ON and persisted in `<state-dir>/hub-mcp.endpoint.json`; required in every request via `X-Mcphub-Instance-Id`. Persistent across hub restarts (operator-installability tradeoff); rotated only by explicit `mcphub hub-mcp regenerate-instance-id` (or a successful endpoint-file recreate, e.g. after corruption-recovery).
- `DACL`: Windows Discretionary Access Control List; per-file ACL entries that grant/deny per-SID access.
- `handle-bound DACL verify`: query security info from an open file handle (not by path), so a swap between stat and read cannot leak access.
- `golden redaction test`: enumerates every emit surface (log/status/install/regenerate/stderr/argv/syscall errors) and asserts zero plain-token bytes.
- `partialFailures`: response-meta field carrying per-daemon error rows with a `stage` discriminator (`initialize` | `tools/list` | `tools/call`).
- `InFlightRequests`: per-session map tracking client-request-id → daemon ref for cancellation routing.
- `idle sweeper`: dedicated goroutine that removes sessions older than 30min idle, respecting in-flight invariants.

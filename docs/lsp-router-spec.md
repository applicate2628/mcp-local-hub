# LSP router — canonical specification

> Status: DOCUMENTATION CONSOLIDATION of the **existing** implementation.
> This document lifts the lifecycle state machine, session-store
> contracts, and concurrency guards of the shipped first-class LSP router
> into one canonical reference. It does NOT propose re-architecture and
> does NOT change code. Every claim is grounded in a `file:line`
> citation against the on-disk source at the time of writing.
>
> Roadmap context: this closes the "LSP router named but undesigned"
> item in [`docs/superpowers/specs/2026-06-10-clean-architecture-redesign.md:278`](superpowers/specs/2026-06-10-clean-architecture-redesign.md)
> (§11.1) and its fail-loud / own-stores constraint
> ([`2026-06-10-clean-architecture-redesign.md:733`](superpowers/specs/2026-06-10-clean-architecture-redesign.md)
> "The LSP router (named-but-undesigned)" and the §15 P2 directive that
> the fail-loud trigger must inventory the router's OWN stores, NOT
> "mirror serena").

## 1. Scope and surfaces

The LSP router is a workspace-agnostic HTTP front door that lets every
MCP client point at a single rendezvous URL per language and have the
hub route each request to the correct per-`(workspace, language)`
backend. Two distinct source surfaces carry the name "LSP router";
keeping them apart is load-bearing:

- **Runtime request handler** — [`internal/gui/lsp_router.go`](../internal/gui/lsp_router.go)
  (~755 lines). Registers `POST|DELETE /lsp/<lang>/mcp`
  ([`lsp_router.go:101-103`](../internal/gui/lsp_router.go)), resolves
  each client request to a `(workspace, language)` backend, and forwards
  it upstream. This is the surface this document specifies.
- **Client-config entry writer** — [`internal/api/lsp_client_router.go`](../internal/api/lsp_client_router.go).
  Reconciles each MCP client's config file so its
  `mcp-language-server-<lang>` entry points at the router URL
  `http://127.0.0.1:<guiPort>/lsp/<lang>/mcp`
  ([`lsp_client_router.go:15-17`](../internal/api/lsp_client_router.go),
  `LSPRouterURL` at [`lsp_client_router.go:272-274`](../internal/api/lsp_client_router.go),
  `LSPRouterEntryName` at [`lsp_client_router.go:267-269`](../internal/api/lsp_client_router.go)).
  It owns the **client-side wiring**, not the request path; it is named
  here only to disambiguate it from the runtime handler.

The per-`(workspace, language)` backend each request resolves to is a
**LazyProxy** ([`internal/daemon/lazy_proxy.go`](../internal/daemon/lazy_proxy.go),
~966 lines): one HTTP proxy per registered tuple, bound on a
loopback port, answering synthetic handshake traffic from the embedded
catalog and lazily materializing the heavy LSP backend on first
`tools/call`.

> **Drift note (carried to §8):** the redesign spec
> ([`2026-06-10-clean-architecture-redesign.md:734`](superpowers/specs/2026-06-10-clean-architecture-redesign.md))
> calls the runtime peer `internal/api/lsp_client_router.go`. The
> runtime handler is in fact `internal/gui/lsp_router.go`; the
> `internal/api` file is the client-config writer. This document treats
> the GUI file as canonical and records the spec wording as drift.

## 2. Routing modes and the request path

### 2.1 Route shape and method dispatch

`registerLSPRouterRoutes` mounts a single prefix handler behind a
same-origin guard
([`lsp_router.go:101-103`](../internal/gui/lsp_router.go)):

```
s.mux.HandleFunc("/lsp/", s.requireSameOrigin(s.lspRouterHandler))
```

`parseLSPRouterLanguage` ([`lsp_router.go:244-254`](../internal/gui/lsp_router.go))
extracts `<lang>` from `/lsp/<lang>/mcp`; a path that is not exactly
`/lsp/<non-empty>/mcp` is a 404. `lspRouterHandler`
([`lsp_router.go:105-242`](../internal/gui/lsp_router.go)) then dispatches
by HTTP method:

- `DELETE` — session teardown. Reads `Mcp-Session-Id` and calls
  `deps.Sessions.UnbindSession(sessionID)`, then `204 No Content`
  ([`lsp_router.go:111-118`](../internal/gui/lsp_router.go)).
- `POST` — the JSON-RPC request path (below).
- anything else — `405` with `Allow: POST, DELETE`
  ([`lsp_router.go:119-123`](../internal/gui/lsp_router.go)).

### 2.2 Request body validation (POST)

Before any routing decision the handler validates configuration and the
JSON-RPC envelope:

1. `deps == nil || deps.Resolver == nil` → JSON-RPC internal error
   "lsp router is not configured"
   ([`lsp_router.go:125-129`](../internal/gui/lsp_router.go)).
2. `lspBackendKind(deps, language)` maps the language slug to a backend
   kind (`mcp-language-server` | `gopls-mcp`) via
   `deps.BackendKindForLanguage`; an unknown language → JSON-RPC invalid
   params ([`lsp_router.go:130-134`](../internal/gui/lsp_router.go),
   `lspBackendKind` at [`lsp_router.go:256-261`](../internal/gui/lsp_router.go)).
3. Body is read under a 4 MiB cap; an over-cap body → `400`
   ([`lsp_router.go:145-154`](../internal/gui/lsp_router.go)).
4. The envelope must carry `jsonrpc == "2.0"` and a non-empty `method`
   ([`lsp_router.go:156-173`](../internal/gui/lsp_router.go)).

### 2.3 Synthetic vs. forwarded methods

The handler answers handshake and enumeration methods **synthetically**
from the embedded catalog without touching any backend
([`lsp_router.go:176-214`](../internal/gui/lsp_router.go)):

- `initialize` → `handleLSPInitialize`: builds
  `api.SyntheticInitializeResponse`, **mints a new client session id**
  (`newMcpSessionID`), calls `deps.Sessions.EnsureSession(sessionID)`,
  and returns it on the `Mcp-Session-Id` response header
  ([`lsp_router.go:263-285`](../internal/gui/lsp_router.go)).
- `tools/list` → `handleLSPToolsList` (synthetic catalog for the
  backend kind, [`lsp_router.go:287-303`](../internal/gui/lsp_router.go)).
- `resources/list`, `prompts/list` → synthetic empty lists
  ([`lsp_router.go:305-334`](../internal/gui/lsp_router.go)).
- `ping` → echoes the id (`{}` result), or `202` for a notification id
  ([`lsp_router.go:189-200`](../internal/gui/lsp_router.go)).
- `notifications/initialized` and other `notifications/*` → `202`; the
  `notifications/*` family with a bound single-workspace session may be
  forwarded via `handleLSPNotification`
  ([`lsp_router.go:201-214`](../internal/gui/lsp_router.go),
  `handleLSPNotification` at [`lsp_router.go:673-702`](../internal/gui/lsp_router.go)).

`tools/call` is the only method that resolves to and forwards to a
backend ([`lsp_router.go:216-241`](../internal/gui/lsp_router.go)). Any
other non-notification method → JSON-RPC "unsupported method".

### 2.4 How a `tools/call` resolves to a `(workspace, language)` backend

The resolution is owned by `resolveLSPToolWorkspace`
([`lsp_router.go:342-374`](../internal/gui/lsp_router.go)). It splits on
whether the tool arguments carry a path:

`lsp_routing.ToolCallParams` extracts `name` + `arguments`; an empty
name → required-field error
([`lsp_router.go:230-234`](../internal/gui/lsp_router.go),
[`path_args.go:11-24`](../internal/api/lsp_routing/path_args.go)).
`ExtractPathArgs` scans the arguments for the first path-bearing field
in priority order `file`, `files[]`, `filePath`, `dir`
([`path_args.go:42-75`](../internal/api/lsp_routing/path_args.go)).

**Path-bearing call** (`resolveLSPToolPathArg`,
[`lsp_router.go:376-415`](../internal/gui/lsp_router.go)):

1. `deps.Resolver.ResolveByPath(pathArg, language)` maps the absolute or
   relative path to a canonical workspace root + (optional) registry row
   ([`lsp_routing/resolver.go:22-32`](../internal/api/lsp_routing/resolver.go)
   `ResolveResult`; resolver behavior mirrors
   [`serena_routing/resolver.go:200-310`](../internal/api/serena_routing/resolver.go)).
2. A relative path that fails as `ErrInvalidPath` is retried joined under
   the session's single candidate workspace; 0 candidates → unresolved,
   2+ candidates → "ambiguous workspace" error
   ([`lsp_router.go:385-400`](../internal/gui/lsp_router.go)).
3. `ErrWorkspaceNotFound` / `ErrInvalidPath` → "no LSP workspace for
   path"; any other resolver error → `503` "resolve LSP workspace"
   ([`lsp_router.go:401-414`](../internal/gui/lsp_router.go)).
4. A multi-file `files[]` batch must resolve to ONE workspace;
   `sameLSPResolvedWorkspace` ([`lsp_router.go:523-547`](../internal/gui/lsp_router.go))
   enforces this, else "files span multiple LSP workspaces"
   ([`lsp_router.go:361-371`](../internal/gui/lsp_router.go)).
5. `workspaceFromResolvedLSPPath` ([`lsp_router.go:417-474`](../internal/gui/lsp_router.go))
   turns the resolved root into a registry entry — for a registered row
   directly, for an unregistered row via the first-touch auto-register
   gate (§6).

**Pathless call** (`lspPathlessWorkspace`,
[`lsp_router.go:601-656`](../internal/gui/lsp_router.go)): a workspace-
agnostic call (no file argument) can only route when the session has
exactly one touched candidate; 0 → "make a file-scoped call first", 2+
→ "ambiguous workspace". The single-candidate branch may re-ensure a
NEW language on the already-authorized workspace (its trust derivation
is documented inline at [`lsp_router.go:613-622`](../internal/gui/lsp_router.go)).

### 2.5 Forwarding and the upstream URL

On a resolved workspace, `forwardLSPToWorkspace`
([`lsp_router.go:704-755`](../internal/gui/lsp_router.go)) proxies the
original body to the backend. The upstream URL comes from
`deps.UpstreamURLFn` (default `defaultUpstreamURL`,
[`serena_router.go:1883-1888`](../internal/gui/serena_router.go)) which
returns `http://127.0.0.1:<ws.Port>/mcp`. In the LSP path `ws.Port` is
the **LazyProxy** bind port (`LazyProxy.Bind` listens on
`127.0.0.1:<cfg.Port>` and serves `/mcp`,
[`lazy_proxy.go:176-190`](../internal/daemon/lazy_proxy.go),
[`lazy_proxy.go:157-162`](../internal/daemon/lazy_proxy.go)). The
forward strips the client `Mcp-Session-Id`
([`lsp_router.go:731`](../internal/gui/lsp_router.go)), copies
`Content-Type`/`Accept`/`MCP-Protocol-Version`
([`lsp_router.go:723-730`](../internal/gui/lsp_router.go)), and streams
SSE responses through `streamSSE`
([`lsp_router.go:744-754`](../internal/gui/lsp_router.go)). On a
successful forward of a path-bearing call the handler records
`deps.Sessions.TouchWorkspace(sessionID, ws)`
([`lsp_router.go:239-241`](../internal/gui/lsp_router.go)) so subsequent
pathless calls can disambiguate.

Timeouts: `UpstreamTimeout` (default `serenaUpstreamTimeout = 60s`,
[`serena_router.go:85`](../internal/gui/serena_router.go)) governs the
forward; a timeout → `504`, an unreachable backend → `502`
([`lsp_router.go:733-741`](../internal/gui/lsp_router.go)). These two
explicit error responses are part of the fail-loud contract (§5).

### 2.6 Production wiring

`SetLSPRouterProduction` ([`lsp_router.go:57-88`](../internal/gui/lsp_router.go))
assembles the production `lspRouterDeps`
([`lsp_router.go:33-53`](../internal/gui/lsp_router.go)):

- `Resolver` — `*lsp_routing.WorkspaceResolver`.
- `Sessions` — `*lsp_routing.SessionRouter` (see §3.1; this is the LSP
  router's OWN session store, distinct from serena's).
- `BackendKindForLanguage` — built from the manifest `LanguageSpec`
  list.
- `AutoRegisterFn` — `api.NewAPI().EnsureLSPRegistered`.
- `TrustedRootCheckFn` — `api.LSPWorkspaceRootTrusted` (the live
  read-only trusted-root gate, §6).

## 3. The session store(s) and their contracts

This is the section the redesign spec's §15 P2 directive demands be
explicit: the LSP router must inventory **its own** stores rather than
mirroring serena. The reality is that the LSP router has **one** session
store, and it is materially simpler than serena's three-store stack
because the LSP router does not synthesize an upstream handshake per
client session — it forwards the client's body to a LazyProxy that owns
its own backend lifecycle.

### 3.1 LSP router session store — `lsp_routing.SessionRouter`

Source: [`internal/api/lsp_routing/session_router.go`](../internal/api/lsp_routing/session_router.go).
This is the store wired into the LSP router via `SetLSPRouterProduction`
and consumed through the `lspSessionRouter` interface
([`lsp_router.go:26-31`](../internal/gui/lsp_router.go)).

| Property | Value | Citation |
|---|---|---|
| Key | client `Mcp-Session-Id` (minted by the router at `initialize`) | [`session_router.go:23-27`](../internal/api/lsp_routing/session_router.go) |
| Value | the SET of workspaces the session has touched (`map[wsKey]WorkspaceEntry`) | [`session_router.go:15-18`](../internal/api/lsp_routing/session_router.go) |
| Lifetime | idle TTL `DefaultSessionTTL = 24h` | [`session_router.go:11-13`](../internal/api/lsp_routing/session_router.go) |
| Eviction | `CleanupWithTTL` drops bindings whose `lastSeen` is not after `now-ttl` | [`session_router.go:116-131`](../internal/api/lsp_routing/session_router.go) |
| Concurrency | single `sync.Mutex`; every method locks | [`session_router.go:23-27`](../internal/api/lsp_routing/session_router.go) |

Method contracts (the four the router uses, plus lifecycle helpers):

- `EnsureSession(sessionID)` — record a freshly-`initialize`d session
  before it touches any workspace, so an empty session still ages out
  ([`session_router.go:88-101`](../internal/api/lsp_routing/session_router.go)).
- `TouchWorkspace(sessionID, ws)` — add `ws` to the session's touched
  set and refresh `lastSeen`; nil ws or empty key ignored
  ([`session_router.go:43-59`](../internal/api/lsp_routing/session_router.go)).
- `Candidates(sessionID)` — return the touched workspaces, sorted by
  path then key for stable ambiguous-error rendering; refreshes
  `lastSeen` ([`session_router.go:61-86`](../internal/api/lsp_routing/session_router.go)).
- `UnbindSession(sessionID)` — drop the session (DELETE teardown)
  ([`session_router.go:103-110`](../internal/api/lsp_routing/session_router.go)).

The "set of touched workspaces" shape is what powers pathless
disambiguation (§2.4): one client session can legitimately fan out
across several workspaces over its lifetime, and the router refuses a
pathless call only when that set is ambiguous (≠ 1).

### 3.2 Why the LSP router does NOT have the serena three-store stack

The serena router DOES maintain three coordinated stores. They are
**serena-router-owned**, NOT LSP-router-owned, and are documented here
only to make the contrast explicit and to satisfy the spec's "do not
mirror serena" instruction:

1. **`serena_routing.SessionRouter`** (sticky, 1 session → 1 workspace)
   — [`internal/api/serena_routing/session_router.go`](../internal/api/serena_routing/session_router.go).
   `BindSession`/`LookupSession`/`UnbindSession`/`Cleanup`, idle TTL 24h
   ([`serena_routing/session_router.go:13`](../internal/api/serena_routing/session_router.go),
   `:86-159`).
2. **`routerSessionStore`** (router-minted session → negotiated protocol
   version, with a workspace reverse-index) —
   [`internal/gui/serena_router_session.go`](../internal/gui/serena_router_session.go).
   LRU-capped at `maxRouterSessions = 4096`
   ([`serena_router_session.go:79`](../internal/gui/serena_router_session.go)),
   workspace reverse-index `wsByID`/`idsByWS` maintained under the same
   mutex ([`serena_router_session.go:115-133`](../internal/gui/serena_router_session.go),
   `removeWorkspaceIndexLocked` at [`serena_router_session.go:188-202`](../internal/gui/serena_router_session.go)),
   24h idle TTL via `daemonSessionTTL`, expire-on-read in
   `peekVersionState`/`touch`
   ([`serena_router_session.go:462-529`](../internal/gui/serena_router_session.go)).
3. **`daemonSessionStore`** (client session → upstream daemon session
   id + daemon-negotiated protocol version) —
   [`internal/gui/serena_router_handshake.go`](../internal/gui/serena_router_handshake.go).
   `daemonSessionTTL = 24h`
   ([`serena_router_handshake.go:67-73`](../internal/gui/serena_router_handshake.go)),
   LRU + reservation refcount for the in-flight handshake cap
   ([`serena_router_handshake.go:120-160`](../internal/gui/serena_router_handshake.go)).

The serena stack needs three stores because the serena router
**synthesizes `initialize` at the router and performs a separate MCP
handshake with the workspace daemon** — that introduces the
client-session → daemon-session mapping (store 3) and the
negotiated-version record (store 2) on top of sticky routing (store 1).
See the design rationale in
[`serena_router_handshake.go:1-24`](../internal/gui/serena_router_handshake.go)
and [`serena_router_session.go:1-34`](../internal/gui/serena_router_session.go).

The LSP router does **not** synthesize an upstream handshake per client
session: `forwardLSPToWorkspace` proxies the client's body straight to
the LazyProxy with the client `Mcp-Session-Id` stripped
([`lsp_router.go:731`](../internal/gui/lsp_router.go)), and the LazyProxy
answers its own synthetic `initialize`/`tools/list` and materializes the
backend itself ([`lazy_proxy.go:297-326`](../internal/daemon/lazy_proxy.go)).
So the LSP router needs only the touched-workspace disambiguation store
(§3.1). This asymmetry is the concrete content of the spec's "inventory
its OWN stores, not mirror serena" requirement.

## 4. LazyProxy lifecycle state machine

The LazyProxy is the per-`(workspace, language)` backend the router
forwards to. Its observable lifecycle is recorded in the workspace
registry and drives `status` output.

### 4.1 The five lifecycle states

Registry enum ([`internal/api/workspace_registry.go:17-25`](../internal/api/workspace_registry.go)):

| State | Meaning | Citation |
|---|---|---|
| `configured` | registry row exists, proxy running, backend NOT spawned | [`workspace_registry.go:20`](../internal/api/workspace_registry.go) |
| `starting` | materialization in-flight (singleflight call active) | [`workspace_registry.go:21`](../internal/api/workspace_registry.go) |
| `active` | backend materialized and healthy | [`workspace_registry.go:22`](../internal/api/workspace_registry.go) |
| `missing` | materialization attempted; LSP binary not on PATH | [`workspace_registry.go:23`](../internal/api/workspace_registry.go) |
| `failed` | materialization attempted; failed for any non-missing-binary reason | [`workspace_registry.go:24`](../internal/api/workspace_registry.go) |

### 4.2 Transitions

The lazy proxy is the sole writer of these transitions
([`lazy_proxy.go:17`](../internal/daemon/lazy_proxy.go) comment):

- **(construct) → `configured`** — `ListenAndServe` (or an explicit
  `WriteConfiguredState`) writes `LifecycleConfigured` before binding
  ([`lazy_proxy.go:206-213`](../internal/daemon/lazy_proxy.go)).
- **`configured` → `starting`** — `ensureMaterialized` marks `Starting`
  via `reserveMaterializedSlot` BEFORE entering the singleflight gate so
  `status` shows "starting" while the call is in flight
  ([`lazy_proxy.go:667-672`](../internal/daemon/lazy_proxy.go),
  `reserveMaterializedSlot` at [`lazy_proxy.go:717-756`](../internal/daemon/lazy_proxy.go)).
- **`starting` → `active`** — on a successful `Materialize`, the
  endpoint is cached under `p.mu` and the row is written `Active` with
  the materialize timestamp
  ([`lazy_proxy.go:686-714`](../internal/daemon/lazy_proxy.go)).
- **`starting` → `missing`** — `Materialize` failed and
  `IsMissingBinaryErr(err)` is true
  ([`lazy_proxy.go:678-684`](../internal/daemon/lazy_proxy.go)).
- **`starting` → `failed`** — `Materialize` failed for any other reason,
  or the gate returned a non-endpoint type
  ([`lazy_proxy.go:678-691`](../internal/daemon/lazy_proxy.go)).
- **`active` → `failed`** — `onSendFailure`: a backend death mid-stream
  evicts the cached endpoint, tears down the subprocess, clears the
  gate, and stamps `Failed`
  ([`lazy_proxy.go:876-909`](../internal/daemon/lazy_proxy.go)).
- **`active` → `configured`** — `reapIdleBackend`: an idle backend is
  stopped, the endpoint evicted, and the row written back to
  `Configured` (the proxy stays up; the next `tools/call`
  rematerializes) ([`lazy_proxy.go:821-859`](../internal/daemon/lazy_proxy.go)).
  If the idle lifecycle stop itself fails, the row is stamped `Failed`
  instead ([`lazy_proxy.go:843-852`](../internal/daemon/lazy_proxy.go)).
- **(any) → torn down** — `Stop` closes the endpoint, stops the
  lifecycle, clears the gate, and shuts the HTTP server; idempotent via
  `p.closed.CompareAndSwap`
  ([`lazy_proxy.go:220-252`](../internal/daemon/lazy_proxy.go)).

`active` is also refreshed in place by `debounceWriteToolsCallTimestamp`
on a successful `tools/call` forward (coalesced to `ToolsCallDebounce`,
default 5s) so `status --last-used` stays current without churning the
YAML ([`lazy_proxy.go:911-931`](../internal/daemon/lazy_proxy.go)).

## 5. Materialization: the singleflight gate and the hard cap

### 5.1 `ensureMaterialized` and the inflight gate

`ensureMaterialized` ([`lazy_proxy.go:638-715`](../internal/daemon/lazy_proxy.go))
is the heart of the lazy lifecycle:

1. **Fast path** — a cached `p.endpoint` under `p.mu` is returned
   immediately, incrementing the in-flight backend counter
   ([`lazy_proxy.go:639-650`](../internal/daemon/lazy_proxy.go)). If a
   reaper is mid-reap (`p.reaping`), the caller spin-waits with a 10ms
   backoff until the reap completes or the context is cancelled
   ([`lazy_proxy.go:651-662`](../internal/daemon/lazy_proxy.go)).
2. **Slot reservation** — `reserveMaterializedSlot` writes `Starting`
   and enforces the hard cap (§5.2)
   ([`lazy_proxy.go:670-672`](../internal/daemon/lazy_proxy.go)).
3. **Singleflight** — `p.gate.Do(ctx, key, Materialize)` collapses N
   concurrent first-callers into ONE `Materialize`; all observe the same
   result ([`lazy_proxy.go:674-685`](../internal/daemon/lazy_proxy.go)).
   The gate key is `<WorkspaceKey>|<Language>`
   ([`lazy_proxy.go:254-256`](../internal/daemon/lazy_proxy.go)).

The gate ([`internal/daemon/inflight.go`](../internal/daemon/inflight.go))
has two responsibilities ([`inflight.go:11-31`](../internal/daemon/inflight.go)):

- **Singleflight** via `golang.org/x/sync/singleflight`: the winner runs
  `fn`; losers receive the winner's `(result, error)`
  ([`inflight.go:89-108`](../internal/daemon/inflight.go)).
- **Retry throttle**: after a failure for key K, further `Do(K, _)`
  within `minRetryGap` (the proxy's `InflightMinRetryGap`, default 2s,
  [`lazy_proxy.go:133-135`](../internal/daemon/lazy_proxy.go)) return the
  cached error WITHOUT invoking `fn` — a pathological client loop cannot
  re-spawn a wedged backend every millisecond
  ([`inflight.go:64-97`](../internal/daemon/inflight.go)). The throttle
  is re-checked INSIDE the singleflight winner path so a racer cannot
  bypass it ([`inflight.go:90-97`](../internal/daemon/inflight.go)). A
  success clears the throttle ([`inflight.go:104-106`](../internal/daemon/inflight.go)).

**Winner/loser context decoupling (load-bearing):** `Do` runs `fn` on a
context DETACHED from the winner's request cancellation
(`context.WithoutCancel`) but preserving the winner's deadline
([`inflight.go:74-87`](../internal/daemon/inflight.go)). Without this, a
disconnected winner request would abort the shared materialization AND
cache the canceled-error for the retry-gap window, failing healthy
concurrent callers for no reason
([`inflight.go:56-62`](../internal/daemon/inflight.go)).

`Forget(key)` drops both inflight and throttle state and is called on
every teardown path (`Stop`, `onSendFailure`, `reapIdleBackend`) so a
deliberate shutdown does not throttle the next restart
([`inflight.go:112-121`](../internal/daemon/inflight.go),
called at [`lazy_proxy.go:237`](../internal/daemon/lazy_proxy.go),
`:853`, `:903`).

### 5.2 The materialization hard cap — `reserveMaterializedSlot`

`reserveMaterializedSlot` ([`lazy_proxy.go:717-756`](../internal/daemon/lazy_proxy.go))
enforces a process-wide ceiling on concurrently-`starting|active` LSP
backends (`MaterializedHardCap`, default
`api.DefaultLSPMaterializedHardCap`,
[`lazy_proxy.go:53-56`](../internal/daemon/lazy_proxy.go),
`:66-67`):

- A zero cap disables the check and just writes `Starting`
  ([`lazy_proxy.go:719-721`](../internal/daemon/lazy_proxy.go)).
- Otherwise the registry is flock-locked and reloaded, and the count of
  OTHER tuples that are `starting|active` AND whose port is actually
  live (`materializedSlotPortLiveFn`, a real loopback dial) is computed
  ([`lazy_proxy.go:735-744`](../internal/daemon/lazy_proxy.go),
  `lazyProxyPortLive` at [`lazy_proxy.go:780-787`](../internal/daemon/lazy_proxy.go)).
  The port-liveness check means a stale `active` row whose backend is
  dead does not consume a cap slot.
- At/over the cap → an error that fails the materialization (the
  `tools/call` returns a JSON-RPC error)
  ([`lazy_proxy.go:745-748`](../internal/daemon/lazy_proxy.go)).
- Under the cap → the entry is written `Starting` and saved under the
  held flock ([`lazy_proxy.go:749-755`](../internal/daemon/lazy_proxy.go)).

The flock makes the check-and-reserve atomic against concurrent
materializations of other tuples.

### 5.3 Idle reaper lifecycle

`startIdleReaper` ([`lazy_proxy.go:789-811`](../internal/daemon/lazy_proxy.go))
launches one goroutine (guarded by `idleStartOnce`) when
`IdleBackendTTL > 0` (default 30 min, check every 1 min,
[`lazy_proxy.go:66-70`](../internal/daemon/lazy_proxy.go)). It ticks
`reapIdleBackend` and exits on `idleStop`.

`reapIdleBackend` ([`lazy_proxy.go:821-859`](../internal/daemon/lazy_proxy.go))
reaps ONLY when, under `p.mu`: an endpoint exists, no reap is already in
flight, `inflightBackendRequests == 0`, and `lastBackendActivity` is
older than `IdleBackendTTL`
([`lazy_proxy.go:827-834`](../internal/daemon/lazy_proxy.go)). It then
sets `p.reaping = true`, evicts the endpoint, closes it, resets doc-refs
(§7), stops the lifecycle, forgets the gate, and writes `Configured`.
`stopIdleReaper` closes `idleStop` once (`idleStopOnce`)
([`lazy_proxy.go:813-819`](../internal/daemon/lazy_proxy.go)).

In-flight accounting: `beginBackendRequestLocked` increments the counter
and stamps `lastBackendActivity`; `endBackendRequest` decrements
([`lazy_proxy.go:758-778`](../internal/daemon/lazy_proxy.go)). The
counter is what prevents the reaper from killing a backend with a live
request.

### 5.4 `onSendFailure` ordering invariant

`onSendFailure` ([`lazy_proxy.go:861-909`](../internal/daemon/lazy_proxy.go))
handles mid-stream backend death. The ordering `Lifecycle.Stop()` BEFORE
`gate.Forget()` is load-bearing
([`lazy_proxy.go:866-875`](../internal/daemon/lazy_proxy.go)): Stop
invalidates the lifecycle impl's cached host, so a concurrent
`ensureMaterialized` that enters the cleared gate re-spawns fresh
instead of wrapping the dying host. "Disable-then-publish": kill the
shared resource, THEN signal new callers may enter. `isClientCancelErr`
([`lazy_proxy.go:597-612`](../internal/daemon/lazy_proxy.go))
distinguishes a client disconnect (not a backend failure — no teardown)
from a real backend death, so a hung client does not force avoidable
rematerialization for every other caller.

## 6. Trust gate: first-touch auto-register refusal

The authorization boundary for first-touch auto-register is the
trusted-root store ([`internal/api/lsp_trusted_roots.go`](../internal/api/lsp_trusted_roots.go)).

### 6.1 The gate

In `workspaceFromResolvedLSPPath`
([`lsp_router.go:417-474`](../internal/gui/lsp_router.go)), an
unregistered resolved root proceeds to auto-register ONLY when both hold:

1. **Trusted-root containment** —
   `lspWorkspaceRootIsTrusted(deps, resolved)`
   ([`lsp_router.go:448-452`](../internal/gui/lsp_router.go),
   `lspWorkspaceRootIsTrusted` at [`lsp_router.go:488-504`](../internal/gui/lsp_router.go))
   consults `deps.TrustedRootCheckFn`. It fails **CLOSED** on every
   uncertainty: a nil gate, an empty workspace root, or a gate error all
   return `false` ([`lsp_router.go:489-503`](../internal/gui/lsp_router.go)).
   Production wires `api.LSPWorkspaceRootTrusted`, which reads the live
   `<state-dir>/lsp-trusted-roots.json` on each decision
   ([`lsp_router.go:81-87`](../internal/gui/lsp_router.go),
   [`lsp_trusted_roots.go:286-304`](../internal/api/lsp_trusted_roots.go)).
2. **Project marker (defense-in-depth)** — even inside a trusted tree,
   first-touch auto-register requires `resolved.ProjectMarker` (the
   language's own project file, not a bare `.git` ancestor), else
   "refusing .git-only LSP auto-register"
   ([`lsp_router.go:453-466`](../internal/gui/lsp_router.go)). The marker
   is a discovery hint, NOT the authorization boundary
   ([`lsp_router.go:437-447`](../internal/gui/lsp_router.go)).

A failed trust check returns "is not registered; run mcphub register for
this workspace before using the LSP router"
([`lsp_router.go:449-451`](../internal/gui/lsp_router.go)).

### 6.2 What a "trusted root" is and how it is blessed

A trusted root is either an operator-configured allowed path OR the
canonical root of a workspace registered through an EXPLICIT operator
action ([`lsp_trusted_roots.go:17-38`](../internal/api/lsp_trusted_roots.go)).
`rootContains` is separator-aware (so `/dev` does not match
`/developer`) and case-insensitive on Windows
([`lsp_trusted_roots.go:226-252`](../internal/api/lsp_trusted_roots.go)).

Blessing happens ONLY at explicit register sites, NEVER from the router:

- The GUI lsp-register handler blesses the workspace root after a
  successful explicit register
  ([`internal/gui/lsp_register.go:72-94`](../internal/gui/lsp_register.go),
  `blessLSPTrustedRootForGUI` → `api.BlessDefaultTrustedRoot` at
  [`lsp_register.go:102-104`](../internal/gui/lsp_register.go)).
- `BlessTrustedRoot` / `BlessDefaultTrustedRoot` are documented as
  callable ONLY from explicit register entry points; blessing on the
  router path would re-open the vulnerability
  ([`lsp_trusted_roots.go:306-385`](../internal/api/lsp_trusted_roots.go)).

The router's `TrustedRootCheckFn` is the READ path only; it never
blesses ([`lsp_router.go:81-86`](../internal/gui/lsp_router.go)).

The threat this closes: before the gate, any untrusted `tools/call`
naming a marker-bearing directory could spawn a supervised LSP daemon at
an attacker-chosen path
([`lsp_trusted_roots.go:4-15`](../internal/api/lsp_trusted_roots.go)).
The store read applies the same parent-DACL gate the supervisor-intent
reader applies, so a swappable store cannot inject a trusted root
([`lsp_trusted_roots.go:142-180`](../internal/api/lsp_trusted_roots.go)).

## 7. Per-URI document refcount (`didOpen`/`didClose`)

One LazyProxy serves every client/agent for its `(workspace, language)`
tuple, so multiple agents may open the SAME document. The LSP server
tracks a document once; a duplicate `didOpen` is a protocol violation
and a `didClose` from one agent while another still has it open would
drop it for everyone ([`lazy_proxy.go:110-127`](../internal/daemon/lazy_proxy.go)).

`handleDocLifecycle` ([`lazy_proxy.go:473-520`](../internal/daemon/lazy_proxy.go))
gates `textDocument/didOpen` and `textDocument/didClose` behind a
per-URI refcount:

- `applyDocRef(uri, open)` ([`lazy_proxy.go:528-547`](../internal/daemon/lazy_proxy.go))
  forwards `didOpen` upstream only on the `0→1` transition and
  `didClose` only on the `1→0` transition; intermediate opens/closes are
  absorbed with `202`. A `didClose` against a zero count never drives the
  count negative.
- `rollbackDocRef(uri, open)` ([`lazy_proxy.go:553-565`](../internal/daemon/lazy_proxy.go))
  is the exact inverse, applied when the upstream forward fails (so the
  refcount stays consistent with what the backend actually saw).
- `resetDocRefs()` ([`lazy_proxy.go:571-575`](../internal/daemon/lazy_proxy.go))
  clears all counts whenever the backend is torn down (`Stop`,
  `onSendFailure`, `reapIdleBackend`) — a fresh backend has no open
  documents, so retained counts would absorb the first post-restart
  `didOpen`.

The refcount is guarded by a dedicated `docRefsMu`, NOT `p.mu`, so
document bookkeeping never contends with the materialization/reaper
critical section ([`lazy_proxy.go:119-127`](../internal/daemon/lazy_proxy.go)).

## 8. Fail-loud contract

The router surfaces backend-loss, unreachable, and missing-session
conditions as explicit errors rather than silent degradation. The §3.x
design ([`2026-06-10-clean-architecture-redesign.md:725-735`](superpowers/specs/2026-06-10-clean-architecture-redesign.md))
prescribes this; the as-built LSP-router surfaces are:

- **Unreachable backend** → `502 Bad Gateway` naming the port
  ([`lsp_router.go:739`](../internal/gui/lsp_router.go)).
- **Slow/hung backend** → `504 Gateway Timeout` naming the port and the
  timeout ([`lsp_router.go:735-737`](../internal/gui/lsp_router.go)).
- **Empty upstream URL** (no resolvable port) → JSON-RPC internal error
  "LSP upstream URL is empty" ([`lsp_router.go:714-717`](../internal/gui/lsp_router.go)).
- **Resolver transient error** → `503 Service Unavailable` "resolve LSP
  workspace" ([`lsp_router.go:406-408`](../internal/gui/lsp_router.go)).
- **Unregistered + untrusted root** → JSON-RPC invalid params "is not
  registered; run mcphub register"
  ([`lsp_router.go:449-451`](../internal/gui/lsp_router.go)).
- **Auto-register failure / no entry** → `503` "LSP auto-register
  failed" / "returned no entry"
  ([`lsp_router.go:588-597`](../internal/gui/lsp_router.go)).
- **Backend materialization failure** propagates from the LazyProxy as a
  JSON-RPC error (`rpcErrMissingBinary = -32010` for a missing binary,
  else `rpcErrInternalError`) on the `/mcp` path
  ([`lazy_proxy.go:389-398`](../internal/daemon/lazy_proxy.go),
  `:939-943`), which the router forwards to the client.

The DELETE teardown path unbinds the session explicitly
([`lsp_router.go:111-118`](../internal/gui/lsp_router.go)); a subsequent
pathless call with no candidates fails loud with "make a file-scoped
call first" ([`lsp_router.go:601-610`](../internal/gui/lsp_router.go))
rather than guessing a workspace.

## 9. Known drift / follow-ups

These are documented, NOT fixed (scope is "document what exists").

1. **Spec names the wrong runtime file.** The redesign spec
   ([`2026-06-10-clean-architecture-redesign.md:734`](superpowers/specs/2026-06-10-clean-architecture-redesign.md))
   calls the runtime peer `internal/api/lsp_client_router.go`. The
   runtime handler is `internal/gui/lsp_router.go`; the `internal/api`
   file is the client-config entry writer. Drift in the spec wording,
   not in the code.

2. **Spec proposes an LSP fail-loud session sweep the router does not
   implement — RESOLVED 2026-06-17: by design, not a gap.** The request-driven
   fail-loud (502/504/503 on the next forward, §8) IS the correct as-built
   behavior for the LSP router's single-disambiguation-store design; building
   the proposed serena-style proactive backend-loss sweep would CONTRADICT the
   §15 P2 "do not mirror serena" correction (the LSP router deliberately has one
   store, not the serena three-store stack) and is unnecessary because the
   LazyProxy answers `initialize` itself so a client transparently re-handshakes
   after a reap. The redesign-spec line-735 prose (the unimplemented mirror) is
   SUPERSEDED by this; no code change is owed. Original drift note retained
   below for provenance. The spec
   ([`2026-06-10-clean-architecture-redesign.md:735`](superpowers/specs/2026-06-10-clean-architecture-redesign.md))
   says the LSP router should "mirror the serena
   `coordinateExpiredRouterSessionUnbind` pattern" and tear down client
   sessions when a LazyProxy stops. The shipped LSP router has NO
   backend-loss-driven session sweep (no `terminateSessionsForWorkspace`
   equivalent in `lsp_router.go`); its fail-loud is request-driven only
   (502/504 on the next forward, §8). This is consistent with the §15 P2
   "do not mirror serena" correction (the LSP router has one disambiguation
   store, not the serena three-store stack, §3.2) and with the fact that
   the LazyProxy answers `initialize` itself so a client transparently
   re-handshakes after a reap — but the spec's prose at line 735 still
   describes the unimplemented mirror. Reconcile the spec text or
   admit a backend-loss sweep as a separate fix-design; either is out of
   scope here.

3. **`defaultUpstreamURL` comment says "serena daemon".** The shared
   forwarder helper `defaultUpstreamURL`
   ([`serena_router.go:1881-1888`](../internal/gui/serena_router.go)) is
   used by BOTH the serena router and the LSP router (via
   `deps.UpstreamURLFn`), but its doc comment says it "points at the
   workspace's serena daemon." In the LSP path `ws.Port` is the
   **LazyProxy** port. Cosmetic comment drift; behavior is correct
   (both targets serve `/mcp` on a loopback port).

4. **`didOpen`/`didClose` refcount listed as an OPEN bug in the
   roadmap.** The redesign spec's Adjacent findings
   ([`2026-06-10-clean-architecture-redesign.md:822`](superpowers/specs/2026-06-10-clean-architecture-redesign.md))
   and §11.1 ([`:278`](superpowers/specs/2026-06-10-clean-architecture-redesign.md))
   list "`didOpen/didClose` no-refcount multi-agent bug" as OPEN. The
   per-URI refcount IS implemented in
   [`lazy_proxy.go:459-575`](../internal/daemon/lazy_proxy.go) (§7), so
   the roadmap entry is stale and should be closed. Flagged for a
   roadmap reconcile, not fixed here.

5. **The `internal/api/lsp_routing/path_args.go` helper `firstNonEmptyString`
   is unused** ([`path_args.go:85-91`](../internal/api/lsp_routing/path_args.go)).
   Dead-code observation only; no action taken.

## Terms and Abbreviations

- **LSP** — Language Server Protocol; a JSON-RPC protocol exposing
  IDE-grade code intelligence over a stdio/socket channel.
- **MCP** — Model Context Protocol; the JSON-RPC-over-HTTP protocol the
  router speaks to clients and forwards to backends.
- **LSP router** — the runtime handler at `internal/gui/lsp_router.go`
  serving `/lsp/<lang>/mcp`.
- **LazyProxy** — the per-`(workspace, language)` HTTP proxy
  (`internal/daemon/lazy_proxy.go`) that answers synthetic handshake
  traffic and materializes the heavy LSP backend on first `tools/call`.
- **Materialize** — spawn the heavy backend subprocess for a LazyProxy
  (the `BackendLifecycle.Materialize` call).
- **Singleflight** — collapsing N concurrent identical calls into one
  invocation (here: one `Materialize` per `<WorkspaceKey>|<Language>`
  key); provided by `golang.org/x/sync/singleflight`.
- **Inflight gate** — `InflightGate` (`internal/daemon/inflight.go`): the
  singleflight + retry-throttle control for materialization.
- **Hard cap** — the process-wide ceiling on concurrently
  `starting|active` LSP backends (`MaterializedHardCap`).
- **Idle reaper** — the per-proxy goroutine that stops a backend after
  `IdleBackendTTL` of inactivity, returning the row to `configured`.
- **doc-ref / refcount** — the per-URI open count gating `didOpen`/
  `didClose` so one shared backend never sees a duplicate open or a
  premature close.
- **Trusted root** — an operator-configured or explicit-register-blessed
  canonical path that authorizes first-touch auto-register of workspaces
  at or under it (`<state-dir>/lsp-trusted-roots.json`).
- **First-touch auto-register** — the router registering a new
  `(workspace, language)` backend the first time a tool call names a path
  inside a trusted, marker-bearing tree.
- **Sticky session** — the serena-router pattern (1 session → 1
  workspace); the LSP router instead keeps a SET of touched workspaces
  per session for pathless disambiguation.
- **Workspace key** — the 8-char deterministic hash of the canonical
  workspace path (`api.WorkspaceKey`), the registry primary-key
  component and the inflight-gate key prefix.
- **Backend kind** — `mcp-language-server` | `gopls-mcp`; selects the
  synthetic tool catalog.
- **Fail-loud** — surfacing backend-loss / unreachable / missing-session
  as an explicit error (502/503/504 or a JSON-RPC error), never a silent
  empty success.

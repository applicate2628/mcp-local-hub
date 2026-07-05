# LSP router cold-materialize: cold forwarded calls held up to 60s; no cold-start concurrency bound

- **status:** closed (2026-07-03 — shipped in PR #489, released v0.4.20, fleet redeployed). Final shape went BEYOND the design below: an $architect unified probation/delivery/lifecycle model (single-owner `reconcileRegistryLifecycleLocked` + await-after-delivery for response-needed forwards per decision `2026-07-02-lsp-probation-await-after-delivery.md`, $reliability-engineer 8 MUST-DO folded: ColdRequestHoldCeiling 120s + non-retryable 500 + decoupled lspForwardUpstreamTimeout 150s + timeout-ordering clamp) after the original bounded-503 implementation edge-mined 2 bot rounds. 7 review rounds total (3-lane commission ×2 + fable xhigh pre-bot premine) — final bot round clean PASS. Residual/backlog: `2026-07-02-lsp-proxy-preexisting-lifecycle-edges.md`, `2026-07-02-sendrpc-pending-uncapped.md`.
- **severity:** medium (UX: first LSP tool call per language after GUI restart holds 46s+; 8 concurrent colds contend past 90s; `claude mcp list` slowness largely NOT this router — see corrected root cause)
- **filed:** 2026-07-02; **design study:** 2026-07-02 (Fable-5, live-verified)
- **context:** GUI LSP router (`127.0.0.1:9125/lsp/<lang>/mcp`) + per-(workspace,language) lazy-proxy daemons; separate from the hub aggregate (`10275`)
- **prior review:** `.reports/2026-07/report(main)-2026-07-01_23-58_hub-red-incident-review.md` P2c

## Corrected root cause (supersedes the original premise)

The originally filed mechanism — "router holds `initialize` open for the full cold
materialize" — is **refuted by code + live probes** (2026-07-02, deployed binary
2026-07-01):

1. **Handshake surface is synthetic and fast.** `initialize`, `tools/list`,
   `resources/list`, `prompts/list`, `ping`, `notifications/*` are answered from the
   embedded tool catalog with NO backend touch, at BOTH layers: router
   `internal/gui/lsp_router.go:177-213` → `internal/api/tool_catalog.go:47,74`; daemon
   `internal/daemon/lazy_proxy.go:305-318`. Live-probed: 2 ms for warm AND
   never-touched languages; `GET` = 405 in 2 ms.
2. **`claude mcp list` is slow on its own.** Reproduced 2026-07-02 fully warm:
   **1 m 44 s**, all 9 LSP entries `✔ Connected` (millisecond handshakes). The sweep
   cost is claude-side (npx stdio server spin-ups + remote servers), matching the
   incident's ">100s, exit 124" without any router involvement. The incident-review
   attribution of the `mcp list` hang to this router was a misattribution
   (its 12s "cold probe" was made during host-saturation / was a forwarded-method
   probe, not a handshake).
3. **The real defect — cold FORWARDED calls (`tools/call` and other forwarded
   methods):** first call per (workspace, language) after the backend is cold holds
   the connection up to the router's 60 s upstream budget. Breakdown (verified):
   - Router `tools/call` path: `internal/gui/lsp_router.go:235-241` → resolve →
     `EnsureLSPRegistered` (`internal/api/lsp_auto_register.go:32`; prior-row fast
     path :92-101 with 500 ms port probe — cheap post-restart since the supervisor
     respawns proxy daemons from persisted intent; first-touch does intent write +
     supervisor reconcile + 10 s readiness under a 45 s ctx, lsp_router.go:625) →
     `forwardLSPToWorkspace` :744 with a 60 s upstream HTTP client
     (`serenaUpstreamTimeout`, serena_router.go:86; timeout → 504 :775-777; status
     passthrough verbatim :784-793).
   - Daemon: `handleToolsCall` lazy_proxy.go:396 → `ensureMaterialized` :662 →
     `InflightGate.Do` (inflight.go:63; singleflight + 2 s failure throttle; winner
     ctx detached) → `Materialize` (backend_lifecycle.go:288; `exec.LookPath`
     fast-fail for missing wrapper AND wrapped LSP binary :294-306; spawn + 10 s MCP
     handshake :323-332) → **first `SendRequest` blocks behind LSP workspace
     indexing** (gopls on this repo ≈ 35-45 s) — the dominant share of the measured
     46 s.
   - **Held-connection mechanism for concurrent callers:** `x/sync
     singleflight.Group.Do` joiners block **uncancellably** (not ctx-aware), so every
     concurrent cold caller rides the full materialize.
   - **No cold-start concurrency bound:** `reserveMaterializedSlot`
     (lazy_proxy.go:741) caps only TOTAL materialized backends
     (`DefaultLSPMaterializedHardCap = 16`, workspace_registry.go:32). 8 languages
     cold-started in parallel all index the workspace at once → each blew past 90 s
     (measured), saturating the host (which is also what made even synthetic probes
     read slow during the incident window).
   - **Registry truthfulness gap:** `LifecycleActive` is written at materialize
     success (lazy_proxy.go:734-737) — i.e. BEFORE indexing completes — so any
     Starting-count-based gate would under-protect without moving that write.

## Design (Fable-5 deep study, 2026-07-02) — implementation-ready

### 1. Response contract: bounded wait, then 503 + retry (serena mirror), seated in the DAEMON

Chosen: option (c) — bounded wait then fast-fail 503 — over (a) immediate 503
(penalizes the common small-language cold start that materializes in 2-5 s) and
(b) fast-ack/deferred-readiness (MCP `tools/call` has no deferred-result channel a
stock client consumes; not viable).

Reference contract (existing, production-proven in this repo):
`internal/gui/serena_router.go:841` (idle-wake not ready → `503` + JSON-RPC error
"serena daemon waking from idle; retry"), :1032 (handshake-in-flight → 503 retry),
:1284 (stop-gate → 503 retry). No `Retry-After` header in the serena precedent; the
load-bearing retry signal is the error MESSAGE (agent-level retry). We add
`Retry-After: 15` anyway (cheap, standard; hub precedent `hub_mcp_handler.go:615`
uses 429+Retry-After for session cap).

Exact changes:

**(i) `internal/daemon/inflight.go` — bounded join.** Add
`ErrMaterializeInFlight` sentinel + `DoBounded(ctx, key, budget, fn)`:
keep the failure-throttle fast path and the detach semantics, but replace the
blocking `g.sf.Do` with `g.sf.DoChan` + `select { result / ctx.Done() / budget
timer }`. Timer expiry returns `ErrMaterializeInFlight` to THAT caller while the
materialize goroutine continues; the result lands in the singleflight cache-line for
the next caller. IMPORTANT behavioral change: today's winner-deadline propagation
(inflight.go:83-87) must be REPLACED by a fixed detached hard ceiling
(`materializeHardCeiling = 120 s`) — with bounded join, propagating the winner's
deadline would abort the shared background materialize at the winner's budget,
defeating the whole design.

**(ii) `internal/daemon/lazy_proxy.go` — map to 503.** New helper
`writeRPCErrorStatus(w, id, httpStatus, code, msg, retryAfterSecs)` (daemon-side
sibling of the gui `writeJSONRPCErrorStatus`; note existing `writeRPCError`
:974-990 is always HTTP 200 — leave it for non-transport errors).
`ensureMaterialized` (:662) calls `gate.DoBounded(ctx, key,
p.cfg.MaterializeWaitBudget, ...)`; `handleToolsCall` (:396) and `handleForward`
(:436) — the ONLY two `ensureMaterialized` request paths, plus `handleDocLifecycle`'s
forwards — branch on `errors.Is(err, ErrMaterializeInFlight)` BEFORE the generic
error mapping and emit:
`503`, JSON-RPC error code `-32603`, message
`"language backend cold start in progress (<BackendKind>, <WorkspacePath>); retry in ~15s"`,
`Retry-After: 15`. Router change: NONE — `forwardLSPToWorkspace` already passes
upstream status verbatim (lsp_router.go:784-793), and `TouchWorkspace` still binds
the session on a forwarded 503 (desirable: retries route pathlessly to the same
workspace).

**(iii) First-request probation (the indexing window).** Materialize success ≠
usable: gopls indexes for ~35-45 s after the MCP handshake. Add per-proxy
`warmed bool` (under `p.mu`), reset at endpoint publish and on every teardown
(`onSendFailure` :900, idle reap, Stop). While `!warmed`, `handleToolsCall`/
`handleForward` wrap the upstream call in
`ctx = context.WithTimeout(r.Context(), p.cfg.MaterializeWaitBudget)`; `SendRPC`
honors ctx (backend_lifecycle.go:149-150). On OUR deadline (client ctx still live) →
same 503-warming response; the abandoned in-flight request's late response is
dropped by the pending-map cleanup (backend_lifecycle.go:107-111) — no teardown:
`isClientCancelErr` (:628) already classifies `DeadlineExceeded` as non-backend
failure, but add the explicit `ErrMaterializeInFlight`-style branch FIRST so the
client never sees a bare "context deadline exceeded". First SUCCESSFUL response
sets `warmed = true` AND performs the `LifecycleActive` registry write — **move**
the write from materialize-success (lazy_proxy.go:734-737) to first-success; the
row stays `Starting` through indexing (truthful `status`/GUI, and load-bearing for
the concurrency gate below).

**(iv) Singleflight join for same language** — already exists
(`InflightGate`, one `Materialize` per key; N callers observe one flight);
`DoBounded` preserves it via `DoChan`.

### 2. Concurrency/throttle: cross-process cold-start slots in the registry

The 8-at-once blowup is ACROSS daemon processes (one proxy per tuple), so an
in-process semaphore cannot bound it. The shared medium already exists: the
flock'd workspace registry + `reserveMaterializedSlot` (lazy_proxy.go:741) with its
port-live self-heal (`materializedSlotPortLiveFn` :765, already a test seam).

Add a second bound inside `reserveMaterializedSlot`, under the same flock: count
OTHER rows with `Lifecycle == Starting && portLive(e.Port)`; if
`>= p.cfg.ColdStartConcurrency` (default **2**) → do NOT enter the gate; return
typed `errColdStartSlotsBusy` → handler emits
`503 "LSP cold-start slots busy (<n> backends warming); retry in ~30s"`,
`Retry-After: 30`. Client retry IS the queue — no in-process waiting, no held
connections. Because (1.iii) keeps rows `Starting` until first success, the slot
gate bounds the expensive INDEXING window, not just spawn+handshake. Stale
`Starting` rows from crashed daemons are excluded by the port-live check; a live
daemon wedged in probation is bounded by (1.iii)'s probation watchdog: piggyback on
the existing idle-reaper tick (`startIdleReaper`, every 1 min) — if
`!warmed && time.Since(endpointPublishedAt) > p.cfg.ColdStartMaxProbation`
(default **5 min**) → teardown (Stop + `gate.Forget` + `LifecycleFailed
"backend never served a first response within 5m"`), freeing the slot.

Supervisor interaction: the router does NOT spawn daemons — `EnsureLSPRegistered`
writes supervisor intent and IPC-reconciles; the supervisor spawns/respawns proxy
daemons (cheap: port bind only, no LSP). After GUI restart the proxies come back
from persisted intent automatically; backends stay lazy. No throttle needed at the
supervisor layer; the throttle seat is the daemon materialize path.

New `LazyProxyConfig` fields (defaults in `NewLazyProxy`, zero → default):
`MaterializeWaitBudget = 15 s`, `ColdStartConcurrency = 2`,
`ColdStartMaxProbation = 5 min`. Code defaults only for now (no manifest/GUI knobs
this round; note for the GUI-settings backlog).

### 3. Client compatibility (verified against precedent)

- claude's streamable-HTTP client does NOT transport-retry POSTs and does not
  honor `Retry-After` on POST; a non-2xx tool POST surfaces as a tool error to the
  MODEL, which retries on a "retry" message — exactly how the serena router's
  idle-wake 503 already behaves in production (CLAUDE.md serena-interlock section:
  "router maps to 503 → the client retries"). The message text is the contract;
  the header is decoration for other clients.
- `claude mcp list` NEVER sees a 503 under this design: the handshake surface
  stays synthetic-200 (unchanged), so no risk of `✘ failed` listings. This is why
  fast-fail is applied ONLY to forwarded methods, never to `initialize`.
- Fallback if some client hard-fails on non-2xx tool POSTs: flip the cold-start
  refusal to HTTP 200 + JSON-RPC error (serena's `missing_session` shape,
  serena_router.go:403-408) — one-line change in the new helper; not default.

### 4. Edge cases + tests (deterministic; inject via existing seams — `LazyProxyConfig.Lifecycle` interface fake + `materializedSlotPortLiveFn` var; NO real gopls)

- LSP binary missing: already fast (`LookPath` preflight both wrapper + wrapped
  binary → `errMissingBinary` → `LifecycleMissing`, instant). Keep; add timing
  assertion. (Router-side gap, deferred follow-up: first-touch
  `EnsureLSPRegistered` still spawns a useless proxy daemon for a missing-LSP
  language — the D-3 `AvailabilityAdmission` gate exists but the shipped manifest
  declares no probes.)
- Materialize crash: sf goroutine error → `LifecycleFailed` + 2 s retry throttle →
  next call rematerializes (existing). Daemon process crash mid-start: supervisor
  respawns; stale `Starting` row excluded from slot count by port-live.
- Materialize slower than budget: continues detached under the 120 s hard ceiling;
  retries join the same flight; ceiling hit → cached failure → throttle → fresh
  attempt.
- Warm backend goes cold mid-session: `onSendFailure` teardown resets `warmed`,
  docRefs, gate → next call re-enters the bounded cold path (existing teardown,
  new probation reset).

Test names (`internal/daemon`):
`TestInflightGate_DoBounded_TimerExpiryReturnsInFlightWhileWinnerContinues`,
`TestInflightGate_DoBounded_CtxCancelReturnsCtxErr`,
`TestLazyProxy_ColdToolsCall_BoundedWaitReturns503RetryAfter`,
`TestLazyProxy_ColdToolsCall_FastMaterializeAnswersInline`,
`TestLazyProxy_ConcurrentColdCallers_OneMaterialize_AllBoundedAtBudget`,
`TestLazyProxy_Probation_FirstRequestSlow_503Warming_ThenWarmServes200`,
`TestLazyProxy_Probation_LifecycleActiveWrittenOnlyOnFirstSuccess`,
`TestLazyProxy_Probation_DeadlineExceeded_NoBackendTeardown`,
`TestLazyProxy_ProbationWatchdog_WedgedBackendTornDownAndSlotFreed`,
`TestLazyProxy_ColdStartSlots_BusyReturns503WithoutMaterialize`,
`TestLazyProxy_ColdStartSlots_StalePortDeadStartingRowIgnored`,
`TestLazyProxy_MissingLSPBinary_InstantMissingError`,
`TestLazyProxy_WarmGoesCold_ReentersBoundedColdPath`.
`internal/gui`: `TestLSPRouter_ForwardsDaemon503AndRetryAfterVerbatim_StillTouchesSession`.
Consumer sweep (Starting-longer semantics): GUI `LspMatrix` badge rendering +
`mcphub status` output — assert `starting` renders (existing states, changed dwell).

### 5. Adversarial self-check — residual risks, ranked

1. **(medium)** Agent retry-budget exhaustion: model may give up after 2-3 retries
   while a big-repo gopls needs 60-90 s under contention. Mitigations: honest
   "~15s/~30s" hints; slot gate keeps per-backend warm time near the uncontended
   46 s; each retry makes visible progress (join → warm). Residual: user-visible
   "try again" on the very first LSP use after restart.
2. **(low-medium)** Throttle self-queue: `ColdStartConcurrency=2` with 9 cold
   languages demanded at once serializes tails (~4 waves ≈ 3-4 min for the last).
   Only occurs on genuine simultaneous cold demand (rare); tunable constant;
   strictly better than today's all-at-once >90 s each + host saturation.
3. **(low)** Retry storm: each retry costs a bounded join (≤ budget held conn);
   failure throttle (2 s) caps re-materialize churn. Add a rate-capped
   `lsp-cold-start-refused` diag log line for visibility.
4. **(low)** Probation misfire on a legitimately-slow first query (huge
   `references`): 503 + retry until index warm; probation clears on ANY success.
5. **(low)** Late responses of abandoned probation requests: dropped by pending-map
   deletion; engineer must eyeball `readLoop`'s unknown-id path for log spam.
6. **(low)** Lifecycle-timing change: anything treating `Active` as
   "materialize returned" (status text, GUI badges) now sees longer `Starting` —
   sweep consumers (see tests).

### Build order + review gate

1. `inflight.go` `DoBounded` + sentinel + ceiling-detach (replaces deadline
   propagation) + gate tests.
2. Daemon `writeRPCErrorStatus` + 503 mapping in `handleToolsCall`/`handleForward`/
   doc-lifecycle forwards + tests.
3. Probation: `warmed` flag, bounded first-request ctx, Active-write move,
   watchdog on idle-reaper tick + tests.
4. Cold-start slot gate in `reserveMaterializedSlot` + tests.
5. Router passthrough test + `lsp-router-slow-forward` (>5 s) observability log in
   `forwardLSPToWorkspace`.
6. Docs: CLAUDE.md LSP-router note (503-retry contract + "claude mcp list is slow
   by its own nature — do not attribute to this router").

Gate: standard repo PR flow — full commission (sonnet+opus+security lanes) before
bot push; Codex bot PASS on HEAD mandatory before merge; then redeploy
(build → rename-aside → supervisor restart) and a live cold-start smoke:
tools/call on a cold language must return 503-retry ≤ ~16 s, second retry after
warm must return the tool result.

## Not-a-cause

Hub aggregate healthy (partial-failure-tolerant, hub_mcp_aggregator.go:509-514);
serena@9151 was the separate quarantine incident
(`2026-07-02-supervisor-lost-child-quarantine-class.md`); router handshake surface
NOT involved (synthetic, verified). `claude mcp list` total latency is claude-side
sweep cost — out of scope here.

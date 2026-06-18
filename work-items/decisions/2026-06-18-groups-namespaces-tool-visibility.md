---
status: accepted
date: 2026-06-18
driver: competitor-parity keystone (#2 adoption lever) — groups/namespaces is the organizing primitive that smart-routing, multi-tenant, per-client tool filtering, and the §D paid tier all depend on. Source: .reports/2026-06/report(research)-2026-06-16_competitor-audit-mcphub.md §3 item 2, §5 item 2.
supersedes: nothing (new capability)
related: 2026-06-16-hot-swap-zero-downtime-config.md (shares the hub-front / gate-ON endpoint surface)
---

# Decision: groups / namespaces / per-server tool visibility — design + options

## ✅ DECISION (2026-06-18) — operator answers + architecture-reviewer REVISE→PASS folded in

> The body below is the architect's PROPOSAL. This section is the ACCEPTED decision:
> the 3 operator answers + the 3 reviewer-mandated corrections that supersede the
> proposal where they differ. Status flipped proposed→accepted on this basis
> (architecture-reviewer gate = REVISE-then-PASS; model verified, 7/7 claims hold).

**Operator decisions:**
1. **v1 scope = solo tool-visibility + token-seam NOW.** Ship named profiles (e.g. `frontend`/`infra`) AND wire the per-group hub-token row loopback-only (no real key) so the §D auth seam is physically present. Team/auth lifecycle = v2.
2. **URL = `/g/<group>/mcp` — a STRUCTURALLY-SEPARATE prefix** (NOT the proposal's `/groups/` sibling-in-same-keyspace). This is the cleaner resolution of reviewer **Defect C**: the ScopeKey itself is kind-namespaced — a group's scope key carries its kind (e.g. `g:<group>`) distinct from a client key — so a group and a client of the same name **cannot collide** in the shared `tbl.Tokens` (hub_mcp_tokens.go:230) or `Bindings` (hub_mcp_resolver.go:71) maps **by construction**. This SUPERSEDES the proposal's "reserved-name rule" (no name-equality gate needed; the keys live in disjoint kind-prefixed subspaces). Route stays prefix-disambiguated at hub_listener.go:225.
3. **Tool-filter granularity = FINE-GRAINED per-tool in v1** (NOT the proposal's coarse server-subset-only). v1 supports per-server "hide tool Z", not just whole-server inclusion. Reviewer confirmed the filter lands at the tools/list MERGE step regardless of granularity (dispatch path untouched), so per-tool is the same seam, just a richer predicate.

**Reviewer-mandated corrections (fold into the plan):**
- **Defect A (planner-scope):** `PublishResolverSnapshot` / `resolverSnapshot.Store` has **NO production caller** today — the live hub `LoadResolverSnapshot()`s at 3 sites (hub_mcp_handler.go:584, hub_mcp_aggregator.go:605, :855) but nothing publishes. The groups workstream MUST own wiring the publish choke point — it is a v1 prerequisite, not "fold into the existing publish path." (This reinforces the additive-snapshot model; it just isn't wired yet.)
- **Defect B (tests-first inventory is wider than the proposal):** the `sess.Client → sess.ScopeKey` rename touches MORE sites than the 3 listed — add `currentDaemonPort` (hub_mcp_aggregator.go:857), the cross-client 401 gate (hub_mcp_handler.go:366,647 → must become byte-identical `sess.ScopeKey != scopeKey`), and session-cap accounting (hub_mcp_session.go:130,492-494). ALL of these + the original 3 are UNTESTED → **tests-first is mandatory** (pin current per-client behavior byte-identical BEFORE the rename). This is the one hard implementation pre-condition.
- **Defect C:** resolved by operator decision 2 (kind-namespaced keys) — see above.

**Next step = a planner phases this tests-first** (request-path feature serving ALL MCP clients → attended, phase-by-phase, verify each). Phase 1 (add coverage to the untested hot path — pure test addition, no behavior change) is the safe prerequisite and can start immediately. Operator open-questions 1-3 in the proposal body are now ANSWERED above; question 4 (if any) carries to the planner.

---

## Context

### The ask

"Expose a named SUBSET of MCP servers to a named endpoint." This is the
organizing primitive for three things the hub cannot do today:

1. **Per-endpoint tool visibility** — don't dump every tool from every
   server into every client's context window (the tool-context-bloat
   problem, sibling of the process-tail problem the hub already solves).
2. **Smart-routing** — a named, scoped set of tools is the surface a
   semantic router would rank over (roadmap G4/§D, deferred).
3. **Multi-tenant / team scoping** — a group is the unit a future
   per-group auth token / team membership attaches to (§D paid tier).

The competitor audit ranks it the **#1 keystone** competitor-driven item
(audit §5: "the keystone that the smart-routing, multi-tenant, and
commercial-tier futures all depend on, and we have nothing like it
today").

### What the hub does today (verified, file:line)

The hub already serves a per-client aggregate endpoint and fans each
client's traffic to a per-client subset of daemons. The machinery that
makes this work is — structurally — **already a group mechanism keyed on
a string**. The string just happens to be a client id.

- **URL → client id.** The gate-ON hub listener mounts a single handler:
  `mux.Handle("/clients/", handler)` (`internal/gui/hub_listener.go:225`).
  `parseClientPathFromURL` extracts the id from `/clients/<id>/mcp`
  (`internal/api/hub_mcp_handler.go:790`), rejecting embedded slashes /
  whitespace / empty (`:801-805`). The id is then gated against the
  supported-client roster: `isSupportedClient(clientID)` iterates
  `clients.SupportedClientNames()` (`hub_mcp_handler.go:812-818`,
  called at `:138`).

- **client id → daemon subset.** The published routing state is
  `ResolverSnapshot.Bindings map[string][]canonicalDaemonRef`, **keyed by
  client_id** (`internal/api/hub_mcp_resolver.go:69-72`). It is built from
  every manifest's `ClientBindings` field by
  `BuildResolverSnapshotFromManifests` (`hub_mcp_resolver.go:116-163`):
  for each `(client, daemon)` binding row it appends a
  `canonicalDaemonRef{Server, Daemon, Port}` to `Bindings[client]`
  (`:140-161`).

- **The binding source-of-truth is the manifest.** `ClientBinding` is
  `struct{ Client, Daemon, URLPath string }`
  (`internal/config/manifest.go:153-157`), carried on
  `ServerManifest.ClientBindings []ClientBinding` (`manifest.go:48-103`,
  field at the `client_bindings` yaml key). The snapshot is rebuilt +
  republished atomically on any manifest change via
  `BumpResolverOnManifestChange(manifests)` (`hub_mcp_resolver.go:169`)
  → `PublishResolverSnapshot` (`:90`, an `atomic.Pointer` swap).

- **The whole request pipeline keys on `sess.Client` (a string).**
  At initialize, the hub fans out over `sess.IntendedParticipants`
  (`hub_mcp_aggregator.go:181-185`) — the daemon set derived from
  `Bindings[client]`. At tools/call, `resolveToolsCallRoute` revalidates
  the route against the live snapshot with
  `daemonStillBound(current, sess.Client, ref)`
  (`hub_mcp_aggregator.go:607`, predicate at `:918-932` — a lookup into
  `snap.Bindings[client]`). Nothing in the dispatch path inspects a
  physical client config; it only ever uses the **string key**.

- **Tool-name namespacing already exists.** The hub-exposed tool name is
  `"<Server>__<RawName>"` (`hub_mcp_resolver.go:53-57`); the per-session
  `RouteMap` (hub-name → `canonicalToolRef`) is built per session at
  tools/list merge time (`hub_mcp_resolver.go:21-25`). So "per-server
  tool visibility" reduces to **which tools are admitted into the
  RouteMap / merged tools/list** for a session — there is already a
  filtering choke point.

**The load-bearing observation:** `sess.Client` is an opaque map key, not
a physical client. A **group is the exact same shape** — a named key whose
value is a `[]canonicalDaemonRef`. The endpoint, the auth-token keyspace,
the bindings map, and the revalidation predicate are all already
string-keyed. Adding groups is therefore mostly **a new source of keys for
an existing map and a sibling URL route**, not a new pipeline.

### What is intentionally OUT of scope for v1 (named so the design can't paint them into a corner)

- **Smart / semantic routing** (vector search over the group's tools).
  v1 must produce the *named scoped tool surface* a router needs, then stop.
- **Per-group auth tokens / accounts / RBAC** (§D paid tier). v1 must make
  the group the obvious *seam* the token-keyspace attaches to, then stop.
- **Tool-result compression** (audit #1) — independent middleware, separate
  workstream.
- **Per-tool hide within a server beyond an allow/deny list** — v1 ships
  coarse (server-level subset) + a simple tool allow/deny; ranking/UX of
  fine-grained per-tool toggles is a deferred slice.

---

## Concept & relationship to per-client targeting (the model decision)

Three coherent models were considered. The design picks **a group is a
peer of a client in the same `Bindings` keyspace, distinguished by route
prefix** (a "named binding set"), NOT a layer above clients and NOT a
replacement for clients.

- **A client is a size-N binding set with a physical config-rewrite side**
  (the hub writes `mcphub-hub` into `~/.claude.json`). A client id is
  meaningful to the *adapter* layer (config rewriting) AND to the *hub*
  layer (the bindings key).
- **A group is a binding set with NO physical client** — a pure hub-layer
  key. The operator points whatever client they like at the group URL by
  hand (or, later, the adapter writes it for them). It generalizes
  per-client targeting: per-client targeting is "the binding set named
  after a client"; a group is "a binding set named whatever the operator
  wants."
- Keeping groups and clients in the **same keyspace** (rather than a layer
  above) means the entire dispatch pipeline (`AggregateInitialize`,
  `resolveToolsCallRoute`, `daemonStillBound`, the per-session RouteMap)
  is reused byte-for-byte. The ONLY thing that changes is **where the key
  comes from** (URL prefix `/clients/` vs `/groups/`) and **how the key's
  binding list + tool filter are populated** (manifest `client_bindings`
  vs a new groups config).

Why not "group is a layer above clients" (a group = a set of clients):
that forces a two-level resolution (group → clients → daemons), reintroduces
the physical-client coupling groups exist to escape, and makes
"a group of servers, no client involved" — the literal ask — awkward. A
group is a set of *servers*, not a set of clients.

---

## Endpoint / URL shape

Add a **sibling route** that coexists with the existing client route
without touching it:

```
EXISTING (unchanged):  http://127.0.0.1:<hubport>/clients/<client>/mcp
NEW (additive):        http://127.0.0.1:<hubport>/groups/<group>/mcp
```

- `mux.Handle("/groups/", handler)` is added next to the existing
  `mux.Handle("/clients/", handler)` (`hub_listener.go:225`). The bare
  `/groups` no-trailing-slash 404 guard (`:242`) and the `path.Clean`
  pre-filter (`:258-264`) are mirrored so the new route inherits the
  same anti-redirect / empty-404 contract.
- A `parseHubPathFromURL` generalization of `parseClientPathFromURL`
  returns `(kind, name)` where `kind ∈ {client, group}` — same strict
  shape rules (no embedded slash/whitespace), just two prefixes. The
  handler's gate-2 becomes: client kind → `isSupportedClient`; group kind
  → `isKnownGroup`.
- The auth-token keyspace (`ConstantTimeCompareToken(key, tok)`, gate 4 at
  `hub_mcp_handler.go:210`) is **already keyed by a string** — groups get
  their own per-group hub token in the same table, minted at group-create
  time. This is exactly the seam the §D per-group auth attaches to later
  (v1: same loopback-only posture as clients; v2: a real bearer key).
- **A reserved-name rule prevents collision:** a group name MUST NOT equal
  any `clients.SupportedClientNames()` value (and vice-versa, install
  refuses a client whose name shadows a group). Because the route prefix
  already disambiguates `/clients/foo` from `/groups/foo`, this is a
  *usability* guard (avoid operator confusion), not a correctness one.

**Why a sibling route, not a unified `/<namespace>/mcp`:** a unified route
would require either a reserved-name registry to tell client-ids from
group-ids at parse time (fragile) or dropping the `/clients/` route
(breaks every existing gate-ON client config — see Migration). The
sibling route is strictly additive and keeps the two keyspaces visibly
distinct in logs, URLs, and the token table.

---

## Config model — where groups are DEFINED, who owns it, how it reconciles

A group is **NOT** a manifest concept (manifests are per-server; a group
spans servers). It needs a **new top-level config artifact owned by the
GUI/install control plane**, reconciled into the same published
`ResolverSnapshot` the per-client bindings already feed.

**Owner: a new `groups.yaml` in the state dir**, written through the
existing hardened state-file pipeline (the same
`writeHubMcpStateFile` / `SecureWriteClientConfig` posture every other
hub state file uses — see CLAUDE.md "Hardened state-file writes"). Shape:

```yaml
# <state-dir>/groups.yaml  (v1)
version: 1
groups:
  - name: "frontend"
    description: "JS/TS dev tools"
    servers:                      # server names (manifest .Name)
      - "serena"
      - "mcp-language-server"
    tool_filter:                  # OPTIONAL per-server allow/deny (deferred-friendly)
      mcp-language-server:
        mode: "allow"             # allow | deny
        tools: ["definition", "references", "hover"]
```

- **Resolution into the snapshot.** `BuildResolverSnapshotFromManifests`
  (`hub_mcp_resolver.go:116`) gains a sibling input: a groups list is
  resolved against the same manifests (server name → its daemons → ports)
  to produce `Bindings["<group>"] = [...refs]`, written into the SAME
  `Bindings` map under the group key. The existing dedupe `seenKey`
  (`:128-159`) extends unchanged. One published snapshot carries BOTH
  client bindings and group bindings — the dispatch path can't tell them
  apart, which is the point.
- **Reconcile + restart survival.** Groups republish through the existing
  `BumpResolverOnManifestChange` choke point (rename it / wrap it as
  `BumpResolverOnConfigChange(manifests, groups)`), so a group edit is an
  atomic snapshot swap with zero daemon restart — it only changes which
  daemons a *future* group-session fans out to. On hub startup the groups
  file is read alongside manifests and folded into the first published
  snapshot. The file is the durable truth; the snapshot is the in-memory
  cache (same ownership split the rest of the hub already uses).
- **Why a separate file, not derived from per-client sets:** deriving a
  group from existing per-client bindings (option A below) ties group
  membership to which physical clients exist — exactly the coupling groups
  exist to remove. A first-class file lets a group name servers directly,
  independent of any installed client.

---

## Per-group tool visibility

Two layers, both already have a choke point:

1. **Server-subset (coarse, v1 core).** A group's `servers:` list IS its
   binding subset. `AggregateInitialize` already fans out only over the
   session's participants (`hub_mcp_aggregator.go:181-185`), and the
   merged tools/list only contains tools from those daemons. So a group
   that names 2 of 12 servers already exposes only those 2 servers' tools
   — **the "don't dump all tools" win falls out of the binding subset for
   free**, no new code in the dispatch path.

2. **Per-server tool allow/deny (fine, v1-optional / deferrable).** The
   per-session `RouteMap` is built at tools/list merge from per-daemon
   responses (`hub_mcp_resolver.go:21-25`). Adding a filter there — drop
   `<Server>__<RawName>` entries that fail the group's `tool_filter` —
   keys the visibility on the group. `resolveToolsCallRoute`
   (`hub_mcp_aggregator.go:573`) needs NO change: a filtered tool simply
   never enters the RouteMap, so a call to it already returns the existing
   `-32601 "Method not found"` (`:598`). The filter is a pure addition at
   the merge step; the call path's failure mode is reused.

**How resolution keys on the group instead of the client:** `sess.Client`
is repurposed as `sess.ScopeKey` (the binding-map key), set from the URL's
`(kind, name)`. `daemonStillBound(snap, sess.ScopeKey, ref)` looks up
`snap.Bindings[scopeKey]` identically whether the key is a client id or a
group name. The tool filter is the only group-aware branch, and it lives
at the merge step, not the hot dispatch path.

---

## Multi-tenant / team + auth + smart-routing hooks (note, don't build)

The v1 design must leave these seams clean:

- **Per-group auth.** The hub token table is already string-keyed
  (`ConstantTimeCompareToken(scopeKey, tok)`, `hub_mcp_handler.go:210`).
  A group gets a row at create time. v1 keeps loopback-only (same as
  clients today); §D adds a real bearer key per group WITHOUT changing the
  keyspace — the gate-4 compare is unchanged, only the token's lifecycle
  and transport posture differ. **Decision recorded for the planner:** do
  NOT special-case client-vs-group in gate 4; both are scope keys.
- **Team scoping.** A team is "a group + an owner principal." v1 ships the
  group; the principal column is a v2 additive field on `groups.yaml`. No
  schema decision is forced now beyond "groups have a stable name that an
  ACL can reference."
- **Smart-routing.** The group's merged tools/list IS the candidate set a
  semantic router ranks. v1 produces that scoped set deterministically;
  the router is a later consumer that swaps "return all group tools" for
  "return top-k by embedding." Keep the tools/list merge a pure function of
  `(group, manifests)` so the router can wrap it.

---

## Migration / compatibility

- **Existing `/clients/<client>/mcp` URLs keep working byte-for-byte.**
  The `/clients/` route, the per-client bindings, the Servers-matrix
  toggles, and `BuildResolverSnapshotFromManifests`'s client path are all
  untouched. Groups are a sibling route + an additional input to the same
  snapshot builder.
- **Per-client toggles unchanged.** The Servers matrix continues to write
  `client_bindings` into manifests; groups are edited in a NEW surface
  (Groups screen / `groups.yaml`), never co-mingled with the per-client
  manifest field.
- **`groups.yaml` absent = today's behavior exactly.** No file → empty
  group set → snapshot identical to current. Additive-by-omission.
- **Removal / rename / default-flip semantics (the feature-gate
  requirement).** A group is a single owning artifact (`groups.yaml`).
  Deleting a group: drop its `Bindings[name]` key on the next snapshot
  publish AND drop its token-table row AND (best-effort) emit a structured
  event so an operator who still has a client pointed at the dead group
  URL gets a `-32601`/401 rather than silent wrong-routing. Renaming =
  delete-old + create-new (URLs are not auto-migrated; documented).
  Persisted `groups.yaml` rows for a since-removed *server* are skipped at
  resolve time (same defensive skip as the missing-daemon-port case,
  `hub_mcp_resolver.go:144-149`) and surfaced as a validation warning in
  the GUI — never a hard hub-start failure.

---

## Options

### Option A — groups as config-only alias over existing per-client sets

A group is sugar: `groups.yaml` lists *client names*, and the group's
binding set is the union of those clients' existing `client_bindings`.

- **Cost:** Lowest. No new server-naming surface; reuses the manifest
  bindings verbatim. ~Small: a union step in the snapshot builder + the
  `/groups/` route.
- **Risk:** LOW mechanically but **WRONG model** (audit's own framing:
  "a group is a set of servers, not clients"). It keeps the physical-client
  coupling groups exist to remove; a "group of servers with no client"
  (the literal ask) is inexpressible. Per-server tool visibility has no
  home (clients don't carry tool filters). Paints smart-routing and
  multi-tenant into the client-coupled corner.
- **Verdict:** Rejected as the v1 shape — it cannot host visibility or the
  team seam. Mentioned because it's the cheapest and someone will propose it.

### Option B — groups as a first-class config artifact with its own screen (RECOMMENDED)

A group is a named set of *servers* in a new `groups.yaml`, with optional
per-server tool filters, resolved into the same published snapshot, served
on a sibling `/groups/` route, with its own GUI screen.

- **Cost:** Medium. New `groups.yaml` + loader; extend the snapshot builder
  with a groups input; add `/groups/` route + `parseHubPathFromURL`
  generalization + `isKnownGroup`; repurpose `sess.Client` → `sess.ScopeKey`
  (mechanical rename, single keyspace); a Settings/Groups GUI screen
  (additive, isolated like SectionClients — see
  `internal/gui/frontend/src/components/settings/SectionClients.tsx`, the
  draft/dirty/Save pattern to mirror); tool-filter at the tools/list merge
  step (deferrable to a follow-up slice).
- **Risk:** MEDIUM, contained. The dispatch hot path is reused, not
  rewritten — the only new branch is the merge-step tool filter. Highest-
  blast-radius function in this area is `BuildResolverSnapshotFromManifests`
  + the published snapshot; the change is additive (new map keys) and
  flows through the existing atomic-swap publish, so a bad group config
  degrades to "that group routes nothing," never a fleet-wide fault. The
  `⚠️ no covering tests` flag on `resolveToolsCallRoute` / `dispatchToolsCall`
  / `daemonStillBound` (per codegraph blast-radius) means **tests-first is
  mandatory** before the `sess.Client → sess.ScopeKey` rename (same
  discipline the hot-swap decision imposed on the same functions).
- **Verdict:** RECOMMENDED. Correct model, hosts visibility + the team/auth
  seam, strictly additive to the existing route, smallest durable design
  that satisfies the ask.

### Option C — unify client and group into one namespace concept

Drop the client/group distinction entirely: everything is a `namespace`
served on `/<namespace>/mcp`, and a "client" is just a namespace the
adapter layer also rewrites a physical config for.

- **Cost:** HIGH. Requires a unified parse with a reserved-name registry,
  rewriting the `/clients/` route (breaking change to every gate-ON client
  config URL on disk), and reconciling two ownership layers (adapter
  config-rewrite vs hub routing) into one concept.
- **Risk:** HIGH. Breaks existing client URLs (violates the
  must-keep-working constraint) unless a back-compat `/clients/` alias is
  ALSO kept — at which point you have Option B plus a migration, i.e.
  strictly more work for the same end state. The conceptual elegance does
  not buy a capability Option B lacks.
- **Verdict:** Rejected for v1. Revisit only if a future requirement
  genuinely needs clients and groups to be indistinguishable; today they
  differ (clients have a physical config side), so the distinction is real.

---

## Recommendation

**Adopt Option B.** A group is a first-class named set of servers in a new
state-dir `groups.yaml`, resolved into the existing published
`ResolverSnapshot` under its own binding key, served on an additive
`/groups/<group>/mcp` route that coexists with the unchanged
`/clients/<client>/mcp` route. The entire request pipeline is reused by
generalizing the single key `sess.Client` → `sess.ScopeKey`; the only new
hot-path-adjacent logic is a per-server tool allow/deny filter applied at
the tools/list merge step.

### v1 minimal-viable scope (ship together as one coherent workstream)

1. **`groups.yaml`** loader + hardened writer + validation (skip-and-warn
   on missing server, reserved-name collision refusal).
2. **Snapshot builder extension** — fold groups into `Bindings` under group
   keys via the existing `BumpResolverOnConfigChange` choke point;
   tests-first for the rename of `BuildResolverSnapshotFromManifests`'s
   contract.
3. **`sess.Client → sess.ScopeKey` generalization** — mechanical, single
   keyspace; **covering tests added FIRST** for `resolveToolsCallRoute` /
   `dispatchToolsCall` / `daemonStillBound` (they have none today).
4. **`/groups/` sibling route** + `parseHubPathFromURL(kind,name)` +
   `isKnownGroup`, mirroring the `/clients/` anti-redirect / empty-404 /
   `path.Clean` guards.
5. **Per-group hub token row** in the existing token keyspace (loopback-only
   posture, same as clients) — establishes the §D auth seam without
   building auth.
6. **Server-subset visibility** (free — falls out of the binding subset).
7. **GUI Groups screen** — additive, isolated, draft/dirty/Save pattern
   mirroring `SectionClients.tsx`.

### Deferred slices (named, sequenced, NOT in v1 unless cheap)

- **Per-server tool allow/deny filter** at the merge step (the fine-grained
  visibility). Ship right after v1 if the merge-step seam lands clean; it's
  the cheapest first cut at tool-context-bloat *without* embeddings (audit
  #3).
- **Adapter-writes-the-group-URL** (so the operator doesn't hand-edit a
  client to point at a group) — a per-client "join group X" affordance.
- **Per-group bearer-key auth** (§D) — the token row exists; v2 gives it a
  real key + transport posture.
- **Smart-routing** over a group's merged tools/list (audit #5) — the group
  is its home; sequence last.

---

## Claims (falsifiable guarantees this design makes — input to architecture-reviewer)

1. The existing `/clients/<client>/mcp` route, its handler gates, and the
   per-client `client_bindings` manifest path are **NOT modified** — groups
   attach at the new `/groups/` route and a new `groups.yaml` input only.
2. The dispatch hot path (`AggregateInitialize`, `resolveToolsCallRoute`,
   `dispatchToolsCall`, `daemonStillBound`) gains **exactly one** new
   behavior key change — the `sess.Client → sess.ScopeKey` rename — and
   **zero** new client-vs-group conditionals in the call path; the only
   group-aware branch is the tools/list-merge tool filter.
3. The published `ResolverSnapshot` remains a single atomic-swap pointer;
   groups add map keys to `Bindings`, introducing **no second snapshot,
   no second publish channel, no new shared mutable state**.
4. With `groups.yaml` absent, the published snapshot and all routing
   behavior are **byte-identical to today** (additive-by-omission).
5. A malformed / partially-stale group config degrades to "that group
   routes nothing / a `-32601` per missing tool," **never** a hub-start
   failure or a cross-group mis-route (reuses the existing missing-daemon
   skip + `-32601 "tool moved out of scope"` paths).
6. The feature has a **single owning gate** (`groups.yaml` + the snapshot
   builder); removing/renaming/default-flipping a group is handled by the
   one owner (snapshot key drop + token-row drop + event), with **no
   scattered consumer-side group checks**.
7. The per-group auth seam reuses the **existing string-keyed token table**
   (gate 4); no auth mechanism is built in v1, but no design choice blocks
   adding a per-group bearer key later without a keyspace change.

---

## Open questions for the operator

1. **Solo-vs-team intent for v1.** Is v1 strictly the solo-dev
   tool-visibility win (one operator, several named tool profiles like
   "frontend" / "infra" / "writing"), with team/auth explicitly v2? Or do
   you want the per-group token row wired loopback-only NOW so the §D auth
   seam is physically present from day one? (Recommendation: ship the token
   row now — it's nearly free and it's the keystone seam — but keep it
   loopback-only / no real key in v1.)

2. **Naming + URL shape.** Confirm `/groups/<group>/mcp` as the sibling
   route (vs a unified `/<namespace>/mcp`, which Option C shows breaks
   existing client URLs). And confirm the reserved-name rule (a group name
   may not equal a supported-client name) — acceptable, or do you want
   groups in a visibly separate namespace token (e.g. `/g/<group>/mcp`) to
   make collisions structurally impossible?

3. **Tool-filter granularity in v1.** Is the coarse server-subset
   visibility (a group exposes only its named servers' tools — free, falls
   out of the binding subset) enough for v1, with per-server tool
   allow/deny deferred to the very next slice? Or is the fine-grained "hide
   tool Z inside server S" needed in the first cut? (Recommendation: ship
   coarse v1, fine-grained as the immediate follow-up — it's the cheapest
   anti-bloat cut before any embeddings work.)

4. **Is smart-routing or auth in scope for THIS workstream at all?** The
   recommendation explicitly sequences both AFTER the group/endpoint model
   exists (they have no home until it does). Confirm they stay deferred so
   v1 doesn't balloon.

---

## Gate

The design is traceable to accepted research (competitor audit §3/§5) and
to verified live code (every "does X today" claim cites `file:line`).
Alternatives, the endpoint shape, config ownership, the extension seams
(token keyspace, tools/list merge filter, snapshot builder input),
dependency direction (groups depend on the snapshot builder + manifests;
nothing in the existing client path depends on groups), expected blast
radius, failure modes, and the test-first requirement on the untested hot
path are all explicit. No implementation code is included.

**Gate decision: PASS** — design is decision-ready; promotion of this
`status: proposed` decision to `accepted` is the `architecture-reviewer`
gate's call after the operator answers the open questions above.

---

## Phase 3 diagnostic finding (2026-06-18) — Defect A operationally CONFIRMED

Code-traced (read-only, pre-implementation per the diagnostic-first gate): the
`ResolverSnapshot` publish path (`PublishResolverSnapshot`/`BumpResolverOnManifestChange`,
hub_mcp_resolver.go:90/169) has **ZERO production callers** — only test files publish it
(hub_mcp_{aggregator,resolver,scope_characterization}_test.go). `startHubMcpListener`
(internal/gui/hub_listener.go:132) and `BindHubMcpListener` (hub_mcp_bind.go:81) do NOT
publish on startup. `IntendedParticipants` is set at exactly ONE site (hub_mcp_handler.go:602)
inside `if snap != nil` — and `snap` is always nil in prod (handler.go:584
`LoadResolverSnapshot()`).

**Operational impact:** in gate-ON hub-aggregate mode, every session gets EMPTY
`IntendedParticipants` → `AggregateInitialize` fans out to nothing → the aggregate exposes
no tools. **The gate-ON hub aggregate is effectively dormant/non-functional in production.**
NOT currently impacting the operator: gate-ON is OFF in the live config (no
`hub_endpoint_enabled` key in gui-preferences.yaml), and at gate-OFF `startHubMcpListener`
short-circuits at line 133 (`enabled=false`) so the listener never runs and the snapshot
reads never happen. The hub aggregate's manual-publish unit tests masked this prod gap.

**Implication for groups Phase 3:** wiring the publish choke point (build snapshot from
manifests + `PublishResolverSnapshot` on hub-listener startup + on manifest change while
gate-ON) is (a) the groups prerequisite, (b) the fix for the dormant gate-ON aggregate, and
(c) INERT for the operator's current gate-OFF setup (the listener isn't running, so a
published snapshot is read by no one). Lower risk than a generic live-request-path change.
Still tests-first + a gate-ON integration test that exercises the REAL publish path (not the
manual-publish test fixtures) before this lands.

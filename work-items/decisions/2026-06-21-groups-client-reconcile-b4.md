---
status: proposed
date: 2026-06-21
slug: groups-client-reconcile-b4
supersedes: none
relates: work-items/decisions/2026-06-18-groups-namespaces-tool-visibility.md
backlog: work-items/backlog/2026-06-19-groups-v1-followups.md (B4-full)
---

# B4-full: reconcile group endpoints into client configs

## Status

**Proposed — awaiting operator go-ahead to plan + implement.** The operator has accepted the data-model call
(Model B) and chosen to defer the build until they green-light it (and the Codex bot rate-limit clears). This
record preserves the architect design so a `$planner` phase can start on demand.

## Problem

Today a group's `/g/<group>/mcp` connection triple (URL + hub token + instance_id) is SURFACED in
`/api/groups` + the GUI Groups screen for the operator to HAND-COPY into a client config. B4-full = a reconcile
path that WRITES group endpoints directly into client config files (plug-and-play, no hand-copy), mirroring
`mcphub install --reconcile-hub-mode` (which writes the single `mcphub-hub` aggregate entry into every client).

## Decision (operator-confirmed)

**Model B: a separate group→client binding store + a GUI matrix — NOT a `clients:` list in `groups.yaml`.**
A `clients:` field on `Group` would re-couple a group to physical clients, the exact coupling the accepted
`2026-06-18-groups-namespaces-tool-visibility.md` decision removed ("a group is a binding set with NO physical
client — a pure hub-layer key"). The mapping is operator *routing intent* with its own lifecycle (re-wired
often, e.g. after a port reset), so it lives in a separate `group-client-bindings` store driven by a
per-group×per-client matrix in the GUI Groups screen (mirroring the Servers matrix UX).

**Scope: full feature, phased, await go-ahead** (operator chose this over building Phase 1 immediately).

## Design (architect, PASS — verified at HEAD 9747900a)

- **~70% reuse:** a new thin op-source `BuildGroupsReconcilePlan(bindings, groups, endpoint, tokens)
  []ClientUpdatePlan` that emits one `AddReplace mcphub-g-<group>` per (group, target-client) pair + `Remove`
  for unmapped pairs, applied via the existing `ApplyHubReconcileInOrder` → `applyOpsForClient` →
  `SecureWriteClientConfig` pipeline. The `mcphub-hub` gate reconcile (`BuildHubReconcilePlan`) is NOT touched.
- **Client entry:** `mcphub-g-<group>` — a reserved name family (sibling to `mcphub-hub`), reserved at
  manifest validation so a server manifest can't collide. Shape = `{URL: .../g/<group>/mcp, Headers:
  {X-Mcphub-Hub-Token, X-Mcphub-Instance-Id}}` from the existing `groupConnection` assembly. Coexists with
  `mcphub-hub` (a client can have both — distinct names).
- **GUI-first, CLI deferred:** the matrix lives next to the copy-rows it replaces; a CLI subset flag
  (`--group X --client Y`) is a worse ergonomic than a matrix — add it in Phase 2 if headless demand appears.
- **Phasing:** Phase 1 = single-client "wire this group to client X" + the **port-reset safety gate** + the
  reserved-name guard (~90% of the value at ~40% risk; closes a real silent hole — see below). Phase 2 = the
  full matrix + scan-classify recognition + the CLI verb.
- **Two load-bearing gaps the design surfaces:**
  1. **C7 port-reset orphan (safety):** the `--reset-port` exit-8 gate keys ONLY on `mcphub-hub`, so a port
     reset **silently orphans group `/g/` URLs** (CLAUDE.md C7). Phase 1 extends the gate to detect
     `mcphub-g-*` entries (a `GroupBoundClients` sibling to `GatedOnClients`, sharing one entry-prefix probe).
  2. **scan classify:** `scan.go::classify` recognizes hub-ownership by daemon-port match only, so a
     `/g/<group>/mcp` URL falls to `external` (same as `mcphub-hub`, which is handled by a separate name-keyed
     probe). Group entries need their own name-keyed recognition (Phase 2) or a marker (Phase 1).
- **Lifecycle:** delete-group emits `Remove mcphub-g-<group>` for every wired client; uncheck-cell removes one;
  writes are gated on `hubLive` (no dead-URL writes at gate-OFF).
- **Scope/risk:** ~1,200-2,000 LOC across both phases. NOT one bounded PR. High consequence (client-config
  writes via `SecureWriteClientConfig`) but contained surface (additive op-source + two recognition
  extensions). 8 falsifiable claims in the architect package (session transcript).

## Out of scope / not chosen

- Model A (`clients:` in `groups.yaml`) — rejected (re-couples group↔client; contradicts the accepted invariant).
- Building Phase 1 now — deferred to operator go-ahead.

## Next step

On operator go-ahead: a `$planner` phase splits this into Phase 1 / Phase 2 commit-ready plans, starting with
the binding store + `BuildGroupsReconcilePlan` + the port-reset safety gate.

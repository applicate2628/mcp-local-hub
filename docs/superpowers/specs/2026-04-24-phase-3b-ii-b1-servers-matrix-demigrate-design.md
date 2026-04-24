# Phase 3B-II B1 — Servers matrix uncheck-to-demigrate (design memo)

**Status:** design memo (pre-plan)
**Author:** Claude Code (context-recon + user-Codex alignment session)
**Date:** 2026-04-24
**Successor of:** PR #16 (Phase 3B-II A2b edit-mode + 9 security PRs merged to master `4df519b`)
**Backlog entry:** `docs/superpowers/plans/phase-3b-ii-backlog.md` §B1

## 1. Summary

B1 unlocks checkbox-driven reverse-migration in the Servers matrix.
Pre-recon finding: `api.Demigrate` + `/api/demigrate` handler + Migration-screen per-row Demigrate button are **already implemented and merged**. The only remaining B1 gap is the Servers matrix checkbox wiring: today `via-hub` cells are `disabled` per PR #5 R17 because the original Apply flow only called `/api/migrate`, which is idempotent on already-migrated bindings → a silent no-op would result from any uncheck attempt.

This memo covers:

- Enabling the `via-hub` checkbox in the Servers matrix.
- Extending `applyChanges` in `Servers.tsx` to pick `/api/migrate` vs `/api/demigrate` per-change based on direction.
- Reusing the existing per-change sequential orchestration and error surface (no new endpoint, no parallel-apply, no new partial-success UI).
- Closing follow-up item #2 from `a2b-combined-pr-followups.md` as doc-only (`api.NewAPI()` is zero-cost; `EventBus` is an empty struct, `newEventBus()` spawns nothing).
- Filling three test-coverage gaps on `/api/manifest/edit` flagged in the same memo (item #3).

## 2. Context recon

### 2.1 Backend layer — ALREADY DONE

- `internal/api/demigrate.go`: `API.Demigrate(opts DemigrateOpts) (*DemigrateReport, error)`. Rolls (server, client) pairs back by reading each client's most recent backup and writing the named entry (or removing it if the backup predates the migrate that added it). Handles multi-server / repeat-migrate edge cases with sentinel fallback + explicit refuse on "server added after sentinel then migrated twice". 8+ tests in `internal/api/demigrate_test.go`.
- `internal/gui/demigrate.go`: `registerDemigrateRoutes(s *Server)` wires `POST /api/demigrate` with the `demigrateRequest{Servers []string; Clients []string}` body, same-origin guard, 204 on success, 500 on `DEMIGRATE_FAILED`. 3 handler tests in `internal/gui/demigrate_test.go`.
- `internal/gui/server.go`: `s.demigrater` interface field is wired to a `realDemigrater` adapter using `api.NewAPI().Demigrate(...)`.

### 2.2 Migration-screen UI — ALREADY DONE

`internal/gui/frontend/src/screens/Migration.tsx` renders the `Via hub` group with a per-row `Demigrate` button that `POST`s to `/api/demigrate` with `{servers: [name]}` (empty `clients` = roll back every binding in the manifest). Error surface is inline in the group header. Fully exercised by the `migration.spec.ts` E2E suite.

### 2.3 Servers matrix UI — THE REMAINING GAP

`internal/gui/frontend/src/screens/Servers.tsx:231` currently disables the checkbox for `routing === "via-hub"`:

```tsx
//  - "via-hub": MVP has no reverse-migrate API yet (Phase 3B-II B1).
//    Allowing uncheck would let the user dirty, Apply, and receive a
//    silent no-op because MigrateFrom is idempotent on already-migrated
//    bindings.
const disabled = routing === "unsupported" || routing === "not-installed" || routing === "via-hub";
```

The tooltip at line 234 also still points users at `mcphub rollback --client <name>` as the workaround — that CLI path is the user-space equivalent of what we are about to wire into the UI, and the tooltip is now misleading.

`applyChanges` (same file, lines ~85–123) iterates the `dirty` map and `POST /api/migrate` for every dirty change. It collects failures into a `failed: string[]` array and renders `Failed: ...` in `apply-msg` on the toolbar. The `reloadToken` bump at the end triggers a `/api/scan` + `/api/status` refetch. **This is the exact shape we extend for demigrate — we just branch the endpoint per change direction.**

### 2.4 `api.NewAPI()` lifecycle — VERIFIED ZERO-COST

`a2b-combined-pr-followups.md` item #2 flagged `api.NewAPI()` being constructed per-request inside every real adapter (`realManifestCreator`, `realManifestGetter`, `realManifestEditor`, `realManifestValidator`, `realDemigrater`) as a potential goroutine leak if `newEventBus()` spawned a background worker. Inspection of [internal/api/events.go](../../../internal/api/events.go):

```go
type EventBus struct{}
func newEventBus() *EventBus { return &EventBus{} }
```

`EventBus` is an empty struct. `newEventBus()` allocates nothing beyond the struct itself — no goroutine, no channel, no background worker. `api.NewAPI()` is a pure struct allocation. Per-request construction is cheap and safe. The followup item is a **false positive** in the current state; it should be closed as doc-only (verified).

Future-proofing (if `EventBus` gets populated later — the source comment says "Populated in Task 22"): when that work lands, the same task should introduce the shared-`*api.API` refactor as part of its own scope. Threading a shared instance **now** would be speculative coupling against a non-goroutine-spawning struct.

## 3. Scope

### 3.1 In scope (this memo)

**B1-core — Servers matrix wiring (the load-bearing change):**

1. Remove `"via-hub"` from the `disabled` predicate in `Servers.tsx` (line 231).
2. Replace the current "rollback via CLI" tooltip copy with UI-native guidance that matches the new behavior.
3. Extend `applyChanges` to branch per change direction:
   - `direct → via-hub` (check a formerly-direct cell) → `POST /api/migrate` (existing path).
   - `via-hub → direct` (uncheck a via-hub cell) → `POST /api/demigrate` with `{servers: [change.server], clients: [change.client]}` so only that one (server, client) pair rolls back.
4. Keep the existing sequential per-change loop, error-accumulation into `failed[]`, toolbar `apply-msg` surface, and `reloadToken` bump. No new batch endpoint, no Promise.all concurrency, no partial-success UI.
5. E2E regression scenarios (see §6).

**B1-prep — `api.NewAPI()` followup closure (doc-only):**

1. Update `work-items/bugs/a2b-combined-pr-followups.md` item #2: move to FIXED/verified section with a pointer to `internal/api/events.go` proving zero-cost construction.
2. No code change.

**Coverage debt clean-up (a2b-combined-pr-followups.md item #3):**

1. `TestManifestEditHandler_EmptyName_400` (Go) — `POST /api/manifest/edit` with `{name: ""}` returns 400 BAD_REQUEST.
2. `TestManifestEditHandler_MalformedJSON_400` (Go) — `POST /api/manifest/edit` with `{not-json` returns 400 BAD_REQUEST.
3. `TestManifestEditHandler_RejectsNonPOST_405` (Go) — `GET /api/manifest/edit` returns 405 Method Not Allowed.

These mirror existing `/api/manifest/get` and `/api/manifest/create` handler coverage and are trivially written against the existing fake infrastructure.

### 3.2 Out of scope (not this memo)

- **Auto-stop hub daemons when last client unbinds.** Demigrate restores client configs; it does not touch `internal/scheduler` or the running hub daemons. After the last via-hub checkbox uncheck for a server, the hub daemon keeps running with no clients. User can stop it via `mcphub stop <server>` (CLI). Auto-stop would involve scheduler-level state reasoning and per-binding reference counting that belongs to a separate effort. Confirmed out-of-scope with user 2026-04-24.
- **Backup cleanup.** Demigrate reads client backups but does not delete them. Stale backups are left as-is (by design, for manual recovery).
- **Shared `*api.API` refactor.** Speculative coupling; deferred to when `EventBus` is actually populated.
- **Batch/transactional `/api/apply` endpoint.** Deferred; may surface later if a real "batch with atomicity" requirement appears.
- **Parallel Apply (Promise.all per direction group).** Rejected: conflict-order risk + no measurable user-visible latency win at checkbox-matrix scale (handful of changes typical).

## 4. Decisions

### D1 — Sequential per-change orchestration in `applyChanges` (variant A)

**Chosen.** Each dirty entry fires its own POST; endpoint selected by direction:
`direction = initialChecked ? "demigrate" : "migrate"` (`initialChecked === true` means the cell was already `via-hub` at load time → user is rolling it back).

Codex advisory concurs (2026-04-24 consultation): variant A has the best user-value / risk / blast-radius ratio for this milestone. Variants B (parallel) and C (new `/api/apply` endpoint) were both rejected.

### D2 — Demigrate request shape

Per-change narrow: `POST /api/demigrate` with `{servers: [change.server], clients: [change.client]}`. This matches `DemigrateOpts.ClientsInclude` semantics — rolls back only the (server, client) pair the user unchecked, not every client bound to that server. Users can demigrate every binding from the Migration screen's per-row button (which sends `{servers: [name]}` with empty `clients`).

### D3 — Error surface

Reuse the existing `failed: string[]` pattern in `applyChanges`. On a failed POST, push `${change.server}/${change.client}: ${body.error ?? resp.status}` and continue with remaining changes. Render `Failed: <joined>` in the toolbar's `apply-msg` span. No inline per-row error (would require row-state plumbing; not a B1 requirement).

### D4 — Dirty-map tracking

Existing dirty map already records `(server, client) → {next, initial}` and the `applyChanges` closure has access to `initialChecked` for each change. No schema change needed; direction derives from existing fields.

### D5 — Tooltip copy for `via-hub` cells

Replace line 234:

> `Already routed through the hub. To disable, run `mcphub rollback --client ${client}` (Phase 3B-II will add a UI for this).`

with:

> `Currently routed through the hub. Uncheck and Apply to roll this binding back to the original ${client} config.`

The new copy is active (describes the enabled action), references the checkbox semantic directly, and no longer points users at the CLI workaround.

### D6 — No optimistic UI

Keep the existing pattern: after Apply dispatches all changes (success or partial), bump `reloadToken` to trigger a fresh `/api/scan` + `/api/status` fetch. The new checkbox state for via-hub cells will reconcile from the authoritative backend scan — consistent with how migrate-direction changes already reconcile.

### D7 — Apply-button disabled predicate

Already correct: `applying || dirty.size === 0`. Any dirty entry (regardless of direction) enables Apply.

### D8 — Same-origin guard on `/api/demigrate`

Already in place from the existing handler. No change.

### D9 — Scan reload after mixed Apply

The existing post-Apply reload already picks up both directions because `/api/scan` re-reads every client config and recomputes `routing` per (server, client). No per-direction reload logic needed.

### D10 — Manifest cascade when Demigrate removes all bindings

`api.Demigrate` does not modify the manifest's `client_bindings` list; it only restores client configs. The manifest stays authoritative as the list of *allowed* bindings. A future operator re-running `mcphub migrate` (or the user checking the matrix box again) re-migrates using the same binding row. This is consistent with existing Migration-screen Demigrate semantics.

## 5. Risks

### R1 — R17's original concern still valid for direct→via-hub→direct flapping

Imagine a user checks a `direct` cell (queues migrate), then in the same Apply session unchecks a different `via-hub` cell (queues demigrate). Both POSTs fire sequentially. If they happen to touch the same backup file for the same client (e.g., user is migrating server-A while demigrating server-B on claude-code), the migrate's backup rotation and the demigrate's backup read could race at the filesystem level. Mitigation: `api.Migrate` and `api.Demigrate` both serialize through the client's backup-lock (existing `internal/clients` pattern); filesystem-level ordering is preserved because the POSTs run sequentially from the client side and the backend is the same single-writer process. No new mitigation needed.

### R2 — Demigrate on a server not in via-hub

The user unchecks a cell that is "via-hub" at page load but a background external-config change between load and Apply moved it to "direct". The POST `/api/demigrate` fires for a server whose latest-backup no longer contains a hub-routed entry → `Demigrate` auto-falls-back to the `-original` sentinel, or if the sentinel lacks the entry, refuses with a Failed row that surfaces in `apply-msg`. Existing behavior; no new defense needed.

### R3 — Empty `clients` slice semantics

`DemigrateOpts.ClientsInclude == nil` means "every client in the manifest's `client_bindings`", while `ClientsInclude == []string{}` (empty non-nil) means "no filter applied"... need to verify the existing handler normalizes this. Task 1 of the plan should grep `demigrate.go` for the zero-length behavior and fix the frontend to always send a non-empty `clients` slice for the per-cell case to match the narrow-rollback intent. (Migration per-row button already sends `servers: [name]` with no `clients` field, which serializes as `null` and means "every binding" — that is the intended broad rollback for that UI.)

### R4 — Tooltip copy change breaks an existing E2E assertion

The current tooltip text "To disable, run `mcphub rollback..." is a candidate substring match for some E2E assertion. Plan should grep for assertions on the old text and update or delete them alongside the copy change.

## 6. Testing

### 6.1 Go unit tests (new)

- `TestManifestEditHandler_EmptyName_400` (follow-up #3 coverage gap)
- `TestManifestEditHandler_MalformedJSON_400` (follow-up #3 coverage gap)
- `TestManifestEditHandler_RejectsNonPOST_405` (follow-up #3 coverage gap)

No new `internal/api/demigrate_test.go` or `internal/gui/demigrate_test.go` tests — the backend and handler are already covered.

### 6.2 Frontend unit tests (no new file)

`Servers.tsx` is pure-render-from-props plus network callback. Its behavior is best covered at E2E level (already the A2a + A2b pattern); Vitest coverage would need fetch mocks that rehearse an integration that E2E exercises faithfully.

### 6.3 Playwright E2E (new scenarios in `internal/gui/e2e/tests/servers.spec.ts`)

1. **"uncheck via-hub + Apply posts /api/demigrate"** — seed a manifest + scan-result fixture where cell `(demo, claude-code)` is `via-hub`; load Servers matrix; intercept `/api/demigrate` on the page; uncheck the cell; click Apply; assert the intercepted POST body is `{servers: ["demo"], clients: ["claude-code"]}` and the row refreshes to show `direct` (or the cell reflects whatever the post-reload scan returns).
2. **"mixed Apply dispatches both endpoints"** — seed two rows: `(a, claude-code)` at `direct`, `(b, claude-code)` at `via-hub`. User checks the first and unchecks the second. Apply dispatches `/api/migrate` then `/api/demigrate` in dirty-map order. Assert both requests posted with their respective narrowed bodies.
3. **"demigrate failure surfaces in apply-msg"** — stub `/api/demigrate` to 500 with a generic body. Uncheck a via-hub cell, Apply. Assert the toolbar `apply-msg` contains `Failed:` and the row stays at `via-hub` after the reload (backend truth wins). Checkbox resets to its pre-change state via the existing reloadToken refresh.
4. **"tooltip copy reflects uncheck-to-demigrate semantic"** — assert the `title` attribute on a `via-hub` cell contains "Uncheck and Apply to roll this binding back" (or the exact substring we land on). Remove/update any prior assertion that expected the `mcphub rollback --client` phrase.

Target: `internal/gui/e2e/tests/servers.spec.ts` grows from 3 → 7 scenarios (4 new). Total E2E suite 47 → 51.

### 6.4 Cross-suite regression

No existing `servers.spec.ts`, `add-server.spec.ts`, `edit-server.spec.ts`, `migration.spec.ts` tests should regress. Plan includes a verification step to re-run the full Playwright suite after B1-core lands.

## 7. Handoff contract to the writing-plans skill

The plan should derive from this memo with the following commit breakdown:

1. **doc-only:** close a2b-combined-pr-followups item #2 as verified (prep). Single commit updating the memo.
2. **Go tests:** add the 3 `/api/manifest/edit` coverage tests. Single commit; no production-code changes.
3. **Servers matrix wiring:** enable `via-hub` checkbox, extend `applyChanges` with direction-branching, update tooltip copy. Single commit including regenerated assets (`go generate ./internal/gui/...`).
4. **E2E:** add 4 scenarios in `servers.spec.ts`; update any `mcphub rollback` tooltip assertion elsewhere. Single commit.
5. **Docs:** update `CLAUDE.md` E2E coverage line (47 → 51). Single commit.

Estimated scope: **~5 commits, ~150–250 LOC total** (mostly TS/TSX and TS test code; minimal Go — 3 short handler tests).

## 8. Self-review checklist

- [ ] Placeholder scan — none found.
- [ ] Internal consistency — §3 scope matches §4 decisions matches §7 handoff.
- [ ] Ambiguity check — demigrate body shape is explicit (`{servers: [one], clients: [one]}` per-cell vs `{servers: [name]}` no-clients for the Migration-screen bulk case).
- [ ] Scope check — single-milestone-sized, 5 commits, one branch.
- [ ] Decision lock — D1 (variant A) confirmed with user + Codex advisory; D10 (no manifest cascade) explicit.

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
- Closing follow-up item #2 from `a2b-combined-pr-followups.md` as doc-only (`api.NewAPI()` is cheap and spawns no background resources — `EventBus` is an empty struct, `newEventBus()` has no goroutine / channel / worker; the only allocations are the `&State{Daemons: map}` and struct headers themselves).
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

The tooltip at line 234 also still points users at `mcphub rollback --client <name>` as the workaround. **That CLI invocation does not exist:** `mcphub rollback` ([internal/cli/rollback.go:19](../../../internal/cli/rollback.go#L19)) takes only `--original` (restore whole-client pristine sentinel) — no `--client` flag and no per-binding granularity. The tooltip has always been wrong, not just stale, and B1 replaces it with action-oriented GUI guidance (see §4 D5) rather than trying to point at any CLI command. **[R5 P3 on this memo corrected an earlier claim that the old tooltip was a "user-space equivalent" of the Matrix Apply we are wiring.]**

`applyChanges` (same file, lines ~85–123) iterates the `dirty` map and `POST /api/migrate` for every dirty server-group. It collects failures into a `failed: string[]` array and renders `Failed: ...` in `apply-msg` on the toolbar. The `reloadToken` bump at the end triggers a `/api/scan` + `/api/status` refetch.

**Critical:** today's `dirty` state is `Map<string, Set<string>>` (server → set of clients). Per-cell **direction is not preserved** — the map only records "this cell is dirty", not "flipping from direct to via-hub or vice versa". `toggleCell` uses the `initialChecked` value at the call site to decide whether to add/remove the entry, but does NOT persist direction into the map. `applyChanges` therefore can't know per change whether to POST `migrate` or `demigrate`. This memo's design has to fix the dirty shape before it can branch the endpoint per change. [Codex R1 P1 on this memo, §A-below.]

### 2.4 `api.NewAPI()` lifecycle — VERIFIED ZERO-COST, but `api.go` comment contradicts current use

`a2b-combined-pr-followups.md` item #2 flagged `api.NewAPI()` being constructed per-request inside every real adapter (`realManifestCreator`, `realManifestGetter`, `realManifestEditor`, `realManifestValidator`, `realDemigrater`) as a potential goroutine leak if `newEventBus()` spawned a background worker. Inspection of [internal/api/events.go](../../../internal/api/events.go):

```go
type EventBus struct{}
func newEventBus() *EventBus { return &EventBus{} }
```

`EventBus` is an empty struct. `newEventBus()` allocates nothing beyond the struct itself — **no goroutine, no channel, no background worker**. `api.NewAPI()` still allocates its own `&State{Daemons: make(map[string]DaemonStatus)}` (tiny map + struct header), so it is not *literally* zero-cost — but it spawns no background resources that would leak across requests. Per-request construction is cheap and safe; the "goroutine leak" concern is a false positive in the current state. [R4 P2 on this memo corrected an earlier overclaim of "pure struct allocation".]

**But the canonical contract is inconsistent on two fronts.** [internal/api/api.go:1–26](../../../internal/api/api.go#L1-L26):

```go
// Package api is the single source of truth for operations exposed through
// the mcp-local-hub CLI and GUI frontends. Every command the user runs (via
// cobra) or every HTTP endpoint the GUI calls dispatches into one function
// here; they never reach directly into internal/clients, internal/scheduler,
// internal/config, or internal/secrets.
//
// This enforces CLI ≡ UI parity structurally: if a capability is in api, it
// is reachable from both frontends by construction; if it is not, neither
// frontend can expose it.
package api
// ...
// API is the orchestration handle held by cli and gui. A single instance is
// created per process via NewAPI. Methods are safe for concurrent use unless
// noted otherwise.
```

Two inaccuracies:

1. **"A single instance is created per process"** is violated by GUI adapters calling `api.NewAPI()` on every request. Harmless today because `EventBus` is empty, but future readers will keep flagging this.
2. **"CLI ≡ UI parity structurally: if a capability is in api, it is reachable from both frontends by construction"** is violated by `api.Demigrate`, which is wired into the GUI (`/api/demigrate` handler + Migration button + B1 matrix Apply) but has **no CLI command** — `mcphub` exposes no `demigrate` / `rollback` subcommand that reaches `api.Demigrate`. The "by construction" claim describes an architectural aspiration, not the current state. [Codex R2 P3 on this memo.]

**Closing followup #2 therefore requires TWO doc updates, not one:** (1) the followup memo entry marked verified with an events.go pointer, AND (2) `internal/api/api.go`'s package + type comments revised to honestly describe current state on BOTH axes (the lifecycle claim AND the CLI parity claim). For the parity claim, the comment should be softened to "capabilities live in `api` so both CLI and GUI *can* reach them without skipping layers", without promising existing CLI command coverage.

Adding a CLI `mcphub demigrate` command to restore structural parity is a separate follow-up (it involves cobra wiring, CLI-flag parsing, `writeOut`/`writeErr` plumbing, and CLI E2E coverage) — out of B1 scope. Tracked below in §3.2.

Future-proofing (if `EventBus` gets populated later — the source comment says "Populated in Task 22"): when that work lands, the same task introduces the shared-`*api.API` refactor AND restates the per-process-instance contract. Threading a shared instance **now** would be speculative coupling against a non-goroutine-spawning struct. [Codex R1 P2 + R2 P3 on this memo.]

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

**B1-prep — `api.NewAPI()` followup closure (docs only, 2 files):**

1. Update `work-items/bugs/a2b-combined-pr-followups.md` item #2: move to FIXED/verified section with a pointer to `internal/api/events.go` proving no background-resource construction (empty `EventBus` struct, no goroutine/channel/worker).
2. Update `internal/api/api.go` package/type comment to accurately describe the current per-request construction pattern (the existing comment "A single instance is created per process" contradicts GUI adapter behavior and will keep confusing future readers and code-review tools). No behavior change; pure comment edit.
3. No production-code change in the Go adapters themselves.

**Coverage debt clean-up (a2b-combined-pr-followups.md item #3):**

1. `TestManifestEditHandler_EmptyName_400` (Go) — `POST /api/manifest/edit` with `{name: ""}` returns 400 BAD_REQUEST.
2. `TestManifestEditHandler_MalformedJSON_400` (Go) — `POST /api/manifest/edit` with `{not-json` returns 400 BAD_REQUEST.
3. `TestManifestEditHandler_RejectsNonPOST_405` (Go) — `GET /api/manifest/edit` returns 405 Method Not Allowed.

These mirror existing `/api/manifest/get` and `/api/manifest/create` handler coverage and are trivially written against the existing fake infrastructure.

### 3.2 Out of scope (not this memo)

- **Auto-stop hub daemons when last client unbinds.** Demigrate restores client configs; it does not touch `internal/scheduler` or the running hub daemons. After the last via-hub checkbox uncheck for a server, the hub daemon keeps running with no clients. User can stop it via `mcphub stop --server <name>` (CLI — verified against [internal/cli/stop.go:23](../../../internal/cli/stop.go#L23)). Auto-stop would involve scheduler-level state reasoning and per-binding reference counting that belongs to a separate effort. Confirmed out-of-scope with user 2026-04-24.
- **Backup cleanup.** Demigrate reads client backups but does not delete them. Stale backups are left as-is (by design, for manual recovery).
- **Shared `*api.API` refactor.** Speculative coupling; deferred to when `EventBus` is actually populated.
- **Batch/transactional `/api/apply` endpoint.** Deferred; may surface later if a real "batch with atomicity" requirement appears.
- **Parallel Apply (Promise.all per direction group).** Rejected: conflict-order risk + no measurable user-visible latency win at checkbox-matrix scale (handful of changes typical).
- **CLI `mcphub demigrate` command.** `api.Demigrate` has no cobra entry point today; adding one requires CLI flag parsing, `writeOut`/`writeErr` plumbing, human-readable per-row report formatting, and CLI-level E2E. Tracked as a follow-up; §2.4's comment rewrite acknowledges the missing parity without trying to fix it in B1.
- **Client-level serialization of backup + live-config I/O.** `internal/clients` uses unguarded `os` I/O; §5 R1 spells out the residual multi-tab / CLI+GUI race. Proper fix (in-process mutex keyed by client OR OS advisory file-lock) belongs to its own plan item; B1 ships the durable `work-items/bugs/b1-backup-file-race.md` entry.

### 3.3 Deliberate ancillary cleanup bundled with B1 (not scope drift)

Three small items adjacent to B1's code surface are bundled into the same branch because grouping them keeps the diff coherent and avoids an orphan "follow-ups sweep" PR. All three have distinct commits per §7 so they can be independently reverted if necessary. [Codex R2 P3 on this memo flagged this as potential scope drift; the framing is now explicit.]

- `api.go` package/type comment rewrite (prep commit 1) — same file as the `api.NewAPI()` followup closure; one edit, two doc gaps covered.
- Three `/api/manifest/edit` handler tests (`a2b-combined-pr-followups.md` item #3) — literally the neighboring handler file of `/api/demigrate`, one commit of Go tests with zero production-code changes.
- `CLAUDE.md` E2E coverage catch-up (43 → 47 stale from A2b Task 19 → 51 after B1) — ships alongside B1 to avoid a separate docs PR.

Each item is explicitly named in §7; none extend B1's ACCEPTANCE path (the matrix uncheck-to-demigrate wiring is the single feature that must land for "B1 done"). Dropping any ancillary item mid-flight is safe.

## 4. Decisions

### D1 — Sequential per-change orchestration in `applyChanges` (variant A)

**Chosen.** Each dirty entry fires its own POST; endpoint selected by direction:
`direction = initialChecked ? "demigrate" : "migrate"` (`initialChecked === true` means the cell was already `via-hub` at load time → user is rolling it back).

Codex advisory concurs (2026-04-24 consultation): variant A has the best user-value / risk / blast-radius ratio for this milestone. Variants B (parallel) and C (new `/api/apply` endpoint) were both rejected.

### D2 — Demigrate request shape

Per-change narrow: `POST /api/demigrate` with `{servers: [change.server], clients: [change.client]}`. This matches `DemigrateOpts.ClientsInclude` semantics — rolls back only the (server, client) pair the user unchecked, not every client bound to that server. Users can demigrate every binding from the Migration screen's per-row button (which sends `{servers: [name]}` with empty `clients`).

### D3 — Error surface (REVISED per Codex R3 P3 — aligned with D4 batching)

Reuse the existing `failed: string[]` pattern in `applyChanges`. Each POST represents a `(server, direction)` batch carrying one or more clients, so the failure label must reflect the batch shape:

```ts
failed.push(`${server}/${direction}/[${clients.join(",")}]: ${body.error ?? resp.status}`);
```

Render `Failed: <joined>` in the toolbar's `apply-msg` span. No inline per-row error (would require row-state plumbing; not a B1 requirement).

### D4 — Dirty-map tracking (REVISED per Codex R1 P1)

Today's dirty state is `Map<string, Set<string>>` (server → set of dirty clients). Direction is NOT preserved. The B1 design requires direction per-cell so `applyChanges` can branch the endpoint correctly. Chosen shape:

```ts
type Direction = "migrate" | "demigrate";
const [dirty, setDirty] = useState<Map<string, Map<string, Direction>>>(new Map());
```

- `toggleCell(server, client, nextChecked, initialChecked)` computes direction from `initialChecked`: if it was `true` (cell was `via-hub` at load) the user is rolling it back → `"demigrate"`; if it was `false` (cell was `direct`) the user is migrating → `"migrate"`. When `nextChecked === initialChecked` (toggle back to the initial state) the entry is removed.
- `applyChanges` iterates the inner map per-server and groups changes by direction. Each distinct (direction, server) batch fires one POST with `{servers: [server], clients: [...matchingClients]}`. Servers with both a migrate-batch and a demigrate-batch (e.g., user checks clients A,B and unchecks client C on the same row) produce two sequential POSTs.
- **Order invariant: demigrate-before-migrate across the entire Apply.** Rationale: `api.MigrateFrom` writes a fresh `.bak-mcp-local-hub-<timestamp>` backup BEFORE mutating the client config. If migrate runs first on a client that also has a queued demigrate for the SAME client, the demigrate's subsequent `latestBackup` read sees the just-written post-migrate backup — the entry it wants to roll back is now in hub-managed form in that backup. `api.Demigrate` then falls back to the pristine `-original` sentinel; if the sentinel lacks that entry (server was added after the sentinel was frozen), demigrate refuses and the matrix shows a Failed row for a change the user expected to succeed. Running all demigrate POSTs before any migrate POSTs keeps each demigrate's backup read pointed at the pre-session snapshot. **[R5 P1 on this memo caught the original "migrate-first" ordering; it was wrong.]**
- `toggleCell` prune invariant: when a toggle returns a cell to its initial state, delete the `client` entry from the inner map AND, if the inner map becomes empty, delete the `server` entry from the outer map. With the invariant enforced at every update, `applying || dirty.size === 0` remains correct without a deep-empty scan. (An earlier draft suggested "skip pruning" as a shortcut — that would leave the outer map non-empty after a toggle-back and re-enable Apply for no-op state. Rejected per Codex R2 on this memo.)

This is a real schema change, not a derivation trick. It is the only B1 change that touches non-trivial component state.

### D5 — Tooltip copy for `via-hub` cells

Replace line 234:

> `Already routed through the hub. To disable, run `mcphub rollback --client ${client}` (Phase 3B-II will add a UI for this).`

with:

> `Currently routed through the hub. Uncheck and Apply to roll this binding back to the original ${client} config.`

The new copy is active (describes the enabled action), references the checkbox semantic directly, and no longer points users at the CLI workaround.

### D6 — No optimistic UI; reload-on-success only (REVISED per Codex R3 P2)

Today's pattern (unchanged for B1): after Apply dispatches, reload only when `failed.length === 0`. On success, clear dirty AND bump `reloadToken` to trigger a fresh `/api/scan` + `/api/status` fetch; the checkbox state reconciles from the authoritative backend scan. On partial/total failure, leave dirty populated and leave cell checkboxes in their user-flipped local state so the user can retry Apply or un-flip a cell manually.

An earlier revision of this memo said "reload after success OR partial failure"; that was a misread. Verified current `Servers.tsx:108-122` only reloads on the empty-failed branch. Keeping the current behavior avoids a hidden behavior change and preserves the "failed cells stay dirty and retryable" UX that A2a established. E2E scenario §6.3 #3 is written to match the current reload-on-success-only contract.

### D7 — Apply-button disabled predicate

Already correct: `applying || dirty.size === 0`. Any dirty entry (regardless of direction) enables Apply.

### D8 — Same-origin guard on `/api/demigrate`

Already in place from the existing handler. No change.

### D9 — Scan reload after mixed Apply

The existing post-Apply reload already picks up both directions because `/api/scan` re-reads every client config and recomputes `routing` per (server, client). No per-direction reload logic needed.

### D10 — Manifest cascade when Demigrate removes all bindings

`api.Demigrate` does not modify the manifest's `client_bindings` list; it only restores client configs. The manifest stays authoritative as the list of *allowed* bindings. A future operator re-running `mcphub migrate` (or the user checking the matrix box again) re-migrates using the same binding row. This is consistent with existing Migration-screen Demigrate semantics.

## 5. Risks

### R1 — Cross-apply backup-file contention on the same client

Imagine a user checks a `direct` cell (queues migrate) and unchecks a different `via-hub` cell on the same client (queues demigrate) — e.g., migrating server-A while demigrating server-B on claude-code. Both POSTs target the same claude-code config file and its backup rotation.

**Verified:** `internal/clients/clients.go` `writeBackup` / `latestBackup` / restore paths use plain `os` I/O with no `sync.Mutex`, no `flock`, no process-level lock. I checked for lock primitives in the package and none exist. [Codex R1 P2 on this memo corrected an earlier false claim in this memo that such a lock existed.]

**Actual mitigation for B1:**

- Within a single GUI tab's Apply invocation, the frontend's sequential per-change loop already serializes POSTs to the backend. Two requests from that tab never overlap on the same client.
- The single-instance mutex at process start (`mcphub gui`'s `<pidport>.lock`) forbids a second GUI process from running concurrently — so there is no two-GUI-process race.

**Residual races for B1 — acknowledged, not mitigated:**

- Multiple TABS of the same GUI process CAN issue interleaved `POST /api/migrate` and `POST /api/demigrate` against the same Go process. Each handler is a separate goroutine; the HTTP mux does not serialize them. This was already latent pre-B1 for two tabs both clicking the Migration-screen's Demigrate button at once; B1 widens exposure (matrix Apply on one tab + Migration-screen Demigrate on another).
- CLI-plus-GUI interleaving (e.g., `mcphub migrate ...` running while the user clicks Apply on the GUI matrix) has the same class.
- **Failure modes go beyond "visible parse error".** I verified `latestBackup` does NOT validate JSON — it picks a path. The backup-write / backup-restore / live-client-config read-modify-write paths in `internal/clients/clients.go` use unguarded `os.ReadFile` / `os.WriteFile` without a locking primitive. Concurrent modifications can therefore produce **silent lost updates** (writer B clobbers writer A's pending rotation) and not just parse failures. The earlier "failure surfaces loudly on read" claim in this memo was wrong and is withdrawn. [Codex R2 on this memo.]

**Decision:** ship B1 without new locks. The cost of a proper fix — client-level serialization covering BOTH backup I/O AND live config read-modify-write paths (either an in-process `sync.Mutex` keyed by client name for the single-process case OR an OS-level advisory file-lock so CLI+GUI don't race) — is a meaningful engineering effort plus its own test matrix, and belongs with a follow-up plan item, NOT this memo. Rationale for acceptance: the exposure is "two tabs / CLI+GUI simultaneously acting on the same client config", which is a rare operator pattern and not a regression introduced by B1 per se.

`work-items/bugs/b1-backup-file-race.md` must be created alongside the B1 implementation plan so the concern is durable, includes the actual (silent lost update) failure mode, and explicitly names the fix candidates.

### R2 — Demigrate on a server not in via-hub

The user unchecks a cell that is "via-hub" at page load but a background external-config change between load and Apply moved it to "direct". The POST `/api/demigrate` fires for a server whose latest-backup no longer contains a hub-routed entry → `Demigrate` auto-falls-back to the `-original` sentinel, or if the sentinel lacks the entry, refuses with a Failed row that surfaces in `apply-msg`. Existing behavior; no new defense needed.

### R3 — Empty `clients` slice semantics (VERIFIED per Codex R3 P3)

Verified against [internal/api/demigrate.go:72](../../../internal/api/demigrate.go#L72):

```go
if len(opts.ClientsInclude) == 0 {
    return true
}
```

`len(nil slice) == 0` in Go, so **both** `ClientsInclude == nil` and `ClientsInclude == []string{}` behave identically: "no filter applied — every client bound in the manifest is rolled back". The Migration per-row button's current body `{servers: [name]}` (no `clients` field → missing JSON key → nil Go slice) therefore produces the intended broad-rollback semantics.

For B1's per-cell rollback we must send a **non-empty `clients` array** in the POST body — `{servers: [change.server], clients: [...toRollBackOnly]}` — so the narrow filter actually narrows. An empty `clients` array (or omitting the key) would widen the rollback to every binding on the manifest, undoing the cell-level intent. Plan's matrix-wiring task must include a typed check that the demigrate POST body has a non-empty `clients` array for per-cell calls. [R3 draft text originally used a placeholder `direction-clients` field name — that was a typo and is corrected here: the handler's decoder looks at `clients`.]

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
2. **"mixed Apply dispatches both endpoints, demigrate first"** — seed two rows: `(a, claude-code)` at `direct`, `(b, claude-code)` at `via-hub`. User checks the first (queues migrate on server `a`) and unchecks the second (queues demigrate on server `b`). Apply must dispatch `/api/demigrate` for `b` BEFORE `/api/migrate` for `a` per §4 D4's order invariant (demigrate-first across the whole Apply so each demigrate's backup read is not polluted by a just-written post-migrate backup on the same client). Assert request ordering via intercepted timestamps or a per-request counter.
3. **"demigrate failure surfaces in apply-msg and keeps dirty state"** — stub `/api/demigrate` to 500 with a generic body. Uncheck a via-hub cell, Apply. Assert the toolbar `apply-msg` contains `Failed:` with the `server/demigrate/[client]` shape from §4 D3. Per §4 D6 (reload-on-success-only): reloadToken is NOT bumped on failure, so the checkbox stays in its locally-flipped (unchecked) state and dirty map retains the entry — user can click Apply again to retry or toggle the cell to revert. No routing-column change expected because the scan did not re-run.
4. **"tooltip copy reflects uncheck-to-demigrate semantic"** — assert the `title` attribute on a `via-hub` cell contains "Uncheck and Apply to roll this binding back" (or the exact substring we land on). Remove/update any prior assertion that expected the `mcphub rollback --client` phrase.

Target: `internal/gui/e2e/tests/servers.spec.ts` grows from 3 → 7 scenarios (4 new). Total E2E suite at current HEAD is **47** (3 shell + 3 servers + 6 migration + 13 add-server + 17 edit-server + 2 dashboard + 3 logs — verified by counting `test(` per spec file). Target after B1: **51**.

**CLAUDE.md accounting is stale.** The committed CLAUDE.md coverage section still says "43 smoke tests total" — written at Task 19 of A2b before the R1 (+3 E2E) and R2 (+1 E2E) Codex-fix commits brought the count to 47. The B1 docs commit must therefore update CLAUDE.md in two steps' worth of delta: catch up to 47 (capture the R1/R2 additions) AND add B1's +4 to land on 51. Plan task for the docs update: one commit that rewrites the coverage section to reflect the current suite, with the 51 final total. [Codex R1 P3 on this memo.]

### 6.4 Cross-suite regression

No existing `servers.spec.ts`, `add-server.spec.ts`, `edit-server.spec.ts`, `migration.spec.ts` tests should regress. Plan includes a verification step to re-run the full Playwright suite after B1-core lands.

## 7. Handoff contract to the writing-plans skill

The plan should derive from this memo with the following commit breakdown:

1. **doc-only prep:** close `a2b-combined-pr-followups.md` item #2 as verified AND update `internal/api/api.go` package/type comment to honestly describe current per-request adapter construction (both must move together — see §2.4). Single commit.
2. **File work-items bug entry:** create `work-items/bugs/b1-backup-file-race.md` so the R1 residual race is durable (see §5 R1). Single commit, doc-only.
3. **Go tests:** add the 3 `/api/manifest/edit` coverage tests (`EmptyName_400`, `MalformedJSON_400`, `RejectsNonPOST_405`). Single commit; no production-code changes.
4. **Servers matrix dirty-shape refactor:** change `dirty` to `Map<string, Map<string, Direction>>`, update `toggleCell` to persist direction, update `applyChanges` to iterate the richer structure and batch per (server, direction) pair (see §4 D4). This is the load-bearing change. Single commit.
5. **Servers matrix wiring:** enable `via-hub` checkbox (remove from disabled list, update tooltip copy per §4 D5), branch `applyChanges` endpoint per direction to call `/api/migrate` vs `/api/demigrate` using the shape from commit 4. Includes regenerated assets (`go generate ./internal/gui/...`). Single commit.
6. **E2E:** add 4 scenarios in `servers.spec.ts`; grep + update any `mcphub rollback` tooltip assertion elsewhere. Single commit.
7. **Docs:** update `CLAUDE.md` coverage section to reflect current suite (43 in committed docs → 47 at HEAD → 51 after B1) per §6.3 note. Single commit.

Estimated scope: **~7 commits, ~200–300 LOC total** (mostly TS/TSX and TS test code; minimal Go — 3 short handler tests). Revised up from 5 commits to account for the dirty-shape refactor now being a distinct load-bearing commit separate from the enable-checkbox wiring.

## 8. Self-review checklist

- [x] Placeholder scan — none.
- [x] Internal consistency — §3 scope matches §4 decisions matches §7 handoff after Codex R1 revision.
- [x] Ambiguity check — demigrate body shape is explicit (`{servers: [one], clients: [one]}` per-cell batch vs `{servers: [name]}` no-clients for the Migration-screen bulk case).
- [x] Scope check — single-milestone-sized, 7 commits, one branch.
- [x] Decision lock — D1 (variant A) confirmed with user + Codex advisory; D4 (dirty-shape refactor) spelled out after Codex R1 P1 caught the missing direction; D10 (no manifest cascade) explicit.
- [x] Codex-review gate — R1 (P1 dirty shape, P2 backup lock myth, P2 api.go contradiction, P3 E2E count) → rev 2; R2 (P2 D4/D7 prune invariant, P2 backup race failure-mode rewrite, P3 CLI parity claim, P3 scope-drift framing) → rev 3; R3 (P2 D6 reload-on-success-only, P3 D3 batch-label shape, P3 R3 empty-clients verified) → rev 4; R4 (P1 typo `direction-clients` → `clients`, P2 NewAPI-not-literally-zero-cost phrasing, P3 `mcphub stop --server` correct CLI shape) → rev 5; R5 (P1 mixed-Apply order: demigrate-first-not-migrate-first, P2 remaining "zero-cost" phrasing in §1/§3.1, P3 `mcphub rollback --client` never existed — removed equivalence claim from §2.3) → rev 6 (this commit).

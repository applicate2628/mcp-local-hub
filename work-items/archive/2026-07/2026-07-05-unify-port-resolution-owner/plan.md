# Delivery plan — Unify the supervisor's daemon port-resolution owner

- **role:** `$planner`
- **date:** 2026-07-05
- **inputs (accepted, consumed as given — not redesigned):**
  - `work-items/active/2026-07-05-unify-port-resolution-owner/status.md` (problem + accepted direction + tracked follow-ups)
  - `work-items/active/2026-07-05-unify-port-resolution-owner/design.md` (accepted `$architect` package; Change-Surface Contract in §6)
  - `work-items/decisions/2026-07-05-daemon-port-resolution-single-owner.md` (ADR, `status: proposed`)
- **no implementation code in this plan.**

---

## 0. How this plan is phased (and where it deviates from the design's suggested grouping)

The design (§14 gate note) suggested three PRs, one of which deletes F5's `server=="serena"`
arm early as a "Tier-1 intermediate." **This plan does NOT take that intermediate.** The user's
hard constraint is: the pure resolver + memoization must land and be consumed by **every**
port-DECISION path **before** F5 is deleted, and F5 (with **both** special-cases) is deleted in
the **last** phase. Deleting the serena arm two phases before deleting the whole file would
create exactly the C6 stale-relation residue the user flagged (an intermediate special-case
deletion superseded a phase later). So:

- **Phase 1** lands the owner (pure resolver + memo) and MOVES the two shared helpers into it.
- **Phase 2** fixes the serena deadline via the manifest (its own phase — it changes liveness
  behavior for serena) and deletes the argv-keyed `isSerenaProxyDescriptor` deadline arm.
- **Phases 3a–3d** migrate each port-DECISION consumer to the owner. After Phase 2 lands they
  are mutually independent (disjoint files, fixed owner contract) → parallel-eligible.
- **Phase 4 (LAST)** deletes F5 in full with **both** special-cases, and the recover 3-way hint
  branch, so no intermediate serena special-case lingers as stale residue.

This is a sequencing decision inside the architect's Change-Surface Contract (§6 of the design);
it changes no architecture and expands no surface. Every phase fits inside the contract — none
requires an unlisted module edit, shared-abstraction churn, dependency-direction change, or a
protected-surface touch.

### Dependency graph

```
Phase 1  (owner)                     ── no deps ── FIRST
   │
   ├── Phase 2  (serena deadline)    ── needs P1
   │       │
   │       └── Phase 3a (liveness + startup-scan)  ── needs P1 + P2
   │
   ├── Phase 3b (squatter identity)  ── needs P1 only   ┐
   ├── Phase 3c (recover CLI)        ── needs P1 only   ├─ mutually independent,
   └── Phase 3d (status resolver)    ── needs P1 only   ┘  parallel-eligible after P1
                                                            (3a additionally needs P2)
Phase 4  (delete F5 + both special-cases + recover hint)
        ── needs 3a AND 3b AND 3c AND 3d AND P2 ── LAST
```

**Why 3a needs Phase 2 (correctness, not convenience):** Phase 3a makes the liveness sweep
resolve a `Port=0` legacy-unified serena row's effective port to `9121` (manifest), which
**activates** serena's port-bind check. If Phase 2 has not yet landed, the deadline for that
row resolves via the deleted-later argv arm to the **60s default** (its args are
`daemon --server serena --daemon unified`, not `daemon serena-proxy`), which is far too short
for serena's cold uvx+SolidLSP start → the exact `daemon-bind-timeout` restart cycle PR #504
mitigated. Phase 2 must give legacy-unified serena its 120s deadline **before** Phase 3a turns
on its port check.

### Per-PR gate preamble (applies to every phase)

Each phase is one commit-ready unit. Whether phases are bundled into PRs is the integration
owner's call, but the repo's standing workflow binds every PR that reaches `master`
(CLAUDE.md "PR review + merge workflow"): the phase's named reviewer gate below → full local
pre-push set → the multi-model commission (sonnet + opus + fable-5, per MEMORY) → **Codex Cloud
bot PASS is the mandatory final gate before merge**. The reviewer roles named per phase are the
*internal* quality gate that must pass before the PR is opened; they do not replace the bot.

---

## Phase 1 — The port/deadline owner (pure resolver + memo + canonical identity)

**Goal:** introduce the single stateless resolver every port-decision path will consume, and
move the two shared helpers into it — without changing any consumer's behavior yet.

### File scope
- **NEW** `internal/api/supervisor_port_owner.go`:
  - `func EffectiveDaemonPort(d SupervisorDaemon) (port int, ok bool)` — `d.Port` when `>0`, else
    the manifest-declared port for the descriptor's resolved `(server,daemon)`; `ok=false` when
    neither yields `port>0` (portless timer row, or renamed/removed manifest).
  - `func EffectiveStartupBindDeadlineSeconds(d SupervisorDaemon) int` — `explicit field > manifest
    deadline > 60` default; **independent of the port short-circuit** (a port-stamped,
    deadline-zero row still resolves the manifest deadline).
  - `func NewDaemonPortResolver() *DaemonPortResolver` + `func (r *DaemonPortResolver) Resolve(d
    SupervisorDaemon) (port, deadlineSecs int, portOK bool)` — per-pass memoized (one manifest
    parse per server per pass); a direct generalization of `supervise_status.go`'s private
    `newManifestPortResolver`.
- **MOVE** into the new file, deleted from `internal/api/intent_port_backfill.go` (same package, so
  callers are unaffected by the move itself):
  - `DescriptorServerDaemon` (currently `intent_port_backfill.go:86`) — the single owner of
    "what `(server,daemon)` is this descriptor, with blank-field args-recovery."
  - `resolveManifestPortAndDeadline` + its package var `resolveManifestPortAndDeadlineFn`
    (currently `intent_port_backfill.go:47,55`) — the one manifest read returning port+deadline.
- **EDIT** `internal/api/migrate.go`: `ResolveManifestDaemonPort` (`:376`, port-only) becomes a thin
  delegate to the owner's `resolveManifestPortAndDeadline` (drop its own `loadManifestForServer`
  call), so the two pre-existing manifest resolvers collapse to **one** owner immediately
  (Claim #1). No caller changes; signature preserved.

### Canonicalize identity — resolve the field/argv-mismatch follow-up (status.md §Progress)
`DescriptorServerDaemon` today returns the struct fields verbatim whenever both are non-blank
(`intent_port_backfill.go:88-90`) and only fills blanks from argv — it never cross-checks a
populated field against a disagreeing argv. Adopt the **fail-closed** canonical rule (argv is the
launch-truth per design §7 security-by-design; the struct field is a cache):

- both field and argv present and **agree** → return them (`ok=true`);
- a field is **blank** → fill it from argv (unchanged behavior);
- both present and **disagree** (`d.Server`/`d.Daemon` ≠ the `--server`/`--daemon` argv tokens)
  → return `ok=false` (unresolvable — refuse to resolve a mixed identity, so no protection
  decision stamps a port the process never binds).

This affects ~0 real daemons (hardened writers never emit a field/argv mismatch) and is the
conservative choice for a corrupt descriptor. Document the rule in the function doc comment.

### Behavior invariant for this phase
Phase 1 is **additive**: F5 still calls the moved `DescriptorServerDaemon` + `resolveManifestPortAndDeadlineFn` from their new home (same package); the status path still uses its own private
`newManifestPortResolver`; no decision consumer is rewired yet. The one behavioral delta is the
new mismatch → `ok=false` rule inside `DescriptorServerDaemon`, which is covered by a unit test.

### Dependencies
None. First phase.

### Acceptance criteria
- **AC1** — `EffectiveDaemonPort` returns `(d.Port, true)` for a `Port>0` row without consulting
  the manifest; returns the manifest port for a `Port=0` manifest-backed row; returns `(0,false)`
  for a renamed/removed manifest and for a portless timer row.
- **AC2** — `EffectiveStartupBindDeadlineSeconds` returns the explicit field when `>0`; the manifest
  deadline when the field is 0 and the manifest declares one; 60 otherwise. It resolves the
  manifest deadline **even when `d.Port>0`** (port short-circuit does not gate the deadline).
- **AC3** — `NewDaemonPortResolver().Resolve` parses each server's manifest **at most once per
  resolver instance** (memo), returning byte-identical `(port, deadline, ok)` to the pure calls.
- **AC4** — `DescriptorServerDaemon`: agreeing field+argv → `ok=true`; blank field recovered from
  argv → `ok=true` with recovered values; **disagreeing** field vs argv → `ok=false`.
- **AC5** — `migrate.go ResolveManifestDaemonPort` returns identical results to before (now via the
  owner delegate); grep shows exactly **one** `loadManifestForServer`-backed port+deadline reader
  in the tree (the owner), no second manifest resolver remains.
- **AC6** — `internal/api/intent_port_backfill.go` still compiles and F5 behavior is unchanged for
  agreeing/blank-field rows (the moved helpers resolve identically from their new home).

### Tests to add/change
- **NEW** `internal/api/supervisor_port_owner_test.go`:
  - `TestEffectiveDaemonPort_PortShortCircuit` / `_ManifestFallbackForPortZero` /
    `_RuntimeSpecReturnsPort` / `_RenamedManifestNotOK` / `_TimerRowNotOK` (AC1).
  - `TestEffectiveStartupBindDeadline_ExplicitWins` / `_ManifestOverDefault` /
    `_DefaultSixty` / `_DeadlineIndependentOfPort` (AC2).
  - `TestDaemonPortResolver_ParsesManifestOncePerServer` — port the once-per-server seam from the
    existing `resolveManifestDaemonPortFn`-injection pattern (AC3).
  - `TestDescriptorServerDaemon_AgreeingFields` / `_RecoversBlankFieldsFromArgs` (migrated from
    `intent_port_backfill_test.go:360 TestBackfillIntentDaemonPorts_BlankFieldsRecoveredFromArgs`,
    identity half) / **`_RejectsFieldArgvMismatch`** (new, AC4).
- **CHANGE** `internal/api/migrate.go` callers' test expectations only if any assert the internal
  call shape; otherwise no change (AC5 verified by grep + existing `ResolveManifestDaemonPort`
  tests staying green).

### Repo-standard checks
`go build ./... && go vet ./...`; `go test -count=1 ./internal/api/`; then the tagged run
`go test -tags=test_state_path_env -count=1 -timeout 5m ./internal/api/`. **State-safe caveat:**
`internal/api` tests can touch the live scheduler/`supervisor-intent.json` — **back up the live
`%LOCALAPPDATA%\mcp-local-hub\supervisor-intent.json` before running** (MEMORY
`feedback_kosyak_subagent_test_wiped_live_supervisor_intent`); prefer `t.TempDir()` fixtures; if a
test redirects `HOME`/`LOCALAPPDATA`, pin `GOMODCACHE`/`GOCACHE` to real paths (MEMORY
`reference_state_safe_test_gomodcache_pin`). Sweep `mcphub.exe` afterward.

### Quality gate
`$architecture-reviewer` — this introduces the new control-plane seam (the ONE port/deadline
resolution surface). **The ADR promotion `proposed → accepted` rides this gate** (the ADR names
`$architecture-reviewer` as the promoter on the implementing PR). `$security-reviewer` consulted
on the `DescriptorServerDaemon` mismatch rule only (it feeds the squatter/recover kill-authority
paths downstream).

---

## Phase 2 — Serena first-bind deadline via the manifest (delete the argv arm)

**Goal:** legacy-unified serena gets 120s from ONE owner (the manifest), with no code
special-case; `supervisorStartupBindDeadline` becomes a thin delegate to the owner.

### File scope
- **EDIT** `servers/serena/manifest.yaml` — add `startup_bind_deadline_seconds: 120` to the
  `unified` daemon (currently `:61-64`, has `port: 9121` + `extra_args`, no deadline).
- **EDIT** `internal/cli/supervise_liveness.go` `supervisorStartupBindDeadline` (`:516-524`):
  - body becomes `return time.Duration(api.EffectiveStartupBindDeadlineSeconds(d)) * time.Second`;
  - **DELETE** the `isSerenaProxyDescriptor` arm (`:520-522`) and update the function doc comment
    (`:510-515`) so it no longer describes an argv-keyed serena branch (C6: erase the stale
    rationale, do not leave "was keyed on argv" residue).
- `isSerenaProxyDescriptor` itself is **untouched** — its non-deadline callers stay
  (`supervise_reconcile.go:190`, `supervise_respawn.go:196`, `supervisor_controller.go:2795`,
  `supervise_squatter.go:203/233`, `supervise.go:3388`).

### Behavior invariant for this phase
- legacy-unified serena (field 0, daemon `unified`) → **120s** via manifest.
- serena-proxy pool rows (explicit 120, `supervisor_intent_build.go:289`) → 120s via explicit
  branch — **unchanged** (the removed arm was only reachable when the field was 0, which these
  rows never are).
- LSP workspace-proxy rows (explicit 120, `register_supervisor.go:58`) → unchanged.
- global/legacy daemons (field 0, no manifest deadline) → 60s — unchanged.
- **Does NOT yet activate serena's port check** — F5 still skips serena so its intent `Port`
  stays 0 and the liveness early-healthy guard (`:592`) still fires until Phase 3a. Phase 2 only
  corrects the deadline in isolation, exactly so Phase 3a can safely turn the port check on.

### Dependencies
Phase 1 (needs `api.EffectiveStartupBindDeadlineSeconds`).

### Acceptance criteria
- **AC1** — `config.ParseManifest` of `servers/serena/manifest.yaml` yields
  `unified.StartupBindDeadlineSeconds == 120` (and still `Port == 9121`).
- **AC2** — `EffectiveStartupBindDeadlineSeconds` for a legacy-unified serena descriptor (args
  `daemon --server serena --daemon unified`, field 0, `RuntimeSpec==nil`) returns 120.
- **AC3** — `supervisorStartupBindDeadline` returns `120s` for that descriptor and `60s` for a
  field-0 global daemon, with the `isSerenaProxyDescriptor` arm gone from its body.
- **AC4** — a serena-proxy pool descriptor (explicit field 120, workspace-hash daemon name)
  still returns 120 (proves the removed arm did not regress the explicit path); grep confirms
  `isSerenaProxyDescriptor` is still referenced by its four non-deadline call sites.

### Tests to add/change
- **NEW/CHANGE** `internal/cli/supervise_liveness_bind_deadline_test.go`:
  - assert `supervisorStartupBindDeadline` returns 120s for legacy-unified serena and 60s for a
    field-0 global daemon (AC3);
  - assert the serena-proxy explicit-120 row is unaffected (AC4).
- **NEW** manifest-parse assertion (co-located with the serena manifest tests, or in the owner
  deadline test): `unified.StartupBindDeadlineSeconds == 120` (AC1).
- The existing writer tests in `internal/api/supervisor_startup_bind_deadline_test.go`
  (`TestSupervisorDaemonsFromPlan_CopiesStartupBindDeadline`,
  `TestSerenaProxyDescriptor_StampsStartupBindDeadline120`) stay green **unchanged** — they test
  the authoritative writers, which this phase does not touch.

### Repo-standard checks
`go build ./... && go vet ./...`; `go test -count=1 ./internal/cli/ ./internal/api/ ./internal/config/`;
tagged run `go test -tags=test_state_path_env -count=1 -timeout 5m ./internal/api/ ./internal/cli/`.
Same state-safe caveat as Phase 1 (back up live intent; `t.TempDir()`; sweep `mcphub.exe`).

### Quality gate
`$architecture-reviewer` (config-as-single-owner; deletion of a code special-case) +
`$qa-engineer` (deadline resolution matrix, incl. the serena-proxy non-regression).

---

## Phase 3a — Liveness sweep + startup running-scan resolve the effective port

**Goal:** the liveness bind-check, P1b deadline, and squatter re-probe resolve a `Port=0` legacy
row's effective port through the owner, so `Port=0` no longer structurally disables protection;
the startup running-scan re-checks legacy rows.

### File scope
- **EDIT** `internal/cli/supervise_liveness.go`:
  - in the sweep (`sweepSupervisorLivenessOnce`, per-daemon loop head after `d, ok := byTask[...]`,
    ~`:287`): instantiate one `api.NewDaemonPortResolver()` per sweep; resolve `(port, _, portOK)`
    for `d` and set the **local** `d.Port = port` (leave the deadline to
    `supervisorStartupBindDeadline(d)`, already owner-backed after Phase 2). Every downstream
    reader in the sweep (`supervisorDaemonEntryLiveWithProbe` at `:591/:597/:648`, the squatter
    re-probe at `:376`, event bodies at `:334/:359`) then sees the resolved port with no further
    change. The `if d.Port <= 0 { return true }` early-healthy guard (`:592`) is retained but is
    now reached only for a **genuine** resolve-miss (`!portOK`) — the intended semantics.
  - `supervisorIntentPortMapForStateDir` (`:701-711`): build with a `NewDaemonPortResolver()` and
    map the **resolved effective port** per daemon instead of raw `d.Port` (single caller:
    `supervise.go:2317`).
- **EDIT** `internal/cli/supervise.go` startup running-scan (`loadSupervisorCurrentRunning`): the
  `if port := intentPorts[canonicalTask]; port > 0` gate (`:2353`) now sees resolved ports, so a
  legacy `Port=0` row that resolves a manifest port is no longer bypassed. The descriptor built at
  `:2362-2366` stays **port-only** (design §3.4 scopes the startup-scan to the port); the one-shot
  seed's deadline/grace is unchanged from today (the 5s-later sweep applies the full descriptor +
  correct 120s — see the note below).
- **Observability** (design §8): from the sweep, emit a `debug`/`warn` `daemon-port-unresolved`
  event for a row that *is* a manifest daemon but resolves `!portOK` (renamed/removed manifest) —
  the successor to F5's `intent-port-unresolved` (`supervise.go:822-828`), so a genuinely
  unprotected daemon stays visible **before** F5's event is removed in Phase 4. Keep it `debug` for
  recurring benign misses.

### Note — startup-scan deadline at seed time is unchanged (not a regression)
The startup-scan's minimal descriptor (`:2362`) carries no `Server`/`Daemon`/`Args`, so its
seed-time deadline resolves to the 60s default today and after this phase (unchanged). The
authoritative 120s serena path is the sweep, which runs within 5s with the full descriptor. The
QA gate must confirm this is byte-identical-to-today at seed time and corrected by the sweep — it
is NOT a new serena 60s exposure.

### Serena activation (the load-bearing behavioral change this phase introduces)
For a legacy-unified serena row this phase resolves the port to `9121` → **activates** the
port-bind check + P2a squatter re-probe, while Phase 2 supplies the 120s deadline. This is the
intended protection gain, and the reason Phase 2 is a hard predecessor. F5 still leaves serena's
intent `Port=0`; the resolver makes that irrelevant to the decision.

### Dependencies
Phase 1 (owner) **and** Phase 2 (serena deadline correct before its port check activates).

### Acceptance criteria
- **AC1** — a `Port=0` legacy descriptor whose manifest declares `port>0` is now
  `port_unbound`/`port_owner_*`-classified by the sweep (NOT early-healthy at `:592`); when the
  port is bound by the tracked PID it latches live.
- **AC2** — the same row is re-checked by the startup running-scan (`supervise.go:2353` gate no
  longer bypasses it).
- **AC3** — a legacy-unified serena row (`Port=0`, args `daemon --server serena --daemon unified`)
  resolves port 9121 with a 120s first-bind deadline and does **not** emit `daemon-bind-timeout`
  during a cold start within 120s (no restart cycle).
- **AC4** — a genuine resolve-miss (manifest-backed row whose server was renamed/removed) keeps
  "no port protection" and emits `daemon-port-unresolved`; a portless timer row resolves `!ok`
  silently (no event, no false protection).
- **AC5** — a `Port>0` row is byte-identical through the sweep (resolver short-circuits to
  `d.Port`); no drift for runtime_spec serena/LSP rows (always `Port>0`).

### Tests to add/change
- **NEW/CHANGE** `internal/cli/supervise_liveness_bind_deadline_test.go` (or a sibling liveness
  test): `TestSweep_PortZeroLegacyRowIsPortClassified` (AC1), `TestSweep_SerenaLegacyUnified120NoBindTimeout` (AC3), `TestSweep_ResolveMissKeepsUnprotectedAndEmitsEvent` (AC4),
  `TestSweep_PortNonZeroByteIdentical` (AC5).
- **NEW/CHANGE** a startup-scan test asserting `supervisorIntentPortMapForStateDir` returns the
  resolved port and the `:2353` gate re-checks a legacy row (AC2).

### Repo-standard checks
`go build ./... && go vet ./...`; `go test -count=1 ./internal/cli/`; tagged run
`go test -tags=test_state_path_env -count=1 -timeout 5m ./internal/cli/ ./internal/api/`. Same
state-safe caveat (back up live intent; `t.TempDir()`; sweep `mcphub.exe`). This phase touches the
restart-decision path — run the full `./internal/cli/` package, not a narrowed subset.

### Quality gate
`$security-reviewer` (this activates restart/reap authority for `Port=0` rows — a
kill/restart-decision behavior change) + `$qa-engineer` (the classification matrix + the serena
no-restart-cycle proof).

---

## Phase 3b — Squatter classifier resolves identity through the owner

**Goal:** the squatter argv gate stops reading `d.Server`/`d.Daemon` raw, so a blank-identity
legacy row (fields blank, args carry `--server`/`--daemon`) classifies correctly without F5's
identity-heal.

### File scope
- **EDIT** `internal/cli/supervise_squatter.go` `commandLineMatchesTaskArgv`, the
  `isGlobalDaemonDescriptor` arm (`:217-222`): resolve `(server, daemon)` via
  `api.DescriptorServerDaemon(d)` and gate on **those** (`ok && server!="" && daemon!="" && …
  hasGlobalDaemonAnchor(tokens) && commandLineHasAdjacentTokenPair(--server, server) && (…--daemon,
  daemon)`) instead of `d.Server != "" && d.Daemon != ""`. `isGlobalDaemonDescriptor` /
  `isSerenaProxyDescriptor` / `isLSPWorkspaceProxyDescriptor` shape predicates are untouched.

### Behavior invariant
A field-populated row classifies identically (owner returns the same fields). A blank-field
legacy row now recovers `(server,daemon)` from its own argv and classifies `own_task` on a
matching squatter — the protection F5's PR #504 blank-field heal currently provides, now provided
without F5. A field/argv **mismatch** row → owner `ok=false` → gate fails → Foreign (fail-closed),
which is the correct conservative outcome for a corrupt descriptor.

### Dependencies
Phase 1 (needs `api.DescriptorServerDaemon` in the owner). Independent of Phase 2/3a — parallel-eligible.

### Acceptance criteria
- **AC1** — a blank-identity global legacy row (`Server=""`, `Daemon=""`, args carry
  `--server X --daemon Y`) whose port is squatted by a verified-own disowned child classifies
  `squatterOwnTask` (identity recovered via the owner).
- **AC2** — a field-populated global row classifies byte-identically to before.
- **AC3** — a field/argv-mismatch row classifies Foreign (fail-closed via owner `ok=false`); a
  sibling subcommand (gui/supervise/status/…) still classifies Foreign (shape predicate unchanged).

### Tests to add/change
- **NEW/CHANGE** in the squatter classifier test file:
  `TestCommandLineMatchesTaskArgv_BlankIdentityRecoveredFromArgs` (AC1),
  `TestCommandLineMatchesTaskArgv_PopulatedFieldsUnchanged` (AC2),
  `TestCommandLineMatchesTaskArgv_MismatchIsForeign` (AC3).

### Repo-standard checks
`go build ./... && go vet ./...`; `go test -count=1 ./internal/cli/`; tagged run over
`./internal/cli/`. State-safe caveat as above.

### Quality gate
`$security-reviewer` (feeds the fail-closed kill/reap classifier) + `$qa-engineer`.

---

## Phase 3c — Recover CLI resolves the effective port at entry

**Goal:** `mcphub daemon recover` resolves a `Port=0` row's effective port through the owner and
proceeds to the squatter check instead of skipping; the 3-way F5-modelling hint branch is removed.

### File scope
- **EDIT** `internal/cli/daemon_recover.go` `recoverReapPortSquatter` (`:160-200`):
  - replace `if desc.Port <= 0 { … skip … }` (`:164-200`) with an entry resolve:
    `port, portOK := api.EffectiveDaemonPort(desc)`; on `portOK && port>0` proceed with `port`
    into the existing owner-probe/classify/reap flow (`:201+`).
  - keep a warn **only** when the resolve also misses (`!portOK`) — the successor to the current
    unconditional skip-warn (`:169`) — then return (nothing to fight over).
  - **DELETE** the 3-way F5-modelling hint `switch` (`:182-198`, the `SerenaServerName` /
    `portOK && port>0` / default branches) and the comment block (`:170-179`) that explains it
    models F5's backfill decision. Once the recover path resolves the port itself, the hint that
    tells the operator to "restart `mcphub supervise` to backfill it" is **obsolete** — and F5 is
    being deleted, so the hint would be a lie. This is a Phase-4-precursor supersession (see
    §Supersession map): remove the F5-shaped rationale here rather than let it linger as C6 residue.

### Behavior invariant
A `Port=0` row whose manifest resolves a port no longer prints the skip-warning; it proceeds to
the squatter identity/reap check with the resolved port. A true resolve-miss (renamed manifest /
non-daemon shape) still warns and returns without a reap (no port to fight over). No kill-authority
change — the reap still runs the identity-gated `classifyPortSquatter` → verified-own-only reap.

### Dependencies
Phase 1 (needs `api.EffectiveDaemonPort`). Independent of Phase 2/3a/3b — parallel-eligible.

### Acceptance criteria
- **AC1** — a `Port=0` descriptor whose manifest resolves `port>0` proceeds past the old skip into
  the owner-probe/classify path (no skip-warning printed) and reaps a verified-own squatter.
- **AC2** — a `Port=0` descriptor whose manifest does **not** resolve (renamed server / non-daemon
  shape) prints one resolve-miss warning and returns without a reap.
- **AC3** — the 3-way F5-modelling hint (serena/backfill/default) is gone; `grep` for "backfill"
  in `daemon_recover.go` returns nothing operator-facing.
- **AC4** — a foreign/unverifiable owner is still refused (exit `daemonRecoverExitRefused`), kill
  authority unchanged.

### Tests to add/change
- **CHANGE** `internal/cli/daemon_recover_test.go`: the cases that assert the `Port<=0` skip
  (around `:220`/`:234`, which reference `DescriptorServerDaemon` + `ResolveManifestDaemonPort`
  for the hint) → retarget to assert the entry-resolve proceed/warn behavior (AC1, AC2); drop
  hint-wording assertions (AC3). Keep the foreign-refusal test (AC4).

### Repo-standard checks
`go build ./... && go vet ./...`; `go test -count=1 ./internal/cli/`; tagged run over
`./internal/cli/`. State-safe caveat as above. Note `recover` runs **out-of-process** — the owner
must resolve identically there (it does: pure function over the embedded manifest).

### Quality gate
`$security-reviewer` (recover reaps a squatter — kill authority) + `$qa-engineer`.

---

## Phase 3d — Status path uses the owner resolver (drop the private memo + F5 comment)

**Goal:** the status producer resolves ports through the owner's `NewDaemonPortResolver`,
collapsing the last duplicate manifest resolver and erasing the "NOT redundant by F5" comment.

### File scope
- **EDIT** `internal/cli/supervise_status.go`:
  - replace the private `newManifestPortResolver` (`:59-84`) usage at `:141` with
    `api.NewDaemonPortResolver()`; delete the private memo function and its
    `resolveManifestDaemonPortFn` var (`:43`) if no other caller remains.
  - keep the inline `port := d.Port; if port == 0 && server != "" { … }` enrichment (`:181-186`)
    but resolve via the owner; feed the **resolved deadline** (not the raw field) into the synthetic
    descriptor's `StartupBindDeadlineSeconds` at `:200`.
  - **DELETE** the "This is the READ-fallback … NOT made redundant by the F5 startup port backfill
    … Do not delete it as 'F5 already fills the port.'" comment (`:131-140`) — C6 stale-relation
    residue once F5 is gone and the owner is the single resolver.

### Behavior invariant
Display is unchanged for the operator (a `Port=0` row still shows its manifest port); this phase is
a dedup + comment-hygiene change, not a display-behavior change. It is a hard precursor to Claim #1
("the two pre-existing manifest resolvers collapse to one") because the status memo was the second
resolver.

### Dependencies
Phase 1 (needs `api.NewDaemonPortResolver`). Independent of Phase 2/3a/3b/3c — parallel-eligible.

### Acceptance criteria
- **AC1** — status for a `Port=0` manifest-backed row shows the manifest port (unchanged operator
  output), resolved via the owner.
- **AC2** — the status manifest read happens **at most once per server per refresh** (owner memo),
  matching the pre-change once-per-server guarantee.
- **AC3** — the private `newManifestPortResolver` and the "NOT redundant by F5" comment are gone;
  grep shows the status path calls `api.NewDaemonPortResolver`.

### Tests to add/change
- **CHANGE** `internal/cli/supervise_status_manifest_memo_test.go` (`:60`): retarget the
  once-per-server assertion to the owner's resolver injection seam (AC2); keep the `Port=0 →
  manifest port` display assertion (AC1).

### Repo-standard checks
`go build ./... && go vet ./...`; `go test -count=1 ./internal/cli/`; tagged run over
`./internal/cli/`. State-safe caveat as above.

### Quality gate
`$architecture-reviewer` (dedup + comment hygiene; display-only) + `$qa-engineer`.

---

## Phase 4 — Delete F5 in full (both special-cases) + tidy the last F5 references

**Goal:** remove the redundant write-convergence and BOTH accreted special-cases now that every
decision consumer resolves through the owner; no startup write-pass rewrites `supervisor-intent.json`.

### File scope
- **DELETE** `internal/api/intent_port_backfill.go` — after Phase 1 it contains only
  `IntentPortBackfill`, `IntentPortBackfillRow`, `BackfillIntentDaemonPorts`, and the package doc
  (the `DescriptorServerDaemon` + `resolveManifestPortAndDeadline` helpers moved out in Phase 1).
  Deleting the file removes `BackfillIntentDaemonPorts` and **both** special-cases
  (`d.Port>0 || d.RuntimeSpec!=nil` at `:189`, `server==SerenaServerName` at `:222`).
- **DELETE** `internal/api/intent_port_backfill_test.go` — its resolver-relevant assertions were
  migrated to the owner test in Phase 1; the F5-write/idempotent/contended/preserves-fields tests
  go with the file.
- **EDIT** `internal/cli/supervise.go` — delete the F5 call site + all four event emits
  (`:777-830`: `intent-port-backfill-failed`, `intent-port-backfill-skipped-contended`,
  `intent-port-backfilled`, `intent-port-unresolved`). The operator-actionable resolve-miss signal
  was already replaced by the sweep's `daemon-port-unresolved` in Phase 3a.
- **EDIT** `internal/api/migrate.go` — if `ResolveManifestDaemonPort` now has **zero** production
  callers (Phase 3c removed the recover caller, Phase 3d removed the status caller), delete it and
  its tests; otherwise leave it as the Phase-1 thin re-export. Confirm by grep at this point.

### Supersession made explicit (C6 — no stale residue survives this phase)
- **PR #504's serena-guard** (`intent_port_backfill.go:222 if server == SerenaServerName`) is
  deleted here, together with the whole file — never edited in place in an earlier phase, so no
  intermediate serena-arm-only deletion lingers.
- The recover **3-way hint branch** was already removed in Phase 3c; this phase only removes the
  producer (F5) it modelled, so the two are consistent.
- The status **"NOT redundant by F5" comment** was already removed in Phase 3d.

### Dependencies
Phase 3a **and** 3b **and** 3c **and** 3d (every decision consumer migrated) **and** Phase 2.
This is the LAST phase.

### Acceptance criteria
- **AC1** — `internal/api/intent_port_backfill.go` and `intent_port_backfill_test.go` are absent;
  `grep -r BackfillIntentDaemonPorts` returns only history/docs (no live `.go` reference).
- **AC2** — no startup write-pass rewrites `supervisor-intent.json`: a pre-change intent fixture
  round-trips byte-identically through a supervisor cold start (Claim #6). A host previously
  F5-backfilled keeps its ports (`Port>0` → resolver short-circuit); a fresh legacy `Port=0` host
  resolves lazily and is protected (the whole point — proven by the Phase 3a tests + §E2E).
- **AC3** — both special-cases are gone from the tree; `grep` for `SerenaServerName` in an F5
  context returns nothing.
- **AC4** — build/vet/test green across `./...`; the removed F5 events no longer appear in
  `supervisor-events.log` on a cold start, and `daemon-port-unresolved` (Phase 3a) is the sole
  resolve-miss signal.

### Tests to add/change
- **DELETE** `internal/api/intent_port_backfill_test.go`.
- **NEW** a guard test (in `internal/cli` or `internal/api`) asserting a `Port=0` legacy fixture
  round-trips byte-identically through `ReadSupervisorIntent` → cold-start path → re-read, with
  **no** in-place mutation (AC2, Claim #6).
- **NEW** a grep/absence guard (or a build-level assertion) that no live `.go` references
  `BackfillIntentDaemonPorts` (AC1).

### Repo-standard checks
`go build ./... && go vet ./... && go test -count=1 -timeout 5m ./...`; tagged run
`go test -tags=test_state_path_env -count=1 -timeout 5m ./internal/api/ ./internal/cli/`. Full
sweep because this removes a mechanism — verify nothing else linked against it. State-safe caveat
as above (back up live intent; sweep `mcphub.exe`).

### Quality gate
`$architecture-reviewer` (mechanism deletion + C6 residue check — confirm no stale F5/serena-arm
text or dead helper survives) + `$qa-engineer` (regression: no protection lost for any daemon
class; byte-symmetric round-trip).

---

## Integration owner + cross-phase re-review

- **Integration owner: the main conversation** (this is a `requiresLead:false` refactor chain). It
  holds the `fix/…` branch, sequences the phases, runs the pre-push set itself after every phase
  (never trusts a subagent "all green" — MEMORY `feedback_kosyak_subagent_summary_overstates`),
  drives the commission + Codex bot PASS loop per PR, and owns the final E2E. Escalate to `$lead`
  only if the phases are split across parallel implementers and integration conflicts arise.
- **Re-review trigger:** if Phase 1's owner API is revised after a downstream phase has consumed it
  (e.g. the mismatch rule changes), mark the dependent phases (3a–3d) for re-review — their
  acceptance rests on the owner contract.
- **Recovery state:** after each phase transition, update this item's `status.md` with
  template/owner/active-phase/next-action, and keep this `plan.md` as the accepted artifact so any
  future session resumes from the last landed phase.

---

## Final end-to-end verification (owned by `$qa-engineer`; executed by the integration owner on a live host)

Do **not** assume the resolver-consuming path behaves like today's F5-backfilled path — prove it on
a real supervisor restart. This runs after Phase 4 is merged and the binary is redeployed.

### Deploy discipline (MEMORY `feedback_always_redeploy_after_merge` + `feedback_mcphub_deploy_cross_volume_recovery`)
Commit → `build.sh` → rename-aside binary swap → **FULL supervisor restart** (the reconcile only
re-reads intent on a cold start). D:→C: `MoveFileEx` can fail cross-volume; batch-stop slow LSP
ports before the upgrade if needed. **Back up `%LOCALAPPDATA%\mcp-local-hub\supervisor-intent.json`
first.**

### E2E steps
1. **Baseline (pre-deploy, F5-present binary).** Record for a known legacy daemon (e.g.
   `memory@9123`): `mcphub status --json` (port, state) and `claude mcp get mcphub-hub`. On the
   live host F5 has already stamped ports, so the common case is `Port>0`.
2. **Genuine `Port=0` path (the load-bearing test).** In a **temp/backup copy** of the intent (or a
   redirected state dir), set a known daemon's `Port` back to `0`, start `mcphub supervise` against
   it with the **new (F5-deleted) binary**, and confirm via `mcphub status`/`--json` +
   `supervisor-events.log` that the daemon is **port-classified and protected** (right port shown,
   Running/latched — NOT early-healthy), i.e. the resolver activated protection **without F5**.
   This is the direct proof that the ~9 legacy `Port=0` daemons keep protection.
3. **Common-case non-regression.** On the real host, after the cold restart the previously-
   F5-backfilled daemons (`Port>0`) short-circuit the resolver and behave identically; confirm
   `mcphub status` + `claude mcp list` / `claude mcp get mcphub-hub` are healthy (MEMORY: `claude
   mcp list` slowness is claude-side, not mcphub — verify the hub via `claude mcp get mcphub-hub`).
4. **Serena-specific.** If a legacy-unified serena row exists, confirm it now resolves 9121 + 120s,
   does **not** emit `daemon-bind-timeout` on cold start (watch `supervisor-events.log`), and serena
   tools answer (`claude mcp get mcphub-hub`). If serena is on the dynamic pool, confirm the pool
   rows (explicit 120) are unaffected.
5. **Round-trip.** Confirm the new binary's cold start does **not** rewrite `supervisor-intent.json`
   (no F5 write-pass): the `Port=0` you set in step 2 is still `0` on disk after the restart, yet
   the daemon is protected (byte-symmetric; the field is honestly a cache).
6. **Cleanup.** Restore the backed-up intent; sweep test `mcphub.exe`; verify the live fleet via
   `claude mcp get mcphub-hub`.

---

## Optional follow-up (OUT of the admitted scope — requires re-admission before it is planned)

**Force-stop `Port=0` gap** (design §12; filed as
`work-items/bugs/2026-07-05-stop-force-supervisor-port-zero-gap.md`).
`internal/api/stop_force_supervisor.go:155-182` falls back to PID-only kill when `d.Port == 0`, so a
legacy row's kill-by-port safety net is off — the same structural gap this plan closes for the
liveness path, but a **different verb** the architect explicitly left out of scope (design §11
Non-goals). Once the owner exists (Phase 1), the fix is a one-line `api.EffectiveDaemonPort(d)`
resolve at entry. **Not part of this plan** — the user/orchestrator must re-admit it (it touches a
protected/adjacent surface outside the Change-Surface Contract). If admitted, it slots as a small
independent phase gated by `$security-reviewer` (force-stop is kill authority).

---

## Gate

Every phase is small enough to implement, review, commit, and roll back independently; file scope,
allowed change surface, nearby test coverage, tests, repo-standard checks, and acceptance criteria
(stable per-phase AC-IDs) are explicit for each. The one hard ordering constraint (Phase 2 before
Phase 3a; all decision consumers before Phase 4) is stated with its correctness rationale.
Parallel phases (3b/3c/3d, and 3a after Phase 2) are used only where the owner contract and write
boundaries are already fixed. No implementation code is included. Every phase fits inside the
architect's Change-Surface Contract; the one out-of-contract finding (force-stop) is escalated as a
re-admission, not folded in. Supersession of the PR #504 serena-guard, the recover 3-way hint, and
the status "NOT redundant by F5" comment is mapped to specific phases so no stale-relation residue
survives (C6).

**PASS** — ready for implementation. Recommended order:
Phase 1 → Phase 2 → Phase 3a → {Phase 3b ∥ Phase 3c ∥ Phase 3d} → Phase 4 → E2E.

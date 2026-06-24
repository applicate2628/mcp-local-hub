---
status: proposed
date: 2026-06-23
slug: d3-availability-probe
---

# D-3 — availability lifecycle gate + install-probe dry-run (catalog Tier-0)

## Context

Some desktop catalog rows depend on a host app/tool that may not be present
(MATLAB, a Mathcad COM server, a locally-built binary). Listing such a row as
directly installable would let the operator install a server that can never spawn —
committing supervisor-intent + client-config state for a guaranteed-dead daemon.
The row must stay INERT until its host dependency is detected, then enable.

Tier-0 adds the SCHEMA SEAM + the gate wiring (no catalog DATA rows yet).

## Decision

Add OPTIONAL fields on both sides of the catalog/manifest boundary:

- `config.ServerManifest.Availability string` (`""|ready|watch|disabled-until-probe`)
  and `InstallProbe *AvailabilityProbe` (`{binaries[], files[]}`).
- `api.MarketplaceEntry.Availability` + `InstallProbe *CatalogAvailabilityProbe`
  (JSON mirror) for `catalog.json`; plus a read-only `availability` field on the GUI
  browse DTO so the frontend can grey a watch row ("probe to enable").

`""`/`ready` == the universal current behavior. `watch`/`disabled-until-probe` mark
a row that MUST NOT spawn a daemon nor write a client config until its probe passes.

### Two gates, no re-implemented detection

- **Gate A — `config.ServerManifest.Validate()`** (pure schema gate):
  - A4: `availability` non-empty and not in `{ready,watch,disabled-until-probe}` →
    REJECT.
  - A5: `install_probe` set with `availability` ready/empty → REJECT (dead config).
  - A6: `availability` watch/disabled with nil/empty probe → REJECT (a watch row with
    no probe can never become ready).
- **Gate B — `api.AdmissionCheck()`** (host-state readiness owner shared by Preflight
  + CheckServerReadiness): if the row is inert AND its probe does NOT pass, append a
  NON-OPTIONAL `availability-probe` finding and short-circuit. Because `Install` runs
  `Preflight` BEFORE `installPlanCore` (`install.go` step 2 before step 4), a
  non-optional finding aborts the install BEFORE any supervisor-intent row or client
  config is written. This is the single chokepoint guaranteeing "an inert row NEVER
  spawns nor writes until the probe passes". When the probe PASSES the row falls
  through and behaves exactly like a ready row.

### The probe is the readiness gate reused as a DRY-RUN

`availabilityProbePasses(p)` composes the EXISTING single-owner readiness primitives
`binaryAvailable` (PATH/toolchain runnability) and `entryScriptStatus` (regular-file
existence) — the exact functions Preflight and CheckServerReadiness already use. It
adds ZERO new detection, so there is no second detection path that can drift from the
install gate (the mirror-gate lesson:
`2026-06-20-draft-readiness-mirrors-write-gate-follow-up.md`). AND semantics: every
declared binary must be runnable AND every declared file must exist.

`availabilityInert(m)` is the single owner of the watch/disabled predicate, consumed
by the gate and the GUI signal so "inert" is defined exactly once.

### No background watcher

The probe is evaluated PULL-only — on every readiness query and at every install
attempt. NO background watcher, scheduler, or timer (governance: explicit-bounds-for-
background-work; supervisor boundary stays PROTECTED). The operator installs the host
app, then re-checks readiness / clicks install, and the probe passes on that next pull.

## Additive guarantee

`Availability` (string `omitempty`) and `InstallProbe` (pointer `omitempty`) are
absent on every existing manifest and catalog entry → `Availability=="" ` (treated as
ready), `InstallProbe==nil`. The Validate helper and the AdmissionCheck block both
short-circuit on `availabilityInert==false`. Byte-identical on a host with no desktop
rows.

## Consequences

- A watch/disabled row is inert by construction at the shared install chokepoint —
  never a half-installed dead daemon.
- Detection cannot diverge from the install gate (same primitives, same owner).
- NO supervisor/IPC/SecureWriteClientConfig/remote-http-allowlist/groups/hub-port
  change; NO catalog DATA row in Tier-0.

## Residual (deferred to Tier-1+): runtime-state-change re-spawn is not gated

The D-3 gate is an **install-entry admission** invariant: it blocks an inert (watch /
disabled-until-probe) row from being installed, registered, or one-click-installed,
enforced at all 5 entry paths (`marketplace_install.go:136`, `install_parsed_manifest.go`
via Preflight, `(*API).Install` via Preflight, `register.go:153`, `lsp_auto_register.go:72`)
through the single owner `availabilityProbeFinding`. The two reconcile surfaces —
hub-mode client-config reconcile (`install_hub_reconcile.go:483`) and supervisor daemon
reconcile (`supervise_reconcile.go:259`) — are NOT independently gated, **by design,
verified safe** ($architect 2026-06-23): both derive EXCLUSIVELY from already-installed,
gate-passed state (hub-reconcile via `manifestHasInstallSignal` `install.go:613`, which
keys only on post-install supervisor rows / scheduler tasks; supervisor-reconcile via
persisted `SupervisorDaemon` rows built ONLY by `supervisorDaemonsFromPlan`
`install_parsed_manifest.go:795`, inside `InstallParsedManifest` after Validate+Preflight).
No inert row can reach them (an inert row is install-blocked → no install signal → never
persisted). Tier-0 ships ZERO catalog rows, so the case is unreachable today.

The uncovered case is a **runtime-state change**: a row installed while READY whose host
app is later removed so its `install_probe` would now FAIL — the next reconcile re-spawns /
re-writes the now-unbacked daemon. Closing this requires persisting `availability` +
`install_probe` into `SupervisorDaemon` (a `supervisor-intent.json` schema change — a
Tier-0-PROTECTED surface) and re-evaluating the probe at reconcile. This is a runtime-
health / staleness concern distinct from the install-time D-3 admission gate → deferred to
`work-items/backlog/2026-06-23-d3-reconcile-reprobe.md` (prereq: Tier-1 inert rows).

## Tier-1 amendment: glob in files[] (version-agnostic probes for a SHARED catalog)

Status stays **proposed** — this is the SAME D-3 probe-model decision owner extended for the
first Tier-1 data batch, not a new decision id.

### Problem

A single `catalog.json` is SHARED across every host. A files[] probe that bakes ONE exact host
path (e.g. `C:\ProgramData\Ableton\Live 11 Suite\…\Ableton Live 11 Suite.exe` or
`C:\Program Files\Microsoft Office\root\Office16\EXCEL.EXE`) only ever matches one product
version / install layout, so the same shared row stays permanently inert on a host that has
Ableton Live **12** or 64-bit / (x86) Click-to-Run Excel. Both the codex bot and a sonnet
commission flagged this baked-exact-path specificity in a shared catalog.

### Decision

A files[] entry MAY carry glob metacharacters (`*` / `?` / `[`). The runtime probe owner
`fileProbeMatches` (`internal/api/readiness.go`) expands the pattern via `filepath.Glob` and
then COMPOSES the existing regular-file owner `entryScriptStatus` over each match — passing iff
ANY match is a runnable regular file. No new detection is added; `entryScriptStatus` stays the
literal-path stat owner and the glob helper only widens the input from one exact path to a
match set.

- **Fail-closed polarity preserved.** `filepath.Glob` returns `(nil, nil)` on NO match →
  `(false, "does not exist")`, byte-identical to today's `os.Stat` on a missing literal. A
  malformed pattern (`ErrBadPattern`) fails inert with a named reason.
- **Metacharacter-free == byte-identical.** `filepath.Glob` of a literal returns exactly that
  one path when it exists, so a probe with no wildcard behaves EXACTLY as before — the only
  behavioral widening is for patterns that actually contain a wildcard.
- **AND-semantics across entries UNCHANGED.** Every declared binary AND every declared files[]
  pattern must still pass; a glob is OR only WITHIN one pattern's own match set (any match
  satisfies that single entry), never across entries.
- **Validator.** `ValidateProbeValuesNonEmpty` (`internal/config/manifest.go`) already accepted
  glob metacharacters (it keys only on the absolute PREFIX via `IsAbsolutePathShape` + the
  non-empty / no-surrounding-whitespace rules); the amendment makes that acceptance EXPLICIT in
  the rule-4 doc so a future edit does not add a wildcard rejection. A glob pattern is still an
  absolute path with wildcards.
- **Browse path UNCHANGED.** `MarketplaceEntryBrowseProbeState` still classifies ANY files[]
  row (glob or literal) as `inert-unknown` and NEVER runs `filepath.Glob` / `os.Stat` on the
  browse projection — the no-os.Stat-on-browse invariant holds for glob patterns too.

### First-batch probes

- ableton: `C:\ProgramData\Ableton\Live * Suite\Program\Ableton Live * Suite.exe` (Live 11/12
  Suite). DISCLOSED scope: Windows Live Suite editions; macOS + non-Suite editions NOT covered
  (a 2nd files[] entry would be AND'd, not OR'd — disclose, do not escalate to or-groups).
- excel: `C:\Program Files*\Microsoft Office\root\Office1?\EXCEL.EXE` (64-bit + (x86)
  Click-to-Run via the single `*`+`?` pattern — `Program Files*` spans ` (x86)`). DISCLOSED
  scope: MSI volume-license installs (no `root\` segment) NOT covered in this batch.
- codex-mcp-server: UNCHANGED (`binaries: [codex]` bare name, already version-agnostic).

### Adjacent footgun (filed, NOT fixed here)

mathcad was DROPPED from the first batch partly because `${workspaceFolder}` is a GENERATE-time
token that freezes to CWD for a `kind:global` daemon (a category error — the spawn-time
workspace token is the DIFFERENT `${workspace.path}`, workspace-scoped only). A catalog-validator
warning for that footgun is filed at
`work-items/bugs/2026-06-24-workspacefolder-global-daemon-footgun.md` (out of scope for this
catalog-data PR).

The `$architecture-reviewer` promotes proposed → accepted.

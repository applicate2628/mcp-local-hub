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
`fileProbeMatches` (`internal/api/readiness.go`) stats the VERBATIM literal FIRST via
`entryScriptStatus`, and only if that literal does not exist falls back to `filepath.Glob` and
COMPOSES `entryScriptStatus` over each match — passing iff the literal OR any glob match is a
runnable regular file. No new detection is added; `entryScriptStatus` stays the literal-path
stat owner and the glob helper only widens the input from one exact path to a match set.

- **EXACT-STAT FIRST (codex catalog finding 1 — literal-path-with-metachars).** A real existing
  manifest can carry a LITERAL absolute path whose dir/file name contains a glob metacharacter
  (e.g. `/Applications/Foo [Beta]/x`). An unconditional `filepath.Glob` would interpret `[Beta]`
  as a character class and miss the literal (VERIFIED: `Glob` of an existing `Foo [Beta]/bin/x`
  returns `[]`), wrongly disabling a row whose file exists. The fix stats the verbatim literal
  first, so a metacharacter-bearing literal that exists passes WITHOUT being globbed; only a
  non-existent literal (the normal case for a genuine version glob like `Live *`) falls to glob.
- **Fail-closed polarity preserved.** When the literal does not exist, `filepath.Glob` returns
  `(nil, nil)` on NO match → `(false, "does not exist")`, byte-identical to today's `os.Stat` on
  a missing literal. A malformed pattern (`ErrBadPattern`) — reachable only when the literal did
  NOT exist AND the glob is malformed — fails inert with a named reason.
- **Metacharacter-free == byte-identical.** The exact stat already satisfies a literal path, so a
  probe with no wildcard behaves EXACTLY as before — the only behavioral widening is for patterns
  that actually contain a wildcard AND whose literal path does not itself exist.
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

### Tier-1 first-batch probes (5 rows: excel, ableton, codex-mcp-server, matlab, ansys)

- ableton: `C:\ProgramData\Ableton\Live *\Program\Ableton Live *.exe` (BROADENED from the
  initial Suite-only glob — the ableton-mcp README supports Live 10+ in ANY edition, so the
  Suite-only pattern wrongly missed Standard/Intro/Lite). DISCLOSED scope: ALL Windows Live
  editions (Live 11/12, Suite/Standard/Intro/Lite); macOS (`/Applications/Ableton Live *.app`)
  NOT covered in this batch (a 2nd files[] entry would be AND'd, not OR'd — disclose, do not
  escalate to or-groups). PRIVACY: the pinned ableton-mcp ships telemetry ON by default
  (`_user_consent = True` in `MCP_Server/telemetry.py` at commit `5e9ffbd`); the curated row sets
  `env.ABLETON_MCP_DISABLE_TELEMETRY=true` (one of the three verified disable vars
  `DISABLE_TELEMETRY` / `ABLETON_MCP_DISABLE_TELEMETRY` / `MCP_DISABLE_TELEMETRY`, accepted values
  `true/1/yes/on`) so no usage data leaves the host by default.
- excel: `C:\Program Files*\Microsoft Office\root\Office1?\EXCEL.EXE` (64-bit + (x86)
  Click-to-Run via the single `*`+`?` pattern — `Program Files*` spans ` (x86)`). DISCLOSED scope
  (now honest in the row name + summary): Click-to-Run / Microsoft 365 only — the `root\` segment
  is the Click-to-Run signature. MSI / volume-license installs (`…\Microsoft Office\Office16\…`,
  no `root\`) NOT covered in this batch; `filepath.Glob` has no `**` and files[] is AND across
  entries, so the optional `root\` cannot be OR'd. Disclose-and-restrict (do NOT change the glob
  to one that silently fails); C2R+MSI coverage is the or-groups D-3 follow-up below.
- codex-mcp-server: UNCHANGED (`binaries: [codex]` bare name, already version-agnostic).
- matlab: `binaries: [matlab-mcp-server]` AND `files: [C:\Program Files\MATLAB\R*\bin\matlab.exe]`
  — `R*` is an intentional release-version glob (R2021a, R2024b, …); the literal `R*` never
  exists, so exact-stat-first falls straight to glob. No finding-1 literal-glob regression.
- ansys: `files: [C:\Program Files\ANSYS Inc\v*\ansys\bin\winx64\ansys*.exe]` — `v*` (v231/v241/…)
  and `ansys*` (ansys241.exe, version-stamped) are intentional version globs; the literal never
  exists, so exact-stat-first falls to glob. No finding-1 literal-glob regression.

### or-groups probe model — D-3 follow-up (NOT built here)

The disclose-and-restrict rows above each leave a minority install layout uncovered because
`filepath.Glob` has no `**` and the files[] probe is AND across entries (it cannot OR alternative
install roots within one row). The next D-3 enhancement is a files[] OR-GROUP probe model: a row
declares a small set of alternative path patterns, ANY of which satisfies the row. It would let a
single row cover, in ONE pass: Excel Click-to-Run + MSI/volume-license; Ableton Windows +
macOS; and any multi-root desktop app (matlab/ansys multiple install roots). Scope: a config
schema addition (`files` becomes a list-of-or-groups, or a sibling `files_any` field) + the
`availabilityProbePasses` owner + browse classifier + validator, with the exact-stat-first
literal handling preserved per group entry. This is a design + research item under the desktop-app
epic, out of scope for the catalog-data PR.

### Adjacent footgun (filed, NOT fixed here)

mathcad was DROPPED from the first batch partly because `${workspaceFolder}` is a GENERATE-time
token that freezes to CWD for a `kind:global` daemon (a category error — the spawn-time
workspace token is the DIFFERENT `${workspace.path}`, workspace-scoped only). A catalog-validator
warning for that footgun is filed at
`work-items/bugs/2026-06-24-workspacefolder-global-daemon-footgun.md` (out of scope for this
catalog-data PR).

The `$architecture-reviewer` promotes proposed → accepted.

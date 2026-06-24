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

### Decision — EXPLICIT files[] vs file_globs[] split (supersedes the glob-in-files[] model)

Glob expansion is now **OPT-IN via a separate `file_globs[]` field**, not inferred from a
`files[]` value. `files[]` is the LITERAL-path field — stat'd VERBATIM via `entryScriptStatus`
and NEVER globbed. `file_globs[]` is the version-agnostic PATTERN field — `filepath.Glob`-expanded
by the runtime owner `globProbeMatches` (`internal/api/readiness.go`), passing iff ANY match is a
runnable regular file. `availabilityProbePasses` AND's all three fields: every binary on PATH AND
every files[] literal exists AND every file_globs[] pattern has a match.

> **Supersedes** the earlier "a files[] entry MAY carry glob metacharacters; the runtime owner
> `fileProbeMatches` stats the literal first then falls back to `filepath.Glob`" model. That
> exact-stat-first-then-glob fallback was ambiguous: an ABSENT literal `files[]` path that happened
> to contain a metacharacter (a real `/opt/Foo*/marker` or `Foo [Beta]` install) would silently
> fall through to glob and a SIBLING (`/opt/FooBeta/marker`) could satisfy it — installing
> `AvailabilityAdmissionEntry` against the WRONG file. The single-field model could not tell
> "literal path that contains a metachar" from "intentional pattern". The explicit split removes the
> ambiguity: `fileProbeMatches` is renamed `globProbeMatches` and is GLOB-ONLY (no exact-stat-first
> branch); literals live in `files[]` (exact only). The codex finding-1 metachar-literal regression
> is now handled structurally — a literal with a metachar simply goes in `files[]` and is never
> globbed.

- **files[] is EXACT-STAT ONLY.** `entryScriptStatus(f)` stats the verbatim path. A metacharacter
  in a files[] value is treated literally (a directory really named `Foo*` or `Foo [Beta]`); the
  exact file at that path must exist as a runnable regular file. No glob, no sibling fallback.
- **file_globs[] is GLOB-ONLY.** `globProbeMatches(g)` calls `filepath.Glob(g)` and composes
  `entryScriptStatus` over each match. This is where version-agnostic patterns
  (`Mathcad Prime *` / `Live *` / `R*` / `v*`) live. No exact-stat-first — the field IS the intent.
- **Fail-closed polarity preserved.** A files[] literal that does not exist → `(false, "does not
  exist")`. A file_globs[] pattern with NO match → `(false, "does not exist")` (byte-identical
  diagnostic). A malformed glob (`ErrBadPattern`) → fail inert with a named reason. A directory
  match is rejected exactly as a literal directory.
- **AND-semantics across ALL three fields UNCHANGED.** Every declared binary AND every files[]
  literal AND every file_globs[] pattern must pass; a glob is OR only WITHIN one pattern's own
  match set (any match satisfies that single entry), never across entries.
- **Validator.** `ValidateProbeValuesNonEmpty` (`internal/config/manifest.go`) applies the SAME
  non-empty / no-surrounding-whitespace / absolute-path-shape rules to BOTH files[] and
  file_globs[] (rules 4 and 5); the metacharacters are explicitly ALLOWED on file_globs[] (that is
  the field's purpose) and treated literally on files[]. A6 now requires a non-empty probe across
  binaries OR files OR file_globs.
- **Browse path UNCHANGED.** `MarketplaceEntryBrowseProbeState` classifies ANY files[] OR
  file_globs[] row as `inert-unknown` and NEVER runs `filepath.Glob` / `os.Stat` on the browse
  projection — the no-os.Stat/no-glob-on-browse invariant holds for both fields.
- **Catalog mirror.** `CatalogAvailabilityProbe` (`internal/api/marketplace_catalog.go`) gains
  `file_globs` (json), `catalogProbeToConfig` carries it onto `config.AvailabilityProbe`, and
  `projectAvailability` (`internal/api/marketplace_generate.go`) projects it into the drafted
  manifest — so a `file_globs[]` row survives generate→create→install with the gate intact.

### Tier-1 first-batch probes (5 rows: excel, ableton, codex-mcp-server, matlab, ansys)

All version-agnostic patterns now live in `file_globs[]` (the opt-in glob field), never `files[]`.

- ableton: `file_globs: [C:\ProgramData\Ableton\Live *\Program\Ableton Live *.exe]` (BROADENED from
  the initial Suite-only glob — the ableton-mcp README supports Live 10+ in ANY edition, so the
  Suite-only pattern wrongly missed Standard/Intro/Lite). DISCLOSED scope: ALL Windows Live
  editions (Live 11/12, Suite/Standard/Intro/Lite); macOS (`/Applications/Ableton Live *.app`)
  NOT covered in this batch (a 2nd file_globs[] entry would be AND'd, not OR'd — disclose, do not
  escalate to or-groups). PRIVACY: the pinned ableton-mcp ships telemetry ON by default
  (`_user_consent = True` in `MCP_Server/telemetry.py` at commit `5e9ffbd`); the curated row sets
  `env.ABLETON_MCP_DISABLE_TELEMETRY=true` (one of the three verified disable vars
  `DISABLE_TELEMETRY` / `ABLETON_MCP_DISABLE_TELEMETRY` / `MCP_DISABLE_TELEMETRY`, accepted values
  `true/1/yes/on`) so no usage data leaves the host by default.
- excel: `binaries: [mcp-excel]` AND `file_globs:
  [C:\Program Files*\Microsoft Office\root\Office1?\EXCEL.EXE]` (64-bit + (x86) Click-to-Run via
  the single `*`+`?` pattern — `Program Files*` spans the `(x86)` suffix). DISCLOSED scope (now honest in the
  row name + summary): Click-to-Run / Microsoft 365 only — the `root\` segment is the Click-to-Run
  signature. MSI / volume-license installs (`…\Microsoft Office\Office16\…`, no `root\`) NOT
  covered in this batch; `filepath.Glob` has no `**` and the probe is AND across entries, so the
  optional `root\` cannot be OR'd. Disclose-and-restrict (do NOT change the glob to one that
  silently fails); C2R+MSI coverage is the or-groups D-3 follow-up below.
- codex-mcp-server: UNCHANGED (`binaries: [codex]` bare name, already version-agnostic).
- matlab: `binaries: [matlab-mcp-server, matlab]` — NO file glob (codex catalog finding 2). The
  matlab-mcp-server README (v0.11.0) is explicit: setup step 1 is "Install MATLAB … and **add it
  to the system PATH**", and "By default, the server tries to find the first MATLAB on the system
  PATH". The row sets no args/env (no `--matlab-root`, no `MW_MCP_SERVER_MATLAB_ROOT`), so PATH is
  the ONLY discovery the server uses. The old `files: [C:\Program Files\MATLAB\R*\bin\matlab.exe]`
  file-glob would pass for a MATLAB INSTALLED-BUT-NOT-ON-PATH host, where the server then fails to
  find MATLAB → broken one-click. Requiring `matlab` on PATH makes the probe AGREE with the
  server's own discovery; the file-glob is redundant once `matlab`-on-PATH is the authoritative
  signal, so it is DROPPED. Operators who keep MATLAB off PATH add `--matlab-root` after install
  (documented in the row summary).
- ansys: `file_globs: [C:\Program Files\ANSYS Inc\v*\ansys\bin\winx64\ansys*.exe]` — `v*`
  (v231/v241/…) and `ansys*` (ansys241.exe, version-stamped) are intentional version globs in the
  opt-in glob field.

### or-groups probe model — D-3 follow-up (NOT built here)

The disclose-and-restrict rows above each leave a minority install layout uncovered because
`filepath.Glob` has no `**` and the probe is AND across entries (it cannot OR alternative
install roots within one row). The next D-3 enhancement is a file_globs[] OR-GROUP probe model: a
row declares a small set of alternative path patterns, ANY of which satisfies the row. It would let
a single row cover, in ONE pass: Excel Click-to-Run + MSI/volume-license; Ableton Windows +
macOS; and any multi-root desktop app (matlab/ansys multiple install roots). Scope: a config
schema addition (`file_globs` becomes a list-of-or-groups, or a sibling `file_globs_any` field) +
the `availabilityProbePasses` owner + browse classifier + validator, with the glob
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

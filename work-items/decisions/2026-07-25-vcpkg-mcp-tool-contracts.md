---
title: vcpkg MCP — tool contracts, discovery, and the behavioural invariants
status: proposed
date: 2026-07-25
owner: lead (design) — operator go-ahead given ("б в первую очередь")
relates-to:
  - work-items/decisions/2026-07-25-lsp-backend-kind-taxonomy-and-local-tool-seam.md (proposed, sibling)
---

## Why this exists

Operator's measured pain from one day of vcpkg work (their words, condensed):

1. **Structural failure analysis.** `build_failed.log` gives only port names. The real error sits in a
   per-configuration log at an unpredictable path; `grep error` catches filenames like
   `error_estimator.h`; libtool erases the return code. Each of seven failures cost 3-4 rounds of
   digging. **~40% of the day.**
2. **Overlay resolution.** A four-level chain (`ports → ports_upd → port-linux → ports_mkl → builtin`);
   "which port definition actually wins for triplet T" repeatedly became a source of wrong assumptions.
3. **Pin status.** Auditing 60 ports against upstream became a 49-agent workflow. The task is
   deterministic — read pin, ask remote, compare. Agents are wrong in the details; twice they were.
4. **Patch verification with the right semantics.** Patches apply SEQUENTIALLY into an accumulating
   tree. Not knowing this deleted a live `gmp` patch: an agent checked the patch in isolation, got a
   rejection, and concluded it was obsolete.

## Behavioural invariants (operator-stated; these are contract, not preference)

- **Read-only by default.** No build is ever started without explicit confirmation — a machine hung
  dead once. Builds live in a SEPARATE tool name, never auto-invoked, gated on `confirm: true`.
- **Return evidence, not conclusions.** Every answer carries `file:line`, log paths, exact commands.
  A tool that returns only verdicts becomes a source of unverifiable claims; the whole day proved
  that verifiability is what catches errors.
- **Never hide uncertainty.** If a pin cannot be resolved, say so — do not return "current". A false
  OK is worse than an honest "don't know": it silently drops a port from the work list.
- Corollary (design): every result is **tri-state** — `ok | failed | unknown(reason)` — with `reason`
  drawn from a CLOSED enum, never free text (free text is unauditable).

## Root discovery — never hardcode, never silently guess

Authoritative rule (Microsoft docs): **there is no single default install path**; the vcpkg root is the
directory containing the `vcpkg` program, and `VCPKG_ROOT` is the recommended convention.
Discovery order, highest first, and the tool ALWAYS reports which rule fired:

1. Explicit parameter supplied by the caller.
2. `VCPKG_ROOT` environment variable.
3. `vcpkg` resolved on `PATH` → its containing directory (the documented rule).
4. Manifest mode: walk up from the working directory to the nearest `vcpkg.json` /
   `vcpkg-configuration.json`.
5. Heuristic common locations, explicitly labelled as heuristic:
   `C:\vcpkg`, `C:\opt\vcpkg`, `C:\dev\vcpkg`, `%USERPROFILE%\vcpkg`, `D:\vcpkg`,
   `%ProgramFiles%\Microsoft Visual Studio\*\*\VC\vcpkg`, `/opt/vcpkg`, `~/vcpkg`.

Outcomes:
- exactly one → use it, report the rule + path.
- **several → report ALL candidates and ask; never silently pick one.**
- **none → say so plainly and OFFER manual specification.** Never report "not installed".

(Operator directive 2026-07-25: "нужно делать поиск по основным популярным местам (как и msys64)" +
"если ничего не найдено, предлагать указать руками".)

## The tools

### 1. `vcpkg_last_failure(port, triplet?)` — the flagship (~40% of the pain)
Returns structured: `{phase: configure|build|install, failed_target, exact_command,
diagnostics[{file,line,severity,text}], exit_code, log_paths[]}`.
- Does NOT parse everything: extracts the FIRST real diagnostic + the failing command, and **always
  returns the log paths** so an agent can read further itself.
- Handles the two traps the operator hit: libtool erasing the return code (classify by diagnostic,
  never by exit code alone) and `grep error` matching filenames (match on diagnostic POSITION/shape,
  not substring).
- `unknown(reason)` when the failing target cannot be identified — with the log paths still returned.

### 2. `vcpkg_port_resolution(port, triplet, overlays?)` — which definition wins
Walks the overlay chain in **vcpkg's own precedence order** — NOT from directory-name conventions
(`ports_upd` / `ports_mkl` / `port-linux` are the operator's convention, not a vcpkg rule; relying on
the names would reproduce exactly the wrong-assumption failure this tool exists to remove).

**Overlay sources, highest precedence first — the tool ALWAYS reports which source it used:**
1. **Explicit `overlays: []` parameter** (operator directive 2026-07-25: "можно указать флагами").
   An ordered list — **order IS precedence**, mirroring repeated `--overlay-ports` on vcpkg's own
   command line. This is the primary path: the operator's overlay set is not discoverable from disk
   conventions, and vcpkg itself takes it this way.
2. `VCPKG_OVERLAY_PORTS` environment variable (platform-separated list, same ordering rule).
3. `overlay-ports` in the nearest `vcpkg-configuration.json`.
4. None → builtin `ports` only, stated explicitly in the result (never implied).

Returns `{winner_path, shadowed[], reason, overlay_source, overlay_chain[]}` — the chain is echoed
back so the caller can verify the precedence the answer rests on, not just trust the verdict.

The same overlay parameter is accepted by every tool whose answer depends on which port definition
wins (`last_failure`, `patches_apply`, `pin_status`), so a session can be run against a consistent
chain rather than re-specified per call.

### 3. `vcpkg_pin_status(ports[])` — replaces the 49-agent workflow
Batched: read pin → query remote (`git ls-remote`) → compare.

**AMENDED 2026-07-26 after a live 61-remote probe (see the ground-truth doc §8). `behind` is NOT
a producible verdict on the default read-only path — the enum is `current | unknown(reason)`.**
`git ls-remote` proves only what a named ref points at NOW. When the pin equals it, `current` is
sound. When it does not, ls-remote cannot establish that the pinned commit still exists, that it is
an ANCESTOR of the tip rather than diverged/rebased-away, or how far behind it is; a
`refs/tags/<40-hex>` lookup is a tag-NAME query whose empty result proves nothing. The original
`behind(details)` promise was therefore unimplementable as specified. Measured on the operator's
59 ports: **40 `current`, 19 `unknown`** (13 with no git remote at all, 6 git-comparable but
non-tip — `libmesh`, `sleipnir`, `hpx`, `ngspice`, `python3`, `skia`). Note the 6 are exactly the
ports an operator most wants adjudicated.

Upgrading any of those 6 to a real `behind` requires ancestry data — a fetch, or a forge-specific
compare API. That is a SEPARATE, explicitly opt-in capability (it costs network and, for a fetch,
disk), never the default. Until then the tool says `unknown(pin_not_at_tip)` and hands back both
SHAs plus the compare URL, rather than guessing a direction.

Cost measured: 61 round-trips / 60.8 s, median ~0.7 s per remote, worst 11.7 s (`skia`). One port is
interactive; a whole-overlay scan is not — cache it, and never present a cached verdict as live
without saying so. Still never coerces unknown → current.

### 4. `vcpkg_patches_apply(port)` — correct semantics
Applies patches **sequentially into an accumulating temp worktree** of the source at the pinned ref —
not each patch in isolation. Per patch: `applies | fails | already-applied` + reject hunks.
This is the tool that would have saved the `gmp` patch.

### 5. `vcpkg_cmake_trace(port, triplet)` — dead branches + undefined-vs-empty
Reads an EXISTING `cmake --trace-expand --trace-format=json-v1` trace / configure log and answers:
which lines executed (and which did NOT = a dead branch), what a variable expanded to, which
`include()`s fired and in what order. `--warn-uninitialized` is the honest answer to
undefined-vs-empty (statically indistinguishable; observable at configure time).
A fresh configure run is a MUTATION and therefore requires explicit `confirm`.

## CMake text-parsing floor (ADDED 2026-07-26 — verified against the official grammar)

Every tool here that reads `portfile.cmake` as TEXT (`pin_status`, `patches_apply`, and the
`cmakegraph` resolver behind `cmake_include_graph`) must handle four shapes from
[cmake-language(7)](https://cmake.org/cmake/help/latest/manual/cmake-language.7.html). Checked against
the published grammar, not assumed — two of them let a naive parser read COMMENTED-OUT text as code:

1. **Bracket comments are MULTI-LINE.** `bracket_comment ::= '#' bracket_argument`, so `#[[ ... ]]`
   spans lines. A line-based `#`-to-EOL stripper removes only the first line and feeds the rest to the
   parser as real code — a commented-out `PATCHES` block or `vcpkg_from_github(` call then gets picked
   up as live.
2. **Bracket delimiters have VARIABLE length.** `bracket_open ::= '[' '='* '['`,
   `bracket_close ::= ']' '='* ']'`, and the content is any text not containing a close with the SAME
   equals-count. `[[...]]`, `[=[...]=]`, `[==[...]==]` are all legal, and a `]]` inside a `[=[ ... ]=]`
   does NOT close it. Match by equals-count, never a fixed `]]`.
3. **Nothing is interpreted inside a bracket argument** — no `\` escapes, no `${var}`, and a `;` does
   not split it. It is exactly ONE argument.
4. **Quoted arguments support line continuation** (`quoted_continuation ::= '\' newline`), so a quoted
   argument can span lines and a per-line quote-state reset mis-tracks it.

**Status at the time of writing: BOTH shipped parsers fail 1 and 2.**
`pinstatus/portfile.go:84 stripComments` is line-based `#`-to-EOL only, and `patchesapply/lexer.go`
has the same shape. Recorded here rather than silently patched because it is a CLASS, not one bug:
any future text-reading tool inherits it. A fixture per shape is required — at minimum a bracket
comment containing a decoy that must NOT be picked up, and a `[=[ ... ]=]` containing a literal `]]`.

This is the same failure mode that cost the cmakegraph resolver six P1s: assuming CMake text parses
the way it looks instead of the way the grammar says.

## Boundary with the cmake side (no duplication)

Rule: **a tool lives where its INPUT lives.**
- Input = CMake source → the cmake side (`internal/cmakegraph` include-graph resolver, the LSP surface).
- Input = vcpkg's model (overlays, buildtrees, logs, pins, patches) → this MCP.
This MCP knows WHERE artifacts live and hands out paths; it does not re-implement CMake analysis.

## Implementation shape — DECIDED (operator, 2026-07-25: "B")

Single static **Go** binary **inside `mcp-local-hub`** as `cmd/vcpkg-mcp/`, registered by an ordinary
manifest — no node/python process tail (the hub's raison d'être).

Why B over a standalone repo:
- `internal/cmakegraph` (already built + measured: 85.6% resolution on the real overlay tree) lives in
  the hub's `internal/`. Go forbids importing another module's `internal/`, so a standalone repo would
  force moving or duplicating it *before* the tool has proven itself in use.
- The hub already owns everything a new repo would have to rebuild: CI, release pipeline, npm
  distribution, the manifest system, the review gate.
- A static Go server with no process tail is exactly the kind of server the hub exists to manage.

**Binding obligation that keeps B reversible:** `cmd/vcpkg-mcp/` must stay **strictly self-contained** —
its own package tree, no dependency on hub internals other than `internal/cmakegraph`, and no hub code
may import it. Extraction to its own repo later must then be a directory move, not a rewrite.

## Effort (honest)

Scaffolding + the evidence/tri-state/discovery contract + tests: 1-2 days.
`port_resolution` 0.5-1 · `pin_status` 1 · `patches_apply` 1-1.5 · `cmake_trace` 1-1.5 (input is
already structured JSON) · **`last_failure` is where estimates die**: MVP against the operator's real
stack 2-3 days, robust across build systems 1-2 weeks.
**MVP covering all five ≈ one focused week.** The single risk is log-format archaeology; mitigated by
"structured enough, plus always return the paths" rather than "complete parser".

## Open

- ~~**Overlay paths for GROUNDING.**~~ **CLOSED 2026-07-26.** The tree is
  `C:\vcpkg\vcpkg-builds\overlays\{ports,ports_upd,ports_mkl,triplets}` — inside the `C:\vcpkg`
  checkout after all, in a `vcpkg-builds\` subdirectory the earlier depth-4 sweep never reached.
  Buildtrees are redirected to the `R:\b\<triplet>\` ramdisk and install roots to
  `Q:\vcpkg-libs\<toolchain>\`. The histogram was measured against it: 201 edges / 131 Resolved
  (85.6% of the 153 `${CMAKE_CURRENT_LIST_DIR}` includes) / 14 Dangling / 56 Unresolved — and that
  figure SURVIVED the cross-family review that fixed six P1 wrong-file-class defects, for a reason
  verified rather than assumed (all 8 newly-reclassified `deferred_macro_context` edges also carried
  an independently-unresolvable `${VCPKG_TARGET_TRIPLET}`, so nothing moved off Resolved). Full
  measured inventory: `work-items/decisions/2026-07-25-vcpkg-ground-truth-measured.md`.
- Repo location for this MCP (new repo vs a module) — operator's call.

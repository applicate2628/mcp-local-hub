---
title: vcpkg MCP — MEASURED ground truth from the operator's real trees
status: reference
date: 2026-07-25
owner: lead
source: read-only scout pass (codex Terra @xhigh) + my own probes; every fact carries an absolute path
relates-to:
  - work-items/decisions/2026-07-25-vcpkg-mcp-tool-contracts.md
---

> Preserved here because the raw scout report lived on the **R: ramdisk** (volatile by the operator's
> own statement). These are the facts the tool contracts must be built against — not assumptions.

## Roots (discovery must handle MULTIPLE — this is why "several candidates ⇒ report all" is contract)

- `C:\vcpkg` — the vcpkg checkout (upstream `ports/`; its own `buildtrees/` is EMPTY).
- `C:\vcpkg\vcpkg-builds\overlays\{ports,ports_upd,ports_mkl,triplets}` — the overlay set.
- `Q:\vcpkg-libs\{cl,gpl,icx,llvm,main,mingw,win}\<triplet>\` — per-TOOLCHAIN install roots.
- `R:\b\<triplet>\<port>\` — buildtrees, redirected to a ramdisk via `--x-buildtrees-root`.

## 1. Overlay inventory + shadowing — SIMPLER than assumed

| Overlay | Port dirs |
|---|---:|
| `overlays\ports` | 21 |
| `overlays\ports_upd` | 35 |
| `overlays\ports_mkl` | 3 |

**Overlay-to-overlay shadowing: NONE.** No port name appears in more than one overlay directory.
Shadowing is exclusively overlay-vs-builtin `C:\vcpkg\ports`:
- `ports_upd`: **all 35** names also exist builtin (it is literally "updated builtin ports").
- `ports`: 2 (`botan`, `licensepp`). `ports_mkl`: 3 (`blas`, `intel-mkl`, `lapack`).

⇒ `vcpkg_port_resolution` only has to adjudicate overlay-vs-builtin. The precedence ORDER among
overlays still matters as contract (a future collision), but today it decides nothing.

## 2. Pin / version shapes — 11 ports are NOT git-comparable

Source acquisition (counts are port dirs; categories overlap):

| Shape | Ports |
|---|---:|
| `vcpkg_from_github(REPO … REF <commit> SHA512 …)` | 39 |
| `vcpkg_download_distfile(URLS … SHA512 …)` | 13 |
| `vcpkg_from_git(URL … REF …)` | 5 |
| `vcpkg_from_gitlab(...)` | 3 |
| provider/metapackage, no fetch | 2 |
| `vcpkg_from_sourceforge` | 0 |

`vcpkg_from_git` variants seen: literal commit REF only; +`FETCH_REF`; +`FETCH_REF`+`HEAD_REF`;
and a **variable** REF resolving to a commit (`ports_upd\libiconv`).

Manifest version fields: `"version-string"` 41 · `"version"` 12 · `"version-date"` 6 ·
`"version-semver"` 0 · `"port-version"` 4 (combined with version / version-date / version-string).

**ARCHIVE-ONLY primary sources — no git URL/REF, therefore NOT comparable by `git ls-remote`** (11):
`ports\ani2d`, `ports\ani3d`, `ports\velopack-bin`, `ports_mkl\intel-mkl`, `ports_upd\{cuda, cudnn,
freexl, gmp, libpq, libspatialite, sqlite3}`.
⇒ `vcpkg_pin_status` MUST return `unknown(reason: not_git_comparable)` for these, never "current".
(`ports_upd\libiconv` and `ports_upd\opencv4` use direct downloads IN ADDITION to a git main source.)

## 3. Patch inventory — sequencing and drift are both real

**201 physical `.patch` files across 39 overlay port dirs** (`ports_mkl` has none).
Heaviest: `opencv4` 22 · `parmmg` 16 · `python3` 14 · `magma` 13 · `libpq` 12 · `icu`/`osg` 11 ·
`nvtt`/`vtk` 10 · `libmesh`/`netgen` 9 · `gmp` 7 · `libspatialite` 6 · `ceres` 5.
69 filenames contain `fix-`; none contain `tmp` or an ISO date.

Two facts that shape `vcpkg_patches_apply`:
- **Order is explicit and conditional.** `ports_upd\python3` builds `PATCHES` as a LIST with
  conditional appends (static-non-MinGW; MinGW; Windows-non-MinGW → then either `0016-fix-win-cross`
  or `0017-fix-win`) and calls `PATCHES ${PATCHES}`. So the applied SET depends on triplet/toolchain —
  the tool must resolve the list for a GIVEN triplet, not read the directory listing.
- **Physical ≠ referenced.** `ports\parmmg` has 16 `.patch` files but the portfile references 14;
  `fix-mingw-skip-git-log-header.patch` and `fix-mingw-git-log-header-generation.patch` are
  unreferenced. ⇒ report referenced-vs-orphaned separately; an orphan is a finding, not an error.
- Non-`.patch` extensions occur: `ports_upd\freexl` applies `android-builtin-iconv.diff`.

## 4. Failure-log anatomy (`R:\b\wingpl\`, 618 log files)

Filename grammar (complete distinct set):
```
stdout-<triplet>.log
extract-{out,err}.log
config-<triplet>-{out,err}.log
config-<triplet>-{rel,dbg}-{ninja,CMakeConfigureLog.yaml,CMakeCache.txt}.log
install-<triplet>-{rel,dbg}-{out,err}.log
patch-<triplet>-<N>-{out,err}.log          # N = 0-based patch ordinal
```
Phases: `patch`, `extract`, `config`, `install` (+ `stdout` = wrapper stream). The `rel|dbg` segment is
absent for `extract-*`, `config-<triplet>-{out,err}`, `stdout-*`.

Diagnostic formats ACTUALLY present (verbatim):
- MSVC: `src.cxx(1): fatal error C1083: Cannot open include file: 'pthread.h': …`
- ninja failed target: `FAILED: [code=2] CMakeFiles/cmTC_e5bae.dir/src.cxx.obj`
- ninja summary: `ninja: build stopped: subcommand failed.`
- CMake probe: `-- Performing Test CMAKE_HAVE_LIBC_PTHREAD - Failed`

**NOT found in 618 logs:** GCC/Clang `path:line:col: error:`, `CMake Error at …`, autotools/libtool.
(One toolchain tree only — treat MSVC+ninja as proven-present, the rest as best-effort.)

**Nesting caveat:** those MSVC/ninja lines sit inside a CMake `try_compile` capability probe
(`FindThreads.cmake`). A probe failing is NORMAL — never report a try_compile diagnostic as the port's
failure cause without labelling it a probe.

### FALSE-POSITIVE TRAPS (the reason a naive parser is wrong)
1. **A non-empty `*-err.log` does NOT mean failure.** Of 184 `-err.log` files only 5 are non-empty, and
   all 5 contain *successful* patch output (`Applied patch CMakeLists.txt cleanly.`).
2. **`FAILED:` can be an interrupt.** `boost-thread`: `FAILED: [code=1]` → `User interrupt` →
   `ninja: build stopped: interrupted by user.` ⇒ classify `interrupted` ≠ `failed`.
3. Comment text: `# A missing CMake input file is not an error.`
4. Echoed flags: `--no-tests=error` inside a printed `ctest` command.
5. Cache variables: `CMAKE_ERROR_ON_ABSOLUTE_INSTALL_DESTINATION:UNINITIALIZED=ON`,
   `CMAKE_ERROR_DEPRECATED:INTERNAL=OFF`.
⇒ Match on diagnostic SHAPE at line position, never a substring scan for "error"/"failed".

## 5. Wrapper artifact (`build_failed.log`) — the operator's own layer, NOT vcpkg's

Stable across all 11 files, exact field order, no missing/extra fields:
```
[YYYY-MM-DD HH:MM:SS] triplet=<triplet>
command: <full vcpkg invocation>
exit_code: <int>
build_failed_count: <int>
failed_ports:
- <port>:<triplet>
```
`exit_code` = 1 in all 11; `build_failed_count` ranged 1..8. The `command:` line carries the
authoritative overlay chain (repeated `--overlay-ports`, in precedence order), `--x-buildtrees-root`,
`--triplet`, `--x-install-root`.
**It is OPTIONAL enrichment** — stock vcpkg does not produce it; the tool must give a full answer from
native artifacts alone and degrade (never fail) when it is absent or malformed.

## 6. THE structural finding (actionable without any tooling)

Their command carries `--clean-buildtrees-after-build`, which **deletes the per-config logs**.
Verified: `R:\b\cl` does not exist at all while `R:\b\wingpl` survives with 618 logs.
⇒ the 3-4 rounds of digging per failure are spent hunting evidence their own flag already erased.
Remedy: drop that flag for diagnostic runs (or clean only successful ports).
`vcpkg_last_failure` must return `unknown(reason: buildtrees_cleaned)` + this remedy — never a
fabricated or confidently-empty answer.

## 7. VERIFIED operator-environment finding — 17 dead patch files across 4 ports

Re-checked directly (2026-07-26), not taken from the scout report: for each port, listed the
physical `.patch`/`.diff` files and grepped `portfile.cmake` for each filename, THEN inspected the
`PATCHES` block shape to confirm the grep is authoritative (a portfile building filenames
dynamically would defeat a literal grep).

| Port | Physical | Dead (never referenced) | `PATCHES` shape |
|---|---:|---:|---|
| `ports_upd\netgen` | 10 | **10 — ALL of them** | no `PATCHES` keyword at all; zero `.patch`/`.diff` references anywhere in the portfile |
| `ports_upd\magma` | 13 | 4 | flat literal list (lines 20-29) ⇒ grep authoritative |
| `ports\parmmg` | 16 | 2 | flat literal list (lines 7-22) ⇒ grep authoritative |
| `ports_upd\icu` | 11 | 1 | flat literal list (lines 7-27) ⇒ grep authoritative |

Dead files: netgen — `142.diff`, `add_filesystem`, `cgns-scoped-enum`, `cmake-adjustments`,
`cross-build`, `downstream-fixes`, `git-ver`, `occ-78`, `static-exports`, `vcpkg-fix-cgns-link`;
magma — `cuda-13-clockrate-compat`, `fix-cmake4`, `no-tests`, `windows-mingw-thread-guards`;
parmmg — `fix-mingw-git-log-header-generation`, `fix-mingw-skip-git-log-header`;
icu — `remove-MD-from-configure` (note the portfile discusses CRT handling in a comment at line 28,
so this one may have been superseded by in-portfile logic rather than simply forgotten).

**netgen therefore builds with none of its fixes applied.** This is the same class that cost the
operator their gmp patch, one order of magnitude larger.

### The false positive that shaped the `patches_apply` contract

The scout also reported `ports\licensepp` as having 3 HARD MISSING patches. **Disproved:** the
portfile sets `get_filename_component(_licensepp_builtin_port_dir "${_licensepp_vcpkg_root}/ports/${PORT}" ABSOLUTE)`
and references `${_licensepp_builtin_port_dir}/add-stdint.diff` etc. — all three exist in
`C:\vcpkg\ports\licensepp`. The overlay deliberately reuses the BUILTIN port's patches.
⇒ `vcpkg_patches_apply` MUST **resolve** each patch path (it may point outside the port directory
through a variable) before judging it present. A port-dir-relative assumption reports a healthy
port as a hard defect.

## 8. MEASURED: `pin_status` can never say "behind" from `git ls-remote` alone

Live network probe (2026-07-26, read-only `git ls-remote` only, 61 round-trips, all exit 0).

**The hard contract limit.** `git ls-remote --symref <remote> HEAD <branch> refs/tags/<ref>` proves only
what a named ref currently points at. When the pin EQUALS that commit the port is provably `current`.
When it does NOT, ls-remote cannot establish any of:

- that the pinned commit still exists or is reachable upstream,
- that it is an ANCESTOR of the tip (behind) rather than diverged, rebased away, or unrelated,
- how many commits behind it is.

Querying `refs/tags/<40-hex-commit>` is a tag-NAME lookup; empty output does not prove the commit is
absent. ⇒ **the verdict enum is `current` | `unknown(reason)` — there is NO `behind`.** Producing a
"behind" verdict at all would require a fetch (or a forge-specific compare API), which is a separate,
explicitly opt-in capability, not the default read-only path.

**Coverage, honest denominator (59 ports total = 21 + 35 + 3):**

| Class | Count | Verdict |
|---|---:|---|
| git remote, pin == advertised tip | 40 | `current` (trustworthy) |
| git remote, pin != tip | 6 | `unknown` — `libmesh`, `sleipnir`, `hpx`, `ngspice`, `python3`, `skia` |
| no git remote at all | 13 | `unknown(not_git_comparable)` — the 11 archive-only + `blas`/`lapack` meta-packages |

So a whole-tree answer is 40 trustworthy / 19 unknown. The 6 non-tip ports are exactly the ports the
operator most wants a verdict on, and they are precisely the ones ls-remote cannot adjudicate.

**Cost:** 61 round-trips in 60.8 s wall-clock; median ~0.7 s per remote, worst `skia` at 11.7 s. A
single-port query is interactive; a whole-overlay scan is NOT — results must be cached, and the tool
must not present a cached verdict as live without saying so.

**Failure modes NOT observed** (so they stay hypothetical, not designed-around blindly): auth
required, rate limiting, moved/deleted remote, GitLab self-hosted failure, unresolvable `${VARIABLE}`
pin (both `libiconv` and `vtk` resolved statically). All three GitLab remotes responded.

## CORRECTION 2026-07-26 — the `parmmg` physical count was wrong

§3 and §7 above state `parmmg` has **16** physical patch files. Re-measured directly: it has **9**
(`ls | grep -E '\.(patch|diff)$'`), with **7** declared in the portfile ⇒ **2 orphaned**. The 16
figure came from the first scout pass and does not reproduce; an independent second pass reached 9
as well.

**The conclusion is unaffected.** The dead-patch total of 17 stands — netgen 10, magma 4, parmmg 2,
icu 1 — and every one of those four orphan counts was confirmed by a second independent pass. Only
the supporting "physical" figure for parmmg was wrong, not the finding it supported.

Recorded rather than silently edited, because a measured number that turned out unreproducible is
exactly the kind of thing that should leave a trace.

## Honest gaps (not closed by this pass)
- No network checks were run (no `git ls-remote`) — matters for `pin_status`.
- `Q:\vcpkg-libs\mingw\` has NO wrapper file ⇒ that variant's format is unverified.
- Diagnostics from the cleaned (non-`wingpl`) buildtrees are simply unavailable.

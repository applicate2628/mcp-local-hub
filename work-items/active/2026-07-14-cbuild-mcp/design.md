# Design — `mcp-cbuild`: native Go C/C++ build MCP (CMake + vcpkg)

## Why (usefulness — verified 2026-07-14)

Agent-driven CMake configure/build/test with **structured diagnostics** (file:line:col:severity)
+ vcpkg dependency management is genuinely useful for native C/C++ dev (this repo owner's
stack: icx-cl / ucrt64 / gdb / lldb). The agent drives the build and gets parsed diagnostics
instead of raw compiler spew, then fixes file:line directly.

**No maintained option exists** (2 web searches, negative-conclusion satisfied):
- `hiono/mcp-cmake` — the only dedicated CMake-build MCP — is **DEPRECATED** (author moved to a
  Claude *skill*, `hiono/cmake-skill`, which is not an MCP server and cannot route through the hub).
- `mpsm/mcp-cpp` — maintained (Rust) but **clangd code-analysis**, not build; overlaps the owner's
  existing serena / mcp-language-server LSP.
- No vcpkg MCP server exists at all.

**Decision (user, 2026-07-14): build our own native Go stdio MCP, in the `mcp-local-hub` repo,
RICH scope; install in the hub; add a marketplace catalog row.** vcpkg folds into the SAME server
(complementary: vcpkg provides deps via the CMake toolchain file, CMake builds).

## Shape

- **Binary:** `cmd/mcp-cbuild/` (a SECOND binary in the mcp-local-hub Go module, alongside
  `cmd/mcphub`). Standalone — imports NO `internal/` mcphub packages; it is an ordinary external
  stdio MCP server the hub manages as a daemon (stdio, like a `uvx`/`npx` server; mcphub bridges
  stdio↔HTTP). Small, self-contained package under `cmd/mcp-cbuild/` (may use internal sub-packages
  under `cmd/mcp-cbuild/internal/…` if it grows).
- **Transport:** stdio JSON-RPC 2.0 (MCP). **stdout carries ONLY JSON-RPC frames; ALL logs → stderr**
  (a stray stdout write corrupts the protocol — hard rule).
- **MCP surface:** handle `initialize`, `notifications/initialized`, `ping`, `tools/list`,
  `tools/call`. Protocol version `2025-06-18` (advertise; accept the client's if supported).
- **Working dir:** per-tool `working_dir` param (absolute), defaulting to a launch `-w <dir>` flag
  then cwd. Both cmake and vcpkg operate relative to the project dir.

## Tools (rich)

### CMake
1. `cmake_list_presets {working_dir?}` → `{configurePresets[], buildPresets[], testPresets[],
   workflowPresets[]}`, each `{name, displayName, description, inherits?, resolvedGenerator?,
   resolvedToolchain?}`. Parses `CMakePresets.json` + `CMakeUserPresets.json` (JSON with `include`
   + `inherits` resolution; version-field aware). READ-ONLY.
2. `cmake_configure {preset, working_dir?, fresh?, defines?{}}` → runs `cmake --preset <p>
   [--fresh] [-D k=v …]` → `{success, exit_code, diagnostics[], cache_summary?}`.
3. `cmake_build {preset, working_dir?, targets?[], jobs?, verbose?, config?}` → `cmake --build
   --preset <p> [--target …] [-j N] [--config …]` → `{success, exit_code, diagnostics[],
   built_targets?, wall_ms}`.
4. `cmake_test {preset, working_dir?, regex?, jobs?, output_junit?}` → `ctest --preset <p>
   [-R …] [-j N] [--output-junit <path>]` → `{success, total, passed, failed, tests[]{name,
   status, wall_ms, output_tail?}, junit_path?}`.
5. `cmake_workflow {preset, working_dir?}` → `cmake --workflow --preset <p>` (configure+build+test)
   → combined structured result.
6. `cmake_clean {working_dir?, preset?, purge_build_dir?}` → `cmake --build --target clean` OR
   remove the build dir (guarded: refuse to remove a dir outside the resolved build tree).

### vcpkg
7. `vcpkg_install {working_dir?, clean_after?, triplet?}` → manifest-mode `vcpkg install` in the
   dir holding `vcpkg.json` → `{success, exit_code, installed[], diagnostics[]}`. Requires
   `VCPKG_ROOT` (env) or a discovered vcpkg; fail-closed with a clear message if absent.
8. `vcpkg_list {working_dir?}` → `vcpkg list` → installed packages `[{name, version, triplet}]`.
9. `vcpkg_manifest {working_dir?}` → parse + summarize `vcpkg.json` (dependencies, features,
   builtin-baseline, overrides). READ-ONLY.
10. `vcpkg_search {query}` → `vcpkg search <q>` → available packages (optional; behind the same
    VCPKG_ROOT gate). READ-ONLY.

## Structured diagnostics — the high-value core

A single `parseDiagnostics(rawCombinedOutput) []Diagnostic` where `Diagnostic{File, Line, Col,
Severity(error|warning|note), Code?, Message}`. Multi-format, order-preserving:
- **MSVC / icx-cl (MSVC driver):** `path(line[,col]): error C####: message` / `warning C####:`.
- **GCC / Clang / icx-cl (clang driver):** `path:line:col: error: message` / `warning:` / `note:`.
- **CMake:** `CMake Error at path:line (cmd):` / `CMake Warning …` (multi-line body captured).
- **linker:** MSVC `LNK####` + GNU ld `undefined reference` (best-effort).
Unrecognized lines are dropped from `diagnostics[]` but the full raw stdout+stderr tail is ALWAYS
returned under `raw_tail` (bounded, e.g. last 8 KB) so nothing is silently lost. Parser is the
single owner — unit-tested against captured real fixtures per compiler.

## Toolchain awareness (their env)

- `cmake_list_presets` surfaces, per configurePreset, the resolved compiler/toolchain when derivable
  (`CMAKE_CXX_COMPILER` in cacheVariables, or the toolchainFile), so the agent sees icx-cl vs cl vs
  gcc without a configure run.
- No hardcoded toolchain paths. icx-cl / ucrt64 are handled purely via the operator's presets +
  the multi-format diagnostics parser (icx-cl emits either driver's format depending on invocation).

## Safety / hygiene

- Every exec has a **timeout** (configurable per call, default 10 min for build, 5 min configure,
  30 min test; hard cap) + is cancellable via the MCP request context.
- `cmake_clean --purge_build_dir` refuses any path that is not strictly inside the preset's resolved
  `binaryDir` (path-escape guard) — never `RemoveAll` an arbitrary dir.
- Command argv is built from a fixed allowlist of flags + validated params (no shell; `exec.Command`
  with explicit args). `defines` k/v are passed as `-D k=v` args, not concatenated into a shell string.
- vcpkg/cmake binaries resolved from PATH or explicit env (`CMAKE_BIN`, `VCPKG_ROOT`); absence →
  a structured fail-closed error, never a silent no-op.
- Untrusted project files (CMakePresets.json, vcpkg.json) are PARSED, never executed by us; we only
  invoke the real cmake/vcpkg which the operator already trusts to run their project.

## Distribution

- Build: `go build ./cmd/mcp-cbuild` → `mcp-cbuild(.exe)`.
- **Hub install:** a mcphub manifest (`manifest create`) whose daemon runs the built `mcp-cbuild`
  binary as a stdio server (`command: mcp-cbuild`, `args: ["-w", "${workspaceFolder}"]` or per-call
  working_dir). Routed through the hub like any daemon.
- **Marketplace:** an S1 (local-stdio) catalog row in `marketplace/v2/catalog.json` — a mcphub-native
  offering. (Distinct from the deprecated `hiono/mcp-cmake` — we do NOT catalog that.)

## Test strategy

- Unit: `parseDiagnostics` against captured MSVC/GCC/Clang/CMake fixtures (table-driven); preset
  parser against sample CMakePresets.json (include + inherits). No real toolchain needed.
- Integration (Windows CI + local): a tiny fixture project (`testdata/hello/` with CMakeLists.txt +
  CMakePresets.json + one failing + one passing target) → configure/build/test → assert structured
  results. vcpkg integration gated on VCPKG_ROOT presence (skip when absent).
- MCP protocol: a stdio round-trip test (initialize → tools/list → tools/call) driving the server
  over a pipe.

## Out of scope (v1)

- Meson/Bazel/Make (CMake + vcpkg only).
- Remote/HTTP transport (stdio only; the hub provides HTTP).
- Editing project files (read presets/manifest, drive tools; never write CMakeLists.txt/vcpkg.json).
- Binary caching / registry config for vcpkg beyond what the operator's env already sets.

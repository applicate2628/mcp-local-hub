# status — mcp-cbuild (C/C++ build MCP)

Template: full-delivery (greenfield). Orchestrator: main conversation ($lead).
Started: 2026-07-14.

## State
DESIGN ACCEPTED (self-authored, 2026-07-14) → IMPLEMENTING.

Native Go stdio MCP server (`cmd/mcp-cbuild/`) wrapping CMake (presets/configure/build/test/
workflow/clean) + vcpkg (install/list/manifest/search) with a single multi-format structured
diagnostics parser. Decision driver: no maintained CMake/vcpkg build MCP exists (hiono/mcp-cmake
DEPRECATED→skill; mpsm/mcp-cpp is clangd code-analysis; no vcpkg MCP). User pre-authorized:
build own Go, in mcp-local-hub repo, rich scope, install in hub + marketplace catalog.

## Plan (phases)
1. **Implement** — `cmd/mcp-cbuild/` stdio MCP scaffolding + 10 tools + `parseDiagnostics` +
   preset/manifest parsers + unit tests + a fixture integration test. Branch `feat/cbuild-mcp`.
2. **Commission gate** — Sol + Terra (codex) + fable-5 (arbiter/hidden-bug) + security-reviewer
   (exec-injection / path-escape / stdout-discipline) + arch + qa. Then Codex bot on the PR.
3. **Hub install** — mcphub manifest for the built binary (stdio daemon), route + smoke.
4. **Marketplace** — S1 local-stdio catalog row in `marketplace/v2/catalog.json`; bot-reviewed PR.

## Acceptance (v1)
- stdout carries ONLY JSON-RPC (logs→stderr); MCP initialize/tools/list/tools/call round-trip test green.
- `parseDiagnostics` unit-tested against MSVC / GCC / Clang / CMake fixtures; `raw_tail` always returned.
- exec timeouts + context-cancel on every tool; `cmake_clean --purge` path-escape guarded; no shell.
- vcpkg tools fail-closed (clear error) when VCPKG_ROOT/vcpkg absent.
- fixture project (testdata/hello) configure/build/test → structured results.
- Windows-first; builds + parses cross-platform.

## Next action
Dispatch backend-engineer to implement Phase 1 on `feat/cbuild-mcp`.

## Notes
Runs in PARALLEL with E de-adopt (Phase 2 #540 in bot review). Disjoint files (`cmd/mcp-cbuild/`
vs `internal/api`+`internal/clients`).

## DEFERRED 2026-07-15 + design correction (user)

**Guiding principle (user, 2026-07-15): "an MCP for cmake USES cmake for everything —
reimplement NOTHING."** The v1 `presets.go` hand-rolled CMakePresets.json interpretation
(hidden/conditions/includes/inherits/macros/duplicates/validation) was the WRONG approach
end-to-end — it triggered 6 Codex-bot rounds chasing cmake-presets(7) spec compliance
(a losing reimplementation race). Correct redesign when resumed:
- `cmake_list_presets` → shell out to `cmake --list-presets=<kind>` (cmake validates
  hidden/conditions/includes/inherits/duplicates — it is the authority; a rejected file =
  cmake nonzero = our structured error).
- **binaryDir → read from `CMakeCache.txt` (`CMAKE_BINARY_DIR`, cmake-computed at configure)**,
  NOT a hand-rolled macro resolver. Purge removes the cmake-configured dir; refuse if unconfigured.
- configure/build/test/workflow/clean already delegate via `cmake --preset` — keep.
- Delete ALL the reimplemented condition/macro/inherit/include/duplicate/hidden logic.
- Keep only the SAFETY guards that are NOT cmake's job (symlink-escape / non-dir / path
  containment on the purge target).

Status: PR #541 PARKED at d521aad3 (8 open bot findings that the delegation redesign obviates).
The cbuild binary + v1 tools work; the preset-listing accuracy is the only open item.

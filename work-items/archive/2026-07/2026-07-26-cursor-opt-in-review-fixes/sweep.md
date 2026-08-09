# Phase C: Cursor opt-in exhaustive sweep — revision 9

## 1. Scope and verdict

This is the final read-only resweep after external Quality Assurance found and the implementation lane corrected `internal/clients/project_scope.go:20` from the stale count-bound phrase “all 46 adapters” to the count-free phrase “every supported adapter.” A fresh whole-repository Repomix pack covered 2,117 current Markdown, Go, TypeScript/TSX, and JavaScript paths; its explicit file-tag inventory contains 1,412 Go files. The generated asset, product fixtures/goldens, help, tests, current task memory, and historical records were checked explicitly.

No product file, command behavior, build, test, generator, frontend command, or `mcphub` command was changed or run by this gate. Revision 9 changes only this sweep artifact and the mandatory session report.

The canonical inventory is 47 supported clients, 2 default-install clients, and 45 opt-in clients; `mimocode` occurs exactly once. Cursor is supported but opt-in through explicit plural `--clients`. Setup points to a separate install command, current architecture §9 is shipped/current, and no stale or unclassified live derivative remains.

## 2. Fresh coverage evidence

| Check | Fresh result | Classification |
| --- | --- | --- |
| Whole-repository file universe | 2,117 in-scope files packed; 1,412 explicit Go file tags | The pack included every current Go, Markdown, TypeScript/TSX path in the working tree rather than reusing the revision-8 inventory. |
|  | `internal/gui/assets/app.js` read separately | Repomix excludes the generated asset by ignore policy, so it was directly inspected rather than inferred from the pack. |
| Canonical registry arithmetic | 47 descriptor rows; exactly 2 `defaultInstall: true`; therefore 45 opt-in | `internal/clients/clients.go` names only `claude-code` and `codex-cli` as defaults. |
| Registry membership | exactly one `mimocode`; ordered between `opencode` and `hermes` | The required supported client is present once and only once. |
| Corrected Go surface | `internal/clients/project_scope.go:20` says “every supported adapter” | Freshly re-read in the complete Go inventory; neither stale count nor replacement contradiction remains live. |
| Supported-client document | `docs/supported-clients.md:4-16` says 47 / 2 / 45 | It uses the valid plural form `mcphub install --server X --clients <name>`. |
| Cursor opt-in command surface | 22 fresh plural-selector matches across the full pack | Live examples and tests use `--clients cursor`, `--clients <name>`, or another explicit plural selection. |
| Setup help | `internal/cli/setup.go:318-327` names the two built-in defaults | It then gives a separate `mcphub install --server <name> --clients cursor` command; no `setup ... --clients` form was found. |
| Invalid singular install probe | one non-task-memory occurrence | The sole product-tree occurrence is historical plan code at `docs/superpowers/plans/2026-05-12-g4-unified-hub-mcp.md:3145`; no live source/help path recommends `mcphub install ... --client`. |
| Three-name co-enumeration | 62 fresh direct-order matches for `claude-code … codex-cli … cursor` | Live matches are two-default-plus-opt-in explanations, supported-client lists, explicit selections, compatibility matrices, legacy boundaries, or bounded tests. |
| Product fixtures/goldens | 6 named product fixture/golden files; zero policy matches | No product fixture or golden asserts a stale client count, default set, or client-selection flag. |
| Generated asset | one current default/opt-in phrase; zero stale count/default/flag phrases | `internal/gui/assets/app.js` says the defaults are `claude-code` and `codex-cli`, with Cursor and other clients opt-in. |
| Authoritative architecture | §9 is shipped/current; the largest-open block is only §0–§8 + §10 | The current specification has no live statement that §9 is future, pending, or part of the largest-open block. |

## 3. Corrected and previously reviewed live surfaces

| Surface | Fresh disposition |
| --- | --- |
| `internal/clients/project_scope.go:20` | Revision-9 correction confirmed: count-free “every supported adapter.” |
| `internal/gui/frontend/src/screens/Servers.tsx:56` | Current 47-backend-client wording retained. |
| `internal/gui/frontend/src/lib/matrix-columns.test.ts:153` | Current 47-client-universe wording retained. |
| `internal/gui/frontend/src/lib/routing.test.ts:663,762,822,825` | Current 47-client wording retained on every previously identified path. |
| `internal/api/install_hub_reconcile.go:459` | Uses valid plural `--clients <name>`. |
| `docs/superpowers/specs/2026-06-10-clean-architecture-redesign.md:273,275` | §9 remains shipped/current and excluded from the largest-open block. |

All previously identified live blockers plus the external Quality Assurance finding were re-read on the current product revision and are closed.

## 4. Generation and derivative evidence

The canonical verification artifact at `verification.md:9-10` was directly re-read:

| Recorded command/check | Preserved result | Revision-9 corroboration |
| --- | --- | --- |
| `go generate ./internal/gui/...` | PASS, exit 0 | The record includes successful `tsc --noEmit`, Vite 8.0.16 production build, 269 transformed modules, and output sizes. This sweep did not rerun generation. |
| `git diff --name-only -- internal/gui/assets` | PASS, exit 0, no output | The preserved gate proves no generated-asset diff after generation. |
| Generated asset content | Current two-default-plus-opt-in wording | Direct revision-9 read found one current phrase and zero stale 46-adapter, 46-backend, three-default, singular-install, or setup-client-selection phrases. |
| Current complete product revision | Orchestrating session recomputed the raw `origin/fix/cursor-not-default-install`-to-worktree product diff after the local functional commit: SHA-1 `8568f0bbf84adabb3c21266819e78c49552ae9d6`, 48,364 bytes across 15 files | Corroborating handoff only; the load-bearing generator and no-asset-diff evidence remains the canonical verification artifact above. |

## 5. Classified nonblocking residual matches

| Match class | Residual surface | Classification |
| --- | --- | --- |
| Closed historical default snapshot | `docs/superpowers/plans/phase-3b-ii-backlog.md:267-273` | Preserved provenance. It declares Phase 3B-II closed and the expansion already shipped before recording the then-current three-client default set. |
| Historical singular install example | `docs/superpowers/plans/2026-05-12-g4-unified-hub-mcp.md:3145` | Historical plan code, not compiled help or live source. |
| Test-local pre-fix count history | `internal/gui/frontend/src/screens/Catalog.test.tsx:1081-1085` | Explicit “pre-fix” regression provenance followed by current capability-derived behavior. |
| Dated 46-adapter decisions | `work-items/decisions/2026-06-24-per-project-gui-design.md`; `work-items/decisions/2026-06-25-per-project-gui-p3-design.md`; archived 2026-06-17 finding | Durable historical decisions from the former 46-client state, not current product inventory or policy. |
| Other numeric 44/46 matches | source-line ranges, request identifiers, elapsed seconds, dependency versions, section/review numbers, and old reports | None is a current client/adapter count. |
| Correct negative assertions | registry/default-install tests, adoption tests, and Cursor opt-in tests | They exclude Cursor from automatic/default selection or prove explicit reachability; they do not make Cursor a default. |
| Intentional client enumerations | release docs, adapter matrices, supported-name help, manifest examples, legacy-boundary tests, and explicit selections | Supported sets, compatibility scopes, explicit selections, or bounded fixture/test sets; none changes the two-client default set. |
| Task-memory search literals | current `work-items/`, `.plans/`, and `.reports/` | Search recipes, findings, and verification history, not live product/help/policy ownership. |
| Unclassified live match | none | Every targeted residual is classified above. |

## 6. Terms and Abbreviations

- Quality Assurance (QA): independent verification of the implemented revision.
- TypeScript/TSX: TypeScript source, including files containing XML-like user-interface syntax.
- PASS: the reviewed acceptance surface is satisfied with no open live blocker.

## 7. Handoff

The frozen revision satisfies the Phase C acceptance surface: 47 supported / 2 default-install / 45 opt-in; `mimocode` present once; Cursor explicit opt-in; valid plural `--clients`; setup pointing to a separate install command; current §9; generator evidence preserved; no generated-asset diff; and no stale or unclassified live derivative.

Phase C may proceed.

Gate: PASS

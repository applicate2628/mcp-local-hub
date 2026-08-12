# Factual research memo

## Files & symbols

- `[runtime-verified]` The connected MCP servers publish exactly six tools: HFSS exposes `hfss_solve`, `hfss_export_mesh`, and `hfss_export_sparams`; CST exposes `cst_solve`, `cst_export_mesh`, and `cst_export_results`. `cst_sample_saved_field` is absent from the current MCP catalogue.
- `[static-read]` Source registration exposes the same inventory: `<repo>/servers/electromagnetics-mcp/src/mcphub_em_mcp/hfss.py:249`, `<repo>/servers/electromagnetics-mcp/src/mcphub_em_mcp/cst.py:259`, `<repo>/servers/electromagnetics-mcp/tests/test_servers.py:8`.
- `[runtime-verified]` Both servers currently return empty prompt and resource lists.
- `[static-read]` The catalogue manifests launch through `uvx` at immutable pin `7d5e41cf...`: `<repo>/servers/hfss/manifest.yaml:5`, `<repo>/servers/cst/manifest.yaml:5`.
- `[runtime-verified]` Audited HEAD was `2658ac85a0e1ee88b01f920af94c2664201e7a1c`; the server subtree was clean.

## Contracts

Current gap count:

| Slice | Complete | Partial | Missing or open |
|---|---:|---:|---:|
| Eight requested capabilities: P0 + P1 + P2 | 0 | 1 | 7 |
| P0 Line10 acceptance | 0 | 0 | 1, fail-closed |
| Six disposable contract-smokes for existing tools | 0 E2E-confirmed | 6 with static/unit evidence | 6 E2E open |
| Total requirement rows | 0 fully closed | 7 partial | 8 missing/open |

### Requirements matrix

| Requirement | Current state | Evidence |
|---|---|---|
| P0 `cst_sample_saved_field` | **MISSING** | `[runtime-verified]` Absent from the live tool catalogue. |
|  |  | `[runtime-verified]` `git grep` found none of `cst_sample_saved_field`, `GetFieldVector`, `Result3D`, `ResultTree`, `allow_solve`, or `zero_ambiguous` at HEAD or the published pin. Required contract: `<vfem-repo>/docs/tooling/mcp-hfss-cst-requirements.md:44`. |
| P0 input/output/provenance contract | **MISSING** | `[static-read]` No models exist for `project_bundle`, E/H, point arrays, frame metadata, expected hashes, or the ordered six-component result. Requirements: `<vfem-repo>/docs/tooling/mcp-hfss-cst-requirements.md:51`, `:95`. |
| P0 disposable-copy/Result3D/ResultTree/process ownership | **MISSING** | `[static-read]` Existing `_copy_cst_bundle` and process ownership belong only to solve, which then invokes `run_solver`: `<repo>/servers/electromagnetics-mcp/src/mcphub_em_mcp/cst.py:153`, `:192`. |
| P0 Line10 acceptance | **NOT PASS; fail-closed** | `[static-read]` The authoritative document records an incompatible zero mask/amplitude and forbids treating `ok=-1` as an oracle: `<vfem-repo>/docs/tooling/mcp-hfss-cst-requirements.md:36`. |
|  |  | `[static-read]` The server repository has no 96-local/90-unique point output, independent native-export comparison, before/after source hashes, or sampler-owned process oracle. Acceptance starts at `<vfem-repo>/docs/tooling/mcp-hfss-cst-requirements.md:143`. |
| P1 `hfss_port_info` | **MISSING** | `[runtime-verified]` No separate tool exists. `[static-read]` `hfss_export_sparams` returns generalized/renormalized Touchstone and reference impedance only: `<repo>/servers/electromagnetics-mcp/src/mcphub_em_mcp/hfss.py:341`. |
| P1 HFSS saved-field/point-field | **MISSING** | `[runtime-verified]` No field tool exists. `[static-read]` HFSS registers only solve, S-parameters, and mesh tools. |
| P1 CST post-mesh port evidence | **PARTIAL** | `[static-read]` `cst_export_mesh` exports discovered `PortN.slim` files as surface Gmsh: `<repo>/servers/electromagnetics-mcp/src/mcphub_em_mcp/cst.py:391`. |
|  |  | `[static-read]` There is no explicit aperture, orientation, boundary, or actual adaptive-pass identity; only the retained final mesh is available: `<repo>/servers/electromagnetics-mcp/src/mcphub_em_mcp/cst.py:378`. |
| P1 CST bulk port-field export | **MISSING** | `[static-read]` `cst_export_results` reads 1D result-tree series, not E/H vectors: `<repo>/servers/electromagnetics-mcp/src/mcphub_em_mcp/cst_results.py:9`. |
| P2 `hfss_build_project` | **MISSING** | `[runtime-verified]` Absent from the tool catalogue and named symbols at HEAD. |
| P2 `cst_build_project` | **MISSING** | `[runtime-verified]` Absent from the tool catalogue and named symbols at HEAD. |
| P2 `cst_archive_project` | **MISSING** | `[runtime-verified]` Absent from the tool catalogue and named symbols at HEAD. |
| `hfss_solve` contract-smoke | **STATIC IMPLEMENTED; E2E UNVERIFIED** | `[static-read]` Preflight/start/status/result/cancel, BasisOrder, delta-S, DoMaterialLambda, timeout, and licence settings exist: `<repo>/servers/electromagnetics-mcp/src/mcphub_em_mcp/hfss.py:250`. |
|  |  | `[static-read]` Unit tests cover schema/preflight and generic job cancel/timeout: `<repo>/servers/electromagnetics-mcp/tests/test_feedback_contract.py:22`, `<repo>/servers/electromagnetics-mcp/tests/test_jobs.py:40`. |
| `cst_solve` contract-smoke | **STATIC IMPLEMENTED; E2E UNVERIFIED** | `[static-read]` Explicit frequency grids, PropagationConstantAccuracy, actions, and progress stages exist: `<repo>/servers/electromagnetics-mcp/src/mcphub_em_mcp/cst.py:259`. |
|  |  | `[static-read]` The latest QA record explicitly says no real solve/job/export was launched: `<repo>/work-items/active/2026-08-10-cst-frequency-default-contract/qa.md:23`. |
| `hfss_export_mesh` contract-smoke | **PARTIAL; STATIC MISMATCH** | `[static-read]` Latest-pass mesh, mesh hash, artifact hashes, and background filter exist: `<repo>/servers/electromagnetics-mcp/src/mcphub_em_mcp/hfss.py:407`. |
|  |  | `[static-read]` Arbitrary pass selection is rejected fail-closed: `<repo>/servers/electromagnetics-mcp/src/mcphub_em_mcp/hfss.py:411`. |
|  |  | `[static-read]` The Gmsh writer emits coordinates with `.16g`, not required `.17g`, and does not declare a coordinate unit: `<repo>/servers/electromagnetics-mcp/src/mcphub_em_mcp/hfss_mesh_vendor/extract_hfss_solver_mesh.py:856`. |
| `cst_export_mesh` contract-smoke | **STATIC IMPLEMENTED; E2E UNVERIFIED** | `[static-read]` Final SLIM validation, materials, `m/mm`, `.17g`, SHA-256, and mesh hash are present: `<repo>/servers/electromagnetics-mcp/src/mcphub_em_mcp/cst.py:377`, `<repo>/servers/electromagnetics-mcp/src/mcphub_em_mcp/slim.py:141`. |
| `hfss_export_sparams` contract-smoke | **PARTIAL; E2E UNVERIFIED** | `[static-read]` Generalized and renormalized exports exist; the AEDT API receives precision argument `17`: `<repo>/servers/electromagnetics-mcp/src/mcphub_em_mcp/aedt_batch.py:88`. |
|  |  | `[static-read]` The tool does not export gamma or a port-info dataset: `<repo>/servers/electromagnetics-mcp/src/mcphub_em_mcp/hfss.py:341`. |
| `cst_export_results` contract-smoke | **STATIC IMPLEMENTED; E2E UNVERIFIED** | `[static-read]` S, reference impedance, gamma, effective permittivity, line/wave impedance, convergence, and port-mode progression are emitted with `.17g`: `<repo>/servers/electromagnetics-mcp/src/mcphub_em_mcp/cst_results.py:9`, `:58`. |

## Flows

- `[static-read]` Solve flow is validation -> job-local copy -> one-worker queue -> vendor solve -> manifest -> terminal result. Job state/progress/cancel/result ownership is in `<repo>/servers/electromagnetics-mcp/src/mcphub_em_mcp/jobs.py:89`.
- `[static-read]` Exporters accept only a successful retained `job_id`: `<repo>/servers/electromagnetics-mcp/src/mcphub_em_mcp/jobs.py:220`.
- `[static-read]` The required P0 flow starts from a retained `project_bundle`, must survive server restart, and cannot rely on in-memory `JobManager` state. No such flow exists.
- `[ASSUMPTION (UNVERIFIED)]` Vendor dynamic edges (`cst.interface.Project.open`, `cst.results.ProjectFile`, AEDT scripting) were not executed in this audit. Resolving probe: a disposable target-environment smoke preserving protocol output and process/hash oracles.

## Tests & coverage

- `[static-read]` Existing unit tests cover exact six-tool inventory and confirmation (`<repo>/servers/electromagnetics-mcp/tests/test_servers.py:8`), real stdio initialization/tool listing (`<repo>/servers/electromagnetics-mcp/tests/test_stdio.py:10`), closed solve schemas and action-dependent CST fields (`<repo>/servers/electromagnetics-mcp/tests/test_feedback_contract.py:22`), generic success/cancel/timeout state transitions (`<repo>/servers/electromagnetics-mcp/tests/test_jobs.py:19`), JSON precision/relative artifacts/CST mesh hash (`<repo>/servers/electromagnetics-mcp/tests/test_provenance.py:10`), and CST port-mode progression (`<repo>/servers/electromagnetics-mcp/tests/test_cst_contract.py:126`).
- `[static-read]` There are no tests for the P0 sampler, metadata frame resolution, E/H ordering, unit conversion, input hashes before/after, zero ambiguity, restart replay, Line10 96/90 selection, independent native export, or sampler process ownership.
- `[ASSUMPTION (UNVERIFIED)]` Archived closure prose reports previous vendor smokes, but the work-item contains no raw reproducible acceptance evidence; this does not prove the newly required disposable contract-smoke. The summary is at `<repo>/work-items/archive/2026-08/2026-08-10-hfss-cst-mcp-servers/closure.md:16`.
- `[runtime-verified]` Tests were not executed in this read-only lane; repository files were not changed during research.

## Similar implementations

- `[static-read]` CST solve already contains bundle copying (`<repo>/servers/electromagnetics-mcp/src/mcphub_em_mcp/cst.py:153`), owned Design Environment detection (`:162`), and SHA-256/relative artifact records (`<repo>/servers/electromagnetics-mcp/src/mcphub_em_mcp/provenance.py:12`).
- `[static-read]` No existing implementation opens a standalone saved `.sct`, registers a copied field through `ResultTree`, or invokes a point sampler.
- `[static-read]` CST 1D result export is the nearest read-only result-tree precedent, but it does not cover the 3D saved-field contract: `<repo>/servers/electromagnetics-mcp/src/mcphub_em_mcp/cst_results.py:42`.

## Constraints

- `[static-read]` P0 must be read-only, enforce `allow_solve=false`, remain restart-independent, and fail closed: `<vfem-repo>/docs/tooling/mcp-hfss-cst-requirements.md:79`.
- `[static-read]` The source bundle must remain unchanged; registrations and temporary `.rex` files may exist only in a disposable copy: `<vfem-repo>/docs/tooling/mcp-hfss-cst-requirements.md:119`.
- `[static-read]` Independent export is mandatory; `ASCIIExport.SetPointFile` is not yet an accepted oracle: `<vfem-repo>/docs/tooling/mcp-hfss-cst-requirements.md:156`, `:166`.
- `[static-read]` Package runtime is restricted to Python `>=3.11,<3.14`: `<repo>/servers/electromagnetics-mcp/pyproject.toml:5`.

## Change risks

- `[runtime-verified]` P0 history is empty: `git log -S cst_sample_saved_field`, `-S GetFieldVector`, and `-G Result3D|ResultTree` found no previous implementation or revert.
- `[runtime-verified]` Focal history is `e56b87da` for the original six tools, `6d6517ec` for strict schemas/preflight, `7d5e41cf` for the explicit CST frequency grid, and `12db7429`, `1d3121dd`, `abd8ed6e`, `d08ab398` for successive output-root safety changes.
- `[static-read]` Published manifests still pin `7d5e41cf`; later `d08ab398` changes safety, README, and tests. Core HFSS/CST/job/provenance/result code is byte-equivalent between the pin and audited HEAD, but the safety surface is not.
- `[runtime-verified]` The live Codex declaration exposes a typed schema for `hfss_solve`, while `cst_solve` appears as `args: unknown`.
- `[ASSUMPTION (UNVERIFIED)]` The cause of runtime CST schema degradation is unknown. Resolving probe: preserve raw `tools/list` from the CST endpoint and compare it with the hub/Codex adapter projection.

## Unresolved questions

- `[ASSUMPTION (UNVERIFIED)]` It is not established that installed CST 2026 can return correct nonzero E/H vectors for both Line10 materials. Resolving probe: the exact acceptance sequence, including independent native export.
- `[ASSUMPTION (UNVERIFIED)]` The domain meaning of previously observed zero values is unknown; they remain `zero_ambiguous`.
- `[ASSUMPTION (UNVERIFIED)]` It is not proven that live processes execute the immutable package object named by the manifest pin. Tool inventories match, but the endpoint does not publish binary/source identity.
- `[ASSUMPTION (UNVERIFIED)]` Real output/error contracts for all six existing tools remain unconfirmed by a new disposable run.

## Research admission gates

| Gate | Result | Evidence |
|---|---|---|
| Regression risk | **PASS for admission** | `[static-read]` Requirements isolate P0 in a new read-only tool and prohibit reimplementing existing solve/export paths: `<vfem-repo>/docs/tooling/mcp-hfss-cst-requirements.md:12`. |
| Metric alignment | **PASS** | `[static-read]` Acceptance directly measures E/H values, identity, hashes, 96/90 coordinates, and independent native comparison: `<vfem-repo>/docs/tooling/mcp-hfss-cst-requirements.md:143`. |
| Known limits | **PASS** | `[static-read]` The document records version-local `ok=-1`, zero ambiguity, invalid whole-port inference, and pending ASCIIExport contract: `<vfem-repo>/docs/tooling/mcp-hfss-cst-requirements.md:131`, `:166`. |
| Bounded falsification | **PASS** | `[static-read]` Line10 acceptance defines a finite deterministic sample and mechanical fail conditions: `<vfem-repo>/docs/tooling/mcp-hfss-cst-requirements.md:147`, `:170`. |

## Adjacent findings

- `[static-read]` HFSS Gmsh output violates the shared `.17g` precision surface: its writer uses `.16g`. This is a confirmed static contract mismatch, not merely missing smoke evidence.
- `[static-read]` The catalogue pin does not include later output-root confinement changes.
- `[runtime-verified]` The current Codex client does not receive typed arguments for `cst_solve`.
- These findings were not written to the bug registry because the research lane prohibited every file mutation except this accepted memo.

### Searched and excluded

- Checked current HEAD, the published pin, runtime tool catalogue, prompts/resources, exact registrations, package manifests, job/provenance owners, result exporters, tests, and focal Git history.
- After runtime inventory and a second independent `git grep`/history check, the conclusion that named P0/P1/P2 surfaces are absent did not change; widening stopped under the saturation rule.
- Did not launch HFSS/CST, solve/export/cancel, retained bundles, `.sct`, Line10 data, or fleet operations.
- Did not read or execute external raw VFEM `.scratch` artifacts; their conclusions were used only as recorded in the authoritative requirements document.
- No install, restart, commit, push, source change, or test execution occurred.

**Gate decision: PASS** — the matrix is complete enough for Lead: the P0 sampler is absent, Line10 acceptance has not passed, capability gap is seven missing plus one partial, and all seven mandatory E2E verification gates remain open.

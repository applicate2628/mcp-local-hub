# MVP design

## Public contract

Each server publishes exactly three domain tools. `*_solve(action=...)` owns the complete asynchronous job lifecycle; the two exporters consume a successful `job_id`. Thus the same-mesh workflow is exactly solve, export mesh, export results without a second solver launch.

## Owners

- `jobs.py`: bounded one-worker-per-solver execution, cancellation, timeout, license-wait diagnostics, progress stage, and terminal result retention.
- `provenance.py`: `%.17g` JSON/CSV output, SHA-256 artifacts, machine-neutral relative names, canonical mesh hash, UTC, settings, and determinism.
- `hfss.py` plus `aedt_batch.py`: official AEDT batch/script process boundary over a job-local project copy, setup overrides, solve, convergence/Touchstone export, typed legacy-kernel and license failures, and validated latest-pass volume-mesh export.
- `cst.py`: CST external-Python API, ordered VBA history generated inside the server from typed settings, solve, result-tree export, and integrated verified SLIM conversion when CST exposes only that mesh artifact.

## Safety and lifecycle

Jobs copy inputs into an isolated output job directory and never modify the caller's project. One worker per solver serializes license-consuming runs. Cancellation calls only the job-owned application/solver stop boundary. Timeout and license exhaustion are distinct terminal causes. Every launched vendor application is closed by its owning job on success, error, cancellation, and timeout.

## Honest capability boundary

The installed AEDT 2025 R1 exposes one validated latest-pass solver-volume cache but no pass-indexed volume-cache inventory. `adaptive_pass=-1` is supported; a specific pass fails with `hfss_adaptive_pass_cache_unavailable` instead of substituting the latest mesh. Legacy ACIS projects requiring AEDT 2023 R1 through 2024 R2 conversion fail with `hfss_project_kernel_migration_required` after an official batch-upgrade attempt on the job-local copy. CST 2026 exposes no official tetrahedral export API, so the server embeds the verified fail-closed SLIM 1.4 decoder and records that source format and its validation.

## Acceptance oracles

- Tool inventory is exactly six.
- Job state transitions and cancel/timeout are deterministic under injected runners.
- JSON and Gmsh writers use full precision and contain no absolute paths.
- Export artifact hashes and canonical mesh hash recompute exactly.
- External Python imports installed `cst.interface` and `cst.results`; AEDT 2025 R1 batch-save, batch-solve failure classification, and `RunScriptAndExit` create/open smokes are verified.
- Existing hub remains untouched until the feature commit is ready to integrate.

## Terms and Abbreviations

- AEDT: Ansys Electronics Desktop.
- CST: CST Studio Suite.
- HFSS: High Frequency Structure Simulator.
- MCP: Model Context Protocol.
- MVP: Minimum Viable Product.

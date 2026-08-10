# Delivery brief

## Objective

Ship operator-owned HFSS/AEDT and CST Studio Suite MCP servers and make them installable from mcphub.

## Accepted contract

- Six public MVP tools: `hfss_solve`, `hfss_export_mesh`, `hfss_export_sparams`, `cst_solve`, `cst_export_mesh`, and `cst_export_results`.
- Each solve tool owns `start`, `status`, `result`, and `cancel` actions, bounded timeouts, explicit license-wait policy, truthful progress stages, and one job identity reused by its exporters.
- Every artifact has SHA-256 provenance, full-precision numeric serialization, a canonical mesh hash where a mesh exists, relative artifact names, and an explicit determinism statement.
- Python 3.11-3.13 adapters use PyAEDT and CST's installed external-Python API. No agent-side VBA/IronPython templating, embedded-Python subprocess, log-scraping control path, machine-local runtime path, vendor binary, or proprietary module is committed.
- Solver launch/mutation requires positive `confirm=true`; exporters are read-only over a completed job.
- Windows is the solver target; unsupported platforms fail before launch.

## Deliverables

- An operator-owned package with two stdio entrypoints, common job/provenance owners, integrated export logic, tests, license, and usage documentation.
- Exact-SHA marketplace entries and install probes in mcphub.
- Focused protocol, safety, precision/provenance, packaging, catalog, installed-API, and live hub verification.

## Deferred after MVP

Port-field exports, declarative project builders, restartable CST archives, and HFSS/CST per-adaptive-pass meshes remain second-queue work exactly as stated in the delivered requirements.

## Integration owner

The root Lead session owns repository integration, publication, merge, and post-merge hub recovery.

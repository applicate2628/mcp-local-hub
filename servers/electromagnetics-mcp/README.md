# mcphub Electromagnetics MCP

Two operator-owned Model Context Protocol (MCP) servers for noninteractive local electromagnetic simulation:

- `mcphub-hfss-mcp` uses the installed Ansys Electronics Desktop batch/script interface with a private, non-graphical process.
- `mcphub-cst-mcp` uses the installed CST Studio Suite external-Python API with a private hidden Design Environment.

They expose exactly six MVP tools:

| Server | Tools |
|---|---|
| HFSS | `hfss_solve`, `hfss_export_mesh`, `hfss_export_sparams` |
| CST | `cst_solve`, `cst_export_mesh`, `cst_export_results` |

## Job model

`hfss_solve` and `cst_solve` take an `action`:

- `start`: validate an existing project and output root, require `confirm=true`, copy the project into an opaque job directory, and return immediately with a job ID.
- `status`: return the truthful stage and progress if the vendor API provides one. Indeterminate solver progress is `null`, never fabricated.
- `result`: return a terminal result or error.
- `cancel`: invoke only the stop/close boundary of the job-owned solver process.

Each solver has one worker. Concurrent starts queue instead of racing for the same license. `timeout_s`, `license_wait_timeout_s`, and `license_poll_s` are explicit. A busy license reports `*_license_unavailable`; it never waits forever.

## Artifact contract

- Inputs are never modified. Each job works on its own project copy.
- Job copies are confined to the local, per-user hub state directory
  (`%LOCALAPPDATA%\\mcp-local-hub\\electromagnetics-jobs` on Windows or the
  equivalent XDG state directory). Operators may choose another trusted local
  directory with `MCPHUB_EM_OUTPUT_ROOT`; requested output roots must be inside
  it, and Windows network/UNC destinations are rejected.
- Manifests contain the solver version, input and artifact SHA-256 hashes, complete settings, UTC timestamps, and a determinism declaration.
- Manifest artifact paths are relative to the job directory. Absolute workstation paths are excluded from generated exports.
- JSON, CSV, Gmsh, and HFSS network export requests use `%.17g` numeric precision.
- Mesh artifacts include a SHA-256 `mesh_hash` over canonical ordered coordinates and material-tagged tetrahedra.
- CST results include raw generalized S data, a 50-ohm renormalized two-port projection, port data, and convergence series from the result tree.

## Mesh capability boundary

CST 2026 has no official volume-tetrahedron export API. The server embeds the previously verified, fail-closed CST SLIM 1.4 reader and emits documented Gmsh 2.2 ASCII for the volume and port surfaces. Provenance names the source format; unsupported layouts fail instead of being guessed.

HFSS volume meshes are exported by an embedded, fail-closed reader for the validated AEDT solver cache and written as Gmsh 2.2 ASCII. The current AEDT installation exposes one validated latest-pass volume cache, so `adaptive_pass=-1` is supported. A specific non-negative pass returns `hfss_adaptive_pass_cache_unavailable`; it never silently substitutes the latest mesh. The mesh manifest records validation results and a canonical `mesh_hash`.

AEDT 2025 R1 cannot directly open legacy projects whose geometry is still stored with the ACIS kernel. The server first runs the official batch-upgrade operation on the job-local copy and returns `hfss_project_kernel_migration_required` when an intermediate AEDT 2023 R1 through 2024 R2 conversion is required. The input project is never modified.

`cst_solve` requires `frequency_range_ghz=[low, high]`; every sample interval must lie inside that global range. The exported result bundle includes the documented port-mode refinement progression from `1D Results\\Convergence\\Portmodes\\Progression` when CST produced it.

## Installation and development

Use Python 3.11, 3.12, or 3.13. CST 2026's installed API does not support Python 3.14.

```powershell
uv sync --extra test --python 3.13
uv run pytest
uv run mcphub-hfss-mcp
uv run mcphub-cst-mcp
```

The package contains no vendor binaries, proprietary Python modules, projects, credentials, or license material. It embeds only the independently maintained mesh-decoding source needed by the server. Set `CST_INSTALL_ROOT` or `MCPHUB_ANSYSEDT_EXE` only when normal installation discovery is insufficient.

## Determinism

HFSS and CST adaptive meshers expose no server-controlled deterministic seed. CST is additionally sensitive to the order of mesh-setting commands. The server fixes its command order and records it, but declares the mesh nondeterministic. Same-mesh comparisons must bind the exported `mesh_hash`, not only the settings.

This project is not affiliated with or endorsed by Ansys or Dassault Systèmes.

## Terms and Abbreviations

- AEDT: Ansys Electronics Desktop.
- CST: CST Studio Suite.
- Gmsh: A documented text mesh format used for portable exports.
- HFSS: High Frequency Structure Simulator.
- MCP: Model Context Protocol.
- MVP: Minimum Viable Product.
- SLIM: CST's binary mesh container used as the verified fallback source.
- stdio: Standard input and standard output transport.

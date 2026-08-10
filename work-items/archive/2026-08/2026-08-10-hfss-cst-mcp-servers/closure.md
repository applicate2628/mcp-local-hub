# Closure — HFSS and CST MCP servers

Closed: 2026-08-10T09:17:11Z
Outcome: DELIVERED — PR #596 was merged, the canonical hub was upgraded, and the new HFSS and CST MCP servers are running and connected.
Evidence: PR #596 merged as commit `2d9301affa29c893f2b8f963c1ea5779b4a90c02`; the installed hub reports that commit; HFSS and CST report Running on ports 9139 and 9140; direct and isolated protocol, vendor-runtime, package, catalogue, and publication gates passed.
Residual risk: HFSS exports the latest adaptive mesh pass; selecting an arbitrary historical pass remains fail-closed because AEDT does not expose a stable documented cache interface for it. Legacy ACIS projects may require a one-time vendor conversion before automated project construction.

## Outcome

The repository now owns one Windows Model Context Protocol package with six domain tools: three for Ansys HFSS/AEDT and three for CST Studio Suite. Long operations use cancellable start/status/result jobs, artifacts carry SHA-256 provenance and machine-neutral paths, numeric output preserves `%.17g`, and mesh exports include a deterministic mesh hash.

The package source was published first as commit `e56b87dad0395fff90365e40259eb9dc84802d2f`. Catalogue manifests then pinned that immutable commit, and PR #596 merged the package and one-click hub entries into `master` as commit `2d9301affa29c893f2b8f963c1ea5779b4a90c02`.

The deployed canonical hub runs that merge commit. The pre-existing daemon fleet was restored after upgrade; HFSS is Running on port 9139 and CST is Running on port 9140. Both endpoints completed real Model Context Protocol initialization and tool discovery, and both clients report them connected.

## Verification

- 24 Python tests passed; Ruff lint and formatting passed.
- Direct stdio and isolated `uvx --from .` handshakes passed for both servers.
- AEDT 2025.1 batch/script project creation and CST 2026 external-API solve/export smoke tests passed.
- Two independent HFSS mesh exports produced identical mesh and validation hashes.
- Go embedded-manifest and catalogue tests passed.
- Staged and commit-range publication-safety scans passed.
- After deployment, both loopback ports listened and real MCP initialize/tools-list lifecycles succeeded.

## Archive location

`work-items/archive/2026-08/2026-08-10-hfss-cst-mcp-servers/`

## Terms and Abbreviations

- AEDT: Ansys Electronics Desktop.
- CST: Computer Simulation Technology Studio Suite.
- HFSS: High Frequency Structure Simulator.
- MCP: Model Context Protocol.
- PR: pull request.
- SHA-256: Secure Hash Algorithm with a 256-bit digest.
Lifecycle-schema: work-items-physical-v1

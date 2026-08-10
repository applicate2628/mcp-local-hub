# Closure — HFSS/CST MCP feedback correction R1

Closed: 2026-08-10T10:45:07Z
Outcome: DELIVERED — the reproduced transport/session/schema defects are fixed, published, deployed, and verified on the live HFSS/CST endpoints.
Evidence: product commit `6d6517eca4e1f5219d20838c4dee318eee2ea375`; published tip `2a311dd402e861c8c9468e7b4793dd68ce3b7fb6`; installed binary commit `2a311dd4`; live HFSS/CST ports 9139/9140 passed the complete safe contract matrix.
Residual risk: no real HFSS or CST solve was launched in this corrective cycle; vendor-runtime numerical acceptance remains a separate explicit-approval activity.

## Outcome

The shared stdio HTTP adapter now issues bounded unique per-initialize sessions, binds them to negotiated protocol versions, rejects invalid or mismatched states before subprocess dispatch, and invalidates a deleted session. HFSS and CST publish closed constrained tool schemas and expose a no-launch `preflight` action that validates before confirmation.

The evaluation's resource-bridge claim was refuted against current source and live behavior: missing native and synthetic resources preserve the causal JSON-RPC error. A regression test now locks that behavior without changing the bridge.

The exact rollout binary replaced the canonical hub atomically. The GUI was restored, all 37 registered entries returned with zero non-maintenance failures, and both live endpoints passed initialize, unique-session, protocol rejection, session deletion, closed-schema, extra-field, bounds, and preflight checks without starting a solver.

## Verification

- Full `internal/daemon` normal and race suites passed.
- Full `internal/api` and daemon-related `internal/cli` tests passed.
- All 34 electromagnetics Python tests, Ruff, and Go vet passed.
- Isolated and live HFSS/CST safe runtime matrices passed.
- Staged and exact commit-range publication-safety scans passed.
- Installed and rollout binary SHA-256 values matched exactly.

## Archive location

`work-items/archive/2026-08/2026-08-10-hfss-cst-mcp-feedback-r1/`

## Terms and Abbreviations

- MCP: Model Context Protocol.
- HFSS: High Frequency Structure Simulator.
- CST: Computer Simulation Technology Studio Suite.
Lifecycle-schema: work-items-physical-v1

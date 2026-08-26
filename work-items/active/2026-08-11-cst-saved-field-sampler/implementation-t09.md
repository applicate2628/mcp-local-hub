# T09 implementation — closed path identity and authorized transfer

Gate: PASS

## Scope and invariant

T09 establishes one closed `WindowsPathIdentityV1` proof and one transactional transfer owner. Caller paths and caller manifests grant no authority: transfer requires the policy-derived complete manifest plus held source/destination capabilities, copies from the held source handles, validates the destination, and commits only on exact equality. Every failure rolls back the owned workspace.

`AuthorizedWorkspaceSnapshot` keeps the workspace path private and is the sole factory of a one-use, non-copyable vendor path lease. Workspace settlement refuses deletion while that lease lacks a complete terminal receipt. There is no ordinary path, existence, reopen, hashing, copying, or cleanup API on the snapshot.

## Change surface

| Owner | Change |
|---|---|
| `servers/electromagnetics-mcp/src/em_backend/cst_saved_field_policy.py` | Canonical path proof type is `WindowsPathIdentityV1`; all policy references use the single current name. |
| `servers/electromagnetics-mcp/src/em_backend/cst_saved_field_transfer.py` | `ManifestV2` validates exact canonical rows and aggregate; transfer builds the workspace transactionally; snapshot owns the one-use vendor lease and settlement gate. |
| `servers/electromagnetics-mcp/tests/test_cst_saved_field_policy.py` | Updated the internal proof-name contract. |
| `servers/electromagnetics-mcp/tests/test_cst_saved_field_path_identity_windows.py` | Updated Windows identity expectations. |
| `servers/electromagnetics-mcp/tests/test_cst_saved_field_t09_safety_transfer.py` | Added T09 RED/GREEN ownership and failure-path coverage. |

## Contract effect

This is an internal Python contract change, not a JSON or Model Context Protocol wire-shape change. `ObjectIdentityEvidence` is replaced by `WindowsPathIdentityV1`; malformed `ManifestV2` objects now fail at construction; transfer accepts an optional trusted vendor-lease factory; and snapshots expose `create_vendor_path_lease()` instead of ordinary workspace path/file helpers. The transfer receiver consumes the exact complete manifest, while the snapshot/lease boundary owns the vendor settlement receipt.

## Evidence

| Check | Receipt |
|---|---|
| CodeGraph pre-edit | Exact symbol queries resolved the current policy, held-capability, transfer, snapshot and vendor-lease owners and their callers; no stale-index banner appeared. |
| Strict RED | `uv run pytest tests/test_cst_saved_field_t09_safety_transfer.py -q --tb=short` — 3 failed: missing closed identity type, permissive manifest construction, and missing snapshot-only lease factory. |
| T09 GREEN | Same command — 3 passed. |
| T09 affected matrix | Policy, Windows path identity, transfer and T09 tests — 348 passed. |
| Prior-phase regression | Focused T03–T09 matrix — 444 passed; only the already-recorded Pydantic lifespan warning remained. |
| Static | Ruff passed; five scoped files were already formatted; scoped `git diff --check` passed. |
| CodeGraph post-edit | Exact current-symbol query was issued for `WindowsPathIdentityV1`, `ManifestV2`, `AuthorizedWorkspaceSnapshot`, `create_vendor_path_lease`, and transfer callers. Its response exceeded the result budget, so it is not used as correctness evidence; execution and static receipts above are authoritative. |

## All-return ownership

- Manifest construction rejects missing, extra, duplicate, non-canonical, partial, or aggregate-drift rows before copying.
- Copy reads the already-held source capability; it does not reopen a caller path.
- Transfer failure leaves no accepted snapshot and rolls back the owned workspace.
- Vendor lease creation is one-use and non-copyable.
- Workspace settlement is denied while the vendor lease is active or incompletely settled.

## Rollback

Revert the two T09 production changes and the three scoped test changes together. This restores the prior internal identity name, permissive manifest construction, and pre-T09 snapshot surface without touching live CST, Service Control Manager, hub, fleet, Git index, or deployment state.

## Terms and Abbreviations

- MCP: Model Context Protocol.
- SCM: Windows Service Control Manager.
- RED/GREEN: failing test before implementation, then passing test after the minimal implementation.
- PASS: the phase acceptance criteria are satisfied by fresh focused evidence.

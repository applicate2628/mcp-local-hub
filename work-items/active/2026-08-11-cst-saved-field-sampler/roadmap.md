# Roadmap — CST saved-field point sampler

Admission source: direct human handoff of `<vfem-repo>/docs/tooling/mcp-hfss-cst-requirements.md` on 2026-08-11.

## Goal

Ship and verify the P0 `cst_sample_saved_field` MCP capability defined by the authoritative requirements document: read complex E/H values at physical points from an already-solved retained CST project bundle, with no solve, remesh, source mutation, or result fitting.

## Priority

1. P0 sampler contract and fail-closed Line10 acceptance.
2. Verification of the six already-published solve/export tools on disposable projects.
3. P1/P2 capabilities remain explicitly deferred unless the user expands this item.

## Success signals

- The tool resolves field frames by metadata and returns ordered `ReX, ReY, ReZ, ImX, ImY, ImZ` values with units, phasor convention, hashes, and machine-neutral provenance.
- `allow_solve=false` is enforced; missing or ambiguous results fail without solve or fallback.
- Original `.cst`, `Result/3d.slim`, and selected `.sct` bytes are unchanged before/after.
- Line10 acceptance produces the required deterministic 96 local / 90 unique point set and independent native-export comparison, or fails closed without a physics claim.
- Only sampler-owned CST resources are cleaned up; pre-existing CST/MCP processes are untouched.
- The sampler is disabled by default and accepts only operator-approved bundle roots and identities at the server boundary, including direct-daemon invocation.
- Every sampling call runs in one isolated Windows helper/Job Object with a 60-second bound; timeout settlement is accepted only after target evidence proves the exact owned process tree is gone and foreign CST sessions are unchanged.

## Operator decision — 2026-08-12

The operator explicitly approved the default-off allowlist plus isolated helper/Job Object design expansion after the first candidate failed the independent security gate.

## Scope boundary

P1 `hfss_port_info`, saved-field HFSS access, CST bulk field/port evidence, and P2 project builders/archive remain outside this P0 delivery.

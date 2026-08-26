# Bug: Saved-field core acceptance oracles omit three required falsifiers

- id: 2026-08-11-cst-saved-field-core-qa-oracle-gaps
- context: 2026-08-11-cst-saved-field-sampler
- status: open
- severity: high
- area: servers/electromagnetics-mcp/tests/test_cst_saved_field_*.py
- found-by: qa-engineer

Exact candidate `14a9b6b4cb9fc1e7248bd3b782b9e00d499181df` passes all 42 sampler tests and all 85 package tests, but three accepted Phase 0–5 criteria do not have non-degenerate regression oracles.

## Reproduction and expected-versus-actual

Run the source/test coverage probe recorded at `/.scratch/work-items/2026-08-11-cst-saved-field-sampler/qa-core/23-qa-gap-proof.txt`.

1. Selected-field copy identity:
   - Expected: `P1-AC5` and `P2-AC3` independently corrupt the copied selected field and assert `cst_saved_field.source_changed` at stage `copy_field`, with no success.
   - Actual: production contains the `copy_field` rejection branch at `servers/electromagnetics-mcp/src/mcphub_em_mcp/cst_saved_field.py:720`, but the sampler tests contain zero `copy_field`/corrupted-field-copy falsifiers. The existing parameter table covers only project and mesh copies at `servers/electromagnetics-mcp/tests/test_cst_saved_field_contract.py:252`.
2. Foreign identity at each acquisition boundary:
   - Expected: `P3-AC3` interleaves a foreign identity at every handle/identity/liveness/token/before-transfer boundary and proves it is untouched.
   - Actual: the foreign set is asserted only in `test_saved_field_owned_session_identity` at `servers/electromagnetics-mcp/tests/test_cst_saved_field_vendor.py:98`; the five-boundary parameterized test at line 116 contains no foreign identity or preservation assertion.
3. Cleanup during the engineered blocked window:
   - Expected: `P5-AC3` asserts zero cleanup and zero settlement events while the vendor call remains deterministically blocked, before releasing it.
   - Actual: `test_saved_field_nonpreemptible_cancellation` establishes the blocked window and asserts only that the future is not done at `servers/electromagnetics-mcp/tests/test_cst_saved_field_integration.py:310`; it does not expose or assert the trace/events until after release.

## Required resolution

Create a new candidate with persistent RED-before-GREEN tests for all three falsifiers, preserving the exact candidate-C production behavior unless a falsifier exposes a product defect. Re-run the full Phase 6 QA gate against that new immutable candidate; do not amend the evidence for candidate C.

## Terms and Abbreviations

- AC — acceptance criterion.
- CST — CST Studio Suite.
- QA — quality assurance.

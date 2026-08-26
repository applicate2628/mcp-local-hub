# Bug: CST W7 candidate fails full regression and composition closure gates

- id: 2026-08-13-cst-w7-candidate-regression-gate-fails
- context: 2026-08-11-cst-saved-field-sampler
- status: open
- severity: high
- area: CST saved-field W0-W7 candidate and repository regression gates
- found-by: qa-engineer

Fresh W7 verification of the exact dirty working candidate returned `REVISE`.

Reproduction:

- `go test ./... -count=1 -json`: exit 1 after 349.542 seconds; 11,902 test passes, 9 test failures, 114 skips; 41 package passes, 3 package failures, 11 package skips. The failures are two routing capture-before-release guards and seven Windows CLI upgrade/review/staging guards.
- `uv run --frozen --python 3.13 pytest -q`: exit 1 after 16.697 seconds; two failures. The topology guard detects duplicate endpoint ownership in `cst_saved_field_policy.py` and the transport owners. The W0 RED contract remains red because `WorkerCapabilityReceiptV1` is absent.
- `uv run --frozen --python 3.13 ruff check .`: exit 1; one unsorted import block in `tests/test_cst_saved_field_w0_gap_baseline.py`.
- `uv run --frozen --python 3.13 ruff format --check .`: exit 1; the same W0 RED file would be reformatted.
- CodeGraph and the Go test inventory found no real native frontend child plus Go capability/local-pipe end-to-end test satisfying W06-AC01. The real native five-handle worker test passes but is not a frontend/Go substitute.

Expected: every W7 command and W06-AC01 evidence finishes fresh and green, every prior RED contract has a GREEN resolution, and only one endpoint owner exists.

Actual: full Go, full Python, Ruff and format are red; one W0 contract and the W6 frontend integration proof remain open. No candidate commit or staged index was created.

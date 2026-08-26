# Bug: CST saved-field settlement receipt can bypass quarantine

- id: 2026-08-12-cst-saved-field-settlement-receipt-bypasses-quarantine
- context: 2026-08-11-cst-saved-field-sampler
- status: open
- severity: high
- area: helper settlement, parent validation, and containment quarantine
- found-by: security-engineer
- candidate: c7654794c79ee4d8eed5378117e2ce84f7608040
- fix-class: implementation

## Reproduction

1. `HelperResponseV1.from_wire` verifies settlement field types, and
   `validate_response` verifies only response identity and settlement presence; it
   does not reject `workspace_settled=false`, `session_settled=false`, or
   `owned_remaining>0` (`cst_saved_field_helper_protocol.py:147-216,333-348`).
2. `_contained_saved_field_invoker` publishes the helper-provided failure identifier
   without checking settlement truth and then the runner releases the admission
   lease normally (`cst.py:584-600`; `cst_saved_field_containment_windows.py:271-294`).
3. The helper failure path hardcodes `session_settled=true` and
   `owned_remaining=0` independently of the actual application settlement
   (`cst_saved_field_helper.py:363-383`).
4. The helper first creates a directory with `tempfile.mkdtemp`, then passes that
   already-existing directory to `AuthorizedBundleTransfer`, whose default lease
   constructor rejects an existing child before entering its cleanup `try`
   (`cst_saved_field_helper.py:235-238`;
   `cst_saved_field_transfer.py:200-215,306-318`). This leaves the workspace for the
   outer failure path while the receipt still asserts session settlement.
5. Current-session smoke evidence: a response with all settlement booleans false
   and `owned_remaining=7` was accepted by `validate_response`; the caller path then
   published its failure without containment quarantine.
6. Native handle settlement records removal from an in-memory list even though
   `CloseHandle` return values are never checked; setup/runtime exceptions raised by
   `checked` are not marked quarantine-worthy (`cst_saved_field_containment_windows.py:455-468,577-667,702-706`).

## Expected versus actual

- Expected: every response is accepted only after truthful complete application and
  process settlement; any unproved workspace/session/handle/thread/Job proof
  atomically quarantines before permit release.
- Actual: application receipts are trusted as shape-only data, helper failure
  receipts are synthesized, and native close/configuration failures may release the
  gate without proof.

## Security impact

An unsettled CST session, workspace, handle, reader, or Job can coexist with later
invocations. Resource ownership becomes ambiguous and restart-only containment is
silently bypassed. This falsifies SEC-C03, SEC-C04, SEC-C08, and SEC-C10.

## Required correction and falsifying probe

Make the helper derive its sole receipt from the actual application settlement and
make the parent enforce the exact success/failure receipt predicates. Treat every
missing/false proof and every native cleanup/configuration exception after ownership
begins as quarantine-worthy. Prove close success rather than list removal. Inject
each false receipt field, each workspace creation/transfer failure, each Win32 call
failure, and each close failure; assert quarantine precedes wake/re-entry and all
routes perform zero later work.

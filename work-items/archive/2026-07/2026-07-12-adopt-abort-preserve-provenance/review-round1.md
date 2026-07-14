# Commission round 1 — verdict: REVISE (Sol P1)

Date: 2026-07-12. Lanes: Sol (gpt-5.6-sol xhigh, mandatory acceptance + fable's adversarial lane), Terra (gpt-5.6-terra high), architecture-reviewer (opus), fable (FAILED — budget limit, no verdict).

## Sol — REVISE

### P1 (BLOCKER, data-loss still open) — compensation registered after the mutation boundary
`executeInstallToWithSymlinkConsents`: the restore closure is appended only AFTER
`AddEntryWithConfigWriter` returns success (install.go ~:2747). But `AddEntryWithConfigWriter`
→ `SecureWriteClientConfig` has TWO post-rename error paths that return an error with the live
config ALREADY MUTATED (CONFIRMED via secure_write_client_config.go:71-76 doc + existing tests
`TestSecureWritePostRenameVerifyFailureLeavesNoFile` / `...TransientReopenFailureKeepsPublishedFile`):
  1. definitive owner/mode/DACL verify failure → REMOVES the just-published file (config deleted);
  2. transient post-rename re-open failure → KEEPS the published file (config = hub relay), returns error.
In either, on the failing client: no restore closure registered → `failAfterRollback` can't restore it
→ its name can't enter `rollbackClientRestoreFailures` → if earlier clients restored, Install returns a
PLAIN error → adopt ABORTS → snapshot deleted while THIS client stays mutated = P1 data-loss.
The test seam (:75) fails the 2nd write before `realWrite`, so it never exercises write-then-error.
**Fix direction (Sol):** establish compensation BEFORE entering the mutation, OR change the mutation
API to report explicit commit/mutation state. Any ambiguous write error must trigger restoration of
the current client; a failed/unprovable restoration must produce the sentinel.

### P2 — recovery/GC tests don't prove complete artifact lifecycle
- snapshot check = dir-exists only, not every recorded `SnapshotRef` present+valid;
- vault check = `len(List())>0` only; an unrelated key hides deletion of the required routed keys;
- no "manifest absent + client still mutated ⇒ KEEP" GC test;
- after final GC, no assertion that preserved routed keys were DELETED → orphaned secret material risk.
Fix: assert every snapshot, exact `plan.SecretRoutedKeys`, exact event clients/count; seed an
unrelated vault key to prove selective cleanup; add both missing GC states.

### P2 — sentinel rendered error not names/counts-only
`InstallClientRollbackIncompleteError.Error()` appends the `%v` cause; adopt exposes it via `%w`.
A later backup/adapter/intermediate failure cause carrying `C:\Users\...` leaks into the user/event-
facing string despite the "names only" posture. (Structured event body IS clean — Terra+Sol agree.)
Fix: keep `Unwrap()` for causality; ensure the user-facing rendering applies the redaction contract.
[Orchestrator note: low-exposure — operator's OWN stderr on their OWN machine; the persisted event is
clean; GUI already redacts adopt errors (bug 2026-07-12-adopt-refusal-gui-redacted). Decide in the fix.]

### P3 — recovery wording asserts more than the sentinel proves
Adopt error says clients were "left pointing at the hub relay", but a restore can write original bytes
then post-write-error (sentinel = "could not prove"). Say "restoration could not be confirmed".

## architecture-reviewer — PASS (but MISSED the P1: assumed AddEntry atomic ⇒ fail-unmutated)
All 10 design claims CONFIRMED; single-owner invariant HOLDS. 3 P3: (P3-1) error forward-refs unbuilt
de-adopt → soften; (P3-2) flip bug file to closed on commit; (P3-3) multi-client aggregation test gap.
Its "AddEntry atomicity ⇒ safe" note is REFUTED by Sol's P1 (post-rename verify/reopen leaves mutated).

## Terra — PASS
concurrency (slice function-local, synchronous append/read), `%w` integrity, redaction (event clean) all PASS.

## Disposition
P1 is an install-transaction-boundary compensation-model change → architect designs. Batch with P2
(test hardening), P2 (redaction — decide), P3 (wording), arch P3-1/P3-2/P3-3. Then re-run Sol+Terra.

---
status: open
context: A3-a Secrets registry — concurrent vault edits limitation
phase: Phase 3B-II A3-a
---

# A3-a — Vault concurrent edits are last-write-wins

The age-encrypted vault has no version field analogous to manifests'
`expected_hash`. Two GUI tabs (or a GUI tab + CLI invocation) racing
on the same vault produce last-write-wins semantics. Ship A3-a with
this known limitation; future work can add an optimistic-concurrency
hash field analogous to manifest's `expected_hash`.

**Failure mode:**
- Tab A reads snapshot at T=0 (vault has `{KEY1:"oldA"}`).
- Tab B reads same snapshot at T=1.
- Tab A rotates `KEY1` to `"newA"` at T=2.
- Tab B rotates `KEY1` to `"newB"` at T=3, unaware of A's change.
- Vault now `{KEY1:"newB"}`. Tab A's `"newA"` is silently lost.

**Why we accept this for A3-a:** the failure is operator-recoverable
(the user can re-rotate). The cost of a proper fix (vault hash field,
bump on every Set, 409 on mismatch, CLI must thread the hash too) is
comparable to a small feature in itself; out of A3-a scope.

**Future fix candidates:**
- Add a `vault_version` integer header in the encrypted body; Set
  bumps it; PUT/DELETE requires `If-Match: <version>`; 409 on
  mismatch.
- CLI continues to no-op on the version field (always passes the
  fresh value), or grows the same `--if-version` flag.
- E2E test for two-tab conflict.

**Out of A3-a scope.**

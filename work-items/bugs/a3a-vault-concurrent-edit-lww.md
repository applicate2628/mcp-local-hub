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

---

## Resolved: process-local concurrency (PR #18 mutex extension)

`vaultMutex` in `internal/api/secrets.go` now serializes all vault-mutating
API wrappers — `SecretsInit`, `SecretsSet`, `SecretsDelete`, `SecretsRotate`
(vault-write phase only; lock released before the restart loop), and
`SecretsListWithUsage` (read-during-write queues briefly; RWMutex deferred
for V1 where contention is not measurable).

`SecretsRestart` does not write the vault and was intentionally left without
the mutex.

**What this fixes:** two concurrent GUI calls can no longer interleave
`age.Encrypt → buf → os.WriteFile` and produce a file that is not a valid age
ciphertext. FS-level corruption (entire vault unreadable) is now prevented
in-process.

**What remains open:** cross-process concurrency (CLI + GUI running
simultaneously, or two GUI processes) is still last-write-wins because
`vaultMutex` is process-local. An OS-level advisory file lock
(`github.com/gofrs/flock`, already in `go.mod`) is the correct fix and
remains future work.

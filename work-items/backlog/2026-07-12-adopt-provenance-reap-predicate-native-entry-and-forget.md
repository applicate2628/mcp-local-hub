# Backlog: sharpen the adopt-provenance reap predicate (native-entry, both lanes) + ship a `forget` escape

> **DELIVERED 2026-07-15.**
> - **Improvement A (entry-shape reap predicate, both lanes)** — shipped in **#551**
>   (`adoptRowProvablyUnmutated` rewritten to a write-target-physical entry-shape proof via
>   `ClassifyEntryUnderLock`, churn-immune, skip-absent + skip-merged-lower). Bot PASS +
>   fable adversarial security review (REFUTATION-HOLDS: the semantic-DeepEqual gate is
>   data-safe because de-adopt's own disposition consumes the same verdict). Merged master
>   `0336572f`.
> - **Improvement B (`mcphub adopt-provenance forget <manifest>`)** — shipped in the PR that
>   carries this doc edit: parent `adopt-provenance` cobra group + `forget` subcommand,
>   lease-held plan/execute, dry-run without `--yes`, `adopted`-row de-adopt-capability-loss
>   warning, `adopt-provenance-forgotten` event, reuses `reapAdoptProvenanceRow` /
>   `removeAdoptSnapshots`. Removes the row + snapshot dir only — NOT routed vault keys
>   (de-adopt's `--reclaim-crashed` owns those).
>
> Both improvements delivered; this backlog item is CLOSED. The historical analysis below is
> retained for reference.

Filed: 2026-07-12
Priority: P3 (usability + precision; NOT data-loss — the shipped whole-file-sha gate is fail-safe)
Source: fable adversarial lens on #532's capture-UPSERT Part-2 gate — the better predicate + the escape the minimal fix defers
Depends-on / relates: #532 (adds `adoptRowProvablyUnmutatedFn` to the capture-UPSERT reap), decision `2026-07-10-adopt-provenance-crash-consistency-model.md`

## Context

#532 closes a P1 all-return-paths data-loss gap by adding the GC's Part-2 gate
(`adoptRowProvablyUnmutatedFn`) to the capture-UPSERT reap. That predicate is a
**whole-file sha** of each `present` client's live config vs its capture snapshot
(`adopted_entries.go:993-1012`). The architect chose it deliberately for **lane symmetry**
(one predicate, one owner across GC + capture) — the minimal, safe, mergeable fix.

fable flagged two disclosed costs of the whole-file-sha predicate, both **over-block, not
data-loss** (the refusal preserves the prior row + snapshots):

1. **Config churn:** client apps (`claude.json`, VS Code settings) rewrite their own config
   files constantly (key reorder, unrelated servers, formatting). A genuine pre-Install
   crash orphan retried later sha-mismatches on unrelated churn → capture refuses, even
   though the adopted entry itself is untouched and there is zero restore value at risk.
2. **Absent / present-merged-lower clients:** `adoptRowProvablyUnmutated` returns false
   unconditionally for any non-`present`/empty-sha client (:996). An entryless-fanout adopt
   that crashed in the tiny capture→ManifestCreate window becomes **permanently** refused —
   no config state can satisfy the predicate — with no in-product escape (de-adopt not built;
   the GC shares the same predicate so the row is immortal).

## Improvement A — entry-shaped predicate (churn-immune), applied to BOTH lanes

Replace the whole-file-sha proof with a **per-snapshot-bearing-client entry-shape** check:
reap is safe iff the live config STILL contains `SourceEntryName` as an adoptable **native
stdio** entry. Install replaces the native entry with a hub relay, so *native-still-present
⇒ Install never committed on that client* — independent of surrounding file churn.

- **Owner already exists:** `clients.EntryPresentInBytes` (`internal/clients/entry_bytes.go`)
  — byte-level, per-adapter, read-only, the exact capability. (Plus a native-vs-hub-shape
  discriminator: a present entry that is a hub relay must still count as "Install committed."
  Check whether `GetEntry`'s shape / `liveEntryMatchesManifestBinding` already distinguishes
  native-stdio from hub-relay, or add a small `IsNativeStdioEntry`-style check.)
- **Both lanes, not capture-only** — applying it only to capture re-introduces the exact
  lane divergence #532 closes (the architect's hard constraint). So GC's Part-2
  (`:1138`/`:993-1012`) moves to the same predicate. Requires re-verifying the case-3 /
  case-5 analysis under the new predicate (native-present ⇒ reap; case-3 stays KEPT because
  Signal 2b short-circuits before Part-2; case-5's mutated=hub-shaped client ⇒ native-absent
  ⇒ KEEP — same verdict, churn-immune).
- **No-snapshot clients (absent / present-merged-lower) must NOT block** — skip them (they
  hold no restorable pre-adopt bytes), fixing over-block class 2.

## Improvement B — `mcphub adopt-provenance forget <manifest>` (confirmed escape)

The whole-file-sha gate (and even the sharper predicate, for a genuinely committed row)
can leave an operator refused with no clean in-product recovery until de-adopt ships. Add a
confirmed operator affordance to discard a stale/blocking provenance row + its snapshot dir
under the per-manifest lease:

- `mcphub adopt-provenance forget <manifest> [--yes]` — acquires the lease, prints what will
  be removed (row + `adopt-provenance/<manifest>/` snapshot dir, NAMES only), requires
  confirmation, removes both. Emits an `adopt-provenance-forgotten` audit event.
- This is the clean escape the #532 refusal error message points at (currently it names the
  manual `adopted-entries.json` row + dir removal — a hand-edit under flock).

## Not doing now / why

- The whole-file-sha gate SHIPS in #532 as the minimal P1 closure — it is fail-safe (refuse
  preserves data) and symmetric. This backlog is the *precision + ergonomics* follow-up, not
  a correctness blocker.
- The real "reclaim a crashed adopt's manifest+vault+row" teardown is a SEPARATE item routed
  to de-adopt (bug `2026-07-12-adopt-preinstall-crash-orphan-triple.md`); `forget` here is
  the narrower "discard a blocking provenance row" escape.

## Update 2026-07-12 — the full refinement is DONE-but-superseded on a branch (Sol Option-2)

After #532 merged the core all-return-paths P1 fix with the CONSERVATIVE whole-file-sha
predicate, the churn-immune refinement turned into an information-theoretic edge-mine (the
reap gate cannot satisfy both no-over-block AND no-under-protect — case-3 ≡ committed-then-
reverted; abort-residue ≡ committed-with-deleted-snapshot; merged-lower-crash ≡
merged-lower-committed are indistinguishable on disk). The architecture lane (Sol, gate PASS,
2026-07-12) chose **merge-core + defer-refinement**: ship the conservative gate (uncertainty
=> KEEP), move the refinement here.

**The refinement work is COMPLETE (not just planned) on branch `wip/adopt-reap-predicate-refinement`**
(tip commit `217a439c`), which the follow-up PR should build on:
- write-target entry-equality predicate (`EntryRawFromBytes` per-adapter + `adoptWriteTargetEntry
  Unchanged`, reads the write-target on both live+snapshot — NOT the merged view);
- snapshot integrity (recorded-SHA verify) + hardened inode-anchored read + 16 MiB `.snapshot`
  read-cap;
- skip-absent + skip-merged-lower + abort-residue (os.ErrNotExist) reclaim;
- both-lane mutation-point manifest re-checks;
- GUI 409 for the `errAdoptPriorConfigMutated` refusal;
- ~30 tests locking Terra/Sol/fable edges + the per-client committed-entry lock.

**Blocking issue the follow-up MUST resolve before that predicate is safe:** the "reap more"
skips UNDER-protect — a skipped client's row can still hold `RoutedSecretKeys` (a committed/
failed adopt whose snapshot dir is gone), so reaping orphans the routed vault keys de-adopt
must delete (Codex bot :1205/:1211 on `217a439c`). The clean resolution is the **`forget`
operator escape** (conservative gate + explicit operator discard) rather than an
information-theoretically ambiguous aggressive predicate. See also
`work-items/bugs/2026-07-12-adopt-reap-native-revert-deletes-committed-provenance.md` and
`work-items/bugs/2026-07-12-adopt-reap-present-merged-lower-permanent-keep.md`.

# Bug: an adopt crash between ManifestCreate and Install leaves a THREE-part orphan (manifest + routed vault keys + provenance row/snapshot) that blocks clean re-adopt

Status: open
Filed: 2026-07-12
Severity: P3 (bounded, owner-only, secrets-also-in-vault; blocks same-name re-adopt until manual manifest+vault cleanup)
Source: $architect adjacent finding during #532 (adopt-GC classifier) review — the real substance of "case-3 closure"
Context: adjacent-finding (independent of the #532 GC dispute)
Route-to: de-adopt work-item (`work-items/active/2026-07-09-deadopt-hub-to-native/`) — cleanup belongs to an operator-driven, hash-gated teardown, NOT the background GC

## What happens

`ExecuteAdoptWithOpts` (`internal/api/adopt.go` ~:286-336) commits durable side effects in
this order: `captureAdoptProvenance` → `persistAdoptRoutedSecrets` (:291) →
`ManifestCreate` (:297) → `Install` (:310) → promote (:336).

A hard crash **after `ManifestCreate` succeeds but before `Install` rewrites any client
config** ("case-3") leaves a THREE-part orphan on disk:

1. the **provenance row + secret-bearing snapshot dir** (`adopting` state);
2. the **created manifest** `<state-dir>/servers/<M>.yaml` (or wherever manifests live);
3. the **routed vault keys** for M (`persistAdoptRoutedSecrets` already ran).

## Why it is not self-cleared by the GC

The adopt-provenance GC (and capture-UPSERT) reap **only the row + snapshot**
(`reapAdoptProvenanceRow` → `removeAdoptSnapshots`; no `ManifestDelete`, no vault
`Delete`). And in #532 the row is deliberately KEPT while the manifest exists (Signal 2b /
Part-3 — see decision `2026-07-10-adopt-provenance-crash-consistency-model.md`), because a
manifest-present row is indistinguishable from a committed adopt whose config was later
reverted (the case-3 ≡ case-d indistinguishability — architect adjudication 2026-07-12).
So the row/snapshot is intentionally retained, and the manifest + vault keys are never
touched by any autonomous path.

## Why it blocks re-adopt

Both disk orphans fail-close the natural retry:
- `BuildAdoptPlan` refuses a pre-existing manifest (`adopt.go:163-167`).
- `persistAdoptRoutedSecrets` refuses an existing vault key (`adopt_secret_route.go:143-145`).

So the operator cannot cleanly re-adopt M until they manually delete the stale manifest
AND clean the routed vault keys. Once the manifest is removed, the next adopt's
capture-UPSERT re-classifies the prior row (`classifyDeadAdoptingRow` sees the manifest
absent → `CrashReap`) and reaps the snapshot — so the row/snapshot is self-healing once
the manifest blocker is cleared.

## Harm (bounded)

- The snapshot is owner-only (DACL-protected); its secret VALUES also live in the vault,
  so no new exposure.
- The manifest is a non-secret YAML; visible via `mcphub manifest list`.
- The routed vault keys linger under `<M>_<ENV>` names.
Net: operator-visible crash debris that requires a manual 2-step cleanup before re-adopt.

## Recommended fix (route to de-adopt, NOT the GC)

"Tear down an orphaned committed/crashed adopt" is de-adopt's job — operator-driven,
hash-gated (`AdoptManifestHash`/`ExpectedManifestHash`, decision
`2026-07-10-deadopt-manifest-delete-hash-gate.md`). A background GC must NEVER
autonomously delete a manifest + vault keys (a manifest-present row can be a LIVE
committed adopt whose supervisor daemon is still running — deleting under it is
live-process/secret destruction; the GC runs on every unrelated adopt, `adopt.go` step 0a).

Proposed: a `de-adopt --reclaim-crashed <manifest>` (or an option on de-adopt) that, under
the per-manifest lease and the hash gate, positively confirms the adopt never committed
(no live binding, configs unmutated) and then removes manifest + vault keys + row +
snapshot as ONE operator-confirmed teardown. Design owner: de-adopt work-item.

## Do NOT

- Do NOT "fix" this by removing Signal 2b / Part-3 from the GC classifier — that reopens a
  committed-row-destruction path (case-d) and does not even clean the manifest+vault. See
  decision `2026-07-10-adopt-provenance-crash-consistency-model.md` + the 2026-07-12
  architect adjudication.

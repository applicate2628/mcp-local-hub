# Design — Adopt abort must preserve provenance when Install's client-config rollback is incomplete

Bug: `work-items/bugs/2026-07-12-adopt-abort-deletes-provenance-on-rollback-failure.md`
Branch: `fix/adopt-abort-preserve-provenance-on-rollback-failure`
Architect gate: PASS (2026-07-12)

## Approach

Install emits an **additive typed sentinel** naming exactly which clients its rollback
failed to restore. `ExecuteAdoptWithOpts` inspects it via `errors.As` and, when present,
**preserves the whole partially-committed state** (provenance row `adopting` + snapshots +
manifest + routed vault keys) instead of aborting. The existing post-#532 GC reclaim gates
(`classifyDeadAdoptingRow` Signal 2b + `adoptRowProvablyUnmutated`) reclaim the preserved
row with ZERO change once the operator reverses the partial commit.

The signal keys on the restore **operation's own outcome** (the callback's `err`), not a
re-derived comparison — fires precisely in the data-loss window; does not over-preserve on
common failures (bad manifest, port conflict, absent-fanout clients).

## Contract

### Install return-signal (new, additive) — `internal/api/install.go`

```go
// InstallClientRollbackIncompleteError is joined into the error Install returns when a
// mid-install failure triggered rollback but ≥1 client-config restore callback FAILED.
type InstallClientRollbackIncompleteError struct {
    Clients []string // NAMES only. No paths, no config bytes, no secret values.
    cause   error
}
func (e *InstallClientRollbackIncompleteError) Error() string  // "...client-config rollback incomplete (clients: a, b)"
func (e *InstallClientRollbackIncompleteError) Unwrap() error   // returns cause (preserves %w chain)
```

Present iff ≥1 client-restore callback errored during `runRollback`. Otherwise Install's
error is **byte-identical to today** (no sentinel).

### Plumbing — single owner `executeInstallToWithSymlinkConsents` (install.go:2480)

1. Add `var rollbackClientRestoreFailures []string` beside the rollback stack (:2532).
2. In the client-restore closure (:2702-2708), on
   `RestoreEntryFromBackupForRollbackWithConfigWriter` error, **append `clientName`**
   (keep existing print).
3. One helper closure; route every failure return through it:
   ```go
   failAfterRollback := func(cause error) error {
       runRollback()
       if len(rollbackClientRestoreFailures) == 0 {
           return cause // byte-identical to today's happy-failure path
       }
       return &InstallClientRollbackIncompleteError{
           Clients: append([]string(nil), rollbackClientRestoreFailures...),
           cause:   cause,
       }
   }
   ```
   Replace `runRollback(); return <errExpr>` at :2580, :2635, :2658, :2672, :2690, :2718
   with `return failAfterRollback(<errExpr>)`.

Scopes the signal to **client-config restore** only — scheduler-task / supervisor-intent
undo failures do NOT trigger preserve. (adopt sets `SkipSchedulerTasks=true` anyway.)
Rollback stack type stays `[]func()`; ordering unchanged.

### `ExecuteAdoptWithOpts` decision — Install branch only (adopt.go:318-338)

```go
if err := a.Install(...); err != nil {
    var rbErr *InstallClientRollbackIncompleteError
    if errors.As(err, &rbErr) {
        // PRESERVE. ≥1 client config Install rewrote was NOT restored.
        emitAdoptProvenancePreserved(plan.ManifestName, rbErr.Clients) // NAMES/COUNTS only
        return fmt.Errorf(
            "adopt install failed and its client-config rollback could not be fully reversed "+
            "(clients left pointing at the hub relay: %s); the pre-adopt provenance snapshot, "+
            "manifest %q, and routed vault keys were PRESERVED so the state stays recoverable — "+
            "restore those clients from the timestamped .bak-mcp-local-hub-* backup printed above, "+
            "then remove manifest %q, or reverse it with de-adopt: %w",
            strings.Join(sortedAdoptStrings(rbErr.Clients), ","), plan.ManifestName, plan.ManifestName, err)
    }
    // ABORT — rollback provably complete OR nothing client-side mutated. Existing cleanup UNCHANGED.
    ... (existing :326-337 verbatim) ...
}
```

Secrets branch (:299) + manifest branch (:305) **untouched** — abort unconditionally.

## Preserved end-state (all four kept)

| Artifact | Action | Why |
|---|---|---|
| provenance row | keep `adopting` | partial commit; de-adopt-ready (both hashes at capture) |
| secret snapshots | keep | pre-adopt recovery copy for un-restored clients |
| manifest | keep (skip `ManifestDelete`) | client points at its hub relay; deleting drops Signal-2b protection |
| routed vault keys | keep (skip `deleteAdoptRoutedSecrets`) | daemon needs them to spawn+auth so relay works |

Do NOT promote to `adopted`. `adopting`+manifest-present == Signal-2b committed-keep state.

## Per-branch decision table

| Branch (adopt.go) | Client mutated? | Signal | Decision |
|---|---|---|---|
| secrets fail (:299) | No (before Install) | N/A | Abort (unchanged) |
| manifest fail (:305) | No (before Install) | N/A | Abort (unchanged) |
| Install fail — none rewritten / all restored | maybe, all restored | no sentinel | Abort (existing cleanup) |
| Install fail — ≥1 restore FAILED | yes | `*InstallClientRollbackIncompleteError` | Preserve + emit + `%w` error |

## Change-surface

- `internal/api/install.go` :: `executeInstallToWithSymlinkConsents` (aggregation + helper + closure append)
- `internal/api/install.go` :: new `InstallClientRollbackIncompleteError`
- `internal/api/adopt.go` :: `ExecuteAdoptWithOpts` INSTALL branch only
- `internal/api/adopt_provenance_events.go` :: new `emitAdoptProvenancePreserved` (mirror `emitAdoptProvenanceAbort`)
- docs: CLAUDE.md "Adopt provenance" events 6→7 (+ `-preserved`); close bug file

**Protected / must-not-touch:** `func (a *API) Install(opts) error` signature (error-only);
rollback stack shape + `runRollback` ordering; `abortAdoptProvenance`, `removeAdoptSnapshots`,
`reapAdoptProvenanceRow`, `classifyDeadAdoptingRow`, `adoptRowProvablyUnmutated`,
`gcOrphanedAdoptingProvenance` (consumed as-is, zero change); adopt secrets+manifest branches;
`.bak-mcp-local-hub-*` backstop; every non-adopt Install caller's happy-failure error text.

## Tests (8)

1. Signal presence — AddEntry ok(A), fail(B), restore(A) forced fail ⇒ `errors.As` sentinel, `Clients=[A]`.
2. Signal absence byte-identical — AddEntry fail(B), restore(A) ok ⇒ no sentinel; error string == baseline.
3. No client write ⇒ no sentinel (pre-client-loop failure).
4. Full-chain survival — drive `a.Install(...)` (not just executeInstallTo) with forced restore fail ⇒ sentinel recovered at top.
5. Adopt preserve — Install seam returns sentinel-wrapping err ⇒ row present+`adopting`, snapshot dir present, manifest present, keys present, `adopt-provenance-preserved` emitted, `%w`+names.
6. Adopt abort regression — plain err ⇒ abortAdoptProvenance + ManifestDelete + deleteAdoptRoutedSecrets all fire.
7. Secrets/manifest branches — unconditional abort, sentinel never consulted.
8. Reclaim composition — preserve → gc(0) reaped==0 (Signal 2b); restore client bytes to snapshot + remove manifest → gc(0) reaped==1 (sha-gate).

Gate: `go build ./... && go vet ./... && go test -count=1 -timeout 5m ./internal/api/...`
+ `go test -tags=test_state_path_env -count=1 -timeout 5m ./internal/api/ ./internal/cli/`
Sweep `mcphub` processes after.

## Residual (fail-safe = PRESERVE on uncertainty)

Over-preserve cost = `adopting` row + manifest blocks re-adopt of same name — **correct**
(prior adopt left client committed-ish; blind re-adopt would double-adopt). Matches subsystem
destructive-default polarity. The one real residual: fail-open drift — a FUTURE install path
rewriting a client config OUTSIDE the `BackupKeep→AddEntry→restore-closure` triad would not
feed the aggregation. Bounded by the single-owner invariant (true today); pinned by claim-2
probe + standing check "all install client-config mutations route through the triad".

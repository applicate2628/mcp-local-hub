# Bulk-prune paths clear the default-workspace marker outside the registry-lock hold

- **Status:** open
- **Context:** adjacent-finding
- **Severity:** P2
- **Found:** 2026-07-26, while fixing the same class on `mcphub workspace unregister`
  (branch `fix/workspace-register-materializes-intent`)

## Summary

Two bulk-prune call sites clear the `default-workspace.txt` marker AFTER
`api.PruneWorkspace` has returned — i.e. outside every registry-lock hold. That is
the same ordering defect just fixed on the `mcphub workspace unregister` path,
where the clear was moved INSIDE `DeleteSerenaRow`'s existing hold and gated on a
non-zero row delete.

Affected sites:

- `internal/cli/workspace_prune_cmd.go:377-386` — `applyWorkspacePrune`'s `record`
  closure, gated on `report.SerenaRemoved > 0`.
- `internal/gui/workspace_prune_sweeper.go:63-69` — `pruneClearDefaultFn`, called
  by the auto-prune sweeper after a successful `PruneWorkspace`.

Both gate correctly on "a serena row was actually removed", so the ownership half
of the invariant already holds. Only the ORDERING half is missing.

## Mechanism

`api.PruneWorkspace`'s own `DeleteSerenaRow` closure
(`internal/api/prune_workspace.go:161-179`) commits the row delete under
`reg.Lock()` and releases it on return. The marker clear then runs in a later,
unlocked step. In that gap a concurrent `mcphub workspace register --default` can
recreate BOTH the serena row and its marker (register writes the marker inside its
own registry-lock hold — `internal/cli/workspace_cmd.go` step 6a). The older prune
then clears the NEW marker even though that registration succeeded, leaving the
operator registered with no default and nothing in the output to explain it.

The GUI sweeper case is the more likely one in practice: it runs unattended on a
~60s tick, so it can collide with an interactive `register --default` without
anyone driving both.

## Suggested fix

Do NOT patch the three call sites individually. Move the clear into
`api.PruneWorkspace`'s `DeleteSerenaRow` closure, inside the hold and gated on
`n > 0` (mirroring what `runWorkspaceUnregister` now does), then DELETE the
post-hoc clears at both sites above. That leaves ONE owner of the
"whoever deleted the row clears the marker naming it" invariant.

Note the CLI `unregister` path deliberately keeps its own `DeleteSerenaRow`
closure (it carries the CLI's test-injectable seams), so after this change the two
production closures would both clear in-hold; that duplication is pre-existing and
tracked separately by whatever consolidates the two closures.

## Why it was not fixed in the same change

Out of the approved change surface for the reviewed finding, which cited
`internal/cli/workspace_cmd.go:~864` only. Fixing it properly edits
`internal/api`, `internal/cli`, and `internal/gui` and changes the auto-prune
sweeper's behavior, which needs its own tests and review.

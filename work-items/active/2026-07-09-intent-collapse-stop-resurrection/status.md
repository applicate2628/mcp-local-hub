# status - intent collapse stop resurrection

Template: full-delivery (behavioral supervisor-intent fix). Orchestrator: `$lead`.
State: ACTIVE - PR #525 is open; Codex bot review has COMMENTED on the current
head, so bot PASS is not verified. A linked worktree contains uncommitted
absent-only rewrite changes on top of the PR head.

PR: #525 `fix(supervisor): legacy_stop_watermarks blocks stale daemon-intent stop-resurrection`
Branch: `fix/intent-collapse-stop-resurrection`
Base: `master`
Current HEAD: `016896418f321c0923f57465b4487ff79e51ed5d`
GitHub state: OPEN
GitHub reviewDecision: empty
Decision: `work-items/decisions/2026-07-09-absent-only-legacy-stop-watermarks.md`

## Verified facts
- `gh pr view 525 --json headRefName,headRefOid,state` reports branch
  `fix/intent-collapse-stop-resurrection`, state `OPEN`, head
  `016896418f321c0923f57465b4487ff79e51ed5d`.
- `git log master..fix/intent-collapse-stop-resurrection` reports:
  - `01689641` `test(supervisor): close 4 commission P3 gaps on the legacy_stop_watermarks fix`
  - `46ca5dc4` `fix(supervisor): legacy_stop_watermarks blocks stale daemon-intent stop-resurrection`
- The branch is checked out in a linked worktree. `git status --short` there
  reports uncommitted changes in:
  - `internal/api/intent_collapse.go`
  - `internal/api/intent_collapse_e2_test.go`
  - `internal/api/intent_collapse_test.go`
  - `internal/api/register_supervisor.go`
  - `internal/api/register_supervisor_rollback_test.go`
  - `internal/api/stop_intent_subblock.go`
  - `internal/api/stop_intent_subblock_test.go`
  - `internal/api/supervisor_intent.go`
  - `internal/cli/intent_collapse_cmd.go`
  - `internal/cli/intent_collapse_cmd_test.go`
- Current verified Codex bot review state: `chatgpt-codex-connector` submitted
  a `COMMENTED` review on current head `016896418f`.

## Active agents / lanes
- `$lead`: active orchestration and gate ownership.
- Implementation lane: active in the linked PR worktree; finish the absent-only
  rewrite and the narrow P3 fix before publication.
- Independent audit lane: handoff says the invariant-ownership lane died on a
  session limit and must be rerun independently.
- Review lane: Codex bot re-review pending after the fix is pushed.

## Completed agents / lanes
- `$architect`: accepted the absent-only / lazy representation decision captured
  in the decision record for this item.
- GitHub Codex bot: completed review comments on current head; gate not PASS.
- Handoff-reported review history, not independently re-verified in this
  archivist pass: two commissions accepted the original eager design; later review
  invalidated that representation with a P1 size-cap finding and a P2 rollback
  guard finding; a narrow re-commission found one restore-path P3.

## Current open findings on head
- P1: eager full watermarks duplicate every active stop and can push
  `supervisor-intent.json` over the shared 16 MiB read cap.
- P2: rollback can skip watermark-only restores.
- Handoff-reported P3: the restore path's watermark branch writes without
  re-checking `Stops[key]`, transiently violating the absent-only invariant on a
  race.

## Next action
Finish the linked-worktree absent-only rewrite and P3 fix, rerun the independent
invariant-ownership audit, force-push PR #525, then request Codex re-review.
Do not treat this PR as bot-PASS until the current head has a passing review
record.

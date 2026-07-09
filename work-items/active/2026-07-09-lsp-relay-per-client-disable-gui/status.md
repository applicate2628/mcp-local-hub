# status - LSP relay per-client disable GUI

Template: full-delivery (behavioral GUI/API fix). Orchestrator: `$lead`.
State: ACTIVE - PR #524 is open; Codex bot review has COMMENTED on the current
head, so bot PASS is not verified.

PR: #524 `feat(gui): per-client LSP-relay enable/disable in the Servers matrix`
Branch: `fix/lsp-relay-per-client-disable-gui`
Base: `master`
Current HEAD: `41750adeb464ed4c5a02aed82a9dd7a62d315b90`
GitHub state: OPEN
GitHub reviewDecision: empty

## Verified facts
- `gh pr view 524 --json headRefName,headRefOid,state` reports branch
  `fix/lsp-relay-per-client-disable-gui`, state `OPEN`, head
  `41750adeb464ed4c5a02aed82a9dd7a62d315b90`.
- `git log master..fix/lsp-relay-per-client-disable-gui` reports:
  - `41750ade` `fix(gui): LSP-router toggle derives state from presence, not transport`
  - `31085bb3` `Merge branch 'master' into fix/lsp-relay-per-client-disable-gui`
  - `669d6019` `feat(gui): per-client LSP-relay enable/disable in the Servers matrix`
- The linked worktree for this branch is clean as of this recovery pass.
- Current verified Codex bot review state: `chatgpt-codex-connector` submitted
  `COMMENTED` reviews on commits `669d601978`, `31085bb39d`, and current head
  `41750adeb4`.

## Active agents / lanes
- `$lead`: active orchestration and gate ownership.
- Implementation lane: active; next patch must address current Codex P2 comments.
- Review lane: Codex bot re-review pending after the next fix.

## Completed agents / lanes
- GitHub Codex bot: completed review comments on the current head; gate not PASS.
- Handoff-reported review history, not independently re-verified in this
  archivist pass: a concurrency commission found the settings lost-update race;
  a second commission found deterministic row aggregation and URL parser gaps.

## Current open findings on head
- Disable toggles for unusable or absent client configs.
- Keep opt-out possible when router entries are absent.
- Disable or specially handle present non-router entries that the backend cannot
  replace.
- Treat disabled router entries as unchecked/enablable.
- Check relay ownership before marking relay rows checked.
- Serialize the full per-client enable/disable operation, not only the preference
  write.

## Next action
Fix the current-head Codex P2 findings, then request Codex re-review on PR #524.
Do not treat this PR as bot-PASS until the current head has a non-commented
review outcome or an explicit passing review record.

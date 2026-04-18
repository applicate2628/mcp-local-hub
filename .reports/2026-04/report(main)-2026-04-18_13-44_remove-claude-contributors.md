Summary:
The user asked to remove Claude from the GitHub contributors/co-author surface for `applicate2628/mcp-local-hub`, citing commit `582c1bf1cd183183cca7f010775ec2d711f1a909`. GitHub and local inspection confirmed the cause was published `Co-Authored-By: Claude ... <noreply@anthropic.com>` trailers in commit messages. I rewrote local `master` history to remove those trailers from the current branch, verified that `git log master --grep="Co-Authored-By: Claude"` now returns zero matches, and verified that the old and new `HEAD` trees are identical. Outcome: PASS locally, but GitHub is not updated yet because applying the change remotely requires a reviewed `git push --force-with-lease`.

Participants involved:
- main conversation (Codex)

Canonical artifact:
- none

Follow-ups / open items:
- Backup tag saved locally as `backup/pre-remove-claude-contributors-2026-04-18`
- Current branch state is rewritten local history: `master...origin/master [ahead 83, behind 83]`
- Remote update still required: reviewed `git push --force-with-lease origin master`

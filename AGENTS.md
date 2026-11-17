# mcp-local-hub — repository instructions for coding agents

This is the **single canonical instruction entry point** for this repository. It replaces the former root `CLAUDE.md`; that file imports this one only for older Claude Code versions. The complete former guidance is preserved **byte-for-byte** in [the project operational runbook](docs/agent-runbook.md). Use that runbook for relevant subsystem procedures, not as a second general instruction layer. The runbook contains historical "REMOVED" mechanisms; do not treat them as implemented without verifying current code.

## Scope and safety

- Work from the actual repository state and the current requested task. Check the base commit, open pull requests (PRs), worktree dirtiness, and the owning component before editing. Preserve unrelated working trees and changes.
- Prefer one coherent owner and the smallest root-cause correction. Do not add duplicate PRs, layers, dependencies, workflows, or generic frameworks merely because a symptom appeared in several places. Keep PRs independently reviewable and avoid unnecessary rebases of other active work.
- Never infer consent for paid/quota-consuming model or agent execution, Codex/Work dispatch, installed-state changes, scheduler/system-policy changes, or manually triggered continuous integration (CI). Obtain the user's explicit authorization for the exact action. A prerequisite that needs such consent holds that action, not unrelated authorized work.
- On a remotely controlled machine, remain inside the user-authorized work root. Do not use a system drive as scratch. Preserve acceptance evidence and unique work outside trash; move only confirmed disposable, quiescent items to the authorized trash without purging it.
- Do not kill processes by image name. Only release test-owned processes using retained creator identity and native ownership evidence. Never disturb an installed Hub or unrelated listeners.
- Avoid secrets, credentials, customer data, and machine-specific private paths in commits, logs, and PR descriptions. Prefer source-relative paths and redacted evidence.
- Before claiming success, distinguish actual execution and tested properties from mock assumptions, unexecuted branches, historical receipts, and release/production readiness.

## Build and test gates

The original mandatory repository commands, from the [operational runbook](docs/agent-runbook.md#pr-review--merge-workflow-mandatory), are:

```sh
go build ./... && go vet ./... && go test -count=1 -timeout 5m ./...
go test -tags=test_state_path_env -count=1 -timeout 5m ./internal/api/ ./internal/cli/
```

- Use the repo's supported toolchain and isolated test-state roots. Perform these gates before publication/merge as required by the repository policy. If they are red or cannot run, report exact command, target commit, failures, and unverified cases; do **not** call a draft or PR ready to merge.
- Never treat `go build` as a qualified Windows Product. Windows releases require the documented `build.ps1` artifact and original executable/identity admission; test-only Product and source-issued Serena qualification roles are distinct from installable Products.
- For a narrowly scoped regression, run the relevant focused unit/native tests first, then the required full checks. Existing native acceptance and source/image identities bind to their exact commits; a changed commit requires fresh qualification where identity matters.
- Inspect owned resources and process settlement on every exit. A wrapper exit code of 0 does not imply that a nested test passed; read the nested command result and persisted receipt.

## Pull request review and integration (mandatory)

- Search current PRs for related or superseding work before opening another. Keep one PR per independent, reviewable responsibility and make dependencies explicit. An integration worktree is evidence, not an excuse to open a huge aggregate PR.
- After local checks, publish only the reviewed, non-sensitive diff to the intended existing/new branch under the owner's explicit authority. Do not bypass blocked host operations by changing executors.
- Check Codex review against the **exact current head commit**, including the latest no-major-issues summary (or qualifying thumbs-up/approval), all paginated inline comments anchored by original commit, and absence of newer inline findings. An old PASS is not a new PASS.
- Perform own full-diff review and the additional independent security reviews required by [the runbook](docs/agent-runbook.md#pr-review--merge-workflow-mandatory). Do not launch quota-consuming reviewers without authorization; keep merge blocked when required review is unavailable.
- Manual CI workflows require explicit dispatch permission. Do not trigger CI to manufacture a green result. No `--admin` bypass, forced history rewrite, or installed software rollout without explicit approval. Merge only after all current-head gates and accepted reviews pass, then verify the actual merged tree.
- Preserve sources, unique evidence, historical failures, and rollback paths before removing obsolete branches or local worktrees. A PR with overlapping files is not a duplicate unless its unique changes are proven already incorporated.

## Product and subsystem reference (read when relevant)

- Windows canonical entry, version resources, console and command behavior: [Windows console contract](docs/agent-runbook.md#windows-console-contract).
- GUI/frontend, embedded bundle and end-to-end (E2E) tests: [GUI frontend](docs/agent-runbook.md#gui-frontend-phase-3b-ii-onward) and [GUI E2E tests](docs/agent-runbook.md#gui-e2e-tests-phase-3b-ii-onward). Regenerate embedded assets whenever frontend source changes.
- Supervisor, state files, upgrade transactions, recovery, and process custody: [Supervisor](docs/agent-runbook.md#supervisor-v050) and [Managed upgrade transaction](docs/agent-runbook.md#managed-upgrade-transaction).
- Serena preparation and migration safety: [Serena supervisor-lock interlock](docs/agent-runbook.md#serena-migrate-supervisor-lock-interlock-v05x). The live acceptance source is the current code plus its pinned evidence, not a prior PR comment alone.
- Release and npm publication: [npm publish](docs/agent-runbook.md#npm-publish--canonical-tag-push-ci). A tag push may trigger publication and is not an ordinary source PR operation.
- Relay/marketplace, groups, listener and language server (LSP) behavior: use the corresponding runbook section and inspect the actual producer and consumer code before changing a wire or lifecycle contract.

## Planning, evidence and terminology

- For the current open-PR integration sequence, see the [audited concise work plan](.plans/2026-11/open-pr-integration-20261117.md). That plan is a snapshot of observed GitHub state; query GitHub again before any PR action.
- At the end of each material implementation or integration step record **what changed, what ran and passed/failed, remaining blockers, and the updated residual plan**. Do not repeat already completed checks on unchanged inputs without a distinct boundary reason.
- Apply relevant available skills to the actual change: Git worktrees and versioning, architecture and contract review for cross-owner changes, Go testing/concurrency for relevant code, simplicity review to remove duplicate fixes. No fixed count of skills is a correctness gate.
- Expand new abbreviations on first use in human-facing documentation. Keep formulas, identities, file paths, dates, and claims tied to explicit sources and avoid invented verification receipts.

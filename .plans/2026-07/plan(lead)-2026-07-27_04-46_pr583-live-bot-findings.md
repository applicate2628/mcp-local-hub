# Plan: PR #583 live Codex-bot findings

- Classification: breaking-or-cross-cutting behavioral correction.
- Template: full-delivery.
- Admission: direct human dispatch on 2026-07-27.
- Integration owner: main-session `$lead`.

## Stages

1. Completed — evidence-only classification and class inventory.
2. Completed — backend implementation with class-wide regression coverage.
3. Completed — independent QA mutation proof and safe verification.
4. Completed — claim-verify architecture review of concurrency snapshot and cleanup safety.
5. Completed — local fix commit `50a0e4b0` and final evidence report; no push.

## Safety

Every `internal/api` or `internal/cli` test uses
`-tags=test_state_path_env` and a fresh `MCPHUB_STATE_DIR_OVERRIDE`. Never run
unscoped `go test ./...`; never launch GUI/tray/supervisor; never kill by image
name; never leave this worktree.

## Result

PASS. Six findings were already closed by `c826a48d`; the two router-liveness
duplicates were closed by `50a0e4b0`. The canonical record is archived at
`work-items/archive/2026-07/2026-07-27-pr583-live-bot-findings/`.

## Terms and Abbreviations

- API: Application Programming Interface.
- CLI: Command-Line Interface.
- PASS: the accepted scope and required gates completed successfully.
- PR: pull request.
- QA: Quality Assurance.

# Plan Snapshot: PR #266 Deep-Security Fixes

Objective: Fix all nine deep-security review findings for PR #266 without committing or deploying, keeping tests narrow and adding race coverage for daemon concurrency fixes.

Plan:

1. Add focused red tests for the nine findings, especially deterministic lazy-proxy Stop/Materialize race windows and supervisor unregister/re-register stale state.
2. Implement root fixes in the owning modules: supervisor intent refresh cleanup, lazy-proxy reaping/closed gates and synthetic lists, fatal live-port kill failures, schedulerless supervised register, typed scheduler-unavailable sentinel, exact direct-LSP cleanup matching, and LSP client-config ownership/backup rollback guards.
3. Run focused API, CLI, daemon, scheduler, and daemon race tests; fix regressions.
4. Run adversarial self-review over concurrency, schedulerless, durable mutation ordering, and cross-platform reachability; fix any findings.
5. Run requested `go build ./...`, scoped `go vet`, fresh focused tests, race tests, and `git diff --check`; report exact outputs and remaining non-owned worktree entries.

Execution role: none
Assigned / replaced internal role: none
Requested provider: none
Resolved provider: none
Actual execution path: main Codex session with local shell/tooling and one read-only subagent review
Model / profile used: unspecified by runtime
Deviation reason: none

## Terms and Abbreviations

API: application programming interface. CLI: command-line interface. DS1-DS9: deep-security findings 1 through 9 from the user request. IPC: inter-process communication. LSP: Language Server Protocol. PR: pull request. SM: state machine.

# Plan Snapshot

Objective: Fix PR #524 P2 so registry-backed legacy relay LSP entries are replaceable/enabled in the GUI while foreign or unowned entries remain disabled.

Status:
- completed: Collect diagnostic evidence and locate backend/frontend ownership points.
- completed: Add failing frontend test for legacy relay replaceability and foreign relay negative case.
- completed: Implement minimal helper change mirroring backend rule.
- completed: Run required frontend, generate, Go build/vet/test gates.
- completed: Write required report/session log and clean scratch files.

Execution role: external-worker
Assigned / replaced internal role: frontend-engineer
Requested provider: codex
Resolved provider: Codex CLI
Actual execution path: external CLI (Codex CLI)
Model / profile used: unspecified by runtime
Deviation reason: none

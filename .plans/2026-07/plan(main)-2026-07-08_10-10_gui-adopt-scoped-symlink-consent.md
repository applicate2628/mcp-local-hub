Summary: Fix PR #516 GUI adopt acceptance findings by replacing execute-time boolean symlink approval with a reviewed `(client, resolved_path)` consent set, removing GUI adopt's process-global `api.InteractiveSymlinkConsent` usage, redacting path-bearing adopt execute failures, and preserving rollback narration in server logs.

Plan:
1. Capture failing scoped-consent and redaction tests.
2. Thread symlink consent data through GUI and API install writes.
3. Update frontend request shape and test expectations.
4. Run required Go and frontend verification.
5. Write session report and summarize diff/results.

Execution role: main conversation
Assigned / replaced internal role: backend-engineer, frontend-engineer
Requested provider: none
Resolved provider: none
Actual execution path: local Codex session
Model / profile used: unspecified by runtime
Deviation reason: none

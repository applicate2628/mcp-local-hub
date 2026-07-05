Plan snapshot for round-11 LSP-through-hub fixes:

1. Read findings and repo rules, inspect current diff and touched code paths. Status: completed.
2. Add narrow failing tests for F1, F2, and F3 and verify red. Status: completed.
3. Implement minimal root fixes. Status: completed.
4. Run narrow tests plus requested build and vet. Status: completed.
5. Run adversarial self-review on diff, fix any issues, and re-run checks. Status: completed; no scoped code changes were required after review.
6. Persist required session log and report final diff/output. Status: completed by this snapshot and the companion report.

Execution role
Assigned / replaced internal role: none
Requested provider: none
Resolved provider: none
Actual execution path: main Codex session plus local multi-agent reviewer subagent
Model / profile used: unspecified by runtime
Deviation reason: none

## Terms and Abbreviations

- F1, F2, F3: the three numbered findings in `.scratch/r11-findings.txt`.
- LSP: Language Server Protocol.
- MCP: Model Context Protocol.
- PR: pull request.

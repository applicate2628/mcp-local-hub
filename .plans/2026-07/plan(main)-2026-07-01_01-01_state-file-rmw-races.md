Plan snapshot: PR #473 state-file RMW races

1. Sync branch and capture diagnostic evidence. Status: completed.
2. Add red interleave/cancel tests for the missed RMW and blocking-lock paths. Status: completed.
3. Implement the shared gui-preferences locked RMW mutator, default-workspace locked conditional clear, strict-mode supervisor-intent locked mutation, and context-aware supervisor-state lock acquisition. Status: completed.
4. Run the requested gated tests plus vet, build, Linux/Darwin builds, and publication safety. Status: completed.
5. Commit and push `fix/p3-state-file-rmw-races` to `origin` after checks pass. Status: pending at snapshot time.

Execution role
none
Assigned / replaced internal role
none
Requested provider
none
Resolved provider
none
Actual execution path
main Codex session
Model / profile used
unspecified by runtime
Deviation reason
none

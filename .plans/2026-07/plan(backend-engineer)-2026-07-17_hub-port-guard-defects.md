# Hub Port Guard Defect Fix Plan

Role: backend-engineer
Scope: PR #559 Unit A on `feat/item3-unitA-hub-port-guard`

1. Trace the client factory-failure and asynchronous rollback paths.
2. Add failing tests for construction failure, relay-stdio exclusion, and a group committed after the initial clear snapshot.
3. Make `ProbeHubGate` the single owner of client-side gate uncertainty.
4. Re-probe at the port-reset mutation boundary while `hub-mcp.lock` is held.
5. Run formatting, build, vet, and the exact tagged API/CLI/GUI test command required by the user.

Acceptance: both reported fail-open paths preserve the port unless a fresh, complete dependency probe is clear; the dependency-free rollback still resets it.

## Terms and Abbreviations

- API: Application Programming Interface.
- GUI: Graphical User Interface.
- PR: Pull Request.
- TOCTOU: Time Of Check To Time Of Use.

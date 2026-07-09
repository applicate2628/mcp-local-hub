# brief - LSP relay per-client disable GUI

## Problem
The Servers matrix can show a client as disabled while LSP-router relay entries
remain active or are re-added by setup. The original bug was operator-visible:
Antigravity stayed wired to multiple `mcphub relay --url .../lsp/<lang>/mcp`
entries after the GUI Servers-tab disable path appeared to disable it.

## Accepted scope
- Add per-client LSP-router enable/disable controls in the GUI Servers matrix.
- Keep the API owner for per-client router preferences in
  `internal/api/client_install_prefs.go`.
- Ensure disable and enable mutations cover preference state and client-config
  mutation as one coherent operation.
- Keep ownership predicates single-owned through shared helper logic for router
  URL shape and client-config usability.

## Out of scope
- Global LSP-router rollback behavior.
- Product-code changes in this recovery-state patch.
- Treating open bot comments as passed gates.

## Resume point
PR #524 is open at `41750adeb464ed4c5a02aed82a9dd7a62d315b90`. The next session
should fix the current Codex P2 review comments, verify the changed behavior, and
trigger re-review.

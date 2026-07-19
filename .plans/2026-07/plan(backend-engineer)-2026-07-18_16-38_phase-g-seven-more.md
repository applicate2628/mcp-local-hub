# Phase G seven-more-defect correction plan

Scope: the seven defects commissioned directly on 2026-07-18 for the live
`feat/gui-restart-unitb-gated` Phase G worktree.

1. Repair the same-port child bind retry to use the shared Windows bind-refusal
   predicate and make the regression inject Winsock `WSAEADDRINUSE` on Windows.
2. Distinguish physical listener closure from Serve-loop settlement so same-port
   rollback rebinds after a close-plus-wait-timeout.
3. Bound the hub-producer join while preserving closed admission and late
   producer self-teardown.
4. Reset the in-memory coordinator guard when the marker was proved cleared even
   if nonce or retained-handle cleanup reports residue.
5. Terminate the retained child on every pre-release rollback, including an
   unauthenticated same-port standby.
6. Sweep stale generation nonce leaves and their state-file lock leaves before a
   new marker generation begins.
7. Acknowledge an encoded 202 even without `http.Flusher`, and keep the active
   duplicate-response phase synchronized with coordinator progress.
8. Run targeted tagged red/green tests, all `TestRestartV3_*` with race and
   `-count=2`, then the commissioned build, vet, format, full tagged race suite,
   and `mcphub.exe` sweep.

Constraints: no Graphify; no `claude` CLI; no `MCPHUB_GUI_SPAWN_TESTS`; no real
GUI spawn; no commit; every GUI/CLI test uses `-tags=test_state_path_env`.

## Terms and Abbreviations

- GUI: Graphical user interface.
- HTTP: Hypertext Transfer Protocol.
- P1/P2/P3: Finding priorities from the accepted commission.

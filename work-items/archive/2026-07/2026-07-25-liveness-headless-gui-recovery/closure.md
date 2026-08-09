# Closure — supervisor liveness and PR #589 review findings

Closed: 2026-07-27

## Outcome

DELIVERED LOCALLY — the branch classifies and closes all seven supplied Codex
review findings. F1-F4 were already closed by `f150be61`; F5-F7 are fixed in
the closing change with class-wide tests and mutation evidence. The branch was
not pushed.

## Evidence

- F5 lifecycle propagation, three relaunch gates, and both F7 ordering sites
  passed their focused and adjacent guards.
- F6 prepared bytes, carrier persistence, replay, retained-history
  deduplication, process-exit survival, recovery finalization, and exit-7
  wording passed their focused and integration guards.
- Five reversible mutations failed at the intended assertions and restored
  exact source hashes.
- Tagged `go build ./...` and tagged `go vet ./...` exited 0 in both the
  initial QA pass and the architecture-correction re-verification.
- The same architecture reviewer closed the only REVISE finding after the
  event owner separated trusted prepared integrity from untrusted carrier
  structure validation.

## Residual risk

- F6 exactly-once identity is bounded to complete rows retained in the active
  event log plus `.1`; power-loss durability and unbounded history are not
  claimed.
- F7 diagnostics may delay function return after the recovery decision; the
  accepted guarantee is action-before-observability.
- Five API state-path tests and eleven CLI tests retain pre-existing fixture
  incompatibilities with the mandatory isolated test environment. Controls
  reproduced both classes, and their owning files are outside this change.

## Archive location

`work-items/archive/2026-07/2026-07-25-liveness-headless-gui-recovery/`

## Terms and Abbreviations

- API: application programming interface.
- CLI: command-line interface.
- F1-F7: the seven supplied PR #589 findings.
- GUI: graphical user interface.
- QA: quality assurance.

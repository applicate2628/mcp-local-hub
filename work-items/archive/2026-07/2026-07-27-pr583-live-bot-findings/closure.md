# PR #583 live Codex-bot findings closure

Closed: 2026-07-27

## Outcome

PASS. All eight supplied findings were classified against the local branch. Six
were already closed by local commit `c826a48d`; the two duplicate router-liveness
findings were fixed by `50a0e4b0`.

The cleanup owner now requires one cached proof that the configured router
listener has the GUI-managed identity before a router-origin direct entry can be
removed. Resolver, entry-read, scan, listener, and identity failures fail closed
and produce a deduplicated warning. Bound clients written during the same
registration retain their explicit cleanup authority without a redundant
listener probe.

## Verification

- Required destructive mutation proofs failed on the expected direct-entry
  assertions, and the restored implementation passed; evidence is indexed in
  `verification.md`.
- The final isolated Application Programming Interface suite passed 18
  top-level tests and 33 subtests.
- The focused Graphical User Interface and Command-Line Interface suites passed.
- `go build ./...` and `go vet ./...` both exited 0 with the test-state build tag
  and a fresh state-directory override.
- `git diff --check` and the publication-safety scan passed.
- Independent Quality Assurance passed, followed by an architecture review
  covering 16 of 16 claims.

## Residual risk

Point-in-time liveness can change after a successful probe and before a later
client uses the router; that is inherent to an external listener. The cleanup
decision itself consumes one registration-local proof and does not rescan.

There is no dedicated multi-port duplicate-warning test. The deduplicating
warning owner, one-call guards, and per-failure count assertions are covered;
the independent reviewers classified this as non-blocking coverage debt.

## Archive location

`work-items/archive/2026-07/2026-07-27-pr583-live-bot-findings/`

## Terms and Abbreviations

- API: Application Programming Interface.
- CLI: Command-Line Interface.
- GUI: Graphical User Interface.
- PASS: the accepted scope and required gates completed successfully.

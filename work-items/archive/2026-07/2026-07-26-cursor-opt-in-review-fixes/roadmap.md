# Cursor opt-in review fixes

## Admission

- Source: direct maintainer instruction on 2026-07-26.
- Decision: implement all six supplied review findings in branch `fix/cursor-not-default-install`, verify them under the maintainer's live-fleet safety constraints, commit locally, and do not push or touch a pull request.

## Outcome

Bare install and workspace-register fallback behavior use the client registry's default-install metadata as one policy owner, Cursor remains explicit opt-in, every user-facing default-client statement agrees, generated GUI assets match frontend source, and the requested safe gates pass.

## Sequence

1. Establish the complete behavioral and stale-text inventory.
2. Define the existing registry symbol as the single owner and plan the bounded changes.
3. Implement Go behavior/tests, frontend text/assets, and documentation/help synchronization.
4. Integrate and run narrow tests plus full build/vet.
5. Run an independent review, commit locally, and close without push/PR activity.

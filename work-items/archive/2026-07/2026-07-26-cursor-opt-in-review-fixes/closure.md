# Closure

Closed: 2026-07-26

## Outcome

`PASS` — all six admitted Cursor opt-in review findings are corrected and accepted for the requested local commit. The complete product revision contains 15 files relative to `origin/fix/cursor-not-default-install`.

The default-install membership owner is `clientDescriptor.defaultInstall` in `clientRegistry()`, exposed through `DefaultInstallClientNames()`. `buildDefaultClientBindings()` derives the workspace-register fallback from that owner. `effectiveClientBindings()` separately owns the per-registration target set used by both entry writes and replacement cleanup.

## Verification

- Four narrowly scoped Go test commands passed, including both register regression guards.
- `go build ./...` and `go vet ./...` passed.
- `go generate ./internal/gui/...` passed; generated assets have no diff.
- `git diff --check` passed.
- Revision-9 exhaustive sweep passed across 2,117 in-scope files.
- Same-revision external QA passed.
- Same-revision external architecture review passed with `CLEAN-SINGLE-OWNER`.

## Residual risk

- The unscoped `go test ./...` suite was intentionally not run because it can disturb the maintainer’s live fleet.
- No live `mcphub`, scheduler, client-config, or fleet-process smoke was run.
- Architecture review left one nonblocking wording advisory in the `clientsForBindings()` comment; it is outside the six admitted findings and does not affect runtime behavior.

## Publication state

The product revision and this closure are committed locally together. No push or pull-request action is authorized or performed.

Archive location: `work-items/archive/2026-07/2026-07-26-cursor-opt-in-review-fixes/`

## Retrospective

The recurring defect class was policy drift across derivatives. The durable correction is to keep runtime membership derived from the registry and to treat help, tests, fixtures, counts, and architecture text as sweep-verified derivatives rather than independent policy owners.

## Terms and Abbreviations

- `API`: Application Programming Interface.
- `CLI`: Command-Line Interface.
- `QA`: Quality Assurance.

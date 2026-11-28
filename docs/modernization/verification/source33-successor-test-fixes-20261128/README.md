# Minimal test fixes for the PR666 successor

## Status and scope

This v2 capsule corrects only the command spelling and package-scope metadata in v1. All seven code afterimages and all three patches are byte-identical to v1. The earlier metadata did not express the required build/vet conjunction or the tagged API/CLI-only package scope; it must not be used as the acceptance contract. No execution was claimed for either draft.

This static candidate applies to exact commit `33eaf8af8fc46cb231f48c70f4775922393245b1`, tree `9e46575153d8a70c98c5629fdaa4767f61401f6c`. It changes seven test files and no production file. It has not been applied, formatted, built, or tested.

Applying the exact patch to all 3,369 unchanged base paths produces tree `f68f7584dba7744e7607b07f615758f3ff633f54`. The root owner will review, apply, and commit the changes through the existing native route. Existing commits 06f77b and 33eaf8 remain in history.

## Changes

| Area | Files | Reason and resulting behavior |
| --- | --- | --- |
| Canonical fixtures | Four CLI test files | An explicit fixture-key parameter separates same-image command/preflight fixtures from the two state-isolated native Product fixtures. All admission and ownership checks remain unchanged. |
| Job full-suite entry | One process test file | Reuses the existing Source3ebe/727 TestMain seam: receive the original grant before any test can create children, clear consumed control environment for helper children, restore NUL input, then run the whole test inventory. |
| Tagged CLI caller | One CLI test file | Receives the four existing helper results; ignores only its unused completion channel and preserves the original joining cleanup. |
| Proof-window fixture | One process test file | Receives both the real original handle signal and real zero-active accounting from the same Job within one five-second native synchronization deadline, then uses existing now/sleep seams to test the unchanged 50 ms proof decision. |

The new native fixture key is `MCPHUB_CANONICAL_NATIVE_ENTRY_FIXTURE`. Commands and preflight keep `MCPHUB_CANONICAL_ENTRY_FIXTURE`. Both existing required flags must equal `1` in acceptance. The native cases keep their explicit state-tag check; the command fixture keeps pathname and actual file-object equality with `os.Executable()`.

The Job TestMain addition is exactly the existing 1,943-byte seam: one constant and one function. The resulting file equals known Source3ebe/727 Git blob `b079fc962fae048d8e6e72234f3247bcfc019550`. No other change from that source is copied.

The proof-window v2 resolves the root review of v1: native accounting cannot consume model time before the first delayed observation. One common native deadline covers signal and accounting polling; it is not five seconds per poll. Accounting is not mocked. The fast branch still performs and requires an actual native handle wait. The independent positive settled-exit test, production 50 ms constant, original 80 ms test context, and 10/100 ms modeled delays remain unchanged. This fixture tests the proof decision and makes no native exit latency claim.

## Files and reconstruction

- `successor.patch`: complete seven-file patch with full Git before/after blobs.
- `patches/strict-proof-window-v2.patch`: focused deterministic fixture change.
- `patches/full-suite-entry.patch`: focused existing TestMain seam.
- `afterimages/`: all seven complete proposed test files.
- `evidence.json`: exact before/after hashes, expected whole tree, scope, and unperformed checks.
- `artifact-manifest.json`: SHA-256 and byte size for every payload.
- `.gitattributes`: preserves payload bytes.

Pure DATA replay reconstructs all seven afterimages exactly, and the full source map reconstructs both base and proposed trees. This is byte-integrity evidence, not Go execution.

## Required acceptance

The exact required command contracts are:

```text
go build ./... && go vet ./... && go test -count=1 -timeout 5m ./...
go test -tags=test_state_path_env -count=1 -timeout 5m ./internal/api/ ./internal/cli/
```

The future route must preserve each incoming executable's bytes, original arguments, and complete root inventory. In each mode it must receive all 14 migration roots, both native canonical roots, the original three API stderr roots, and the three extra CLI stderr roots as executed acceptance roots. No subset run or SKIP substitutes for these obligations.

The runtime and Product inputs must bind the actual successor commit and tree. The earlier exact33 producer plan remains frozen and does not qualify this new source. Source33 uses its own consumer contracts; Source727 private migration images, DLL schema, and external canonical controller are not silently inherited.

The historical Source727 full FAIL remains unchanged. Any future PASS belongs only to the actual successor revision executed. No test, build, model, publication, source write, or Git mutation occurred while preparing this capsule.

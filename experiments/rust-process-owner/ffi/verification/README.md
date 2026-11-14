# Recoverable verification evidence

This directory is part of PR #631, not a reference to an unversioned PC folder.
The implementation is in the PR commits; the historical logs below preserve
successful checks, failed/negative controls, commands and source/artifact hashes.
They are historical evidence, **not new test runs or independent certification**.

## Contents

- [Historical index](history/index.json): original byte hashes, published-content
  hashes, associated completed source commits and exact record names.
- [Standard streams](history/stdio.log), [explicit native launch context](history/launch-context.log),
  [creator crash](history/owner-crash.log), [native failure boundaries](history/native-failures.log).
- [First Go/Rust ABI](history/go-ffi.log), [checked admission](history/checked-admission.log).

The first four bundles qualify the unchanged native dependency in PR #625;
keeping its evidence here does not add another implementation or change its branch.
Within a bundle, RED and intentionally mutated runs can precede the associated
completed commit. They must not be reported as tests of the final head or as
unrepaired product failures. See the per-stage RESULTS and command/log records.

Text logs were imported without editing conclusions: line endings normalized,
trailing spaces/tabs removed,
the task's absolute lab prefix replaced with `<LAB>`, and personal user-profile
paths replaced with `<USERPROFILE>`. `original_sha256` authenticates the original
bytes, while `sanitized_sha256` authenticates the published record content.
No token, secret environment dump, binary, compiler cache or transient fixture
was published. An artifact hash is provenance, not a downloadable executable.
Rebuild binaries from the exact commits using the instructions in [../README.md](../README.md).

Only the immutable text records are durable evidence. Temporary DLLs used for
fault tests are reproducible from the documented mutations and are intentionally
not uploaded. Tests which require another platform or actual installation remain
unperformed unless a record explicitly says otherwise. Source hashes and PASS
logs are not substitutes for a threat model, native-platform parity or review.

New work must commit its reproducible commands, results (including failures),
source identity and limitations in this directory or the owning PR before being
reported as complete. A local machine is disposable; no required recovery step
may depend solely on an uncommitted file, cache or session transcript.
## Current explicit-environment increment

[Results and limits](environment-2026-11-14/RESULTS.md),
[successful full run](runs/environment-2026-11-14-published/COMPLETED.txt),
[failed first run](runs/environment-2026-11-14/steps.json), and
[self-contained offline runner](run.ps1) are versioned here.

Historical sanitized-content hashes apply to the normalized record text before
the bundle's separator newline; the bundle SHA256 covers all published bytes.

## Explicit standard-stream calling boundary

[Results and remaining limits](stdio-2026-11-14/RESULTS.md),
[full final run](runs/stdio-2026-11-14-final/COMPLETED.txt),
[exact source hashes](runs/stdio-2026-11-14-final/source-hashes.json),
[negative mutations and commands](stdio-2026-11-14/MUTATIONS.md) and raw logs
are versioned. File/pipe transfer is not a complete cancellable I/O pump.
## Context-aware observation and pipe completion

[Contract, results and self-review](cancellation-2026-11-14/RESULTS.md),
[negative-control design](cancellation-2026-11-14/MUTATIONS.md),
[focused runner](cancellation-2026-11-14/probe.ps1),
[executable mutation runner](cancellation-2026-11-14/mutations.ps1), and
[complete implementation-run evidence](runs/cancellation-2026-11-14-implementation/steps.json)
are committed here. Canceling observation does not grant permission to stop the
operation or close its streams. Native I/O qualification preserves the initially
failed assumptions and their corrected tests, not just the final PASS.
The [final cancellation run](runs/cancellation-2026-11-14-final/COMPLETED.txt)
was repeated after implementation commit `e973ee2`; code/runner hashes match.
Both complete runs and both negative-control passes are retained with exact
source identities. The later negative script improves reproducibility, not the
production implementation or its recorded successful outcomes.
## Actual native cleanup failures across FFI

[Results and self-review](native-cleanup-2026-11-14/RESULTS.md),
[method, boundaries and exact mutations](native-cleanup-2026-11-14/README.md),
[copy-only runner](native-cleanup-2026-11-14/run.ps1),
[final native qualification](runs/native-cleanup-final/steps.json), and
[full ordinary regression run](runs/native-cleanup-ordinary-final/steps.json)
are versioned here. The runtime did not need a fix: the new result is a tested
end-to-end path for selected real Windows failures. Negative controls and the
initial preparation failure remain visible, and ordinary artifacts are checked
not to contain the fixture-only handle accessor.

The [post-rebuild artifact readback](native-cleanup-2026-11-14/ARTIFACT_READBACK.md)
keeps per-run DLL identities distinct across the later ordinary rebuild and
records fresh checks of the current binaries. Old hashes are not overwritten
and binary-reproducible builds are not implied.

## Native loader refusal and module-reference qualification

[Loader report/reproduction](loader-2026-11-14/RESULTS.md),
[copy-only negative runner](loader-2026-11-14/run.ps1),
[final loader steps](runs/loader-final/steps.json), and
[ordinary regression steps](runs/loader-ordinary-final/steps.json) are retained.
The runtime needed no change. Initial oracle/mutation-construction failures,
three semantically failing negative controls, input hashes and grouped raw logs
remain visible. Per-run binary identity is not confused with a later rebuild.

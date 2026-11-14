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

# Serena migration qualification on Windows

The dedicated source-issued publisher builder is documented in
[`tools/test-serena-publisher`](../../tools/test-serena-publisher/README.md).
Its explicit role is separate from the ordinary runtime Product.

## Private migration lanes in the full Go gates

The private migration receiver in `internal/cli` has a different publisher
contract from the dedicated two-tag qualification image above. Its compiler
inputs come from the existing `build.ps1` entry:

```text
build.ps1 -PrepareCLIPrivatePreparationInputs -PrivateCLIPreparationLane <default|test_state_path_env> -WindowsTargetArch amd64 -OutputDirectory <fresh-absolute-owned-directory> -ReleaseVersion <version> -ReleaseCommit <full-Source-commit> -ReleaseBuildDate <UTC-seconds> -SourceSnapshotPath <issued-source-snapshot>
```

The command receives an actual Source snapshot and rechecks that Source after
resource generation. It emits `cli-publisher-test-inputs.json` with schema
`windows-cli-publisher-test-inputs-v1`, the generated VERSIONINFO resource and
its configuration. The explicit role is `publisher-test`, the Go entrypoint is
`mcp-local-hub/internal/cli.test`, and the executable subsystem is GUI (2).
The default lane has no build tags; the state lane has exactly
`test_state_path_env`. Both receive the original Source epoch, build tuple and
the exact compiled Windows V4 registry. `-PrintPlanJson` emits only a plan with
`prepared=false`; it creates no resource and grants no qualification.

These are compiler inputs, not an admitted executable or native ownership
receipt. The reviewed full-gate issuer must own the subsequent real Go test
compilation, preserve the exact generated test main and incoming binary, and
receive their closure. It must then obtain the existing publisher, namespace
and original process admissions for that Source. The standalone
`serena-qualification` role and its two build tags do not satisfy this lane.

The migration cases also need a separate, real runtime Product built from the
same final Source. Its supported current build is:

```text
build.ps1 -CurrentWindowsPackage -CurrentStateIsolatedTestProduct -WindowsTargetArch amd64 -OutputDirectory <fresh-absolute-owned-directory> -ReleaseVersion <version> -ReleaseCommit <full-Source-commit> -ReleaseBuildDate <UTC-seconds> -SourceSnapshotPath <issued-source-snapshot> -RuntimeArchiveDirectory <existing-pinned-runtime-archives>
```

This mode uses the current V4 staging and validation path. It compiles the
actual `cmd/mcphub-launcher` and `cmd/mcphub` with `test_state_path_env`, retains
their distinct launcher/Product identities, and validates the nested V2
identity plus runtime payload index. Only after the complete build observation
does it emit `private-serena-runtime-product.json`. That input contains the
actual `entry_set` hashes and `runtime_payload_sha256`; no legacy runtime DLL
is part of this contract. The consumer independently checks the actual Go
entrypoint/tag, current Source/runtime relationship and the held original V4
cohort. An environment marker or this JSON alone cannot provide that admission.

Both modes require fresh outputs outside Source and refuse ambient Go compiler
overrides other than `-mod=readonly`. Both test-only Product builders require
fresh outputs and refuse pre-existing generated resources. A compiler failure
keeps their original resources in place. After successful compilation, the
existing exact-path ownership checks move those inputs to
`retained-build-inputs`, outside trash; only complete Source/admission/fixture
success moves that directory to the output's `trash`. Failed private compiler
input generation also retains its output residue for the original owner's receiving.
These modes neither install a Product nor execute migration cases. They do
not change the mandatory literal Go gates or turn missing prerequisites into
skipped tests.

## Run the real integration scenarios

Use a separate runner-owned private directory for the test's temporary and
state roots. Before launching the test process, set:

- `MCPHUB_SERENA_QUALIFICATION_RUN_ROOT` to that absolute directory;
- `TEMP` and `TMP` to an existing child directory beneath it;
- `MCPHUB_SERENA_QUALIFICATION_PRODUCT` to a separately admitted ordinary
  Product and `MCPHUB_SERENA_QUALIFICATION_PRODUCT_SHA256` to its exact hash;
- a minimal, existing `PATH` with the intended compatible `uv` and Python.

Never supply this qualification image as the Product. The fixture checks
both roles independently. The existing positive migration tests replace only
the old example package source with the pinned source from the embedded
catalog, retaining their registry, fan-out, unchanged-manifest and commit
outcome assertions. They use real preparation and an unpublished local
Serena MCP (Model Context Protocol) host; launching them authorizes real
pinned-package acquisition and child processes in the owned directory.

Without the explicit qualification build and namespace, those positive
scenarios fail with `SERENA_QUALIFICATION_REQUIRED`; they do not skip or
fabricate source, namespace or package receipts. Other migration tests retain
their existing ordinary fixtures. A qualified subset is not a claim that
the ordinary full repository test suite passed.

The private namespace receipt has its own observation kind. Its validation
binds the real directory identity, owner/DACL, current security identifier
(SID), executing image, source epoch and scope lifetime. It does not claim
a Windows Known Folder call or `CoTaskMemFree` observation.

The production Serena work limit remains 300 seconds per preparation plus
45 seconds of bounded cleanup. A runner may need a longer aggregate suite
deadline for multiple independent cases; it must report that deadline and
actual covered tests, and must observe child cleanup before reporting success.

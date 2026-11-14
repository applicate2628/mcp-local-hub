# Serena migration qualification on Windows

The dedicated source-issued publisher builder is documented in
[`tools/test-serena-publisher`](../../tools/test-serena-publisher/README.md).
Its explicit role is separate from the ordinary runtime Product.

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
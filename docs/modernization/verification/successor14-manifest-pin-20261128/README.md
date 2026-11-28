# Minimal source pin for the fourteen migration roots

## Scope

This candidate changes exactly two literals in one test file on `04e5b4eadd6407cbff4e04ba453546d278314bd7`. It is not applied or executed by this review. The expected whole Git tree after application is `bcf1bd3539a8ba5ab54cff123adbb4bc071276d3`.

The replacement is copied from this revision's own `servers/serena/manifest.yaml`: `git+https://github.com/oraios/serena@5e4f5cd173c7ff3eed37ee3f1de6c266b937c5dc`. The official file is independently bound by Git blob and SHA-256 in `evidence.json`. No production predicate, dependency version, test selection, timeout, or preparation implementation changes.

## Concrete defect

Both `legacy2DaemonManifestYAML` (line 402) and `alreadyMigratedManifestYAML` (line 429) currently use an unversioned example repository. The positive migration tests invoke the real default dependency assembly. The path is:

1. `runMigrateSerenaDynamicPool` selects `defaultMigrateSerenaRunDeps`.
2. `BuildInMemorySerenaDynamicPoolManifest` copies the seeded `BaseArgs` unchanged.
3. `PrepareSerenaParsedInstall` invokes the original preparation assembly before supervisor exclusion and intent installation.
4. `NewSerenaPreparationEnvironmentSource` requires an HTTPS repository plus a lowercase 40-hex commit (`serena_build_environment_source.go:71–85`).

The unversioned example is refused before the intended positive migration behavior. External environment values cannot replace these two literal manifest inputs.

## What the two-line change establishes

The two source requirement strings now satisfy that input shape and name the same pinned upstream revision as the current catalog. All fourteen mandatory roots remain part of the ordinary default and tagged inventories. Their fixture, real install path, assertions, source bytes on disk and lifecycle stubs are otherwise unchanged.

This is the smallest demonstrated correction to this source-input defect. It is **not a claim that the full suite will now pass**.

## Remaining real preparation dependencies

The production pipeline still requires a marked current publisher image. `internal/buildinfo.CurrentPublisherSource` receives the final Source epoch through compiled metadata; `qualifyPublisherImage` checks that epoch, version, commit and date against the running image's actual PE resource. A test-only state Product deliberately has no publisher epoch and alone cannot supply this role.

The current preparation policy requires UV at least 0.13.4 and an already available Python at least 3.11. Interpreter discovery is offline and Python downloads remain disabled. Bootstrap declarations are exactly SYSTEMROOT, WINDIR, COMSPEC and PATH; their ordinary file/directory identities are received.

Every preparation owns its own attempt and generation. `BuildSerenaPreparationInputs` sets the probe cache to its acquired attempt/cache. `installPreparedSerena` separately sets UV_CACHE_DIR to the installation attempt/cache. Consequently, setting an ambient cache variable or citing a closed historical cache does not populate the current operation's owned cache. Reusing any seed requires a supported original-owner operation with truthful new identities; this candidate does not add such an operation.

The installed package, UV receipt, interpreter/launcher provenance, actual listener and MCP protocol qualification, original native waits and IO closure remain required. No old Source3ebe or Source727 image/receipt can be relabelled as this Source.

The original Go package budget remains five minutes. This review has not measured the aggregate cost of the real preparation calls. The caller-only dependency seam in this file returns no prepared authority; it cannot replace a successful real install and is not used by this change.

## Fixture roles

Thirteen roots keep ordinary runtime Product selection. `TestMigrateSerena_CallsInstallParsedManifest_NotDiskWrite` explicitly selects its test-owned inert canonical path using the existing API test seam. This choice remains unchanged; publisher qualification still concerns the actual running test image. Exact per-root fixture calls and source lines are in `evidence.json`.

## Verification and files

Pure DATA checks verified the exact base blob, two replacements, full afterimage and reconstructed whole tree. The patch is 710 bytes and contains two one-line changes. No Go compilation, native test, model, network package preparation, source mutation or Git operation was performed by this review.

The required commands remain:

```text
go build ./... && go vet ./... && go test -count=1 -timeout 5m ./...
go test -tags=test_state_path_env -count=1 -timeout 5m ./internal/api/ ./internal/cli/
```

The original fourteen tests must execute and pass in both modes. Their names and mandatory status are preserved even if later native receiving exposes another failure.

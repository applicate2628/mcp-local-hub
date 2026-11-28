# Bounded Serena preparation/cache review

## Scope and result

This was a source-only review of Source74e, commit 74e7563db244ede5984834233ce8d59072e92c0a, tree bcf1bd3539a8ba5ab54cff123adbb4bc071276d3. It ran no product, test, Git command or cache consumer and read no active cache payload. Its conclusions apply to the inspected production call chain, not an exhaustive search of the entire repository.

The inspected implementation reuses an already qualified Ready package under its original namespace and request identity. Fresh preparation creates owned probe/install caches and overwrites UV_CACHE_DIR. No general seed/copy callback was found in the completely read acquisition/install chain. An ambient UV cache directory is therefore not a demonstrated handoff interface for these preparations.

## Source evidence

**Table 1. Primary source locations in the reviewed Source74e.**

| File and lines | Actual operation | Consequence |
|---|---|---|
| internal/api/serena_preparation_assembly.go, 91–117 | PrepareInputs, composition, original owner Prepare | Live preparation uses the actual owner/effects path. |
| internal/cli/serena_preparation_assembly.go, 35 and 78 | Fresh IDs and BuildSerenaPreparationInputs | This assembly supplies no external UV seed-copy callback. |
| internal/api/serena_prepared_inputs.go, 163–177 | Fresh owned probe tree; overwrite UV_CACHE_DIR and TMP/TEMP | Inherited probe cache settings are replaced. |
| internal/api/serena_prepared_acquisition.go, 52, 65 and 103–114 | Fresh generation/attempt, private install effect | Install receives the owned attempt without a general cache-copy parameter. |
| internal/api/serena_prepared_install_effects.go, 35–55 | Owned attempt cache, actual UV invocation | Install also replaces the inherited cache setting. |
| internal/api/serena_build_environment.go, 280 and 290 | Refuse unqualified artifact-source UV declarations and unsupported UV/PIP/GIT/PYTHON overrides | Ambient cache forwarding is not an admitted substitute. |
| internal/api/serena_prepared_runtime.go, 222 and 369–393 | Request identity, prior Ready validation, reuse when admissible and no expected prior-record hash is supplied | Reuse is tied to matching qualified state and its original physical/publication requirements. |
| internal/api/serena_prepared_runtime.go, 338 | Require PublicationAvailable | A matching-looking JSON document is insufficient. |
| internal/api/serena_prepared_consumer.go, 51–129 | Observed original consumer/publication and qualified cold recovery | Cold recovery is an owner/namespace observation, not arbitrary cache import. |
| internal/cli/migrate_serena_test.go, approximately 242–274 | Fresh fixture temporary/state roots | Copying old Ready JSON into a fresh root does not preserve namespace/file identity/publication observations. |

The request identity includes package, declared build environment, source commit, UV/interpreter/launcher/ABI/policy/build-setting identities. Operation/generation IDs are not the only reuse constraint: physical namespace and publication observations remain necessary.

## Deadline and qualification limits

This source review proves neither that real preparation fits the unchanged five-minute package deadline nor that it cannot. Go cache warming and a qualified Serena Ready package are distinct inputs. Their effects require the relevant original native run.

Source727 API timings and Source04e warm compilations are different measurements. The warm producer did not execute fourteen migration roots. This review grants no private Source727 migration protocol, mock preparation, copied Ready document or ambient cache override.

The inspected negative test TestMigrateSerenaPreparationFailurePrecedesReapAndRetainsRegistry has not been identified by historical primary evidence as the user's originally remaining expected-refusal control. It must not silently replace that unresolved control, nor may a later DWORD or namespace refusal replace it.

## Read coverage

Complete reads covered build-environment source, prepared inputs, install effects, acquisition/effects/consumer, API/CLI assemblies, install materialization, CLI preparation environment and the small migration-preparation fixture. Bounded reads covered runtime lines 1–500, build-environment lines 101 onward and migration fixture/environment sections. Broader searches were bounded or unavailable; no repository-wide absence claim is derived from them.

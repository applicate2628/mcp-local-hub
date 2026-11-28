# B: original failed-owner physical receiving — 26 November 2026

**Both negative fixtures passed their separate physical receiving check. The original qualification remains FAIL.** The `run-return-v1` and `stdout-reader-v1` runs each used one original controller invocation. Their original command receipts remain `FAIL`, `quiescence_state=unknown`, exit code `1`, and `captures_measured=false`. The runtime adapter remains disabled.

## Contents

1. [Scope and source](#scope-and-source)
2. [Observed results](#observed-results)
3. [Ownership and gates](#ownership-and-gates)
4. [Evidence and exact pins](#evidence-and-exact-pins)
5. [Remaining qualification](#remaining-qualification)
6. [References](#references)

## Scope and source

This is a metadata-only account of two negative native Windows probes for `TestCanonicalSuccessorNativeControlRetainsOriginalFiles`. It reports physical receipt of the original process, observed process chain, native handles and readers after the original failure. It grants no access to failed captures, source or caches and does not permit further commands through the failed invocation.

The exact private Source is `e66f5c6fab7da2a2eff16ff1f0891991ca1a839c`, tree `4df69e99dca3f808b26e6a5bf7721bdd0ba3a91c`, represented by the existing composition in PR #664 [1, 2]. These runs precede the separately prepared Abort correction. No claim about a later Source is transferred.

The two Products have distinct roles. Both were produced by the existing builder from this Source, with the `test_state_path_env` tag and test-only marker:

**Table 1. Exact Product roles.**

| Role | Existing builder flags | Product SHA-256 |
|---|---|---|
| P1: native test Product | `-TestStateIsolatedProduct` | `7f4ca997f14e10dbe2dc0280f23f18cc54bbe79aacf43e9e097fd48e7a83cc6c` |
| P2: canonical command test Product | `-TestStateIsolatedProduct -CanonicalEntryCommandTestProduct` | `808561f5adffc6778aeee3d8f8de1f784a125261188f7c6e93e856de4e09c951` |

The native fixture, command fixture, canonical entry images, identities, same-build identifiers and builder hash are recorded in [evidence.json](evidence.json). These test Products do not establish ordinary installable Product qualification.

## Observed results

**Table 2. Negative fixtures and preserved verdicts.**

| Observation | run-return-v1 | stdout-reader-v1 |
|---|---:|---:|
| Invocation count | 1 | 1 |
| Fixture result | PASS | PASS |
| Parent duration, seconds | 120.21119070000714 | 124.98160389997065 |
| Parent exit code | 0 | 0 |
| Parent original terminal exit received | true | true |
| Parent capture handles closed | true | true |
| Parent diagnostic deadline exceeded | false | false |
| Original command result / exit | FAIL / 1 | FAIL / 1 |
| Original command quiescence state | unknown | unknown |
| Original captures measured | false | false |
| Separate physical receipt | CLOSED | CLOSED |
| Separate physical receiving time, seconds | 0.016587299993 | 0.019233200001 |
| Outer original native members exited / active count | true / 0 | true / 0 |
| Outer native handles closed | true | true |
| Attempted native cases | 1 | 1 |
| Observed native event count | 23,722 | 16,002 |
| Qualification passed | false | false |

The native result returned during late receiving retains its own original verdict. In `run-return-v1`, its `NativeComplete`, `IOComplete` and `Complete` fields are true, `Unknown` is false, and its functional result remains `exit status 1`. The run-return gate held return after that native physical work completed.

In `stdout-reader-v1`, the returned native result preserves `NativeComplete=false`, `IOComplete=false`, `Complete=false`, `Unknown=true`, and the original failure:

> context deadline exceeded  
> exit status 87  
> owned capture worker did not join by original cleanup deadline

The separate physical report observes that the same stdout and stderr workers subsequently returned with EOF and their original reader/writer handles closed. It does **not** rewrite the frozen native verdict. EOF means the original reader reached the end of its stream.

For both runs, the actual returned metadata records no pending debug event, closed image pins and creating-thread handle, known original root wait, and closed outer standard-stream endpoints. Each original control channel joined and stopped. The full exact booleans, stream byte counts and metadata hashes are in [evidence.json](evidence.json).

The event counts characterize the fixed-source observed debug process chain. They are not evidence of universal containment of every possible descendant or launch mechanism.

## Ownership and gates

The outer wrapper retained the original `System.Diagnostics.Process` created by its own Start call through its WaitForExit result. The inner parent retained the original Python process and its own capture handles. The original controller retained its original case owner. A process identifier was not used to reconstruct or replace ownership.

Each test gate was bound before native allocation. The original controller alone released it, after successfully publishing the initial failure. The Python parent did not release it. Each gate was reached, released and passed exactly once:

**Table 3. Gate observations in UTC.**

| Mode | Reached | Released | Passed |
|---|---|---|---|
| run-return | 13:57:43.5581778 | 13:57:55.7328728 | 13:57:55.7334070 |
| stdout-reader | 14:01:11.7005767 | 14:01:22.2014478 | 14:01:22.2014478 |

The receiver uses the same prebound one-shot original metadata reader for normal and failed receiving. A first metadata read or close error remains a failure; a later reopen or repeated close cannot replace its outcome. No such error occurred in these two probes.

The second probe was started only after the first probe's complete metadata showed original physical closure and no stop condition. All original qualification deadlines and negative verdicts were preserved. The original records and prior V1/FAIL/UNKNOWN evidence remain in place.

## Evidence and exact pins

[evidence.json](evidence.json) is the sanitized machine-readable companion. It includes:

- exact Source commit/tree and distinct P1/P2 build artifacts;
- launcher, runner, guard, disabled adapter, receiving helper, controller, plan, interpreter and binding hashes;
- the original command and controller verdicts, separate physical closure facts and gate timestamps;
- complete SHA-256 hashes and byte lengths for nine original metadata records per mode.

The logical evidence names identify original metadata such as `run/command-receipt.json`, `controller/result.json` and `controller/failed-physical-closure.json` within each mode. They are references, not copies of native files in this public package. The full metadata records remain private. Native paths, user names, process identifiers and raw capture content are omitted.

Failed raw captures were neither read nor rehashed for this account. Stream byte counts come from the original control metadata.

## Remaining qualification

The unchanged native happy qualification remains **FAIL**. Its Abort failure and separate BindControl status timeout require independent verification on the resulting changed Source. The accepted-proof Abort candidate was prepared separately; these two negative probes do not test it.

The following remain outside this result:

- native first-read failure after closure, frozen-outer HOLD and first-close refusal;
- admission of a complete current runtime composition; `REVIEW_COMPLETE=False` remains in the runtime adapter;
- ordinary Product/Serena qualification for changed production inputs;
- both mandatory full commands, still **NOT_RUN** on this Source:

```text
go test -count=1 -timeout 5m ./...
go test -tags=test_state_path_env -count=1 -timeout 5m ./internal/api/ ./internal/cli/
```

The original owner, process-tree and reader closure facts in this report are narrower than those remaining gates. Neither historical FAIL/UNKNOWN nor the existing overall acceptance result is reclassified.

## References

1. [Existing composition PR #664](https://github.com/applicate2628/mcp-local-hub/pull/664).
2. [Exact e66 Source composition and prior verification](https://github.com/applicate2628/mcp-local-hub/blob/10a68908390128a7df9e61193c89bac106661506/docs/modernization/verification/composed-functional-source-20261126/README.md).
3. [Previously reviewed failed-owner receiving candidate](https://github.com/applicate2628/mcp-local-hub/blob/8b1d27290812717736691fc9745e0da9509da9f6/docs/modernization/verification/failed-owner-receiving-20261126/README.md).
4. [Sanitized exact metadata and pins for these two probes](evidence.json).


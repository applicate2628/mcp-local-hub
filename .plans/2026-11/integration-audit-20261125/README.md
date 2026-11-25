# Reproducible integration source audit — 2026-11-25

## Contents

1. [Result and boundary](#result-and-boundary)
2. [Pinned inputs and owner mapping](#pinned-inputs-and-owner-mapping)
3. [Run the source replay](#run-the-source-replay)
4. [Recorded conflict decisions](#recorded-conflict-decisions)
5. [Test-only follow-ups and negative control](#test-only-follow-ups-and-negative-control)
6. [Product qualification and native gates](#product-qualification-and-native-gates)
7. [Evidence contents and remaining work](#evidence-contents-and-remaining-work)

## Result and boundary

The recorded recipe reproduced **all 38 intermediate source trees** and the final tree exactly. The machine-readable result is [verification.json](verification.json), bound to the SHA-256 digest of [composition.json](composition.json) and the executed [replay.py](replay.py). This is an observed source replay PASS. Compilation, native behavior, Windows Product qualification, complete test gates and PR readiness have separate evidence and acceptance requirements.

The final source checkpoint is commit **`c730cae455ef84d226ac5323953d9e7209b4016e`**, tree **`0c42ea39f241077cb6a9e1a64efe6d1e0a726751`**. Stage 38 adds only the four-file test fixture delta to `14af01e`; the first 37 steps and their expected trees are unchanged. The recipe reproduces the Git tree: file bytes, paths and modes. It records the historical composition commit IDs for traceability but does not recreate their author/time metadata or require those private aggregate commit objects. It creates no commits or refs and does not merge a PR.

**Table 1. Final source checkpoints.** Full IDs are in the composition ledger.

| Checkpoint | Historical commit | Tree |
|---|---|---|
| Public and owner deltas plus API helper overlay | `2c71dd70fba1` | `f500c523b030c6aa7739446973615cbbd539e4f7` |
| Native Go formatting of one test struct | `c2901b4a006e` | `6081e50fc5f3c33c27b223cfd68e2d1dbe404356` |
| Test-image startup deadline adjustment | `14af01ea0606` | `386923930b874526a0e6896bde1857c4ecc95d63` |
| Original-owner cleanup and explicit test fixtures | `c730cae455ef` | `0c42ea39f241077cb6a9e1a64efe6d1e0a726751` |

## Pinned inputs and owner mapping

The starting tree is captured PR **#671**, commit `09dc07e01d1a3236ab5f45f384e11fe2f9dfd30d`. The following 26 captured public deltas are applied in this exact order:

```text
668, 669, 632, 635, 643, 649, 637, 646, 647, 650, 653, 654, 655,
628, 660, 659, 663, 657, 661, 664, 665, 666, 667, 624, 626, 629
```

Every step records its complete source base/head IDs, selected paths, prior tree, expected result tree and any resolution. These are explicit two-commit deltas; the recipe does not infer the audited base from branch ancestry. The #664 input is the original captured `d07f77b...` source, not a later audit publication. Roadmap #623 and Rust experiments #625/#627/#631/#644 remain separate and are excluded.

The seven reviewed owner inputs are mapped to their exact owner commits in [owner-inputs.json](owner-inputs.json). All **89 owner/path inputs** were checked against the original frozen code SHA-256 values and their Git blobs. Those seven commits have been published to their existing open draft PRs: the coordinating reviewer's exact head readback is recorded under `owner_publication_readback` in [composition.json](composition.json), and all seven heads match the frozen pins. The original `publication_state_at_capture` fields remain historical capture metadata. Later authorized owner HEAD advancement did not replace the frozen source pins. Owner documentation is excluded from the replay; the selected Windows npm README is part of the reviewed packaging input.

**Table 2. Owner commit mapping.** Abbreviations below are display labels; the JSON retains complete IDs and all file hashes/modes.

| Owner area | Existing PR | Captured base | Published input commit | Selected source paths |
|---|---|---|---|---|
| cmake | #624 | `ee20f6009a85` | `5c36f626467b` | 2 |
| relay | #629 | `e33924ac9744` | `6808e99bacc0` | 3 |
| serena | #655 | `1a5a7dd04fe7` | `6743f3d55202` | 5 |
| scheduler | #660 | `892fa82e0e1a` | `8d4f2606c9af` | 29 |
| process | #628 | `e7fee02b5ce1` | `0b3a9e0edafc` | 1 |
| windows | #671 | `09dc07e01d1a` | `891ef8bfd076` | 27 |
| force_stop | #659 | `a3568dfe3426` | `9043acae5a77` | 22 |

The first five owners are followed by the Windows delta, its separately recorded one-file npm expectation correction, and force-stop. The Windows split preserves the original intermediate tree sequence while using one published Windows owner commit. The API helper overlay and the three native follow-ups are then applied as the separately hashed text patches in Table 1. The replay refuses an unavailable input instead of silently using a branch tip. It does not fetch or independently query current PR state.

## Run the source replay

Use Python 3.9 or later and Git with the pinned input objects available in an existing local repository. The script performs no network access. The `required_input_commits` list identifies exactly which public and owner commits must be available; a shallow checkout may need additional pinned base objects. Original public binary evidence is read from those Git objects and is not duplicated in this evidence directory.

Choose a new replay directory **outside the source worktree and this evidence directory**. It must not already exist.

```text
python path/to/integration-audit-20261125/replay.py --repository path/to/existing/repository --work-dir path/to/new-replay-directory
```

The script writes a private Git index, immutable Git objects and a verification report. It leaves the repository's ordinary index, tracked working files and refs unchanged. It does not execute Go, a helper process, a test Product, an installer, or a remote command. Its temporary index is retained for inspection; disposal belongs to the caller's normal owned-work cleanup policy.

For each step, the script verifies the preceding tree, computes the original selected base-to-head delta, and applies it to the private index. It then supplies only the explicitly recorded conflict snapshots and reviewed post-merge adjustments. Unrecorded conflicts, changed artifact hashes, missing source commits and unexpected trees stop replay. Every resulting tree must match its full expected ID before the next step begins. The final report distinguishes source replay from runtime qualification.

Historical aggregate composition commits are reference labels, not replay inputs. A reviewer can therefore reconstruct the exact source tree from the public/owner inputs and this directory without importing the private native-transfer Git pack.

## Recorded conflict decisions

There are **14 conflict file/step pairs**, represented by exact UTF-8 source bytes, mode, Git blob ID and SHA-256 digest in `resolutions/`. Their combined raw source size is 640,877 bytes. The original binaries and evidence archives are not embedded in these snapshots.

**Table 3. Preserved contracts at explicit conflict points.** Exact paths and the source snapshots are indexed under each ledger step.

| Step | Files or boundary | Resolution retained |
|---|---|---|
| Public #647 | Serena build environment | #643 owned UV build constraints plus Windows PATHEXT/executable-only and Git long-path policy |
| Public #653 | Serena TCP observation | #637 qualification/stream custody decomposition plus remaining millisecond budget in both observation entrypoints |
| Public #659 | API TestMain | Private caller-start-time oracle plus port-probe fixture; obsolete directory-based oracle authority stays removed |
| Public #657 | `build.ps1` | #671 V4 runtime/source admission plus canonical command test Product variables and separate Product roles |
| Public #665 | API oracle and TestMain | Exact role and fixed delay, private stdout result, and mixed-role checks before oracle execution |
| Public #666 | CLI secrets, TestMain and helper classifier | Original fixture identities and fixed values, exact selector, admitted state root, and other helper exit contracts |
| Public #667 | Performance-tool discovery/capture | One existing environment-capture seam and same-test-image stdout oracle, plus ordinary-tool console policy |
| Force-stop owner #659 | Supervisor spawn/creator records and controller | One existing creator/route-aggregate owner, both event cases, retained route fields, and the original terminal operation |

Four additional post-merge patches preserve changes which did not all produce conflict markers: #637 removes two extra blank EOF lines; #664 removes one historical JSON blank EOF line; #665 aligns dispatcher tests with the retained closed oracle contract; #666 adapts the end-to-end secrets fixture and rejects the superseded arbitrary-path/value envelope. These patches cover seven paths, total 16,133 bytes. They are independently hashed and applied only after their exact recorded preceding tree.

Other automatic overlaps remain visible in the pinned selected-path sequence and per-step tree checks. These include the Scheduler unknown-mutation rollback guard, original Windows V4 transaction, Serena cache hook, Windows Status hook and force-stop TestMain seal. Whole unrelated owner files are not substituted during ordinary three-way application.

## Test-only follow-ups and negative control

The [API helper admission overlay](patches/api-port-helper-admission.patch) changes four test files. The copied netstat/wmic fixture runs only after both pure role classifiers; the shared dispatcher counts its marker as a distinct role. The unexpected-WMIC call witness remains. The overlay preserves the composed API oracle and the force-stop seam's capture, default seal and restoration.

The [native formatting patch](patches/native-gofmt.patch) changes only four struct field alignments. The [test fixture margin patch](patches/native-test-fixture-margin.patch) changes only the copied-image child deadline from 10 to 30 seconds after the initial native attempt exceeded 10 seconds. Failure-on-timeout and all exit/output/call/side-effect assertions remain; `WaitDelay` remains one second. This changes no production timeout. The six sequential cases do not each receive a guaranteed 30 seconds within a 90-second package budget; runtime evidence must state the actual budget and duration without claiming spare capacity.

The stage-38 [gate fixture patch](patches/native-gate-fixtures.patch) changes four test files. Legacy Windows namespace fixtures explicitly set the current token user as owner of newly created objects, read the owner back through the same handle, and verify the access-control list remains unchanged. The CLI redaction fixture keeps the real planner with automatic port allocation; the namespace-refusal fixture uses the existing plan-builder seam so unrelated host socket occupancy cannot preempt its assertion. Recovery cleanup releases the original audit flock and joins the original `Execute` call before temporary-directory cleanup, including a `Goexit` path with no returned result. Sonnet closes the original server on every test exit and observes its error. Native `gofmt` returned all four files byte-for-byte unchanged. This stage changes no production code or assertion deadline.

The [negative-control patch](negative-controls/api-port-helper-admission-negative.patch) is separately labelled and **never applied by replay.py**. It restores only the two pre-fix admission behaviors while retaining regression assertions and unrelated seals. It belongs only in a separate causal-control copy. Its observed native result is recorded by the native evidence owner, not inferred from its presence here.

## Product qualification and native gates

An exact source tree does not establish an admitted Windows Product, image identity, source-issued Serena qualification or native process-closure receipt. The source replay is not a substitute for `build.ps1` and the relevant fixture publishers/controllers.

Ordinary package TestMain setup creates owned roots and private configuration/IPC fixtures. That setup alone does not require a Product. Exact helper roles must be issued only for their intended child; globally injecting helper markers does not qualify a suite. In contrast, canonical command/upgrade tests require the actual admitted test Product and fixture, and prepared Serena migration binds the current source, build tags, qualification image, source epoch/receipt and a distinct runtime Product. An ordinary `go test` image or an old environment/JSON file cannot acquire those identities. Default and `test_state_path_env` configurations need their own appropriate evidence. Job-entry and recovery-namespace controllers have separate original image/root requirements and focused scope.

Before native execution, materialize the verified tree in an isolated source checkout and issue fresh qualifications for that actual source and images. Do not reuse stale Product identity from an earlier checkpoint. Consult the current repository governance and operational runbook for the supported qualified build procedure.

The canonical common gates remain separate:

```text
go build ./...
go vet ./...
go test -count=1 -timeout 5m ./...
go test -tags=test_state_path_env -count=1 -timeout 5m ./internal/api/ ./internal/cli/
```

A focused owner or API PASS does not establish these full gates. The full native test gates on `14af01e` were **FAIL**; the additional fixture stage does not relabel those results. This source artifact set makes no full-gate PASS claim for `c730cae`. Focused validation of that checkpoint, native failures, fixed runs, causal control, qualification and common gate receipts are maintained separately in [native-evidence.md](native-evidence.md) and its JSON companion, owned by the coordinating reviewer. Those files have their own update lifecycle and are not part of the source-artifact digest manifest.

## Evidence contents and remaining work

[composition.json](composition.json) is the authoritative sequence; [owner-inputs.json](owner-inputs.json) records the seven exact owner mappings; [verification.json](verification.json) records the completed 38-tree replay; [ARTIFACTS.json](ARTIFACTS.json) lists the publication files and transport digests. Only text metadata, the replay script, explicit source resolutions and small patches are included. No private machine paths, raw apply logs, public evidence archive copies or native Git pack are included.

Git workflow/versioning was applied to pinned inputs and isolated replay; Ponytail to reuse of public objects and existing owners; Go data consistency/security to creator, private-helper and rollback boundaries; Go testing/verification to separating causal controls, source replay, focused tests and complete native acceptance.

Remaining work: record focused validation and resolve the failed full-gate obligations against the final source; complete the required fresh Product/fixture qualifications; review and publish the test fixture follow-up and this evidence through existing #664. The seven owner input commits are already published; their publication does not establish complete integration acceptance. No new aggregate PR or agent-side PR merge is part of this recipe.

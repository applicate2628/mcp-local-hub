# Branch inventory — pinned before snapshot

[Audit and decisions](RESULTS.md) · [Machine-readable records](branches-before.json)

## Contents

1. [Open pull requests](#open-pull-requests)
2. [All remote branches](#all-remote-branches)

## Open pull requests

**Table 1. Own delta relative to each actual base, not accumulated stack size.**

| PR | Base | Own files | Behind accepted master | Responsibility |
|---|---|---:|---:|---|
| [#623](https://github.com/applicate2628/mcp-local-hub/pull/623) | `master` | 3 | 53 | Own isolated review target |
| [#624](https://github.com/applicate2628/mcp-local-hub/pull/624) | `master` | 2 | 53 | Own isolated review target |
| [#625](https://github.com/applicate2628/mcp-local-hub/pull/625) | `master` | 14 | 53 | Own isolated review target |
| [#626](https://github.com/applicate2628/mcp-local-hub/pull/626) | `master` | 5 | 53 | Own isolated review target |
| [#627](https://github.com/applicate2628/mcp-local-hub/pull/627) | `master` | 7 | 53 | Own isolated review target |
| [#628](https://github.com/applicate2628/mcp-local-hub/pull/628) | `master` | 1 | 53 | Own isolated review target |
| [#629](https://github.com/applicate2628/mcp-local-hub/pull/629) | `master` | 4 | 53 | Own isolated review target |
| [#630](https://github.com/applicate2628/mcp-local-hub/pull/630) | `fix/relay-output-failure-contract-20261113` | 8 | 53 | Combine in629 after verified containment |
| [#631](https://github.com/applicate2628/mcp-local-hub/pull/631) | `spike/rx02-native-process-owner-20261112` | 619 | 53 | Own isolated review target |
| [#632](https://github.com/applicate2628/mcp-local-hub/pull/632) | `master` | 2 | 0 | Protected parallel session |
| [#633](https://github.com/applicate2628/mcp-local-hub/pull/633) | `master` | 1 | 0 | Protected parallel session |
| [#634](https://github.com/applicate2628/mcp-local-hub/pull/634) | `master` | 1 | 0 | Protected parallel session |
| [#635](https://github.com/applicate2628/mcp-local-hub/pull/635) | `fix/serena-qualified-uv-path-20261114` | 9 | 0 | Protected parallel session |
| [#636](https://github.com/applicate2628/mcp-local-hub/pull/636) | `fix/serena-qualification-publisher-20261114` | 14 | 0 | Protected parallel session |
| [#637](https://github.com/applicate2628/mcp-local-hub/pull/637) | `fix/serena-qualification-namespace-20261114` | 6 | 0 | Protected parallel session |
| [#638](https://github.com/applicate2628/mcp-local-hub/pull/638) | `master` | 2 | 0 | Protected parallel session |
| [#639](https://github.com/applicate2628/mcp-local-hub/pull/639) | `master` | 2 | 0 | Protected parallel session |
| [#640](https://github.com/applicate2628/mcp-local-hub/pull/640) | `master` | 1 | 0 | Protected parallel session |
| [#641](https://github.com/applicate2628/mcp-local-hub/pull/641) | `master` | 1 | 0 | Protected parallel session |

## All remote branches

**Table 2. All93 refs from refs-before.tsv. Behind/ahead are exact graph counts, not unimplemented-feature counts.**

| Branch | Snapshot head | Last associated PR | Ahead / behind master | Disposition |
|---|---|---:|---:|---|
| `arch/wp11a-archcheck-core` | `d689d63e676f` | #602 | 1 / 224 | Merged PR exact head, different ancestry; retain history |
| `arch/wp11a-repository-ratchet` | `5d7b9e3980da` | #612 | 10 / 223 | Closed but inclusion not proven; preserve for separate residue review |
| `chore/refresh-go-dependencies` | `af7e346f70a4` | #593 | 1 / 338 | Merged PR exact head, different ancestry; retain history |
| `chore/remove-github-ci` | `cc11d5e6d564` | #603 | 1 / 312 | Merged PR exact head, different ancestry; retain history |
| `codex/fix-hub-auto-rebind-token-leak-vulnerability` | `1d2fe917cf02` | #561 | 1 / 365 | Closed but inclusion not proven; preserve for separate residue review |
| `codex/fix-initialize-cache-poisoning-issue` | `23e39f10f06e` | #599 | 1 / 318 | Merged PR exact head, different ancestry; retain history |
| `codex/fix-npm-postinstall-vulnerability` | `0fd9ec35e281` | #585 | 1 / 340 | Closed but inclusion not proven; preserve for separate residue review |
| `codex/fix-posix-sigkill-race-condition` | `f6d9aba694a5` | #595 | 2 / 329 | Merged PR exact head, different ancestry; retain history |
| `codex/fix-provider-recovery-after-pre-install-crash` | `5c8c995c08bf` | #615 | 0 / 56 | Historical contained ref; retain, no automatic deletion |
| `codex/fix-unbounded-durable-vtune-runs` | `602f8e883d4c` | #608 | 2 / 225 | Closed but inclusion not proven; preserve for separate residue review |
| `codex/fix-vcpkg-pin-status-vulnerability` | `8dc3338f9820` | #600 | 2 / 318 | Merged PR exact head, different ancestry; retain history |
| `codex/fix-vulnerability-in-default-marker-read` | `2740cee5d5ff` | #609 | 0 / 222 | Historical contained ref; retain, no automatic deletion |
| `codex/fix-vulnerability-in-hfss/cst-output-root-validation` | `abd8ed6ecf1b` | #598 | 2 / 323 | Merged PR exact head, different ancestry; retain history |
| `codex/fix-windows-upgrade-rejection-of-prior-binary` | `9b207e8a9e91` | #601 | 2 / 313 | Merged PR exact head, different ancestry; retain history |
| `codex/propose-fix-for-attacker-controlled-output-root-vulnerabilit` | `12db7429a0c3` | #597 | 1 / 323 | Closed but inclusion not proven; preserve for separate residue review |
| `codex/propose-fix-for-respawn-handling-issue` | `f8d4b9cf91bc` | #610 | 0 / 224 | Historical contained ref; retain, no automatic deletion |
| `codex/propose-fix-for-scheduler-failure-handling` | `920f0374ec03` | #607 | 1 / 276 | Closed but inclusion not proven; preserve for separate residue review |
| `docs/canonical-modernization-roadmap` | `38e31c8d3e4b` | #605 | 10 / 284 | Merged PR exact head, different ancestry; retain history |
| `docs/rust-cross-platform-no-com-20261112` | `db7b806b54b0` | #623 | 4 / 53 | Own PR: synchronize/qualify; relay630 may be superseded by629 |
| `feat/adopt-provenance` | `7bf579149605` | #528 | 12 / 442 | Merged PR exact head, different ancestry; retain history |
| `feat/adopt-provenance-forget` | `4385b27bb61a` | #553 | 3 / 393 | Merged PR exact head, different ancestry; retain history |
| `feat/adopt-reap-predicate-entry-shape` | `20952b431c10` | #551 | 2 / 393 | Merged PR exact head, different ancestry; retain history |
| `feat/anti-drift-unmanaged-signal` | `c2548d49d3f2` | #523 | 2 / 472 | Merged PR exact head, different ancestry; retain history |
| `feat/cbuild-mcp` | `d521aad303ab` | #541 | 6 / 411 | Closed but inclusion not proven; preserve for separate residue review |
| `feat/deadopt-phase1-manifest-delete-hash` | `b0de9ea8ffb7` | #539 | 2 / 415 | Merged PR exact head, different ancestry; retain history |
| `feat/deadopt-phase5-snapshot-cap` | `122160a8c35e` | #543 | 2 / 407 | Merged PR exact head, different ancestry; retain history |
| `feat/deadopt-phase6-prov-v2` | `27a622ac9c36` | #545 | 2 / 407 | Merged PR exact head, different ancestry; retain history |
| `feat/gui-daemon-recovery` | `6119295e1a73` | #557 | 2 / 375 | Merged PR exact head, different ancestry; retain history |
| `feat/gui-restart-unitb-foundations` | `ab0810fc162f` | #560 | 1 / 367 | Merged PR exact head, different ancestry; retain history |
| `feat/gui-restart-unitb-gated` | `4424d4a3ea4f` | #563 | 12 / 363 | Merged PR exact head, different ancestry; retain history |
| `feat/liveness-headless-gui-recovery` | `4cf1497f6025` | #589 | 50 / 332 | Merged PR exact head, different ancestry; retain history |
| `feat/mathcad-mcp` | `d18783189548` | — | 4 / 481 | No associated PR; preserve unique work |
| `feat/mcp-front-daemon` | `5bd3f64f431c` | #588 | 64 / 333 | Merged PR exact head, different ancestry; retain history |
| `feat/mcphub-adopt` | `4c3dc86b4885` | #513 | 8 / 498 | Merged PR exact head, different ancestry; retain history |
| `feat/reaper-v1-preview` | `95d0b8049341` | #527 | 2 / 448 | Merged PR exact head, different ancestry; retain history |
| `feat/supervisor-death-forensics` | `ee6d0bb4b06c` | #570 | 7 / 357 | Merged PR exact head, different ancestry; retain history |
| `feat/supervisor-stall-dump-trigger` | `e2ba671f5d4a` | #574 | 2 / 352 | Merged PR exact head, different ancestry; retain history |
| `feat/vcpkg-mcp` | `70a472770cff` | #591 | 77 / 330 | Merged PR exact head, different ancestry; retain history |
| `fix/archcheck-filename-constraints` | `4c20cc5bbdcd` | #613 | 1 / 223 | Closed but inclusion not proven; preserve for separate residue review |
| `fix/auto-reaper-client-direct` | `8234331fcc49` | #565 | 1 / 361 | Closed but inclusion not proven; preserve for separate residue review |
| `fix/cleanup-aggressive-exe-ext-panic` | `235302c8acce` | #587 | 2 / 340 | Merged PR exact head, different ancestry; retain history |
| `fix/cleanuporphans-identity-reverify` | `448b07f53855` | #511 | 6 / 498 | Merged PR exact head, different ancestry; retain history |
| `fix/cli-first-run-ux` | `ffceded10524` | #575 | 4 / 357 | Merged PR exact head, different ancestry; retain history |
| `fix/cursor-not-default-install` | `fcbea4994d33` | #583 | 24 / 335 | Merged PR exact head, different ancestry; retain history |
| `fix/e2e-status-seam` | `1f7ca3a3e8bb` | #526 | 1 / 456 | Merged PR exact head, different ancestry; retain history |
| `fix/gate-gui-spawning-cli-tests` | `5cf1b0145585` | #556 | 7 / 383 | Merged PR exact head, different ancestry; retain history |
| `fix/gate-on-classify-reap` | `4c31b4d6fe34` | #554 | 1 / 389 | Merged PR exact head, different ancestry; retain history |
| `fix/gui-diagnostics-stderr-misrouting` | `008fc676121a` | #576 | 3 / 352 | Merged PR exact head, different ancestry; retain history |
| `fix/gui-state-dir-path-leak` | `87b9090ce410` | #552 | 4 / 393 | Merged PR exact head, different ancestry; retain history |
| `fix/intent-collapse-stop-resurrection` | `aafa1604764d` | #525 | 3 / 469 | Merged PR exact head, different ancestry; retain history |
| `fix/lsp-relay-per-client-disable-gui` | `e405898965db` | #524 | 9 / 469 | Merged PR exact head, different ancestry; retain history |
| `fix/lsp-router-per-client-disable` | `c419b7e46057` | #512 | 7 / 498 | Merged PR exact head, different ancestry; retain history |
| `fix/npm-canonicalize-local-bin` | `10f75b402232` | #584 | 1 / 341 | Merged PR exact head, different ancestry; retain history |
| `fix/npm-postinstall-global-guard` | `951c51c6aa20` | #586 | 3 / 340 | Merged PR exact head, different ancestry; retain history |
| `fix/npm-release-windowsgui-subsystem` | `382eb450fe5b` | #582 | 1 / 343 | Merged PR exact head, different ancestry; retain history |
| `fix/pr616-client-config-read` | `93cf926a56b7` | #618 | 0 / 52 | Historical contained ref; retain, no automatic deletion |
| `fix/pr616-daemon-launch` | `e30e492db6e0` | #619 | 0 / 51 | Historical contained ref; retain, no automatic deletion |
| `fix/pr616-native-test-custody` | `62a7b0dc1028` | #620 | 0 / 26 | Historical contained ref; retain, no automatic deletion |
| `fix/pr616-portable-test-fixtures` | `64b0eddb3a86` | #621 | 0 / 52 | Historical contained ref; retain, no automatic deletion |
| `fix/pr616-serena-invocation` | `9034a661a1dd` | #622 | 0 / 52 | Historical contained ref; retain, no automatic deletion |
| `fix/prespawn-missing-binary-hold-gate` | `a05f32834444` | #573 | 2 / 357 | Merged PR exact head, different ancestry; retain history |
| `fix/process-terminal-admission-fixture-20261114` | `06c0d25995b3` | #634 | 1 / 0 | READ ONLY: parallel session |
| `fix/readiness-handler-unbounded-probe` | `641fc5aeb68e` | #572 | 3 / 357 | Merged PR exact head, different ancestry; retain history |
| `fix/recovery-audit-deadline-fixture-20261114` | `7e0b89f987e1` | #641 | 1 / 0 | READ ONLY: parallel session |
| `fix/relay-output-failure-contract-20261113` | `fb00f95d0c37` | #629 | 3 / 53 | Own PR: synchronize/qualify; relay630 may be superseded by629 |
| `fix/relay-process-input-owner-20261113` | `1326779cd9b2` | #630 | 4 / 53 | Own PR: synchronize/qualify; relay630 may be superseded by629 |
| `fix/serena-integration-qualification-20261114` | `6b888eaba4c4` | #637 | 9 / 0 | READ ONLY: parallel session |
| `fix/serena-qualification-namespace-20261114` | `acac454e539a` | #636 | 5 / 0 | READ ONLY: parallel session |
| `fix/serena-qualification-publisher-20261114` | `6ca53c83aa4f` | #635 | 3 / 0 | READ ONLY: parallel session |
| `fix/serena-qualified-uv-path-20261114` | `8cead428a6ce` | #632 | 1 / 0 | READ ONLY: parallel session |
| `fix/serena-readiness-timeout-inversion` | `8c18c35ac1ef` | #569 | 2 / 357 | Merged PR exact head, different ancestry; retain history |
| `fix/serena-unbuffered-build-input-20261114` | `98c4b54a69f2` | #639 | 1 / 0 | READ ONLY: parallel session |
| `fix/status-identity-path-memo` | `886faa44e3f9` | #578 | 1 / 349 | Merged PR exact head, different ancestry; retain history |
| `fix/strict-mode-intent-fixtures-20261114` | `0abf739a4883` | #633 | 1 / 0 | READ ONLY: parallel session |
| `fix/supervisor-ipc-poll-flood` | `581dd80e698f` | #564 | 2 / 361 | Closed but inclusion not proven; preserve for separate residue review |
| `fix/supervisor-priority-class` | `8ac3cd40b6d5` | #577 | 1 / 350 | Merged PR exact head, different ancestry; retain history |
| `fix/supervisor-priority-normal` | `6e6ed8b09b90` | #581 | 1 / 345 | Merged PR exact head, different ancestry; retain history |
| `fix/tray-protocol-cancel-20261114` | `94804bd37d6b` | #638 | 1 / 0 | READ ONLY: parallel session |
| `fix/vtune-shutdown-budget-fixture-20261114` | `5883fbb5d517` | #640 | 1 / 0 | READ ONLY: parallel session |
| `fix/workspace-register-materializes-intent` | `02a64c0329c4` | #590 | 19 / 331 | Merged PR exact head, different ancestry; retain history |
| `hotfix/0.4.36-wolfram-legacy` | `c88be8a81313` | #614 | 0 / 162 | Historical contained ref; retain, no automatic deletion |
| `master` | `17bc78fc602e` | — | 0 / 0 | Leave master unchanged |
| `perf/rx01-cmaketrace-benchmark-20261112` | `98527f37d432` | #624 | 3 / 53 | Own PR: synchronize/qualify; relay630 may be superseded by629 |
| `perf/rx01-relay-baseline-20261112` | `7dad295acb3e` | #626 | 3 / 53 | Own PR: synchronize/qualify; relay630 may be superseded by629 |
| `perf/rx16-native-observation-20261112` | `30dc96ded011` | #628 | 1 / 53 | Own PR: synchronize/qualify; relay630 may be superseded by629 |
| `release/0.4.29-non-em` | `32fab6d20cfe` | — | 0 / 308 | Historical contained ref; retain, no automatic deletion |
| `release/0.4.37-win32-x64-observable-final` | `ed6859d68990` | #616 | 0 / 1 | Historical contained ref; retain, no automatic deletion |
| `release/0.4.41-portable-fixtures` | `0e75dcda7e1f` | #617 | 0 / 54 | Historical contained ref; retain, no automatic deletion |
| `spike/rx02-go-rust-boundary-20261113` | `a7a4c2d19c5a` | #631 | 22 / 53 | Own PR: synchronize/qualify; relay630 may be superseded by629 |
| `spike/rx02-native-process-owner-20261112` | `7ce04f7ef2f9` | #625 | 5 / 53 | Own PR: synchronize/qualify; relay630 may be superseded by629 |
| `spike/rx04-anchored-read-20261112` | `57f69575bbe2` | #627 | 1 / 53 | Own PR: synchronize/qualify; relay630 may be superseded by629 |
| `test/console-tray-default-bare-launch` | `381cbe936148` | #592 | 4 / 338 | Merged PR exact head, different ancestry; retain history |
| `verify/serena-postmerge-20261114` | `03be8fe02803` | — | 10 / 0 | READ ONLY: parallel session |

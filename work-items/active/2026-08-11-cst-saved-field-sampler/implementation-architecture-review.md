# W8 Independent Implementation Architecture Re-review

Date: 2026-08-13

Execution role: `$architecture-reviewer`

Review strategy: independent Claim-Verify against the sanitized immutable
candidate. The prior review and correction receipts were inputs, not proof.
CodeGraph was refreshed before owner/call-path inspection; Git object reads and
fresh executable falsifiers were then bound to the exact candidate. Product
source, the Git index, live services, CST, Service Control Manager, App Control,
virtual disks, CiTool, signing and publication state were not mutated.

## Immutable binding

| Bound input | Identity | Result |
|---|---|---|
| Candidate commit | `7aa0a2168e09139bf8deeac0fe31b8d0260c017d` | Exact `HEAD` at review start |
| Parent candidate | `bab886092ae0a4148c05f1e057eeedd73731eedf` | Exact parent |
| Candidate tree | `b3fe2b5f95ceaa6c51204ac38d29b4563bac81d4` | Exact |
| Candidate delta | `19` paths | Exact `git diff-tree` count from parent |
| Cumulative candidate | `83` paths | Exact count from base `43fee019d46c69522ebe79be952d5f139bd4854f` |
| Candidate content SHA-256 | `552A0345ED9369CBD578723DA45838235D5D0418DCABB37CC7BB4729395DE595` | Independently recomputed over ordinal path-sorted `<path><TAB><blob-OID><LF>` rows |
| Candidate receipt | `5280243F6111DF48EE138FB06BF4CA6E4F0C8FDC8B338F544775B825161FD3E3` | Excluded post-commit receipt |
| Accepted design | `7423D56DD33394336A06AB8C515D12F4496B4AC0533F2901BF5A0EE1436756ED` | Unchanged accepted input |
| Accepted decision | `8EAD15D041781A05ED192107D997693E27B79175C0D0125BB09F1C0A6DE8696A` | Unchanged accepted input |
| Accepted plan | `484883EDBAD02333162C61FAF78B99AA56C402FD64D5955F0E6B65BDDEC82E14` | Unchanged accepted input |

`git show --check` passed, the index contained zero paths, and candidate
execution-source drift was zero before and after the probes. A full `git archive`
extraction could not complete because the workspace volume reported no free
space; the incomplete scratch directory created by this review was removed.
Tests therefore ran in place only after the exact no-source-drift guard. This is
a verification-environment limitation, not a substitute for immutable binding.

## Reviewed surfaces and topology

The review read the accepted design's 34 claims and diff-invisible invariants,
plan W8 criteria, current candidate receipt, W9 dispatcher correction, W10
correction and prior W8 result. It inspected the exact 19-path successor delta,
the complete 83-path cumulative name/status surface, and these implementation
owners directly:

- `internal/api/apitest/testmain_cleanup.go` and its tests;
- `internal/api/port_alloc_excluded_windows.go`;
- `internal/cli/settings_registry_test.go`, `testmain_helpers_test.go`,
  `supervise.go`, `supervise_reconcile_wiring_test.go`, and overlay-marker tests;
- `internal/gui/main_test.go`, `testmain_dispatch_test.go`,
  `audit_lock_terminal_worker.go`, `audit_lock_terminal_worker_test.go`, and the
  terminal-worker spawn in `audit_lock_state.go`;
- the cumulative CST Go launch, native runtime, Python frontend/daemon/broker,
  containment, worker, endpoint, protocol, policy, manifest and focused test
  surfaces reached by fresh CodeGraph and the executable falsifiers below.

Fresh CodeGraph `status -> sync -> status` reported 2,118 indexed files and
`[OK] Index is up to date`. It confirmed the established single owners:
`runSupervise` owns event-loop creation/cancellation/join; the API port allocator
owns the no-console `netsh` construction; package `TestMain` owns test-only
pre-root dispatch; the saved-field route retains one Go launch owner, one native
pre-main revocation owner, fixed endpoint/schema owners, and owner-local receipt
boundaries. The successor changes no production CST/native/protocol/endpoint,
policy, containment, settlement, vendor, composition or manifest source.

## Blocking finding

### F1 — Valid R6 receiver protocol is unreachable through its exact dispatcher

- Defect class: test bootstrap protocol reachability / self-invalidating exact
  dispatch.
- Severity: blocking because an accepted security/resource-settlement
  falsifier cannot execute, while the W9 correction receipt says that protocol
  passed.
- Evidence: `classifyGUITestHelperDispatch` rejects the exact R6 selector when
  the child marker is absent (`internal/gui/main_test.go:280-320`). The test at
  `internal/gui/audit_lock_terminal_worker_test.go:496-507` requires that
  unmarked parent invocation so it can create the marked isolated receiver.
  Fresh execution of
  `go test -tags=test_state_path_env ./internal/gui -run '^TestAuditLockTerminalWorker_RealHTTPEventPersistenceAndSecondRun$' -count=2 -v`
  terminates in `TestMain` with `internal/gui: invalid test helper dispatch`
  before `m.Run`.
- Second return path: a direct valid marked receiver reaches the scenario, but
  its production-shaped terminal worker (`internal/gui/audit_lock_state.go:1547-1561`)
  inherits the R6 marker. The terminal-worker classifier rejects that inherited
  marker (`internal/gui/main_test.go:292-298`), so both recovery calls settle as
  `RECOVER_OUTCOME_UNCERTAIN` instead of the intended first uncertain / second
  durable sequence.
- Failure scenario: the ordinary tagged parent cannot start; bypassing that
  first contradiction still makes the nested worker fail. Thus neither the
  cross-process persistence/recovery result nor the correction's framing claim
  is proved.
- Single owner: GUI compiled-test-binary composition (`TestMain` classifier plus
  the R6 receiver test protocol). Production terminal-worker framing remains
  separately owned and must not be weakened.
- Violated architecture laws: C1 single-owner invariant, D4 resource-lifetime
  verification, and the receiving-side requirement that named regression guards
  be executable rather than skipped or rejected.
- fix-class: `design-decision`. The dispatcher is a security-sensitive trust
  boundary and at least two owner choices exist: distinguish normal parent test
  selection from marked child selection and explicitly end marker scope before
  nested worker launch, or introduce a dedicated bounded receiver harness whose
  environment contract cannot leak into the production-shaped worker.
- ADVISORY HOW (non-binding): keep one package-local classifier, allow the exact
  unmarked outer selector to enter normal `m.Run`, validate the marked receiver
  child as today, and have the test-only receiver composition end its marker
  scope before it invokes the production-shaped terminal worker. Do not make the
  terminal worker accept arbitrary or conflicting ambient helper state.
- Material alternative: a dedicated receiver harness avoids inherited test
  state but adds another helper protocol and maintenance surface.
- Falsifying verification guard: the exact tagged outer test passes twice; its
  marked receiver performs the intended uncertain-then-durable sequence; direct
  malformed/selector-only/conflicting helper matrices remain nonzero; terminal
  worker framing tests remain green; focused `go vet`, CST preservation, native
  verification and the canonical range scanner remain clean.
- Required route: `$architect` for the security-sensitive owner choice, then a
  bounded fix-design review and implementation/re-review. The ratchet cannot be
  downgraded to `inline-sufficient`.

The prior successor defects otherwise remain correctly disposed:

| Prior defect class | Disposition | Evidence |
|---|---|---|
| Supervisor event-loop lifetime | `fixed` | `runSupervise` cancels and joins its exact goroutine on every return; held-dispatch guard passes twice. |
| Windows excluded-port console policy | `fixed` | One API constructor applies established `process.NoConsole`; exact test and scoped vet pass. |
| CLI selected-test helper package-root residue | `fixed` | One CLI pre-root classifier and exact-root cleanup owner pass malformed and bypass matrices twice. |
| GUI R6 exact helper dispatch | `failed` | F1 reproduces both unreachable outer selection and inherited-marker nested-worker rejection. |

Anti-layering verdicts are `CLEAN-SINGLE-OWNER` for supervisor shutdown, API
port command policy and CLI helper cleanup. GUI dispatch is not accepted as
clean because the single classifier currently contradicts the protocol it is
meant to admit. No production CST `PILED` class, default-on route, fallback or
parallel receipt owner was found.

## Exact 34-claim reconciliation

Canonical per-claim verdict vocabulary is `verified`, `failed`, or
`not-verifiable (with reason)`.

| Claim | Verdict | Candidate evidence and enforcement probe |
|---:|---|---|
| 1 | verified | Existing-six registration/stdio and direct default-off composition are unchanged; focused cumulative tests pass. |
| 2 | verified | Request-local transaction ownership and no-job edge remain unchanged. |
| 3 | verified | Solve policy remains rejected before sampler work; no solve/job route was added. |
| 4 | verified | Broker capability ownership and native five-handle pre-main revocation remain unchanged; native verifier passes. |
| 5 | verified | Source snapshot equality and mutation matrices remain in the passing focused suite. |
| 6 | verified | Frame resolution retains exact metadata selection without fallback. |
| 7 | not-verifiable (target-only: installed CST Result3D/header seal/ResultTree/status trace is required) | Synthetic vendor tests are not promoted to target proof. |
| 8 | verified | Response order and six sampled components remain unchanged. |
| 9 | verified | Exact zero remains `zero_ambiguous`. |
| 10 | verified | One m/mm unit contract and transform owner remain unchanged. |
| 11 | verified | Only vendor-returned owned identity authorizes close; no snapshot-derived authority was added. |
| 12 | verified | Saved-field receipts remain exact/no-default and owner-local. F1 concerns an unrelated GUI recovery test protocol. |
| 13 | verified | One bounded redacted text publication shape remains unchanged. |
| 14 | verified | No production Line10/VFEM edge exists in the successor delta. |
| 15 | not-verifiable (target-only: independent native producer and exact dual-field/port/material comparison are required) | Local synthetic evidence is not provider independence. |
| 16 | verified | Existing six retain the direct native frontend; only the seventh crosses the fixed service/worker topology. |
| 17 | verified | Acquisition transaction ownership and partial-return cleanup remain unchanged. |
| 18 | verified | Neutral application port and inward concrete-adapter dependency remain unchanged. |
| 19 | verified | Workspace create/transfer/rollback retains one transaction owner. |
| 20 | verified | Stable no-follow transfer and complete-manifest equality remain enforced. |
| 21 | verified | One absolute SCM-daemon invocation budget remains propagated unchanged. |
| 22 | verified | Vendor records remain validated before selection and allocation. |
| 23 | verified | Daemon entry resolution and broker reauthorization remain independent and default-off. |
| 24 | verified | No deploy/sign/import route was added; missing target receipt still denies production work. |
| 25 | verified | `TrustedWorkspacePolicy` remains the injected workspace-root owner. |
| 26 | verified | Read-only snapshot and write-capable vendor roles remain separated. |
| 27 | verified | Broker epoch, Job, ordered five handles and native pre-entry revocation remain unchanged. |
| 28 | verified | Four protocols and exactly three saved-field endpoints retain owner-local receipts and bounded sequences. |
| 29 | verified | Existing Go launch lifecycle remains sole frontend owner; real Go-to-native fixed-pipe test passes. |
| 30 | verified | No content-key-use/signing path was added; target signing remains later work. |
| 31 | verified | Saved-field daemon settlement/quarantine and frontend publication receipts remain independent owners. |
| 32 | verified | Production source/vendor work remains unreachable without target ceremony/policy/package composition. |
| 33 | verified | Native pre-entry revocation and absent-provision failure remain intact; exact image verifies. |
| 34 | verified | One Windows path grammar/identity contract remains unchanged; neutral synthetic fixture values add no owner. |

Result: 32 `verified`, 0 `failed`, 2 `not-verifiable (target-only)`. Claims 7
and 15 alone remain target-only. F1 is an additional successor architecture
deviation outside the numbered product claims and blocks acceptance.

## Fresh verification receipts

| Probe | Terminal result |
|---|---|
| CodeGraph status/sync/status plus owner queries | PASS: fresh index, 2,118 files; topology bound to exact Git objects. |
| Git identity, cumulative digest, `git show --check`, index/source drift | PASS: all exact values above; zero index and execution-source drift before/after. |
| API cleanup + Windows netsh focused tests | PASS. |
| CLI malformed dispatch, package-root bypass, held reconcile join, marker and original supervise tests, `-count=2` | PASS. |
| GUI malformed dispatch and terminal-worker focused protocols, `-count=2` | PASS except the separately isolated tagged R6 protocol in F1. |
| Exact tagged R6 outer test | FAIL: `TestMain` rejects the valid selector before `m.Run`. |
| Direct marked R6 receiver scenario | FAIL: inherited R6 marker makes both nested terminal-worker calls execution failures/uncertain. |
| Real Go `StdioHost` to native frontend to fixed pipe | PASS. |
| Scoped `go vet` for API/CLI/GUI/daemon owners | PASS. |
| Explicit 32-file CST/native Python test set | PASS: all collected tests, one known Pydantic warning. |
| Native `verify.ps1 -Unsigned` | PASS; image SHA-256 `38D87C50F716E334F89628D4F35604534C3A35BDCF35F378F1D939137BAB89E3`. |
| Canonical `--range origin master` publication scanner | PASS: clean version-2 receipt, 165 files, 5 commits, tip `7aa0a216...`. Non-authorizing without human approval. |

## Blast radius and residual risk

The production CST result remains executable and default-off locally; no
fallback, default-on activation or new production owner was introduced. F1 is
test-only code, but it suppresses a cross-process durability/security oracle and
therefore prevents independent acceptance of the sanitized candidate. Claims 7
and 15, signing, App Control, VHDX, CiTool, Service Control Manager provisioning,
installed CST behavior, manifest promotion, publication and deployment remain
open. Any candidate source change invalidates this review.

## Gate

`REVISE`

The candidate remains aligned with the accepted production topology, but the
W9 exact TestMain dispatcher correction makes its R6 cross-process settlement
guard unreachable and rejects the nested production-shaped worker. Route F1 to
`$architect`, then bounded fix-design review, implementation and fresh W8
re-review. This result authorizes no installation, registration, publication,
push or deployment.

## Terms and Abbreviations

- App Control: Windows application-control policy enforcement.
- CST: CST Studio Suite electromagnetic solver.
- MCP: Model Context Protocol.
- PE: Portable Executable.
- R6: the real cross-process GUI recovery receiver regression scenario.
- SCM: Windows Service Control Manager.
- VHDX: Hyper-V virtual hard disk format.
- W8/W9/W10: architecture, security and quality phases of the working-result plan.

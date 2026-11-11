# PR #616: unified CARBON remediation plan

Review baseline: `e3c7cbabc97d218a86599b93218edabd33dee159`.
PR base: `76d11b33bd37e30125525ce8e0e582122f29e91b`.
The original five CARBON findings were reproduced on Windows before implementation.
Additional findings from implementation review and adjacent tests are included below.

## Contents

1. [Outcome and boundaries](#outcome-and-boundaries)
2. [Ordered work](#ordered-work)
3. [Recovery decisions](#recovery-decisions)
4. [Verification and remaining evidence](#verification-and-remaining-evidence)
5. [Remaining verification and separate work](#remaining-verification-and-separate-work)
6. [Execution record](#execution-record)
7. [Terms and abbreviations](#terms-and-abbreviations)

## Outcome and boundaries

Correct the CARBON findings and confirmed adjacent recovery defects in the existing PR. Preserve concurrent work,
native file ownership, exact journal readback, rollback identity checks, process
generation checks, and the current launcher/Product layout. Use existing owners
and standard-library operations; add no dependency or generic recovery layer.

Changes are prepared in an isolated worktree. Native tests use temporary files
and owned fixture processes. They do not modify the user's installed service,
scheduled tasks, or release packages. No external model or paid test is needed.

## Ordered work

Table 1. Implemented corrections and their acceptance criteria.

| ID | Responsible boundary | Required correction | Acceptance |
| --- | --- | --- | --- |
| F01 | `WindowsProductTxn.Prepare` and directory ownership | The first published complete-set journal includes the received native directory identity. Preserve stages after an uncertain journal write; safely settle unjournaled owned resources. | First-write crash checkpoints are recoverable; write-before/after-error paths preserve correct ownership; old ambiguous journals cannot authorize deletion. |
| F02 | Prior-runtime recovery callback | Respect setup/canonicalize modes, which do not produce an Upgrade prior-runtime projection. | Real cold rollback reaches settlement for both modes; invalid Upgrade projection still fails. |
| F03 | Prior-runtime recovery scenario | Decide from the saved prior state. A previously live supervisor must be restored and ready; current absence alone is insufficient. | Absent prior remains absent; live prior with current absence reaches owned start/readiness; missing original owner and readiness failures refuse success. |
| F04 | Identity-v1 parser and all complete-set readers | Reject metadata larger than one shared 64 KiB limit before allocating the complete input. | Limit and limit+1, whitespace, native/build/prior readers; executable payload sizes remain unaffected. |
| F05 | Configured-cache test fixture | Use `projectPathKey` for the fixture's insertion and lookup. | Existing Windows stale-mapping test passes without changing production normalization. |
| F06 | Replayed supervisor readiness | Bind readiness to the actual native server of the same pipe connection, using the existing strict status API. | A foreign server echoing the expected hello is refused before a status request; the actual peer succeeds. |
| F07 | GUI recovery after partial restart | Recognize an already recovered independent prior supervisor after a crash before GUI startup. Preserve native image, generation, user, role, fleet and peer checks. | Resume GUI recovery without starting, stopping or claiming creator authority over the observed supervisor. Preserve it on subsequent failure. |
| F08 | Setup/canonicalize fence test | Supply the now-required build binding and check the current fence diagnostic. | Both actual command adapters refuse before recovery or mutation while the upgrade fence is held. |

F06 was caught in the newly added replay branch during independent review and
corrected before publication. F07 is an inherited availability defect, confirmed
in both the PR base and reviewed head by its reachable crash sequence. F08 is a
test adaptation missed by the complete-set command change in this PR.

F01 and F02/F03 share the transaction contract but edit separate responsible
functions. F04/F05 are independent. Integration and final checks use one frozen
candidate, with independent review after implementation.

## Recovery decisions

1. Every newly published journal must contain evidence sufficient for its own
   recovery phase. Receiving a native directory owner is part of preparing the
   transaction, not a second optional publication after the first checkpoint.
2. A failed or uncertain publication cannot make a later `Prepare` return success
   or delete staged bytes referenced by an already published journal.
3. Legacy journals with `PayloadDirectory.Original == nil` cannot prove ownership
   of an existing directory. If the directory was previously absent, the current
   occupant could be foreign. Keep the journal and return an explicit diagnostic;
   do not invent native identity or delete the occupant. Preventing new incomplete
   journals does not certify automatic recovery of every old ambiguous journal.
4. The runtime callback owns the mode and previous-state decision. File restoration
   alone does not prove that a previously running service has been restored.
   A failed preparation must also leave `prepared == nil`: existing GUI-stop
   guards use this as an authorization condition. Cache the failure on the
   transaction after closing preparation owners, so a retry cannot report success.
5. Identity-v1 is a small fixed-shape metadata document. The observed normal fixture
   is 333 bytes; the old reader accepted 1,048,909 bytes after whitespace padding.
   A 65,536-byte ceiling provides substantial formatting/value headroom while
   bounding allocation. Oversized input is rejected explicitly, never truncated.
6. A previously restarted supervisor is an observed process, not a newly created
   child of the current attempt. Receive readiness through its native pipe peer;
   never reconstruct a creator launch handle or overwrite the historical journal
   generation. GUI replay may continue only after the saved fleet is proved.

## Verification and remaining evidence

- Retain the original failing observations. Add focused permanent regressions at
  the producing/callback boundaries; do not turn the old failure into a success
  assertion by weakening ownership checks.
- Run native Windows tests for complete-set admission, journal preparation,
  pending recovery, cold rollback, prior-runtime settlement, and the affected
  CMake tests. Check formatting and the complete patch.
- Exercise the existing built-entry-set transaction fixture if its toolchain and
  cached build dependencies are available; otherwise report the exact missing
  prerequisite without installing tools or claiming end-to-end success.
- The prior full Windows run failed in 11 packages. Attribute remaining failures
  using the relevant base/current comparison before calling them PR regressions.
  A scoped passing run does not certify the entire release.
- Repeat independent review on the final patch. Publish coherent fixes to the
  existing PR with a current-head check; do not merge or release.

## Remaining verification and separate work

Table 2. Open evidence gaps and work outside the recovery correction.

| Priority | Finding/evidence | Next correction or verification |
| --- | --- | --- |
| Before merge: native integration | The new GUI tests prove the continuation after native classification, not a real Product crash followed by complete GUI recovery. `TestBuiltWindowsCanonicalEntrySetTransaction` needs a qualified built fixture. No fixture was available, and neither the extracted `goversioninfo@v1.5.0` module nor its download cache was present. | With an approved build dependency cache, build an isolated Product/launcher set and run the built transaction fixture with `MCPHUB_CANONICAL_ENTRY_FIXTURE_REQUIRED=1`. Exercise crash after independent supervisor readiness and before GUI startup. |
| Before merge: old GUI command fixture | `TestUpgradeGUIOwnedPairCallerPersistsBeforeExitAndRetainsPendingOnFailedRecovery` still constructs the old single-Product fixture and calls the adapter without its required complete-set build binding. It fails before the scenario with `canonical entry build binding is missing or invalid`. This adapter/fixture mismatch predates this remediation. | Migrate the command integration fixture to the qualified complete set and native held process owners. A dummy binding alone cannot supply executing-Product ownership. Keep the failure visible until that migration is verified. |
| Separate client-config correction | `TestProjectsScan_NoWrite` leaves `.cursor/mcp.json.lock` and `.mcp.json.lock`. Reproduced on both base `76d11b33` and the current candidate with the same isolated test. | Give read-only project inspection a non-mutating client-config read path; verify locking/concurrent writes without removing another owner's locks. Keep this independent of transaction recovery. |
| Separate fixture corrections | API/GUI adopt fixtures pass an uncreated child home to a sandbox API requiring an existing directory. Those fixture and contract sources are identical in base and reviewed head. Two old CLI rollback expectations also contradict the already-existing creator-owned abort path. | Repair fixture setup and stale expectations in their own scope; repeat the affected tests. Do not weaken sandbox or process ownership checks. |
| Environment-specific gates | Prior failures explicitly named parent DACL rejection, unavailable symlink privilege, PowerShell script policy and Hyperfine shell parsing. A scan fixture also incorrectly assumes every automatically allocated port exceeds 10,000. | Use correctly owned test directories and supported fixture/tool settings; fix the port assumption separately. Preserve machine security policy. |
| Unattributed runtime failures | The prior full suite also reported daemon subprocess handshakes, Kimi stubborn-child cleanup and process-custody failures. No base/current causal result was obtained for these. | Compare minimal owned-process cases in a qualified harness. The stubborn-child fixture needs guaranteed hidden-window launch and cleanup before another run. Do not classify these failures as PR defects or environment-only without evidence. |
| Static-analysis baseline | Four `unsafe.Pointer` diagnostics in `internal/process/proc_thread_attribute_observed.go` and one copied-mutex diagnostic in `internal/daemon/stdio_launch_preparation_test.go`; both files are unchanged from the PR base. | Address in separate native-process/test ownership work; a scoped test pass does not clear these diagnostics. |

The Kimi cleanup classification subtest passes on both base and current candidate.
The earlier full-suite failure was not reproduced in that isolated comparison.
The read-only project scan remains a confirmed baseline failure. The full repository
suite is therefore not certified green by this correction.

## Execution record

- Initial state: remote PR head matches the reviewed baseline; 26 bot threads are
  resolved. Original alternative worktrees remain preserved.
- F01-F08 implemented. Native Windows run: full `internal/binaryadmission`
  and `internal/cbuild` pass. Final affected CLI selection: 77 top-level tests and
  102 subtests pass, zero skips. This includes all seven preparation subcases,
  non-upgrade cold rollback, restart/readiness failure, native peer rejection,
  the existing failed-prepare GUI guard, both command fence cases, five new GUI
  replay subcases, and existing GUI prior-recovery/diagnostic cases.
- `TestReliability_VersionCommandExitsZero` passes independently in the current
  worktree; the earlier full-suite failure was not reproduced by this run.
- Full `cmd/mcphub`, `cmd/mcphub-launcher`, and `tools/windows-product-validate`
  package tests pass. No installed service or visible GUI was launched.
- F01/F04 ownership and F02/F03/F06 runtime changes received independent source
  review. Own review also caught and corrected the failed-preparation GUI guard.
  F07 received a separate independent review with the integration boundary above.
- Validation used Windows/amd64 and Go 1.27.3, existing caches, `GOTOOLCHAIN=local`,
  `GOPROXY=off`, and `GOSUMDB=off`. No dependency was installed or downloaded.
- Formatting and patch whitespace checked; published source must match the native
  test worktree bytes. Publication targets the existing PR branch; no merge/release.

Final affected CLI selector:

```text
go test ./internal/cli -run '^(TestCanonical.*|TestPayloadDirectory.*|TestWindowsProduct.*|TestGUIOwnedStopUsesActualPriorRuntimeProjection|TestUpgradeGUIOwnedPrior.*|TestUpgradeGUIPrior.*)$' -count=1 -timeout 120s -v
```

All counts describe that explicit selection, not the full CLI package. The old
GUI command fixture in Table 2 was also run and failed as recorded; it is not
hidden in the passing count. Raw machine-local logs are kept out of the PR because
they contain temporary user paths. This record retains selectors, results, limits
and baseline attribution needed to assess the correction.

## Terms and abbreviations

- PR: pull request, the existing request to merge the source changes.
- Product: nested GUI executable `mcphub-product/mcphub.exe`.
- Launcher: root waiting command-line executable `mcphub.exe`.
- Native ownership: retained Windows handles and verified original object identity.
- Checkpoint: a durable intermediate journal state that recovery may encounter.
- Cold rollback: restoration in a new process after an interrupted transaction.
- Runtime: running supervisor, GUI, tray, and managed service state.
- GUI: graphical user interface.
- KiB: 1,024 bytes; 64 KiB is 65,536 bytes.
- Fixture: isolated test data and owned test doubles/processes.
- API: application programming interface.
- CLI: command-line interface.
- DACL: Windows discretionary access-control list.
- PID: operating-system process identifier; generation also includes creation time.
- Job: Windows process containment object.

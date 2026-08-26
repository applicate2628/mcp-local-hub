# Bug: CST W10 exact candidate does not pass the independent regression gate

- id: 2026-08-13-cst-w10-exact-candidate-regression-gate-fails
- context: 2026-08-11-cst-saved-field-sampler
- status: open
- severity: high
- area: immutable-candidate Go regression, vet, and cleanup gates
- found-by: qa-engineer

Exact candidate `bab886092ae0a4148c05f1e057eeedd73731eedf` fails W10-AC01 and
W10-AC02 when verification is correctly executed from its immutable Git archive.

Reproduction:

- `go test ./... -count=1 -json`: exit 1 after 363.931 seconds; 7,745 test
  passes, 11 failures, 37 skips; 40 package passes, 4 package failures and 11
  package skips. Nine failures equal the accepted clean-HEAD signatures. Two
  additional `internal/cli` tests fail Go `t.TempDir` cleanup with
  `hardened-parent: The directory is not empty`:
  `TestSuperviseCommand_AcquiresLockAndExitsOnSignal` and
  `TestSuperviseCommand_SweepsOldBinariesOnStartup`.
- The two cleanup tests then pass 10/10 in an isolated `-count=5` run. Per the
  QA flake rule, rerun-to-green does not clear a must-not-break failure. This is
  the already registered defect class in
  `2026-07-25-supervisecommand-tempdir-cleanup-race-lingering-subprocess-handle`;
  that existing adjacent bug remains the owner for the harness race.
- `go vet ./...`: exit 1 after 2.832 seconds because
  `internal/api/netsh_no_console_windows_test.go:12:9` references undefined
  `newExcludedPortNetshCommand`. The definition exists only in the unrelated,
  uncommitted live-worktree file `internal/api/port_alloc_excluded_windows.go`,
  not in the immutable candidate tree. Candidate-bound W7 vet acceptance was
  therefore not proved by the prior worktree-based run.
- The full Go run left seven new `R:\TEMP\mcphub-cli-test-state-*` /
  `mcphub-gui-test-state-*` directories after all owned test processes exited.
  No candidate native child, local pipe, service, App Control policy, VHDX or
  CST process residue remained, but W10's complete zero-artifact-residue oracle
  is not satisfied.

Expected: the exact archived candidate has a terminal W7 regression result with
only the nine accepted immutable-HEAD failures, `go vet ./...` completes, and
all test-created state roots are settled before exit.

Actual: two additional flaky failures, one deterministic exact-tree vet build
failure, and seven new temporary state roots remain. W8-W10 are invalidated by
any correction and must rerun against a new immutable candidate.

Owner correction surfaces:

- `$backend-engineer` for the exact-candidate `internal/api` source/test closure:
  admit the owning production file or remove/update the dangling test contract,
  then prove it on the Git archive rather than the dirty worktree.
- `$qa-engineer` / test-harness owner for the existing `TestSuperviseCommand_*`
  cleanup race and complete cleanup of `mcphub-*-test-state-*` roots. Fix the
  shared spawn/shutdown/temporary-state lifetime owner; do not add retry-only
  suppression around Go's final cleanup.

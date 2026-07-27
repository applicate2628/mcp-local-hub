# Close PR #589 live Codex-bot findings

- Admission source: direct user request on 2026-07-27, superseding the
  2026-07-26 read-only re-verification admission.
- Objective: classify and close all seven supplied Codex-bot findings against
  the current local branch `feat/liveness-headless-gui-recovery`.
- Delivery decision: fix every `REAL, open` finding at its owning invariant,
  leave `ALREADY FIXED` and `WRONG` findings unchanged, add a regression test
  for every real fix, mutation-prove each test, run the required build/vet and
  scoped package gates, and create one focused local commit.
- Safety boundary: never launch GUI, tray, supervisor, or another `mcphub`
  process; never touch the operator's Task Scheduler or real state directory;
  never run an unscoped `go test ./...`; never kill by image name; never push;
  never modify another worktree.
- State-path rule: every test/build/vet command that includes
  `internal/api` or `internal/cli` uses `-tags=test_state_path_env` and a fresh
  `MCPHUB_STATE_DIR_OVERRIDE`.
- Completion signal: all seven findings have evidence-backed classifications;
  every real finding has a class-complete sweep, a root-cause fix, a test that
  fails under the corresponding mutation, fresh green scoped tests, green
  `go build -tags=test_state_path_env ./...`, green
  `go vet -tags=test_state_path_env ./...`, and a local unpushed commit.

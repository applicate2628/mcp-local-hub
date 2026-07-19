# Phase I ensure-alive commission corrections

Date: 2026-07-18
Owner: `$lead` in the main session
Implementation role: `$backend-engineer`
Admission source: direct human Phase-I commission and `$lead` message-resolution decision
Classification: behavioral, bounded Phase-I seam

## Constraints

- No Graphify and no `claude` command-line interface.
- Do not set `MCPHUB_GUI_SPAWN_TESTS`.
- Every CLI/API test uses `-tags=test_state_path_env`.
- No commit.
- Do not edit Phase-F files.

## Plan

- [x] Verify the existing Phase-I branch, Phase-D marker store, Phase-E real-flock probe, Phase-F identity model, and unchanged supervisor-liveness body.
- [x] Add red tests for both-dead message reconciliation, total classifier timeout, CAS/lease fencing, and real-flock concurrent ticks.
- [x] Implement one total classifier budget; retain the owned Free lease through CAS completion; reconcile manual versus automatic-recovery messages.
- [x] Centralize the pidport leaf in its `internal/gui` path owner and remove the invalid Phase-I `handoff_id` alias.
- [x] Run focused tagged tests, the race detector, build, vet, touched-file format check, and the mandatory tagged API/CLI/GUI suite.

## Result

PASS. No commit or publication action was performed.

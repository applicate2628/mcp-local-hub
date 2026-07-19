# Item 3 Unit B Phase F — child-half review

Date: 2026-07-18. Integration owner: `$lead`. Branch: `feat/gui-restart-unitb-gated`.
Scope is the uncommitted Phase F child half and the authorized `server.go` continuation seam. Separate
Phase-I changes in `internal/cli/supervise_ensure_alive.go` and Phase-G parent implementation were excluded.

## Gate

`REVISE`. The normal `Server.Start` extraction is behavior-preserving and the child gates mutable GUI
runtime work behind the designated-child flock. Acceptance is blocked by nonce access enforcement,
commit/liveness settlement, Linux test portability, exact parent-death authority, child HTTP timeout
drift, and all-path identity-bound nonce cleanup.

## Findings

- **P1 — owner-only nonce access is not enforced.** `internal/gui/gui_restart_protocol.go:112` calls the
  generic default-relax state reader. `gui-restart-nonce` is absent from the secret classifier at
  `internal/api/state_read_inode_anchor.go:42-65`, so read-broadened access is accepted on Windows and
  POSIX. Fix with a strict, secret-bearing, one-shot consume primitive owned by the state-file layer.
- **P1 — Commit settlement is detached from runtime liveness and durable recovery classification.**
  `internal/gui/gui_restart_protocol.go:265-296` can write `committed` after the runtime dies, or exhaust
  retries under a healthy holder while leaving durable `reserved`; the latter deterministically probes
  as `Held`/busy. Fix with one liveness-aware settlement owner and a classification-safe terminal outcome.
- **P1 — Linux Phase-F tests fail before exercising their assertions.**
  `internal/gui/gui_restart_protocol_test.go:149` hardcodes `C:\state\gui-restart-nonce`; the Linux test
  binary rejects it as non-absolute and fails seven focused tests. Build the fixture from `t.TempDir()`
  or pass a platform-absolute path into the helper.
- **P2 — exact parent-death authority is absent.** `internal/gui/gui_restart_protocol.go:27-36,183-244`
  carries only a PID and has no retained parent handle/death signal. The named test injects an acquire
  error rather than parent death. Add an owned exact-process death seam to the standby select and test it.
- **P2 — the promoted child changes the full GUI header timeout.** `internal/cli/gui.go:117` constructs
  the child listener owner with the 2-second bind budget, while normal `Server.Start` owns a 10-second
  HTTP `ReadHeaderTimeout` at `internal/gui/server.go:788`. Preserve the normal HTTP policy and use the
  bind deadline only to bound binding.
- **P2 — nonce deletion is neither all-path nor identity-bound.**
  `internal/gui/gui_restart_protocol.go:117-121` returns on malformed length before removal, and valid
  removal is path-based after the anchored read. Consume and unlink the verified entry in one strict
  state-file operation; preserve the validation error and zero the bytes on every path.

## Verified properties

- Normal `Server.Start` remains bind → port store → ready close → full serve → one shared hub/events/
  restart/shutdown continuation, with the original 10-second header timeout and independent 5-second
  shutdown budgets. No double serve or listener leak was found.
- Before the flock, the child performs only nonce consumption, in-memory construction, and challenged
  standby binding. Hub, poller, supervisor adoption, browser, tray, and other mutable runtime work start
  only after reservation-aware acquisition and exact marker revalidation; activation waits on no parent
  or hub signal.
- The raw nonce value has no argv, environment, log, event, or durable-marker sink. Only its path enters
  the handoff environment. Challenged ping binds handoff ID, generation, sequence, PID, port, challenge,
  and proof; ordinary ping bytes are unchanged.
- Gate-off normal launch and legacy `"1"` handoff remain on the pre-existing bounded-acquire path.
  `gui-restart-lock-acquired`, successful child `committed`, and the unchanged `os.Exit`/supervisor-adoption
  path are wired as intended.

## Verification evidence

- `git diff --check` and `gofmt -d`: pass.
- `go build ./internal/gui ./internal/cli`: pass.
- `go vet ./internal/gui ./internal/cli`: pass.
- Focused Windows GUI/CLI tests: 99 pass, 0 fail.
- Broad Windows GUI/CLI tests: 2,764 pass, 0 fail, 21 declared skips.
- Linux-targeted focused Phase-F execution: 1 pass, 7 fail due to the hardcoded Windows path.
- Deterministic probes reproduce malformed nonce retention and activated + `reserved` + held-flock
  recovery classification.
- Raw evidence: `.scratch/phase-f-qa-20260718/`.

## Participants and decisions

- Analyst: `REVISE`.
- Quality assurance engineer: `REVISE`.
- Architecture reviewer: `REVISE`.
- Security reviewer: `REVISE`.
- `$lead`: accepted the six findings above; Phase F remains uncommitted and open for correction.

## Terms and Abbreviations

- **DACL** — discretionary access control list.
- **Flock** — operating-system file lock used as the GUI ownership lease.
- **HMAC** — keyed hash used to authenticate challenged readiness.
- **POSIX** — the Unix-like operating-system interface family used by the Linux implementation.


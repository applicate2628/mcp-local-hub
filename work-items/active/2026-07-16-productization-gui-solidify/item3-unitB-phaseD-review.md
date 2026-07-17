# Item-3 Unit B Phase D — implementation + commission record

Date: 2026-07-17. Author: $lead. Branch: `feat/gui-restart-unitb-gated` (the D–J atomic group; NOT
deployed until Phase J per plan §3). Phase D is fully INERT (feature gate default OFF, zero production
callers).

## What Phase D delivers (codex Sol, plan §4)
- **`internal/gui/gui_restart_gate.go`** — `RestartV3Enabled()`, package const `restartV3DefaultEnabled=false`
  + `MCPHUB_GUI_RESTART_V3` env override, resolved once at the composition root, threaded into `Server`
  (mirrors `MCPHUB_STRICT_JOB_PROTECTION`).
- **`internal/gui/gui_restart_record.go`** — `HandoffMarkerStore` owning `<state-dir>/gui-restart.json`
  (v3.1, 16 fields, four phases, two routes) with generation+sequence CAS under a bounded record flock;
  fail-closed reads (DisallowUnknownFields + EOF + validate; absent→nil; unknown version/malformed→typed
  `HandoffMarkerError{FailClosed}`); `Begin` refuses to erase an unreadable prior marker;
  `ClearAfterProvedPreReleaseRollback` only for a gen-matched in-progress marker. Plus `RestartDeadlines`
  (injected clock + eight budgets).

## Verification (by $lead)
build/vet clean; the full tagged `./internal/api/ ./internal/cli/ ./internal/gui/` suite green (re-run
after fixes); the 11 Phase-D tests pass.

## Commission (diverse-family, neutral)
- **fable — PASS.** Deepest: CAS correct (one flock across read→match→mutate→seq++→validate→write, no
  TOCTOU), fail-closed reads complete, inert (zero production callers), version "3.1" + four phases +
  two routes + injected clock correct, no CUT field can round-trip. Findings: 1×P2 + 3×P3 (below).
- **Sol — REVISE (1×P1):** the marker has no `state_dir` provenance field, so a valid marker
  copied/restored/planted at the path is trusted — claimed AC-D3 / design §730 violation.
- **opus — TIE-BREAK: `RESOLUTION: PATH-OWNED-CORRECT`.** The fable/Sol split was on a genuine design
  question; opus (fable already a consilium member) cast the deciding vote, siding with fable on all
  four grounds: (1) §5's closed field list + explicit exclusion list = deliberate location-binding, not
  a forgotten field; (2) the residual is benign across the WHOLE D–J design — the marker carries no
  kill authority (PIDs diagnostics-only), ensure-alive's predicate is AND-gated on the REAL OS flock (a
  planted JSON cannot free it; spawn count 0 in every branch), and `operator_action` is enum-mapped to
  fixed literals, never an arbitrary command; (3) Sol's fix (add a `state_dir` field) violates AC-D2 +
  the closed §5 list, is itself FORGEABLE by a planter (so it does not defend the plant threat), and
  adds Windows path-normalization fail-closed hazards; (4) repo precedent (`supervisor-intent.json`)
  embeds no origin field. Sol's ONE legitimate point — the design PROSE "state-dir-matching" is
  ambiguous enough to split two competent reviewers — is resolved by a one-line design-doc clarification
  (NO schema/code change), now added to `item3-restart-design.md` §5.

## Findings + dispositions
- **P2 (fable, `gui_restart_record.go` record lock) — FIXED:** unbounded blocking `lock.Lock()` (a
  wedged holder would block every ensure-alive tick / Phase-G handler / `mcphub gui` entrant forever, and
  the Phase-I bounded-hold invariant would be unimplementable). Added a `RecordLock` budget to
  `RestartDeadlines` (5s default) + `flock.TryLockContext(ctx, 10ms)`; expiry is a typed fail-closed
  error. Fixed NOW while the store is callerless (fable's recommendation) rather than as a mid-group API
  change at Phase I.
- **P3-2 (fable, `server.go`) — FIXED:** duplicate gate storage (`s.cfg.RestartV3Enabled` + a mirror
  `s.restartV3Enabled`) → dropped the mirror; later phases read `s.cfg.RestartV3Enabled` (single owner).
- **P3-3 (fable, `validateHandoffMarker`) — FIXED:** accepted out-of-range ports/PIDs → added a range
  check (ports `[0,65535]`, PIDs ≥ 0). Diagnostics-only, zero-cost hardening.
- **P3-4 (fable, tests) — FIXED:** added
  `TestHandoffMarkerStore_TrailingJSONUnknownFieldAndBeginOverCorruptFailClosed` (trailing-JSON +
  unknown-field + Begin-over-corrupt-not-erased) and a `RecordLock` assertion in the default-policy test.
- **P3-1 (fable) — CHECKLIST (no code now):** `owner_lease` dropped from `Commit` /
  `ClearAfterProvedPreReleaseRollback` signatures — the lease type doesn't exist until Phase E; thread it
  in F/G so ownership is type-enforced instead of convention.
- **State-dir (Sol P1) — RESOLVED path-owned + design-doc prose clarified.** No schema change.

## Gate state
Phase D re-verified green after all fixes. Committed to `feat/gui-restart-unitb-gated`. NOT deployed
(atomic group §3 — deploy only after Phase J). Next: Phase E (reservation-aware flock + tri-state probe).

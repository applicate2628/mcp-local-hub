# Item-3 Unit B Phase E — implementation + commission record

Date: 2026-07-17/18. Author: $lead. Branch: `feat/gui-restart-unitb-gated` (D–J atomic group; NOT
deployed until Phase J). Phase E is INERT while the gate is OFF (gate-off / no-options acquire = the
legacy single-shot path, no marker read; new callers unwired until F/G/I).

## What Phase E delivers (codex Sol)
`internal/gui/single_instance.go` — reservation-aware GUI single-instance acquisition
(`acquireReservationAwareSingleInstanceAt`) + a tri-state owner-lease probe
(`ProbeGUIOwnerLease → Held | Free(owned lease) | Unknown`), consuming Phase D's `HandoffMarkerStore`
read-only via a `HandoffMarkerReader` seam. A fresh `reserved` rejects ordinary/wrong-nonce entrants
(`ErrHandoffReserved`); the designated child retains only on an owner-only nonce hashing to
`designated_child_hash`. `single_instance.go` is the GUI single-instance lock — lease-lifecycle
correctness is the load-bearing property (a leaked/mis-released flock bricks GUI startup).

## Verification (by $lead)
build/vet/gofmt clean; the full tagged `./internal/api/ ./internal/cli/ ./internal/gui/` suite green,
re-run after every fix.

## Commission (fable + Sol + Terra) → all findings CLOSED
- **fable (initial):** production code PASS (exhaustive lease-release trace, no leak) but REVISE for a
  MISSING TOCTOU test.
- **Sol (initial):** REVISE — **2×P1 + P2 PRODUCTION defects** fable had rated P3/test-only:
  - **P1** `release()` cleared `l.fl` even on `Unlock()` error → held flock, discarded handle.
  - **P1** acquire required BOTH `FreshUntil` AND `ReservationExpiresAt`; after freshness expired but the
    reservation held, an ordinary entrant was admitted WHILE the designated child was rejected — backwards.
  - **P2** post-acquire marker-change detection incomplete + ordered after classification → a rewrite of
    other fields could return `Free`; a reservation appearing mid-probe returned `Held` not `Unknown`.
- **Fixes (codex Sol):** (1) reserved-phase openness now keys on `reservation_expires_at` ALONE (single
  `reservationWindowOpen` owner, both call sites); (2) the COMPLETE validated observation is compared
  immediately after the post-acquire reread, BEFORE classification — any change → release + `Unknown`;
  (3) `release()` retries `Unlock` (bounded 3×) then `Close()`, unconditionally clears `l.fl`. Plus the
  fable TOCTOU pinning tests + a canonical `sha256:<64-hex>` designated-hash validator.
- **Terra confirm:** reserved-deadline + marker-change + tests CLOSED; flagged the release fix STILL-OPEN
  (the retained handle is discarded by the callers, and gofrs/flock `Close()==Unlock()` closes no
  descriptor on a persistent error).
- **$lead resolution of the release residual:** gofrs/flock v0.13.0 structurally cannot close the fd on a
  persistent `UnlockFileEx`/`flock` syscall error and exposes no handle accessor (verified against the
  module cache). ACCEPTED as bounded: near-impossible trigger, process-exit backstop for every current
  (short-lived) tentative-lease caller, pre-existing (legacy `Release` had it), strictly better than the
  legacy error-swallowing single-shot. Tracked: `work-items/backlog/2026-07-18-flock-persistent-unlock-residual.md`
  (definitive fix = raw-`*os.File` handle lock). The codex fix's test was VACUOUS (fake `Close()`
  decoupled from the failing `Unlock()`); replaced with two HONEST tests — transient recovery genuinely
  releases the real OS lock; persistent failure asserts handle-cleared/reported/idempotent WITHOUT a false
  OS-release claim.
- **fable final-confirm: VERDICT PASS** — all four resolutions structurally sound + single-owner (no
  layering), each pinned by a non-vacuous test; the accepted residual rests on an independently verified
  dependency-level premise with a bounded blast radius.

## Carry-forward for Phase F review (fable non-blocking observation)
At Phase-F wiring the handoff PARENT releases the lease MID-LIFE (not only at shutdown). A persistent
Unlock failure there would surface as the child seeing Busy until parent exit — fail-closed, bounded by
the reservation window, parent exiting per protocol anyway. Re-check the backlog's "every current caller
is short-lived" claim at the Phase-F review. (Cosmetic: two `ProbeUnknown…` test-case labels overstate
"after tentative acquire" — the post-acquire release branches are covered via the shared owner.)

## Gate state
Phase E re-verified green after all fixes; full commission PASS. Committed to `feat/gui-restart-unitb-gated`.
NOT deployed (atomic group — deploy only after Phase J). Next: Phase F (child half) ∥ Phase I (ensure-alive).

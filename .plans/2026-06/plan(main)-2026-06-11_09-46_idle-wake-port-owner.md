# Main Plan Snapshot - Idle Wake Port Owner

Objective: fix PR #289 round 2 P1 by making serena idle-wake readiness prove the supervisor-spawned daemon owns the port before accepting readiness.

Plan:

- [x] Add failing wake-readiness regression tests for owner mismatch, happy owner match, and missing supervisor PID polling.
- [x] Implement owner-PID readiness proof using supervisor status plus OS port owner seam.
- [x] Run required focused checks and state-file byte comparison.
- [x] Write required session report and commit without pushing.

Outcome: PASS pending commit at snapshot time.

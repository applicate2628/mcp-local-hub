## Plan Snapshot

1. Capture pre-fix evidence and current test seams. Status: completed.
2. Add failing regression for removed-descriptor PID fallback and ordering. Status: completed.
3. Run focused RED test to confirm failure. Status: completed.
4. Implement minimal PID capture/pass-through fix. Status: completed.
5. Run required verification and live-state hash check. Status: completed.
6. Write session log and commit one focused change. Status: in progress at snapshot creation.

## Scope

Fix one PR #288 round 9 review finding: ensure removed supervisor descriptor cleanup captures live supervisor IPC PIDs before the reconcile nudge and passes them into the existing force-kill path, while leaving actual kill sequencing after the nudge.

## Verification Plan

- Failing focused regression before production change.
- Required build, vet, narrow test, formatting, and live-state hash checks.

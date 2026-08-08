# Bug: Restart-v3 parent probes the child standby only once

- id: 2026-07-18-restart-v3-parent-single-shot-confirmation
- context: 2026-07-16-productization-gui-solidify
- status: fixed
- severity: high
- area: internal/gui/gui_restart_protocol.go
- found-by: qa-engineer

## Reproduction

Run the target-environment scratch probe against a just-closed loopback port under the production two-second confirmation context:

```powershell
go run ./.scratch/phase-g-qa-confirm-probe.go
```

Observed in `.scratch/phase-g-qa-confirm-probe.txt`:

```text
elapsed_ms=1
context_err=<nil>
confirm_err=probe restart standby: ... connectex: No connection could be made because the target machine actively refused it.
```

The spawned child process is only started when `SpawnRestartV3GUI` returns (`internal/gui/gui_self_restart.go:315-319`). The coordinator immediately invokes `deps.Confirm` once (`internal/gui/gui_restart_protocol.go:244-255`), and `ConfirmAuthenticatedStandby` performs one HTTP request then returns the first connection error (`internal/gui/gui_restart_protocol.go:499-531`). A repository-wide call-site inventory found no confirmation retry.

Expected: after spawn, the parent consumes the configured bind/confirmation budget waiting for the exact authenticated standby to become reachable, then either confirms it or reports confirmation timeout. The accepted design requires confirmation before the two-second bind deadline (`work-items/active/2026-07-16-productization-gui-solidify/item3-restart-design.md:229`, `:257`, `:315-316`).

Actual: the first connection-refused result ends the handoff after approximately one millisecond, so normal child startup scheduling can force an immediate healthy-parent rollback before the child has had time to bind.

## Coverage gap

`TestRestartV3_PortChange_ParentClosesHubBeforeFlockReleaseThenChildActivatesImmediately` and `TestRestartV3_SamePort_PreReleaseRollbackRetainsLeaseAndRebindsWithoutReacquire` inject confirmation callbacks that succeed immediately (`internal/gui/gui_restart_protocol_test.go:237-240`, `:294-297`). They do not exercise delayed standby availability and therefore pass with the production race intact.

Add a deterministic delayed-listener confirmation test for both port-change and same-port ordering. It must enlarge the unavailable-before-bind window explicitly and prove the parent retries within the injected deadline without accepting a wrong identity.


## Resolution (2026-07-18)
FIXED — the R1 commission (Sol) raised the same single-shot-confirm false-rollback; `confirm()` now retries
transient/connection-refused failures until `Deadlines.Bind` (auth/protocol failures stay terminal). Test
`TestRestartV3_ConfirmRetriesConnectionRefusedUntilChildBinds`. See item3-unitB-phaseG-review.md.

Terminal-at: 2026-08-08T22:58:13Z
Resolution: Pre-V1 terminal status `fixed` is preserved during operator-authorized V1 physical migration.
Evidence: Historical terminal time is unknown; preserved pre-V1 input SHA-256 `261d931007a65eb16445df021ea70384ebcadbbbf65261dd1ee4548eb4782c20`; original terminal status `fixed`; explicit operator-authorized V1 migration.
V1-Migration-Evidence: Historical terminal time is unknown; preserved pre-V1 input SHA-256 `261d931007a65eb16445df021ea70384ebcadbbbf65261dd1ee4548eb4782c20`; original terminal status `fixed`; explicit operator-authorized V1 migration.

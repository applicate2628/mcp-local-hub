---
status: open
context: adjacent-finding (catalog Tier-0 D-3 — deferred runtime-state-change gate)
---

# Re-evaluate the D-3 availability probe at reconcile time (runtime-state-change re-spawn gate)

Surfaced by codex-v2 on the catalog Tier-0 PR (feat/catalog-tier0-seams) + adjudicated
DEFER by `$architect` (2026-06-23, GATE PASS). The Tier-0 install-entry gate is complete
and the reconcile paths are provably unreachable by inert rows today; this follow-up
closes the runtime-state-change case once Tier-1 ships inert catalog rows.

## Gap
The D-3 gate is install-entry-only. The two reconcile surfaces re-spawn / re-write from
already-installed state without re-checking the probe. The uncovered case: a row installed
while READY whose host app is later REMOVED (probe now FAILS) → the next reconcile
re-spawns the now-unbacked daemon (crash-loop) or re-writes its client config. Unreachable
in Tier-0 (zero catalog rows); becomes reachable once Tier-1 inert rows exist.

## Scope
1. Add `Availability string` + `InstallProbe *config.AvailabilityProbe` (omitempty,
   ADDITIVE — round-trips byte-unchanged, same discipline as `RuntimeSpec`/`Stops`) to
   `SupervisorDaemon` (`internal/api/supervisor_intent.go:52`); persist them from
   `supervisorDaemonsFromPlan` (`internal/api/install_parsed_manifest.go:2447`).
2. Supervisor reconcile spawn branch (`internal/cli/supervise_reconcile.go:248-265`): call
   the existing `availabilityProbePasses` owner before `EvStart`; on a now-failing probe,
   SKIP spawn + emit a structured `severity: warn` event (mirror the orphaned-LSP-descriptor
   exclusion at `supervise_reconcile.go:233-246`) rather than spawn-and-quarantine.
3. Hub-reconcile (`internal/api/install_hub_reconcile.go:303-320`): re-check the probe on
   the manifest before emitting `AddReplace`.

**Single owner:** `availabilityProbePasses` (`internal/api/admission_check.go`) — reuse, do
NOT re-implement (the mirror-gate lesson).
**Blast radius:** `SupervisorDaemon` schema (additive), the two reconcile loops, the
probe-event log. Touches the Tier-0-PROTECTED supervisor-intent schema — needs its own
reviewed PR.
**Depends-on:** the Tier-0 D-3 schema seam (shipped).
**Prerequisite:** Tier-1 inert catalog rows (no reachable consumer until then).

## Related
- Decision: `work-items/decisions/2026-06-23-d3-availability-probe.md` (`## Residual`).
- Epic: `work-items/epics/2026-06-23-desktop-app-mcp-catalog.md` (Tier-0 / Tier-1).

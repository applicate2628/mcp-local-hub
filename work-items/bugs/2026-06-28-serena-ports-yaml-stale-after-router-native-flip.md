---
status: open
severity: low
context: adjacent-finding
---

# configs/ports.yaml still lists the stale serena/unified@9121 global entry after the router-native flip

## Finding (adjacent finding, NOT fixed in the area-4 router-native flip PR)

The area-4 flip changed `servers/serena/manifest.yaml` from `kind: global`
(daemons[unified@9121]) to `kind: workspace-scoped` (daemon_template, dynamic
pool 9150–9199, no static daemons). After the flip, serena no longer binds a
fixed global port — it allocates per-workspace ports from the daemon_template
port_pool. Two surfaces still assert the obsolete relation (C6 stale-relation
residue):

- `configs/ports.yaml` lines 2–4 — a `global: { server: serena, daemon:
  unified, port: 9121 }` entry. `TestPortsRegistryCoversAllShippedManifests`
  (internal/config/serena_test.go) SKIPS non-`KindGlobal` manifests, so the
  flipped serena manifest is no longer cross-checked against this entry — it
  passes but the entry is now dead/stale.
- `internal/api/serena_dynamic_pool.go:60` — a comment enumerating the shipped
  global-daemon band lists "serena 9121/9122" as a current occupant. The
  comment's PURPOSE (justifying the 9150 pool start clearing the global band) is
  still correct, but the "serena 9121/9122" item is now stale (serena is no
  longer in the global band).
- `internal/clients/antigravity.go:21` — a header doc comment says the relay
  "connects to the shared HTTP daemon on 9121". The 9121 is serena-specific and
  now stale (the dynamic-pool serena relay uses the --url form to the
  /serena/mcp router; 9121 no longer exists). The GENERAL relay-to-HTTP-daemon
  mechanism the comment describes is still correct for kind: global servers, and
  the two-shapes contract is correctly documented at antigravity.go:71-86 — only
  the bare "9121" example is residue.

## Why not fixed here

Per the backend-engineer adjacent-findings protocol, this is outside the
approved change surface. The architect's named change surface for the area-4
REVISE was the manifest flip + the `detectSerenaSourceState` comment + tests +
the decision doc; `configs/ports.yaml` and the dynamic-pool band comment were
not named. Neither change is required for any test to pass or for the
dynamic-pool serena to behave correctly (the dynamic-pool serena ignores
ports.yaml entirely — it allocates from the daemon_template port_pool via
AllocateSerenaPort), so removing them is pure C6 hygiene, not a correctness fix.

Removing the ports.yaml serena entry also has a small cross-cutting
consideration: existing hosts still run a 9121 daemon until they migrate, so the
ports.yaml port-map is a SHIPPED-catalog map (parallel to the shipped manifest),
not a live-host map. The proportionate action is to flag it for a follow-up
hygiene pass rather than expand the flip's blast radius.

## Suggested fix (follow-up)

- Remove the `serena/unified@9121` entry from `configs/ports.yaml` `global:`.
- Update the `serena_dynamic_pool.go:60` band comment to drop "serena 9121/9122"
  from the global-occupant list (keep the 9150-start rationale).
- Confirm `TestPortsRegistryCoversAllShippedManifests` and `TestPortsRegistryValid`
  still pass.

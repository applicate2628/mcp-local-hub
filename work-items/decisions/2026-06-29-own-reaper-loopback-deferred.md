---
status: accepted
date: 2026-06-29
---

# Decision: own-loopback-Reaper DEFERRED (keep the catalog 0.0.0.0-warn)

## Context
The catalog's `reaper` row (bonfire-systems/reaper-mcp-server, uvx reaper-mcp-server==0.1.1) carries a 0.0.0.0-LAN-exposure WARNING because its control bridge (python-reapy, RomeoDespres/reapy) binds 0.0.0.0. After successfully shipping a loopback-safe own-Ableton fork (epic closed 2026-06-29, catalog swap #451), the question was whether to do the same for Reaper.

## Investigation (analyst a2cae16f, 2026-06-29)
- The 0.0.0.0 bind is a hard literal at `reapy/tools/network/server.py:20` (`self.bind(("0.0.0.0", port))`); `Server.__init__(self, port)` takes NO host arg; `reapy/config/config.py` exposes NO bind-host key (only the port, REAPY_SERVER_PORT). So there is no config-knob fix.
- The reapy Server runs INSIDE REAPER's embedded Python, planted by `reapy.configure_reaper()` (the `activate_reapy_server.py` ReaScript). reaper-mcp-server hard-depends on python-reapy (`pyproject.toml: python-reapy>=0.10.0`) and is pure stdio (no own listener).
- **Structural difference from Ableton:** the Ableton fix worked because the 0.0.0.0 bind lived in an operator-COPIED Remote Script (a fork-shippable, hub-controlled `.py`). Reaper's bridge is installed by reapy's own `configure_reaper()` into REAPER's Python — NOT an artifact the hub or an MCP fork ships. No clean install seam.
- python-reapy is effectively unmaintained (REAPER 7 / Py 3.13 failures; issue #133 open, no fix).
- **REAPER is NOT installed on this machine** (all probes failed) — a loopback fix could not be live-validated here (the bind only exists once configure_reaper has run against an open REAPER).

## Decision
DEFER. Keep the existing `reaper` catalog row as-is (disabled-until-probe + the thorough 0.0.0.0 WARNING, which already states "the hub cannot patch the operator-enabled bridge" + the firewall/loopback mitigation).

Fix options if revived (none is a clean S-effort win, unlike Ableton):
- (a) config: IMPOSSIBLE (no host knob).
- (b) fork python-reapy (flip the 1 literal) + fork reaper-mcp-server to depend on it + the operator re-installs the forked bridge into REAPER — M-L delivery, two forks of unmaintained upstream.
- (c) thin own ReaScript bridge + own MCP (mirrors own-Ableton) — L-effort, a from-scratch DAW-control project (58-tool surface).
- (d) REAPER native Web/OSC remote — M-L, smaller capability ceiling (action surface, not the object model), localhost-bind needs live verification.

## Revival trigger
Admit as a specialist-lane project (option b or c) only when: monetization/priority justifies it, a host with REAPER is available for the §4 falsification experiment (netstat the bind before/after + confirm the tool surface still drives REAPER over loopback), and the maintenance cost of forking unmaintained upstream is accepted. Until then the warn-and-keep row is the proportionate posture.

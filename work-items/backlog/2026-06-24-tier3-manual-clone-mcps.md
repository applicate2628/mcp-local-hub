---
status: candidate
severity: medium
context: adjacent-finding
defer: true
---

# Tier-3 EDA/CAD MCP rows DEFERRED — they need a manual clone+build mcphub cannot execute

## Summary

Of six Tier-3 engineering/EDA/CAD MCP servers researched alongside `kicad`,
**only `kicad` ships as a clean one-click catalog row** (PyPI-packaged
`kicad-mcp-pro` → `uvx`-runnable, no clone). The other **five are real,
working MCP servers** but each is blocked from being a one-click `disabled-until-probe`
S1 row by the SAME structural limitation. This doc preserves the verified
per-app research (pin / license / launch / probe) so it is not lost, and
records the single shared blocker + the two unblock paths.

## The shared blocker (all five)

Each of the five must be **manually `git clone`d and built** before it can run
— none is published to a package index (PyPI / npm) the way `kicad-mcp-pro`,
`ansys-mapdl-mcp`, or `paper-search-mcp` are. mcphub's catalog has no executed
install step:

- `vendored_source.install_cmd` and `vendored_source.run_cmd` are
  **documentation-only — NOT executed by mcphub**
  (`internal/config/manifest.go:185-186`). They are human-readable hints; the
  real launcher stays `command` / `base_args`.
- A cloned server's launch path (e.g. `python …/server.py`) is an
  **operator-specific absolute path** that is not known at catalog-author time.
  Writing `${workspaceFolder}/<repo>/server.py` into `args` is the **same
  category error** that disqualified the `mathcad` row
  (`2026-06-24-mathcad-mcp-row-deferred.md`): `${workspaceFolder}` is a
  GENERATE-time token frozen to the operator's CWD, not a stable install
  location, and `kind:global` daemons have no per-workspace identity.
- A `disabled-until-probe` row whose probe detects only the host app (not the
  cloned+installed server artifact) would PASS on a host that has the app but
  not the cloned repo, install the daemon, then crash-loop on the absent launch
  target → supervisor backoff → quarantine. That is exactly the false install
  the gate exists to prevent (mathcad disqualifier #2).

So as catalog-data alone, none of the five can be an honest one-click row today.

## Unblock paths (epic-owner scope, not the kicad PR)

These five unblock only via ONE of:

1. **A future D-2 executed-clone mechanism** — mcphub actually runs
   `vendored_source.install_cmd` (clone + build) into a known per-server
   app-data dir, then references THAT stable absolute path in `args`, and the
   `install_probe` detects the cloned+installed artifact (not just the host
   app). This is **security-sensitive**: executing a vendor's arbitrary build
   commands needs sandboxing + explicit operator consent (a D-4-class gate),
   so it is a design+research item, not catalog data.
2. **A docs-only-row transport** — a catalog row that is purely informational
   (homepage + readme + manual-install instructions, no `command`/`args`, never
   auto-installed), so the operator clones+builds by hand and then adds a
   manifest themselves. This needs a new catalog shape (a fourth S-shape
   alongside S1 local-stdio / S2 remote-http / S3 docs-only OAuth connector).

## Deferred-5 research (verified — preserve)

All five are REAL MCP servers (not CLI toolkits like the dropped `cst`). Pins,
licenses, launches, and probes below were research-verified; they are recorded
so a future executed-clone or docs-only PR does not have to re-derive them.

### 1. Onshape — `ReshefElisha/jarvis-onshape-mcp`

- **Pin:** tag `v1.2.0` / commit `b0e7258`.
- **License:** `NOASSERTION` — GitHub could not classify it; needs a manual
  license reconcile before redistribution (treat as `license_status: pending`
  until a human confirms the LICENSE text).
- **Launch / probe class:** cloud Onshape REST API; gated on an Onshape
  **API-key** (a cloud-credential probe, not a local-install file probe — there
  is no host binary to glob).
- **Extra blocker:** depends on `anthropic` / `claude-agent-sdk` (an LLM SDK
  dependency baked into the server itself), so it is not a thin protocol bridge.

### 2. COMSOL — `wjc9011/COMSOL_Multiphysics_MCP`

- **Pin:** commit `526e35b`.
- **License:** MIT.
- **Launch / probe class:** drives the COMSOL Multiphysics solver; the
  solver-launching tools are a **D-4 consent surface** (run arbitrary
  multiphysics solves on the host).

### 3. SolidWorks — `andrewbartels1/SolidworksMCP-python`

- **Pin:** commit `f0858a7`.
- **License:** MIT.
- **Launch / probe class:** drives SolidWorks via VBA; **MOCK-default-safe**
  (defaults to a mock backend, so the unconsented path is non-destructive), but
  the real VBA-exec path runs arbitrary VBA in the live SolidWorks process — a
  **D-4 consent surface**.

### 4. AutoCAD — `daobataotie/CAD-MCP`

- **Pin:** commit `3525418`.
- **License:** MIT.
- **Launch / probe class:** drives AutoCAD over COM; the COM-write operations
  are a **D-4 consent surface**. This repo is the **stalest** of the five
  (least recent activity) — re-verify the pin still builds before any unblock.

### 5. Guitar Pro — `wegitor/guitar-pro-mcp`

- **Pin:** commit `de439ba`.
- **License:** **NULL — no LICENSE file at all.** This is a HARD BLOCKER: a
  no-license repo is "all rights reserved" by default and CANNOT be
  redistributed as a one-click catalog row. Needs upstream to add a license
  before it is eligible at all.
- **Launch / probe class:** file-based (reads/writes Guitar Pro files);
  **GP5-only** (limited format support).

## Disposition

DEFER all five. Re-open under the desktop-app epic
(`work-items/epics/2026-06-23-desktop`) once an executed-clone mechanism (path
1) or a docs-only-row transport (path 2) exists. Guitar Pro additionally needs
an upstream license; Onshape needs a NOASSERTION reconcile. The `kicad` row
(PyPI-packaged, one-click) ships now and is the only clean Tier-3 EDA row in
this batch.

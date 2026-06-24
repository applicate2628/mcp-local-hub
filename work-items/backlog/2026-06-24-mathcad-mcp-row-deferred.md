---
status: open
severity: medium
context: adjacent-finding
---

# `puran-water/mathcad-mcp` Tier-1 row DEFERRED — three disqualifiers

## Finding (architect ruling, binding)

The `mathcad` row was included in the first Tier-1 catalog draft (PR #426) as a
`disabled-until-probe` S1 stdio row driving PTC Mathcad Prime via COM. The
architect DISQUALIFIED it from the first batch on three independent grounds — it
is NOT a working one-click row as drafted:

1. **`${workspaceFolder}` is a category error for a `kind:global` daemon.** The
   drafted `args` were `["${workspaceFolder}/mathcad-mcp/standalone.py"]`.
   `${workspaceFolder}` is a GENERATE-time token: `mcphub marketplace generate`
   freezes it to the operator's CWD at draft time. For a `kind:global` daemon
   (which has no per-workspace identity) that frozen path is whatever directory
   the operator happened to run `generate` from — not a stable install location.
   The spawn-time, per-workspace token is the DIFFERENT `${workspace.path}`,
   which is workspace-scoped only and does not apply to a global daemon. So the
   row's launch path is neither generate-stable nor spawn-resolved — a category
   error.

2. **The server artifact is unprobed AND absent (crash-loop risk).** The repo is
   a bare FastMCP `standalone.py` with no `pyproject.toml` / console-script — it
   must be cloned into a known location and `pip install -r requirements.txt`'d
   before `python standalone.py` can run. The drafted `install_probe` only
   detected Mathcad Prime + a Python interpreter; it did NOT probe for the
   cloned-and-installed server artifact itself. So on a host with Mathcad + Python
   but no cloned repo, the probe would PASS, the daemon would install, and then
   `python …/standalone.py` would fail (file absent) — the supervisor churns it
   through backoff → quarantine. That is exactly the false install the
   `disabled-until-probe` gate exists to prevent.

3. **License pending.** No LICENSE file was detected at the pinned commit;
   `license_status` was `pending`. A pending-license fork should not ship as a
   one-click redistributable catalog row until the license is confirmed.

## Disposition

The `mathcad` row was **DROPPED** from the Tier-1 first-batch catalog PR (final
batch = 3 rows: excel, ableton, codex-mcp-server). This is consistent with the
earlier `cst` drop (`2026-06-24-cst-not-an-mcp-server.md`): ship honest rows that
install + spawn, never a row that lies about being installable.

## Follow-up (epic-owner scope, not this PR) — what would make it a working row

Re-open mathcad as a Tier-1 research item under the desktop-app epic. It needs all
three before it can be a working one-click row:

- **Vendored-clone-to-a-known-location** so the launch path is stable and not
  `${workspaceFolder}`-frozen (e.g. resolve a per-server app-data dir the
  vendored_source clones into, and reference THAT absolute path in `args`).
- **A server-artifact probe** in `install_probe.files[]` that detects the
  cloned + installed `standalone.py` (or its installed entry point), so the row
  stays inert until the server itself is present — not just Mathcad + Python.
  (This can now use a glob, per the D-3 Tier-1 amendment.)
- **License confirmation** — confirm the upstream license and set
  `license_status: confirmed` before redistribution.

This is a design + research question (the right vendored-clone target dir is the
hard part), out of scope for the catalog-data PR.

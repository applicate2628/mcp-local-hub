---
status: closed
Historical closed date: 2026-06-29
---

# Epic: Desktop-app MCP catalog (engineering/creative tool MCPs)

> Commission-adopted 2026-06-23 (research workflow `desktop-app-mcp-survey` w309yz74c: 3 research lanes
> → architect synthesis, PASS-with-flags). Operator principle: **official > maintained-community-fork (pin+vet)
> > build-on-Python/COM-API > none**; Go-native ONLY for CLI-driven tools, NEVER COM/.NET typed APIs.

## Goal
Catalog MCP servers for the operator's engineering/creative desktop apps so agents can drive them. Reuse > build;
fork+pin+vet community servers; never reinvent a COM/.NET wrapper in Go.

## THREE catalog-shapes (verified vs hub code — D-1)
- **S1 local-stdio daemon** — hub supervises the process (Job-Object/restart/IPC). ALL COM/Python/CLI servers.
- **S2 remote-http bare-URL** — hub writes url+headers into client configs + fans out, no process (context7-style;
  bound to `remoteHTTPCapableClients`).
- **S3 client-side OAuth connector** (Fusion, SketchUp) — claude.ai/Claude-Desktop Connectors, OAuth 2.1+PKCE,
  vendor-hosted. Hub CANNOT supervise NOR write these → **docs-only reference row**, never forced into S2.

## PREREQUISITE seams (build BEFORE any catalog row — the schema supports NEITHER today)
- **D-2 vendored-source descriptor** on the manifest: `{repo, pinned_ref(SHA/tag), install_cmd, run_cmd,
  license_status}` (additive; owner `internal/config/manifest.go` + `internal/api/marketplace_generate.go`).
  Enforcement: a community-fork server without a non-empty `pinned_ref` fails the readiness gate.
- **D-3 availability field**: `{availability: ready|watch|disabled-until-probe, probe: <install-detector>}`
  (additive; owner catalog entry schema). A watch entry never spawns/writes until its probe passes; **reuse
  `readiness.go` as a DRY-RUN, do not re-implement detection** (mirror-gate lesson). GUI greys it with "probe to enable".
- Blast radius = catalog/manifest **data layer only**; supervisor + SecureWriteClientConfig + remote_http_matrix
  allowlist + groups all PROTECTED/untouched. A host with no desktop rows = byte-identical behavior.

## Vendor-and-pin safety protocol (D-4 — single owner at manifest-admission)
Pin exact SHA/tag (never `main`) · confirm LICENSE on the real repo (gh API `license!=null`) · audit + gate every
code-exec/write tool behind operator consent (Excel VBA, AutoCAD lisp, Ansys raw run_*, CST/SolidWorks COM-write+
solver, Blender Python, Onshape FeatureScript) · loopback-bind TCP bridges (Ableton/Blender) · secrets via the hub
vault (`secret:<key>`, never plaintext) · re-vet on every pin bump.

## Go-native scope (NARROW — protected boundary)
Only **Sonnet Suites** (em-batch CLI + `.son` text, no typed COM) is a legitimate Go-native build. Every COM app
(Mathcad/Excel/CST/SolidWorks/AutoCAD/AWR) EXCLUDES Go-native — reject any "rewrite in Go" PR at design review.

## Tiers
### Tier-0 PREREQUISITE — D-2 + D-3 schema seams + the Change-Surface Contract. **Do first.**
### Tier-1 — installed + ready-made (S1 local-stdio, catalog after Tier-0)
- **Mathcad Prime 11** (✅ installed+running) → `puran-water/mathcad-mcp` (Python/COM, ~24 tools, full
  open/calc/set-inputs/read-outputs/matrices/units/export). Single-commit, LICENSE=ASSUMPTION → fork+pin+confirm-license+light-harden.
- **Excel** (✅ Office16) → **SPLIT** (F1): PRIMARY `sbroenne/mcp-server-excel` (C#/live-COM, 25 tools/~230 ops,
  real recalc/Pivot/DAX; gate 6 VBA-exec ops) + SECONDARY `haris-musa/excel-mcp-server` (Python/openpyxl, file-based).
- **Ableton Live 11.3+12.4** (✅ both) → `ahujasid/ableton-mcp` default (mature; loopback-bind TCP); alt
  `jpoindexter` (200+ tools, full LOM) / `xiaolaa2` (arrangement view).
- **CST Studio Suite 2026** (✅ running) → `bbl21/CST_MCP` (Python3.13/COM, ~113 tools, full geometry→solve→S-params).
  14★/2026-born → pin EXACT tag + HARD-vet COM-write/solver + gate destructive/solver tools.
### Tier-2 — official vendor (watch/disabled-until-probe via D-3; enable on install)
- **MATLAB** (❌ not installed, stale COM reg) → OFFICIAL `matlab/matlab-mcp-server` (Go binary, 5 tools, v0.11.0,
  1038★) + Agentic Toolkit. Best-in-principle; watch → install official binary on probe-pass, NEVER a Python wrapper.
- **Ansys** (install unknown) → OFFICIAL `pymapdl-mcp` ON `ansys/pyansys-common-mcp` (common-mcp = infra base, NOT
  runnable alone, F2); never seed `knewnothing` (stale, raw-command injection).
### Tier-3 — fork-and-extend (community, git-pin+vet, install unknown → D-3 probe)
- **KiCad** → `oaslananka/kicad-mcp` (kicad-mcp-pro) — MODEL CITIZEN (CI/CodeQL/Scorecard/threat-model, telemetry-off).
- **Onshape** → `ReshefElisha/jarvis-onshape-mcp` (~60 tools, REST+API-key→vault, vision render). Cloud, no desktop.
- **COMSOL** → `wjc9011/COMSOL_Multiphysics_MCP` (80+ tools, 456★; gate the HF-embeddings fetch / run offline).
- **SolidWorks** → `andrewbartels1/SolidworksMCP-python` (109 tools, pywin32/COM). No official Dassault MCP.
- **AutoCAD** → `daobataotie/CAD-MCP` (AutoCAD/GstarCAD/ZWCAD, COM) — confirm distinct from the Mathcad "puran-water" cite (F5).
- **Guitar Pro** (✅ Arobas Music installed; no COM/automation API → file-based) → `wegitor/guitar-pro-mcp` (Python, wraps
  `Perlence/PyGuitarPro` — open/modify/save tab files). ⚠ **GP5-ONLY**: PyGuitarPro reads/writes GP3/GP4/GP5 but
  **GPX/GP6+ (the modern `.gp` Guitar Pro 7/8 format) is OUT of scope** — so it does NOT handle current Guitar Pro 8
  `.gp` files (only legacy `.gp3/.gp4/.gp5`). Fork+pin+vet; flag the format limitation in the catalog entry. Alt
  `blooper20/fingerstyle-tab-mcp` (audio→fingerstyle-tab via Spotify Basic Pitch — a DIFFERENT capability, audio
  transcription not file-edit). Music cluster with Ableton (Tier-1).
### Tier-4 — RF/EM EDA: watch-for-official or build-thin (MCP landscape immature)
- **Keysight ADS 2026** (✅ installed) → NO MCP; Python API + Keysight AI Copilot shipping → **watch-for-official**.
- **AWR Microwave Office** → no MCP; COM/pyawr → build-thin if needed.
- **Sonnet Suites** → no MCP; CLI+`.son` → the ONE legitimate Go-native build (low priority, niche).
### Tier-5 — S3 client-side OAuth connectors (docs-only reference rows)
- **Autodesk Fusion · Blender (Blender Lab) · SketchUp** — official Claude Connectors; hub carries a docs pointer only.

## Decisions to file (status: proposed)
- D-1 three-catalog-shapes (S1/S2/S3; S3 docs-only) · D-2 vendored-source pinned-ref descriptor · D-3
  watch/disabled-until-probe availability field. (Plus D-4 vendor-and-pin safety protocol.)

## Bonus (operator-requested, same epic)
- **codex-mcp-server** (official `codex mcp-server`, stdio, `codex()`+`codex-reply()` tools) → S1 catalog row for
  agents calling codex-as-tool. Keep the direct `invoke-codex-prompt.sh` for orchestration/commission (file-based,
  governance). Generalizes to `claude mcp serve` / Gemini / Qwen = a "coding-agent-as-MCP" sub-section.

## Children (open as work-items as each tier executes)
- [x] Tier-0 prereq seams (D-2 + D-3) — **DONE #424** (c7eb2d2c)
- [x] Tier-1 rows — **Excel + Ableton DONE #426**; Ableton later UPGRADED to the loopback-safe own-fork `applicate2628/ableton-mcp-loopback` (#451, 2026-06-29 — fixes upstream 0.0.0.0 LAN exposure, full tool parity). Mathcad DROPPED (unpackaged + no-license → backlog), CST DROPPED (cst-runtime CLI, not an MCP).
- [x] Tier-2 watch rows (MATLAB, Ansys) — **DONE #426**
- [x] Tier-3 fork rows — **KiCad DONE #427** (uvx kicad-mcp-pro); **Onshape DONE #441** (npx onshape-mcp@0.4.0 clean S1, Apache-2.0); COMSOL/SolidWorks/AutoCAD/GuitarPro → **docs-only pointer rows (#446, S4 transport:docs-only)** — the user accepted docs-only "нормально для незрелых пакетов", superseding the executed-clone-mechanism blocker (executed-clone stays a future backlog upgrade, NOT an epic blocker).
- [x] codex-mcp-server row — **DONE #426**

## Closure (2026-06-29)
All tiers resolved. The catalog grew far beyond the original tier list — a 2026-06-28/29 vendor-breadth sweep added ~40 more rows (Reaper, grafana/tableau OFFICIAL, photoshop/zotero/metabase/jupyter/rmcp, obsidian/logseq/origin-pro + ~19 docs-only pointers) → **52 catalog rows total** (33 entries + 19 docs-only), grouped by theme in the GUI, with the arch-aware install_probe (platforms[] gate) added so a row stays inert on an unsupported arch instead of false-installing. Shipped to npm in **v0.4.9** (2026-06-29).

The original blocker (D-2 executed-clone mechanism for manual-clone Tier-3 MCPs) was SUPERSEDED, not built: the immature/manual-clone servers (COMSOL/SolidWorks/AutoCAD/GuitarPro + 5 more) are now discoverable docs-only pointer rows (operator manual-installs), which the user accepted as the right posture for immature packages. The executed-clone mechanism remains a future backlog item (would upgrade those pointers to one-click) but is NOT required for this epic.

**Net: 8 clean one-click rows + 19 docs-only pointers + the full vendor-breadth catalog, LIVE on npm v0.4.9. Epic CLOSED.**

Closed: 2026-08-08T22:58:13Z
Outcome: Pre-V1 terminal status `closed` is preserved during operator-authorized V1 physical migration.
Evidence: Historical terminal time is unknown; preserved pre-V1 input SHA-256 `3d03fbe03a275d20a35bfb010e5b1111abbab644c701a8f22befbb811da3a26c`; original terminal status `closed`; explicit operator-authorized V1 migration.
V1-Migration-Evidence: Historical terminal time is unknown; preserved pre-V1 input SHA-256 `3d03fbe03a275d20a35bfb010e5b1111abbab644c701a8f22befbb811da3a26c`; original terminal status `closed`; explicit operator-authorized V1 migration.

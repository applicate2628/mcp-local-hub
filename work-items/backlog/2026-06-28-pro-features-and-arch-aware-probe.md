---
status: open
context: backlog
defer: true
---

# Backlog: PRO-feature candidates + arch-aware catalog probe

Captured 2026-06-28 during §9 vendor-breadth. These are deferred candidates, NOT current work.

## PRO-feature candidates (open-core, monetization-gated)

The open-core model is feasible under MPL-2.0 (file-level copyleft: proprietary pro code in SEPARATE files may combine with the MPL public core; only modifications to MPL files must stay open). Shipping shape: a SEPARATE PRIVATE repo (`mcphub-pro`), NOT a private branch (a public repo's branches are all public). Pattern A = private superset fork (GitLab-EE style, pragmatic) or import-based open-core (needs the public repo to expose `pkg/` extension seams — currently everything is in `internal/`, 0 importable). Distribution = public free OSS + private-repo pro binary gated by a license key (GitHub private-repo Releases are collaborator-only; paid users gate via license-key → download endpoint / Sponsors / a store).

Candidate pro features (none started):
- **Own Go-native Ableton MCP** — a Go MCP-server (reuse mcphub's MCP plumbing) + OUR OWN minimal Python Remote Script bound to **127.0.0.1** (Ableton requires a Python Remote Script for LOM access — irreducible Python; but owning it fixes the upstream 0.0.0.0:9877 LAN-bind P1 by construction + owns the protocol, no upstream fork / supply-chain). Est. ~1-2 weeks for a solid subset (transport/tracks/clips/devices); more for full LOM. Worth it IF music/Ableton is a priority or as a pro differentiator; otherwise reuse upstream ahujasid (#442) + the firewall warning.
- **Executed-clone mechanism (§9 P2/P3)** — the consent-gated clone+build+register for unpackaged community MCPs (COMSOL/SolidWorks/AutoCAD/GuitarPro). Designed (architect a7287f8b, C1-C12 + 16 claims) but deferred pending re-survey outcome (most targets are immature; Onshape shipped clean instead). Could be a pro feature (curated+vetted vendor catalog + the executed-clone automation).
- **Curated commercial vendor catalog** — the engineering/CAD/EDA desktop-app rows as a vetted, maintained pro catalog tier.
- **Advanced GUI** — the per-project / store / advanced-management surfaces.

The core hub (serena + mcp-language-server + process-tail compression + the basic catalog) stays free OSS — that is the raison d'être.

## Arch-aware catalog probe (catalog-schema enhancement)

The `install_probe` schema gates on binaries / files / file_globs (presence), but NOT OS/arch. So a launcher-based row (npx/uvx) passes its probe on an unsupported arch (e.g. win32-arm64 for a package that ships only win32-x64), then fails loudly at daemon start with the launcher's unsupported-platform error. Affects Onshape (win-arm64), KiCad, and every launcher row. Low severity (the failure is loud, not silent; the affected arch is niche). Enhancement: add an optional `os`/`arch` (or `platforms[]`) field to `install_probe` so a row is blocked at AvailabilityAdmissionEntry on unsupported arch rather than at spawn. Deferred — proportionate only if win-arm64 (or similar) adoption grows.

---
template: staged
orchestration: full-lead
status: active
started: 2026-08-10
updated: 2026-08-10
---

# HFSS and CST MCP feedback correction R1

Reopens: 2026-08-10-hfss-cst-mcp-servers

## Current state

- **Task**: correct the five reproduced HFSS/CST MCP defects, preserve the refuted resource-error behavior, and keep the live hub available.
- **Current step**: await explicit publication approval for the verified two-commit correction.
- **Last result**: product commit `6d6517eca4e1f5219d20838c4dee318eee2ea375` and its HFSS/CST manifest pin are locally committed; staged publication scans were clean.
- **Next action**: after approval, range-scan and push both commits, verify the hosted branch, rebuild/install the hub, restart only HFSS/CST, and repeat the safe live matrix.
- **Scope boundary**: shared MCP Streamable HTTP transport only where required; electromagnetics package schemas and bridge resource propagation; no solver execution or unrelated daemon behavior.
- **Owner**: main session Lead with analyst, backend-engineer, and QA stages.
- **Integration owner**: main session Lead.
- **Evidence gate**: all six findings independently reproduced before edit; focused and sibling regression tests after edit; explicit user approval before real solver jobs or publication.
- **Primary task**: correct the five reproduced transport/session/schema defects from `MCP-FEEDBACK-2026-08-10.md`, preserve the refuted resource-error behavior, and avoid disrupting the live hub.
- **Primary task status**: locally committed and verified; publication and live upgrade pending
- **Interruption marker**: none
- **Stage**: Publication approval gate
- **Main conv role**: Lead and integration owner; backend-engineer implementation accepted
- **Last accepted artifact**: `design.md` and the implemented RED-to-GREEN owner corrections
- **Open obligations before closeout**: reproduce all six findings; implement owner-level fixes; verify isolated and live-safe behavior; obtain approval before real solver jobs or publication.
- **Epic**: none
- **Depends-on**: none
- **Priority**: urgent

## Active agents

| Agent | Role | Model/effort | Status | Launched |
| --- | --- | --- | --- | --- |
| main session | Lead, backend-engineer, QA | current runtime | active | 2026-08-10 |

## Completed agents

| Agent | Role | Result | Artifact |
| --- | --- | --- | --- |
| main session | Analyst | PASS: five findings reproduced, one resource finding refuted on current runtime | `research.md` |
| main session | Architect | PASS: bounded adapter-owned sessions plus strict schema/preflight design | `design.md` |
| main session | Backend engineer | PASS: focused and full package checks green | source and tests |
| main session | Architecture reviewer and QA | PASS: nine design claims verified; isolated HFSS/CST safe matrix green | `review.md` |

## Next action

After explicit approval, publish the two commits and perform the bounded live upgrade plus safe availability verification.

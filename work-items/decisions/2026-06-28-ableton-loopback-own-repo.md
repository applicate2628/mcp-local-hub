# Decision: build an OWN loopback-bound Ableton MCP (replace the ahujasid catalog row)

status: accepted
date: 2026-06-28
owners: architect (a01151c3 design, PASS) + lead + user (flags resolved)
supersedes: the #442 warn-and-keep interim (stays live until P3 ships); #444 (remove the row) was closed as "hides not fixes"
parent-epic: work-items/epics/2026-06-23-desktop-app-mcp-catalog.md
security-gate: $security-reviewer MANDATORY before P3 (catalog-row replacement) ships

## Problem
The upstream `ahujasid/ableton-mcp` Remote Script binds `HOST="0.0.0.0":9877` (verbatim:
`AbletonMCP_Remote_Script/__init__.py` → `self.server.bind(("0.0.0.0", 9877))`), exposing
unauthenticated Ableton control to the LAN. The live catalog row (#442) documents this as a
warning + firewall guidance, but a textual warning is an operator-side mitigation, not a fix;
the bind stays 0.0.0.0. #444 (remove the row) was rejected — removal drops discovery + the
warning but does NOT fix the vuln (an operator can still manually install ahujasid).

## Decision
Build our OWN loopback-bound Ableton MCP in a NEW public repo and replace the ahujasid catalog
row with it. The fix is structural: the Remote Script binds **`127.0.0.1`** instead of
`0.0.0.0` — the kernel then refuses any non-loopback peer at the socket layer. Loopback by
construction, not by firewall convention.

## Architecture (two components — Ableton's hard constraint)
Ableton's Live Object Model is reachable ONLY from a MIDI Remote Script loaded into Live's
embedded CPython (no out-of-process LOM API, no COM, no HTTP). So irreducibly:
1. **Python Remote Script** (in Live's CPython) — loopback TCP socket SERVER, `HOST="127.0.0.1"`
   a hard constant (NOT operator-overridable to a wider interface). Dispatches `{type,params}`
   → LOM calls on Live's main thread.
2. **MCP server** (stdio) — socket CLIENT dialing 127.0.0.1:9877, relays MCP↔socket.

Approach = fork ahujasid (MIT) → change the bind → pin a SHA → vet → repoint the row; grow
toward a minimal telemetry-free surface later (reuse>build, the epic's fork+pin+vet principle).
Protocol = upstream's JSON-over-TCP verbatim (keeps the Remote Script a thin LOM bridge).

## Flags — RESOLVED
- **Language: Python.** The Remote Script is irreducibly Python → one language, shared
  protocol/LOM types, one test harness; a drop-in `uvx --from git+...` replacement of the
  current row (only the git URL + pin + summary change); ecosystem fit (every DAW MCP is
  Python/uvx). Go would add a 2nd language without removing the Python Remote-Script part.
- **License + free/pro: FREE, MIT, public repo.** A security fix MUST NOT be paywalled (else
  free-catalog operators keep the vulnerable 0.0.0.0 ahujasid). Pro, if pursued, is ADVANCED
  features (full-LOM, multi-DAW, orchestration) layered on the free safe core — NEVER the fix.
- **Repo name:** recommendation `applicate2628/ableton-mcp-loopback` (confirm at P1).

## Change-surface (mcphub-core)
ZERO code change for P1/P2. P3 = a catalog DATA edit only: REPLACE the `ableton` entry in
`marketplace/v2/catalog.json` — repoint command/args/vendored_source/readme/summary at our
repo, DROP the 0.0.0.0 SECURITY paragraph + the telemetry env (our build has no telemetry).
No new manifest field/transport/code path; attaches at the existing D-1 S1 + D-2 vendored_source
+ D-3 availability/probe seams. The supervisor supervises it as any S1 stdio daemon (no change).
v1 catalog FROZEN. Same-version (v2) row edit → no URL-duplication bump.

## Phasing
- **P1** — new repo; Remote Script = ahujasid fork with `HOST="127.0.0.1"` hard constant; MCP
  server (Python) = transport+clip START-SUBSET (get_session_info, set_tempo, start/stop,
  create_midi_track, set_track_name, create_clip, add_notes_to_clip, fire/stop_clip).
  Acceptance: (a) LOM smoke against a REAL Ableton Live (create track→notes→fire→audible);
  (b) SECURITY probe — a non-loopback connection is refused at the socket. **Needs the user's
  Ableton to validate (a).**  Courtesy parallel (non-blocking): an upstream PR offering the
  loopback bind to ahujasid; our row never depends on it.
- **P2** — fuller LOM (audio import, browser tree, arrangement); drop the `user_prompt`
  telemetry param; optional shared-secret/unix-socket hardening flag.
- **P3** — replace the catalog row. **$security-reviewer MANDATORY** (confirm the loopback bind
  in the pinned SHA, the LICENSE, no telemetry, the documented local-process residual risk).

## Security claims (for $security-reviewer at P3)
1. Remote Script binds 127.0.0.1, unreachable from any non-loopback address; HOST is a hard
   constant, no bind-host config key. Probe: a non-loopback connect is refused (with NO firewall
   rule present — proving construction, not convention).
2. No telemetry in the shipped build (removed, not env-disabled).
3. State-mutating LOM calls run on Live's main thread (no off-thread LOM crash).
4. Residual: any LOCAL process can reach 127.0.0.1:9877 (same posture as every local stdio MCP);
   documented in the row summary + repo README; finer per-tool consent is a hub-wide feature, out
   of scope for this row.

## Generalization (DAW re-survey a9d341c9 — resolved)
The re-survey found the CHOSEN DAW rows (Reaper, FL) bind loopback-default; only the NON-chosen
koltyj-Logic (0.0.0.0:7000) + tubone24-MIDI-HTTP-mode (0.0.0.0) carry the wide-bind class. So the
generalization is a **bind-discipline checklist** (vet every DAW row's bind before shipping), NOT
a reusable protocol — Ableton is the one case needing an own-loopback build because its upstream
default is 0.0.0.0.

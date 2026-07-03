---
status: accepted
date: 2026-07-03
slug: capability-probe-declared-capabilities-gating
accepted-by: main-conversation gate (architect design PASS 2026-07-03)
note: "Premise corrected during design — the capability probe is NOT on the /api/status hot path (that consumes the daemons section); it fires only on GET /api/health?include=capabilities (GUI Capabilities screen) behind a 60s TTL. Fix remains valid: spec-correctness (don't list an undeclared category) + round-trips 6→≤4 per cold refresh."
---

# Capability probe uses the MCP initialize `capabilities` declaration as the primary probe-gating signal

## Context

`internal/api/realCapabilityRow` (health.go) discovers each daemon's tools/prompts/
resources for the GUI Capabilities screen (`GET /api/health?include=capabilities`, 60s
TTL). Today it calls `liveCapabilitySubSection` three times — one per category — and each
call does its OWN `initialize` + `<category>/list` round-trip. That is **6 HTTP round-trips
per daemon** per cold refresh, and it probes categories the backend never offered (an
lldb-style tools-only server still gets prompts/list + resources/list that must fail).

Two defects follow:

1. **Wasteful** — undeclared categories are probed and must fail before being classified
   `unsupported`.
2. **Classification leans on a workaround** — `unsupported` is decided by matching a
   method-not-found *message* (`isMethodNotFoundMessage`, the "G2" fallback added because
   some servers report method-absence with a non-`-32601` code) rather than the
   spec-correct signal: the `initialize` result's `capabilities` object. The code at
   health.go:864 already flags this ("PROPER fix (Phase 2): parse the initialize
   response's declared capabilities and skip probing undeclared categories").

The MCP `capabilities` shape is verified against the official spec (revision 2025-06-18,
`modelcontextprotocol.io/specification/2025-06-18/basic/lifecycle`) and an in-repo sample
(`internal/api/tool_catalog.go:61-64`): the server `initialize` result carries
`capabilities` as an object whose category keys (`tools`, `prompts`, `resources`,
`logging`, `completions`) are each an object (possibly empty `{}`, optionally with
`listChanged` / `subscribe`); **presence of the key = the category is offered**. The spec
is normative: "Only use capabilities that were successfully negotiated."

## Decision

`realCapabilityRow`'s live branch performs **one** `initialize` per daemon, parses
`result.capabilities`, and gates the per-category list probes on the declaration:

- **Declared (capabilities non-empty AND the category key is present)** → do the
  `<category>/list` probe, reusing the single initialize's `Mcp-Session-Id`.
- **Declared-set non-empty but this category key absent** → `unsupported`, with **no**
  list round-trip (the new spec-correct path).
- **Fallback: `capabilities` absent OR present-but-empty (`{}`, zero declared categories)**
  → probe all three categories (today's behavior), reusing the one session. This is the
  backward-compat safety net for non-conforming servers that answer lists without
  declaring capabilities; empty-`{}` is indistinguishable from "declares nothing" and must
  never be read as "everything unsupported".

The trigger for fallback is **absent-or-empty** (not present-but-partial): a partial
declaration (e.g. only `tools`) is a trustworthy positive per spec and is honored.

The **G2 message-fallback (`isMethodNotFoundMessage`) is KEPT** as defense-in-depth on the
probed path only: a category that WAS declared (or probed via the fallback path) yet
returns a non-`-32601` method-not-found is a self-contradicting non-conforming server;
G2 still neutralizes it to `unsupported` instead of an alarming red `error`. It is demoted
from "primary unsupported signal" to "non-conforming-server backstop".

The **session is minted once and reused** across the conditional list calls. Each HTTP call
keeps its own 3s deadline (init and each list get a fresh 3s context); the total is at most
`1 initialize + 3 lists = 4` round-trips and as few as `1 + 1 = 2`, strictly below today's
fixed 6 — the fallback path is also cheaper (4 vs 6) purely from session reuse.

## Consequences

- **Wire shape unchanged.** `CapabilityRow` / `CapabilitySubSection` / `CapabilityItem`
  structs, the 4 live states (`ok` / `empty` / `unsupported` / `error`; `stale` is
  cache-owned), and the frontend (`Capabilities.tsx`, `CapabilityCard.tsx`, `types.ts`)
  are untouched. `/api/status` does not consume capabilities and is unaffected.
- **Blast radius = `internal/api/health.go` only.** `liveCapabilitySubSection` and
  `isMethodNotFoundMessage` are called nowhere but `realCapabilityRow` + the capability
  tests.
- **The probes section stays a separate owner.** Its own `initialize`+`tools/list`
  (`singleHealthProbe`, feeding `p.OK` / `p.ToolCount`) is NOT merged into the capability
  initialize — that is a separately-cached, `/api/status`-feeding surface and out of scope.
- Adding non-tools backend catalogs later requires no consumer edits; the declared-gating
  generalizes.
- `$architecture-reviewer` promotes proposed → accepted.

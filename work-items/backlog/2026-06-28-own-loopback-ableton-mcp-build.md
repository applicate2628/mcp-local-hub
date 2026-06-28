---
status: open
context: backlog
defer: true
needs: a real Ableton Live install to validate P1 (create track→notes→fire→audible)
---

# Backlog: build the own loopback-bound Ableton MCP (P1)

Design ACCEPTED (architect a01151c3, PASS) — see
`work-items/decisions/2026-06-28-ableton-loopback-own-repo.md` for the full design, flags
(Python + FREE MIT, resolved), change-surface, phasing, and security claims.

This item is the **P1 build**, queued (user chose record-design-and-queue 2026-06-28 to continue
catalog-breadth first). It is a NEW EXTERNAL public repo + a multi-session build.

## P1 scope
1. Create public repo (recommend `applicate2628/ableton-mcp-loopback`, MIT).
2. Remote Script = fork ahujasid/ableton-mcp's `AbletonMCP_Remote_Script/__init__.py` with the
   ONE material change: `HOST="0.0.0.0"` → `HOST="127.0.0.1"` (hard constant, no override).
3. MCP server (Python, stdio) = the transport+clip START-SUBSET (10 tools — see decision doc).
4. Acceptance: (a) LOM smoke against a REAL Ableton Live — **needs the user's Ableton**;
   (b) security probe — a non-loopback connect is refused at the socket (testable WITHOUT Ableton).
5. Pin the repo SHA.

## Then
- P2 (fuller LOM + drop telemetry), P3 (replace the `ableton` catalog row — $security-reviewer
  MANDATORY). The #442 warn-and-keep ahujasid row stays LIVE as the interim until P3 ships.

## Why queued, not now
P1's main acceptance (the LOM smoke) needs a live Ableton Live instance to drive. Building the
code without that validation would ship an unverified row. Resume when the user has Ableton to
test against. The security probe (non-loopback refused) can be pre-validated anytime.

---
status: accepted
date: 2026-07-08
context: A2 H5 spike — is pipe-peer-liveness a viable safety gate for the auto-reaper?
---

# Decision: the auto-reaper must NOT gate on pipe-peer correlation; adopt is the primary fix

## H5 question
Can the auto-reaper (proposed A2 PR5) reliably determine, from OUTSIDE a process,
that a LIVE client still holds the other end of an orphaned MCP server's stdin/stdout
pipe — so it never friendly-fires an in-use server — via
NtQuerySystemInformation(SystemHandleInformation) + GetNamedPipe*ProcessId /
NtQueryObject correlation?

## Empirical evidence (throwaway Go probe, run live 2026-07-08 on this host)
Probed a bypass npx-spawned node MCP server (@mui, PID 252772) and a hub-supervised
stdio-bridge node (memory, PID 385144):
- The bypass server held **36 pipe handles**. `GetNamedPipeServerProcessId` /
  `GetNamedPipeClientProcessId` DID resolve PIDs (so the "no peer-PID API for
  anonymous pipes" hypothesis is REFUTED). But for each pipe both APIs returned the
  SAME PID, and those PIDs were mostly DEAD (179416, 38852, 129300) with one ALIVE
  (111816 = Antigravity.exe). NtQueryObject(ObjectName) returned
  STATUS_OBJECT_PATH_INVALID (0xC0000039) — no usable name.
- The 36 handles are dominated by `\Device\Afd` sockets (outbound net) + INHERITED
  pipe handles from dead ancestor processes and one live unrelated app (Antigravity).
- The hub-supervised node held ONE clean pipe (peer 199144) — StdioHost spawns with
  controlled handles, no inheritance junk.

## Why pipe-peer is UNRELIABLE (the corrected reason)
1. **Handle-inheritance pollution**: npx → node inherits a pile of ancestor pipe
   handles. A leaked orphan therefore holds pipe handles whose "peer" is a LIVE but
   UNRELATED process (Antigravity). A "does any live process hold a pipe-peer?" gate
   FALSE-POSITIVES → the reaper spares genuinely-leaked orphans.
2. **Cannot isolate the real stdio pipe** from the inherited noise from outside the
   process (fd 0/1/2 handle values are not externally distinguishable here).
3. **No queryable name** (STATUS_OBJECT_PATH_INVALID) → cannot correlate ends by name.

## Decision
- **Primary fix = ADOPT** (absorb the bypass server into the hub → supervised, single
  instance, never orphans). Reliable; being surfaced in the GUI (A2 PR4b).
- **If an auto-reaper ships at all**, its safety gate must be **config-presence
  heuristics** (no live client config references the server as an active/enabled entry
  AND parent dead AND age>threshold), reliable to COMPUTE from configs + the live
  process table — NEVER pipe-peer. Ship dry-run-first + operator-confirm.
- **Deprioritize the pipe-peer spike** — it is a dead end.

---
status: proposed
---

# Decision: recover pool-port / ephemeral-range collisions by supervisor self-heal + setup detect — NOT excludedportrange or pool relocation

Date: 2026-07-13
Owner: architect (design PASS) + reliability-engineer (thresholds) → planner → backend
Relates: `work-items/bugs/2026-07-12-daemon-port-pool-overlaps-os-ephemeral-range.md` (accepted research), hub-partial/WSAEACCES memory.

## Context
The Windows TCP ephemeral (dynamic) range on affected hosts is widened to 1024-15000 (default is
49152-65535; here widened by WSL2/Hyper-V — `hns` Running). It fully contains mcphub's daemon ports
(fixed globals 9123-9136 from configs/ports.yaml; the serena/workspace DYNAMIC pool ~9150-9205; the
mcp-language-server manifest pool 9400-9599). The OS hands pool ports to foreign apps (AdGuard held
9205 via an established outbound socket); the daemon then fails to bind with WSAEACCES (10013,
access-denied — NOT 10048 in-use) → crash-loop → quarantine → hub "partial". `AllocatePort` probes only
at allocation, never re-checks; no reallocate-on-bind-fail path exists.

## Decision (layered)
- **L1 (primary, mcphub-side, no admin):** on a typed bind-refused exit (WSAEACCES/WSAEADDRINUSE) of a
  DYNAMIC-pool daemon, the supervisor reallocates a fresh pool port via `AllocatePort`, re-persists
  descriptor Port + argv --port (atomically, must agree) + workspaces.yaml row under the registry flock,
  and respawns — bounded by a per-crash-window reallocation cap (then falls through to quarantine).
  ZERO client-config churn: LSP/serena proxies resolve their port internally via the GUI LSP router /
  hub (`DaemonPortResolver`/`EffectiveDaemonPort`). Fixed global-port daemons are NOT reallocated
  (their ports are baked into gate-OFF client URLs).
- **L2 (host-level, opt-in, admin):** `mcphub setup` DETECTS the pool⊂ephemeral overlap (non-admin) and
  warns + offers `netsh int ipv4 set dynamicport tcp …` (MOVE the window) behind `--fix-ephemeral-range`
  (admin, prints before/after, never silent).
- **L3 (observability):** classify the bind-refused exit → `daemon-bind-access-denied` event (warn) +
  GUI daemon-card degraded reason with the remedy pointer.

## REJECTED
- **excludedportrange over the pool** (my initial instinct): self-defeating — `AllocatePort` treats
  OS-excluded ranges as UNUSABLE → would report the pool exhausted; and bind-to-excluded is the same
  WSAEACCES class we're fixing → could turn partial into TOTAL hub failure. The accepted memo warns
  against it explicitly.
- **Relocate the pool above ephemeral:** cross-cutting (ports baked into supervisor-intent/workspaces/
  managed-entries/client URLs); no universally-safe fixed range; memo rates weakest.

## Immediate machine relief (operator, admin, NEEDS user consent — affects WSL2/Hyper-V)
`netsh int ipv4 set dynamicport tcp start=49152 num=16384` (restore default). CAVEAT: WSL2/Hyper-V
(`hns`) may have widened the range deliberately; restoring could reduce its headroom. Targeted variant
(protect the pool, keep a wide window): `netsh int ipv4 set dynamicport tcp start=9600 num=…` above all
pools — but verify against existing excludedportrange (3080-3789) + WSL2 needs. DO NOT
`add excludedportrange` on the pool.

## Adjacent finding (resolved this session)
The failing clangd proxy on 9205 belongs to the serena/workspace DYNAMIC pool (~9150-9205, per
workspaces.yaml/runtime_spec), NOT the shipped mcp-language-server manifest pool (9400-9599). L1's
reallocation must resolve the correct pool PER DESCRIPTOR. Filed for planning.

## Phasing (architect)
Phase 0: operator machine-relief (no code, consent-gated). Phase 1: L2 detect+warn + L3 observability
(low-risk PR). Phase 2: L1 self-heal (larger PR, reliability-engineer thresholds first).

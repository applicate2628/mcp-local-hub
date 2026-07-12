# Bug: daemon port pool overlaps the Windows OS ephemeral range → foreign apps steal pool ports → daemon bind fails WSAEACCES → crash-loop → quarantine → hub "partial"

Status: open
Filed: 2026-07-12
Severity: P2 (single daemon down + hub shows "partial"; no data loss; self-heals via unregister/re-register or OS ephemeral-range fix)
Source: live diagnosis of "why is hub partial?" — VFEM clangd LSP (port 9205) crash-loop → quarantine
Blocks: nothing (cosmetic-to-functional degradation of one workspace daemon)

## Symptom (as observed live 2026-07-12)

GUI Dashboard shows the hub **"partial"**. `mcphub status` = `{Running: 21, Stopped: 1,
Restarting/Quarantined: 1}`. The one bad daemon: **`mcp-language-server` clangd LSP for
`d:\dev\VFEM`, task `\mcp-local-hub-lsp-6935d24c-clangd`, port 9205** — exit code 1 on
EVERY spawn, `daemon-respawn-scheduled backoff_seconds:60`, then `daemon-quarantined
failures_in_30m:10`. clangd, `compile_commands.json`, and the VFEM workspace are all
fine — control test: the same daemon binds a free port (9405) cleanly and starts clangd.

## Root cause (verified)

The daemon exits 1 because its bind fails:

```
bind proxy: bind 127.0.0.1:9205: listen tcp 127.0.0.1:9205:
bind: An attempt was made to access a socket in a way forbidden by its access permissions.
```

That is Winsock **WSAEACCES (10013)** — NOT WSAEADDRINUSE (10048). The chain:

1. This host's **Windows TCP ephemeral (dynamic) port range is 1024–15000**
   (`netsh int ipv4 show dynamicport tcp` → start 1024, num 13977 — a NON-default
   widened range; Windows default is 49152–65535, likely widened by WSL/Docker/a tool).
2. mcphub's daemon port pools sit **entirely inside** that range: observed live ports
   span **9123–9302** (serena pool + mcp-language-server LSP pool). Every pool port is a
   valid OS ephemeral port.
3. A foreign app — here **AdGuard VPN** (`NTKDaemon.exe` pid 18180) — made an OUTBOUND
   connection (to `AdGuardVpnSvc.exe`, `127.0.0.1:9205 → 127.0.0.1:12369 ESTABLISHED`),
   and Windows handed it **ephemeral local port 9205** — mcphub's assigned VFEM-clangd
   port. This is a long-lived internal AdGuard IPC connection, so the grip is persistent.
4. mcphub's proxy binds with **`SO_EXCLUSIVEADDRUSE`**, so a bind onto a port already held
   (even by an *established* socket, not a listener) returns **WSAEACCES**, not "in use".
5. Bind fails → daemon exits 1 → supervisor respawns it **on the same fixed 9205** →
   crash-loop → 10 failures/30m → **quarantine** → hub "partial".
6. The supervisor's `daemon-port-squatter-foreign` refusal to reap the holder was
   **correct** (never kill a foreign process — it's the user's VPN); `mcphub daemon
   recover` also (correctly) refuses. So there is no recovery path today except operator
   intervention.

## Why this is a latent mcphub design gap (not an AdGuard bug)

- **`AllocatePort` (`internal/api/port_alloc.go:141`) probes only at ALLOCATION time.** It
  `net.Listen`-probes each candidate (`portAvailable`, :20-27) and skips both
  registry-taken and **OS-EXCLUDED** ranges (`excludedTCPPortRanges` = netsh
  *excludedportrange*, :146-155). But it does **NOT** consult the OS **ephemeral** range,
  and once a port is allocated to a daemon it is a **fixed pin** — a later ephemeral
  consumer can steal it, and nothing re-checks or reallocates.
- **The supervisor has no reallocate-on-bind-fail path.** A supervised proxy that fails to
  bind its persisted port just crash-loops to quarantine on that dead port, even though a
  fresh `AllocatePort` would find a free one immediately.

## Fix options (for triage — NOT yet implemented)

1. **Self-heal (robust, mcphub-side):** on repeated spawn bind-failure for a supervised
   proxy (WSAEACCES / WSAEADDRINUSE), reallocate the port from the pool via `AllocatePort`
   (which already probes + skips occupied ports) and rewrite the intent + client entries,
   instead of respawning on the dead port. Makes the whole collision class recoverable
   with zero operator action.
2. **Detect + warn at `mcphub setup`:** compute pool ∩ OS-ephemeral overlap
   (`netsh int ipv4 show dynamicport tcp`) and, if the pool is inside the ephemeral range,
   emit a warn + offer to narrow the ephemeral range OFF the pool via
   `netsh int ipv4 set dynamicportrange tcp start=<above-pool> num=<n>`.
   **CAUTION: do NOT use `netsh add excludedportrange` on the pool** — `AllocatePort`
   treats OS-excluded ranges as UNUSABLE (`port_alloc.go:153`), so reserving the pool that
   way would make mcphub's own allocator report the pool exhausted. Only `set
   dynamicportrange` (move the ephemeral window) is safe.
3. **Move the default pools above any plausible ephemeral window** (structural; weakest —
   a customized ephemeral range can still be widened over them).

## Operator remediation (immediate — both verified)

- **Restore the default Windows ephemeral range** (admin): `netsh int ipv4 set dynamicport
  tcp start=49152 num=16384` → ephemeral becomes 49152–65535, freeing 9123–9302 from
  ephemeral assignment. Durable for the whole pool; does NOT evict AdGuard's *current*
  9205 grip (that frees when the AdGuard connection cycles).
- **Unregister + re-register the affected daemon** (no admin): `mcphub unregister
  "d:\dev\VFEM" clangd` then re-register (or let first-touch auto-register fire on next
  use) — `AllocatePort` probes and skips the occupied 9205, picking a fresh free port.
  **This is what was done live to green the hub 2026-07-12** (clangd was 5 days unused, so
  zero disruption). Residual: the new port is still inside the ephemeral range and could
  be re-stolen later — hence fix option 1 is the real fix.

## Evidence

- `netstat -ano | findstr :9205` → `127.0.0.1:9205 → 127.0.0.1:12369 ESTABLISHED 18180`
  (NTKDaemon.exe) + reverse in 38116 (AdGuardVpnSvc.exe).
- `netsh int ipv4 show dynamicport tcp` → start 1024, num 13977.
- `netsh int ipv4 show excludedportrange tcp` → 9205 NOT in any excluded range.
- Manual daemon run on 9205 → WSAEACCES ×3 (persistent); on 9405 → clangd starts.
- `supervisor-events.log` 2026-07-11T17:00:59 `daemon-port-squatter-foreign` pid 347940
  (an earlier own-child that later exited; AdGuard grabbed 9205 since).

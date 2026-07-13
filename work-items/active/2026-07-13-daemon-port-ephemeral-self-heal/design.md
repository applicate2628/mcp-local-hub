# D — daemon port ephemeral-collision self-heal (consolidated spec)

Decision: `work-items/decisions/2026-07-13-daemon-port-ephemeral-collision-self-heal.md` (proposed).
Architect gate PASS + reliability-engineer gate PASS. Build FULL (L1 self-heal + L2 setup-detect + L3 observability) in one carefully-reviewed PR. Branch: `fix/daemon-port-ephemeral-self-heal`.

## Root cause (confirmed)
Windows ephemeral TCP range widened to 1024-15000 (by WSL2/Hyper-V `hns`) contains the daemon pools. OS
hands a pool port to a foreign app (established outbound socket) → daemon `net.Listen` fails with
WSAEACCES(10013)/WSAEADDRINUSE(10048) → generic exit 1 → crash-loop → quarantine → hub "partial".
`AllocatePort` probes only at allocation; no reallocate-on-bind-fail path exists.

## L1 — supervisor self-heal (the core fix)
### B. Classification (the load-bearing precondition)
- Daemon proxy MUST exit with a dedicated reserved code `exitBindRefused` (propose 3; document vs existing
  codes; must not collide with cobra's 1) when `Bind()` fails AND the error satisfies the EXISTING
  single-owner predicate (`errors.Is(err, WSAEADDRINUSE) || errors.Is(err, WSAEACCES)` at
  `internal/gui/hub_listener_rebind_windows.go:12`). RELOCATE that predicate into a shared helper both the
  GUI hub-listener AND the daemon-proxy bind path call — do NOT re-implement (arch C1). Sites:
  `internal/cli/daemon_workspace.go:265-267`, `internal/cli/daemon_serena.go`, `internal/daemon/lazy_proxy.go:385`;
  `cmd/mcphub/main.go:53-60` already propagates the child ExitCode into `crashEvent.ExitCode`.
- Two-axis boundary, BOTH required to self-heal:
  - Fault: `ExitCode == exitBindRefused` → self-heal candidate; everything else → existing crash/backoff/quarantine unchanged.
  - Ownership: descriptor is a DYNAMIC-pool proxy (serena / workspace-LSP), via existing
    `IsSerenaProxyDescriptor`/`IsWorkspaceLSPProxyDescriptor` (`supervisor_port_owner.go:105-115`).
    FIXED-globals (9123-9136) are NOT reallocated (ports baked into gate-OFF client URLs) — they emit the
    L3 event with `action:"quarantined-run-host-remedy"` but do not move.
- Never kill the foreign holder (F1 `daemon-port-squatter-foreign` refusal stays as-is) — move OUR daemon.

### C. Reallocation cap + accounting
- Cap = **3 reallocations per 30-min window** (separate per-descriptor in-memory counter, resets like the crash window).
- A within-cap bind-refused reallocation does NOT increment the crash `failures` counter (mirror `holdSpawnInBackoff` "no crash increment" precedent, `supervisor_controller.go:4060-4079`).
- Backoff on reallocation = base step (~1s, `respawnBackoffStep`), NOT escalated crash backoff.
- On the 4th bind-refused within window → STOP reallocating → treat as genuine crash (increment `failures`) → normal exp backoff → existing 10-in-30-min quarantine catches it. Bounded flap: 3 base + ≤10 exp → quarantine.
- **Counter reset DWELL-gated, NOT bare-StRunning:** a bind-refused daemon reaches StRunning (EvHealthOK on process-start, `:3388-3405`) BEFORE it exits on the bind fail → naive StRunning reset = forever-flap. Reset the reallocation counter + crash window ONLY after StRunning + a min stable-bind dwell (reuse `EffectiveStartupBindDeadlineSeconds` or a fixed few-sec dwell). VERIFY whether existing `ClearCrashes` is dwell- or StRunning-gated; if StRunning-gated, fix it here.

### D. Pool exhaustion
`AllocatePort` returns `ErrPortPoolExhausted` → do NOT respawn/retry/re-probe → transition to StQuarantined with a DISTINCT reason (pool-exhausted, not "10 failures") + L3 event `action:"quarantined-pool-exhausted"`, forwarding the allocator's rich diagnostic string verbatim. Stays parole-eligible (F2 ladder).

### E. Atomic re-persist (crash-consistency)
Under the held registry (workspaces.yaml) flock across the whole transaction (AllocatePort holds no lock):
1. Hold registry flock. 2. `newPort = AllocatePort(reg, poolForThisDescriptor)` — resolve pool PER DESCRIPTOR (serena runtime_spec vs workspace manifest), not a global constant. 3. Write registry row → newPort FIRST (atomic temp+rename). 4. Write supervisor-intent descriptor as ONE atomic temp+rename updating `Port` field + `--port` argv + `RuntimeSpec.ExternalPort`(serena) TOGETHER (so the field↔argv fail-closed guard `supervisor_port_owner.go:66-90` + serena self-validate `daemon_serena.go:216` never see a partial). 5. Release, respawn.
- Run OFF the event loop (blocking I/O; mirror F1 portGate worker) → post result back as `EvManualRestart` (mirror `squatterAutoReaped→EvManualRestart`). Dedup per task (mirror `portGateInFlight`).
- Crash-consistency: crash before step3 = no change, retries old port, self-heals again; crash between 3&4 = registry reserves newPort, descriptor still old → relaunch old → self-heal again (AllocatePort skips reserved newPort); never a cross-daemon double-alloc, never a partial descriptor.

## L3 — observability: `daemon-bind-access-denied` event
Single canonical event, action-discriminated. source `restart-policy`. severity `warn` for `reallocated`,
`error` for any `quarantined-*`. Body: `port`, `pool`(serena-dynamic/lsp-manifest/global-fixed),
`inside_ephemeral_range`(bool), `action`(reallocated|quarantined-realloc-cap-exhausted|quarantined-pool-exhausted|quarantined-run-host-remedy),
`old_port`/`new_port`, `reallocation_attempt`/`cap`, optional `foreign_holder`(PID+basename ONLY, no cmdline/secrets),
`remedy`(actionable: names `mcphub setup --fix-ephemeral-range` / `netsh set dynamicport` + `mcphub daemon recover <task>`).
Blocking `Emit` (primary record of a state mutation). GUI daemon-card reason: transient reallocation = Starting/backoff
"port taken, moved to <new>, restarting" (NOT red); quarantined = names the remedy.

## L2 — setup detect + warn (separable, no supervisor change)
Windows-only step in setup RunE (mirror build-tagged netsh pattern in `port_alloc_excluded_windows.go`; POSIX no-op).
`netsh int ipv4 show dynamicport tcp` (non-admin) → compute overlap with the effective pool(s) → if overlap, WARN
naming the overlap+consequence+remedy. NEVER mutate the host range by default; `--fix-ephemeral-range` flag (admin,
reuse `setupIsElevated`, print before/after) does `netsh set dynamicport` (MOVE window, NOT excludedportrange). Uninstall does NOT revert.

## Open items (resolve during impl)
- exitBindRefused value: reserve + document vs existing exit codes.
- ClearCrashes gating: confirm dwell vs StRunning; fix if StRunning.
- F1 pre-spawn squatter → 2nd reallocation trigger: path B (bind-refused exit) is v1; path A (pre-spawn) is a NOTED follow-up, not in v1.

## SLO
Single collision recovery: p50 ≤2s, p99 ≤5s. Full 3-realloc budget ~15s before fall-through. No silent partial —
every state observable (warn+backoff transient / error+quarantine terminal). Terminal quarantine parole-eligible.

## Protected / must-not-touch
`AllocatePort` selection algo + `portAvailable`; daemon loopback-only bind posture (do NOT weaken); configs/ports.yaml
fixed ports + manifest port_pools (no relocation); gate-OFF/ON client-URL contract (no client-config churn on self-heal);
`daemon-port-squatter-foreign` never-kill refusal; `DescriptorServerDaemon` field↔argv fail-closed guard; SM table.

Gate: `go build/vet/test ./...` + tagged api/cli; BACK UP live supervisor-intent.json before state-affecting tests
(memory: subagent go test wiped live intent). Sweep only go-build/Temp mcphub children. Binary change → deploy after merge.

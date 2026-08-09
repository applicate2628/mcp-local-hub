# Supervisor architecture (v0.5.0)

## Overview

v0.5.0 replaces the v0.4.x model of N-scheduled-tasks-per-daemon with a single
long-lived `mcphub supervise` parent process per user that owns every MCP
daemon as a child process under an OS-appropriate lifecycle primitive (Windows
Job Object, Linux `PR_SET_PDEATHSIG` or systemd user service, macOS process
group + kqueue + LaunchAgent). The supervisor observes child exits in real
time, applies a persisted restart-policy state machine with sliding 30-min
failure windows, and exposes a local-only owner-bound IPC for control
commands. The remaining Task Scheduler / autostart entry is the per-user
autostart shim that re-starts the supervisor on logon.

Release scope: **Windows GA**, **Linux beta**, **macOS preview**. POSIX has
no v0.4.x to migrate from (v0.4.x shipped Windows-only) — Linux beta starts
fresh, skipping the migration journal entirely. macOS preview is build-only
Go cross-compile, no automated tests. v0.5.x stabilizes Linux to GA + macOS
CI lane; v0.6 promotes macOS only if a real containment primitive becomes
available.

## New commands

| Command | What it does |
|---|---|
| `mcphub supervise` | The long-lived supervisor process. Idempotent via `supervisor.lock`. Hosts FIFO event loop, reconcile driver, IPC listener, child-exit reaper. |
| `mcphub strict-mode enable` / `disable` | Canonical mutation of `supervisor-intent.strict_mode`. Universal lock order: `migration.lock` BEFORE `--once.lock`. Two-resource atomic write (intent file + autostart shim args) with revert-on-failure. |
| `mcphub strict-mode --recover` | Reconciles after a `STRICT_MODE_REVERT_FAILED` (exit 10) breadcrumb. Prompts operator to drive both intent + shim either to the `intended` value or to `actual_intent_state`. |
| `mcphub daemon recover <task>` | Identity-gates termination of a verified-own disowned port holder, then requests a forced respawn. Exit 7 means termination committed and respawn was attempted (or accepted, as separately reported), but the recovery audit row or its durable handoff could not be preserved. |
| `mcphub autostart enable` / `disable` / `status` | Per-OS autostart shim. Windows: Task Scheduler `LogonTrigger`. Linux managed: systemd user service. Linux unmanaged + macOS: per-OS user-space shim. |
| `mcphub install --upgrade` | Cold-restart upgrade flow. Rename-aside binary replacement (Windows `MoveFileExW`) + atomic rename (POSIX). Issues IPC `quiesce-timers` then `exit{graceful}`; force-kills supervisor with `taskkill /F /T /PID` on timeout; explicitly starts new supervisor. |
| `mcphub install --rollback-to-legacy` | Reverses migration. Translates supervisor-state quarantined entries to `daemon-intent.json` `chronic-failure`; uninstalls supervisor shim; re-registers every v0.4.x `legacy-tasks/<task>.xml` via `schtasks /Create /XML /F`; runs each task and waits up to 60s for the expected port to bind. Unbound ports captured in `rollback-warnings.json`; rollback exits 0 with warnings. |

## State files

All under `<state-dir>` (per-user `%LOCALAPPDATA%\mcp-local-hub\` on Windows;
`$XDG_STATE_HOME/mcp-local-hub` or `~/.local/state/mcp-local-hub` on POSIX):

```text
<state-dir>/
  supervisor-intent.json              # NEW: daemon descriptors + maintenance timers + strict_mode (canonical)
  supervisor-state.json               # NEW: per-daemon runtime state, restart_history (30-min sliding window), transient_pids, maintenance_fired_at
  supervisor-events.log               # NEW: JSONL audit trail (envelope: schema_version, ts, severity, source, event, task_name, body); 16 KB per-entry cap; 10 MB rotation → .log.1
  supervisor-events.log.pending/      # process-exit-safe pending audit rows: <64-lowercase-hex-SHA-256>.jsonl, one normalized record per file, maximum 16 KiB + 1 newline byte
  daemon-recovery-occurrences.json    # compact GUI recovery receipts: exact canonical task + correlation + status/commit evidence and store generation; maximum 64 records
  daemon-recovery-occurrences.json.lock # cross-process serializer for reserve, terminalize, replay, and acknowledgement
  supervisor.lock                     # NEW: supervisor singleton lock + sidecar with {pid, start_time}
  migration-journal-<UTC-ts>/         # NEW: per-install migration journal (retain 5 newest after `committed`)
  daemon-intent.json                  # preserved exactly (byte-symmetric for rollback)
  managed-entries.json                # preserved exactly
  watchdog-state.json                 # preserved unchanged (v0.4.x watchdog diagnostics)
```

Every supervisor-event write checks at most 64 final pending files after it
acquires the existing in-process event-log mutex and cross-process flock, and
before rotating or appending the current row. Replay compares the complete
newline-terminated record against both `supervisor-events.log` and
`supervisor-events.log.1`. An exact retained match retires the handoff without
another append; otherwise replay appends and syncs the row before retirement.
Malformed, oversized, unreadable, mismatched, unappendable, or unremovable
handoffs remain in the pending directory and fail that replay pass.

This handoff provides a process-exit-safe carrier and exactly one retained row
within the active-plus-`.1` history. It does not claim power-loss durability or
exactly-once identity after the row rotates beyond that bounded history.

## GUI recovery receipt safety

The graphical user interface (GUI) recovery route extends the existing backend
recovery owner without becoming a second execution or storage owner. The
frontend posts the raw task name unchanged. The route performs the existing
one-time canonicalization, then requires a complete version-4 Universally Unique
Identifier (UUID) correlation before it can reserve a receipt and invoke daemon
recovery. The receipt store treats the canonical task name as opaque; it does
not normalize again.

The route durably reserves `in_flight` before the destructive call. It records
one of `committed_success`, `committed_error`, `not_committed`, or `uncertain`
from the backend's explicit `termination_committed` result. If terminal storage
fails after the backend call, a generation-keyed in-memory overlay is installed
under the store mutex before same-process readers can run. Lookup, exact replay,
snapshot, and acknowledgement all resolve the same effective `uncertain`
receipt. One inode-anchored read may prove that the exact terminal record is
already durable; the route never retries the terminal write or the destructive
operation. A restart converts a leftover durable `in_flight` receipt to
`uncertain`; the registry never replays an operation. Generation
comparison-and-swap prevents a late completion from an older store generation
from relabeling newer state. The registry is bounded to 64 records and 1 MiB,
and only a final acknowledgement clears it and rotates the server-instance UUID.

Schema version 1 validates status, authorization, backend recovery evidence,
and persisted HTTP error codes against their finite owning enums. An unknown
value fails closed during startup decode and before an active write; the invalid
file is not rewritten. Reverting the reader or Dashboard behavior must not
delete the occurrence store or turn an unresolved receipt into permission to
retry. The accepted invariant is recorded in
[`2026-07-30-daemon-recovery-occurrence-fence`](../work-items/decisions/2026-07-30-daemon-recovery-occurrence-fence.md).

Server-Sent Events are transition-only and lossy, so the Dashboard also uses
bounded `GET /api/daemon/recover/audit-lock-state` reconciliation on mount, stream
open/reconnect, foreground visibility, and every 60 seconds. Each read has an
eight second timeout; latest-issued and monotonic revision checks reject stale
responses. Reconciliation can acknowledge a receipt after a fresh daemon-status
read, but it never repeats the destructive `POST`. A committed-error or
uncertain receipt requires an explicit operator acknowledgement. The Dashboard
fence is keyed by canonical task: an unresolved receipt blocks another recovery
for that task, while a different eligible task may proceed through the same
backend reservation checks. There is no Dashboard-wide recovery veto; the
durable backend task binding remains authoritative.

## Migration from v0.4.x

`mcphub install --upgrade` runs a two-phase journaled migration on Windows
v0.4.x hosts:

1. Enumerates every `mcp-local-hub-*` Task Scheduler task via
   `scheduler.EnumerateAllMcphubTasks()` (regardless of Run As).
2. Renders a canonical-template-snapshot via a v0.4.x-pinned template
   renderer (`internal/migration/v04x_template_defaults.go`) and classifies
   each task's XML as default-match, known-deviation, or hard-deviation.
   Hard deviations abort unless `--discard-scheduler-customizations` is set.
3. Resolves each daemon's PID via `lookupProcessIdentity(port)` (PowerShell
   `Get-CimInstance Win32_Process` with `wmic.exe` fallback for pre-24H2
   hosts), then 4-gate ownership check (image basename, CommandLine, start
   time pre-migration.lock, ExecutablePath under install-dir).
4. `taskkill /F /T /PID` each ownership-verified daemon, drops the
   `pre-os-mutating` marker on first kill.
5. Unregisters each legacy task, installs the supervisor autostart shim,
   explicitly starts the supervisor, waits for reconcile-ready via IPC
   `status` within 30s. On timeout: auto-rollback.

Migration is journaled at every state transition (`prepared` →
`pre-os-mutating` → `os-mutating-complete` → `committed`). The journal
retains forward-progress for crash-resume and rollback evidence.

`mcphub install --rollback-to-legacy` reverses migration from any
`committed` or `pre-os-mutating` marker by re-registering preserved
`legacy-tasks/<task>.xml` and translating supervisor quarantined entries
back to `daemon-intent.json` `chronic-failure`. Token-mismatch pre-flight
catches "rollback caller token is less privileged than supervisor process"
early (exit 13).

## Per-OS behavior matrix

| OS / mode | Job Object support | Restart policy | Autostart backend | Cold-start reaper |
|---|---|---|---|---|
| **Windows** | Yes — `JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE` + `PROC_THREAD_ATTRIBUTE_JOB_LIST` at create-time | Supervisor in-process state machine, persisted to `supervisor-state.json` | Task Scheduler `LogonTrigger` (one entry: the autostart shim) | Not needed (Job Object reaps every child on supervisor exit) |
| **Linux managed** | n/a (cgroup-based via systemd) | Supervisor in-process + systemd `Restart=on-failure` for supervisor itself | systemd user service with `KillMode=control-group` | Not needed (cgroup termination is atomic) |
| **Linux unmanaged** | n/a | Supervisor in-process; `PR_SET_PDEATHSIG` direct-child containment (double-fork OUT OF SCOPE) | None (manual `mcphub supervise &`) | Yes — supervisor sweeps stale `mcphub.exe daemon` children on start, 2-3s settling between reaps for TCP TIME_WAIT |
| **macOS managed** | n/a | Supervisor in-process + LaunchAgent `KeepAlive` for supervisor itself | LaunchAgent (restart-after-exit only; NOT containment) | Yes — same as Linux unmanaged |
| **macOS unmanaged** | n/a | Supervisor in-process; process group + kqueue `EVFILT_PROC NOTE_EXIT` observation (NOT containment) | None | Yes — same as Linux unmanaged |

Full design + invariants live in
[`docs/superpowers/specs/2026-05-16-v0.5.0-supervisor-architecture.md`](superpowers/specs/2026-05-16-v0.5.0-supervisor-architecture.md).

## Terms and Abbreviations

- **GUI** — Graphical user interface.
- **IPC** — Inter-process communication.
- **SSE** — Server-Sent Events.
- **UUID** — Universally Unique Identifier.

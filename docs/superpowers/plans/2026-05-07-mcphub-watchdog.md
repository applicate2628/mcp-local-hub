# mcphub watchdog — implementation plan (v13)

> **For agentic workers:** Use `superpowers:subagent-driven-development` (recommended) to implement task-by-task. Steps use checkbox (`- [ ]`) syntax.

**Revised** 2026-05-07 after **five** rounds of Codex code-review + security-review:

- v1 → v2: `.scratch/codex-review-watchdog-plan.md`, `.scratch/codex-security-watchdog-plan.md`
- v2 → v3: `.scratch/codex-review-watchdog-plan-v2.md`, `.scratch/codex-security-watchdog-plan-v2.md`
- v3 → v4: `.scratch/codex-review-watchdog-plan-v3.md`, `.scratch/codex-security-watchdog-plan-v3.md`
- v4 → v5: `.scratch/codex-review-watchdog-plan-v4.md`, `.scratch/codex-security-watchdog-plan-v4.md`
- v5 → v6: `.scratch/codex-review-watchdog-plan-v5.md`, `.scratch/codex-security-watchdog-plan-v5.md`
- v6 → v7: `.scratch/codex-review-watchdog-plan-v6.md`, `.scratch/codex-security-watchdog-plan-v6.md`
- v7 → v8: `.scratch/codex-review-watchdog-plan-v7.md`, `.scratch/codex-security-watchdog-plan-v7.md`
- v8 → v9: `.scratch/codex-review-watchdog-plan-v8.md`, `.scratch/codex-security-watchdog-plan-v8.md`
- v9 → v10: `.scratch/codex-review-watchdog-plan-v9.md`, `.scratch/codex-security-watchdog-plan-v9.md`
- v10 → v11: `.scratch/codex-review-watchdog-plan-v10.md`, `.scratch/codex-security-watchdog-plan-v10.md`
- v11 → v12: `.scratch/codex-review-watchdog-plan-v11.md`, `.scratch/codex-security-watchdog-plan-v11.md`
- v12 → v13: `.scratch/codex-review-watchdog-plan-v12.md`, `.scratch/codex-security-watchdog-plan-v12.md`

**Round 11 status:** Security review #11 = **APPROVE**. Code review #11 = REVISE for cross-section text inconsistencies. v12 closed those.
**Round 12 status:** Security review #12 = **APPROVE** ("v12 is text-sync only and introduces no new security regression."). Code review #12 = REVISE for ONE remaining straggler — §9 driver pseudocode raw-string call. v13 fixes that single straggler + cleans up Out-of-scope wording consistency.

Per user directive every finding (CRITICAL through LOW + Part C) is closed. v13 incorporates every v12-round finding.

**Goal:** Add an owned auto-recovery layer for daemons that Windows Task Scheduler `RestartOnFailure` can't reliably restore (force-killed via Task Manager, processes started via `schtasks /Run` outside trigger context). The watchdog is a separate scheduled task running `mcphub watchdog --once` every 5 minutes that detects dead daemons and revives them — gated by an intent file, persistent cooldown/backoff with durable restart-pending state, security-validated task XML, an orphan-task filter, and a wall-clock jump detector. Self-quarantines after 4 corrupt-state ticks within a 30-minute sliding window.

**Architecture:** Cross-platform-ready Go state machine (`internal/api/recovery.go`) + run-once CLI (`internal/cli/watchdog.go`) + Windows-specific scheduled task installer. State persists in four files under per-user app-data:

- `daemon-intent.json` — desired state per daemon (running/stopped + reason + UTC timestamp)
- `watchdog-state.json` — cooldown + backoff + restart-pending + persisted wall-clock seen + corrupt-strike sliding window
- `intent-audit.log` — JSON Lines audit log of intent file writes (with `audit-rotated` self-event)
- `watchdog.log` — JSON Lines decision log

All state files fail-closed on corruption (quarantine + suppress, never silent reset). All quarantine files are pruned to 5 newest **after** rename, under the same flock; **prune failure is non-fatal** (logged + retried next tick). After 4 corrupt-state ticks within a 30-minute sliding window, the watchdog self-quarantines (uninstalls its own scheduled task; manual `mcphub watchdog install` resumes — operator must verify state files are clean first). Recovery is a strictly pure function (no I/O, no Cooldown mutation); the CLI driver performs all writes AND all Cooldown mutations, **persisting state BEFORE invoking restart** (durability against mid-restart kill).

Per-entry log size capped at 16KB to prevent disk-exhaustion via malicious task names. Audit-tail in `mcphub watchdog status` redacts `caller_user` if the caller is not the file owner.

**Branch:** `fix/v0.3.0-blockers` (continuation of bug-fix branch). Builds on commit `f094614` (bug #1 fix).

---

## File structure

| Path | Responsibility |
|---|---|
| `internal/api/recovery.go` | Pure-Go state machine. `RecoverStoppedDaemons(now, status, intent, cool, validator, registry) []RecoveryDecision`. Strictly pure. Defines exported `IsRealFailure(int32) bool`. NEW. |
| `internal/api/recovery_test.go` | Unit tests with fake clock + injected interfaces. NEW. |
| `internal/api/daemon_intent.go` | Atomic JSON intent state. Three-state read (missing/corrupt/valid), TTL with clock-skew, fail-closed quarantine, mixed-bootstrap. UTC-only. Quarantine cap = 5 (post-rename, under flock; prune failure non-fatal). 16KB per-entry size cap. NEW. |
| `internal/api/daemon_intent_test.go` | Roundtrip + concurrent + corrupt-quarantine + missing-bootstrap + mixed-bootstrap + TTL + clock-skew + invalid-UTF8 + quarantine-pruning + flock + size-cap tests. NEW. |
| `internal/api/intent_audit.go` | Append-only JSON Lines audit log. Records `caller_exe`, `caller_pid`, `caller_start_time` (UTC RFC3339Nano per-OS), `caller_user`. 10MB rotation; **`audit-rotated` self-event with idempotent retry on rotation-write failure**. 16KB per-entry cap. NEW. |
| `internal/api/intent_audit_test.go` | Append + rotation + idempotent-rotation-retry + caller-fields + redaction + retention + invalid-UTF8 + size-cap tests. NEW. |
| `internal/api/watchdog_state.go` | Cooldown + restart-pending (clock-injected) + persisted wall-clock + corrupt-strike sliding 30-min window. Fail-CLOSED. Quarantine cap = 5 (post-rename, prune non-fatal). NEW. |
| `internal/api/watchdog_state_test.go` | Cooldown persistence + reset-after-Running + chronic + corrupt-quarantine + wall-clock-jump + corrupt-strike-sliding-window + restart-pending-with-injected-now + restart-pending-stale-clear-on-read tests. NEW. |
| `internal/api/watchdog_xml_validator.go` | `ValidateOwnedTaskXML(name) error` — exports task XML with `io.LimitReader` (64KB+1) + DOCTYPE rejection + depth cap (32) + context deadline (2s). Asserts: name, command, args, principal. NO cache. NEW. |
| `internal/api/watchdog_xml_validator_test.go` | Each negation + malformed-XML + billion-laughs + deep-nested + oversize + DOCTYPE-injection + schtasks-timeout. NEW. |
| `internal/api/state_paths.go` | `DaemonStateDir() (string, error)`. Windows: `SHGetKnownFolderPath(FOLDERID_LocalAppData)` **production fail-closed if API fails**; env fallback compiled in only via `_test.go` build tag. POSIX sanity: rejects world-writable/non-owned/group-writable parents. Sets dir 0700, files 0600. NEW. |
| `internal/api/state_paths_test.go` | Per-platform path resolution + permissions + world-writable-rejection (POSIX) + KnownFolder build-tag-test-only stub (Windows). NEW. |
| `internal/api/api_surfaces.go` | New ctx-aware API surfaces: `RestartContext(ctx, server, daemonFilter)` (general-purpose; reads manifest fresh), **`RestartContextWithSnapshot(ctx, server, daemonFilter, snap OwnershipSnapshot)` (v10 §59 — watchdog-only; consumes frozen snap including PortMap; documented as one-tick-scope-only; not for general callers)**, `WaitDaemonRunning(ctx, taskName) bool`, `IntentStillRunning(taskName, now)`, `LoadDaemonRegistry() DaemonRegistry`, `LoadOwnershipSnapshot() OwnershipSnapshot` (§47), **`NewOwnedXMLValidatorFromSnapshot(snap OwnershipSnapshot) OwnedXMLValidator` (v10 §47 — wraps snap so structural ownership checks are tick-stable)**, `StatusContext(ctx) ([]DaemonStatus, error)`, `UninstallWatchdogTaskInternal(reason SelfQuarantineReason) error` (typed enum; called only by self-quarantine; writes audit `Action="watchdog-self-quarantined"` with `Reason=<enum-value>` per §63), `UninstallWatchdogTask() error` (public; interactive confirm + `--yes` per §64; called by `mcphub watchdog uninstall`). Each ctx-aware fn uses goroutine + ctx-select pattern (best-effort cancellation; underlying op continues until ctx-deadline). NEW. |
| `internal/api/api_surfaces_test.go` | Each new surface has its own failing test before implementation. Includes ctx-cancellation tests. NEW. |
| `internal/cli/watchdog.go` | All `mcphub watchdog ...` subcommands. Implements ctx-deadline 4min, restart-budget guard, restart-verify (observation-only), corrupt-strike accumulation, self-quarantine, singleton lock with PID/timestamp stale-detection, 16KB log entry cap. NEW. |
| `internal/cli/watchdog_test.go` | CLI matrix + JSON Lines escape (control + invalid UTF-8 + 16KB cap) + ctx-deadline + restart-budget + corrupt-strike-self-quarantine + singleton-lock-with-stale-detection + audit-tail-redaction tests. NEW. |
| `internal/scheduler/scheduler_windows.go` | Existing `IgnoreNew → StopExisting` (Task 5). MODIFY. + new `buildWatchdogXML`. |
| `internal/scheduler/scheduler_windows_test.go` | StopExisting regression + watchdog-XML shape tests. MODIFY. |
| `internal/api/install.go` | `Install/Restart/Uninstall` write intent + audit. MODIFY. |
| `internal/api/stop.go` (locate via `grep -n "func.*Stop(" internal/api/`) | `Stop` writes intent BEFORE kill; fail-closed if intent OR audit fails. MODIFY. |
| `internal/api/register.go` | Write intent ONLY AFTER successful scheduler create+run+readiness. MODIFY. |
| `internal/cli/setup.go` (locate) | Auto-install watchdog task during `mcphub setup`. Idempotent. MODIFY. |
| `internal/cli/uninstall.go` (locate) | Disable/delete watchdog FIRST, then mark daemons stopped, then delete daemon tasks. MODIFY. |
| `internal/tray/state.go` | Replace local `isRealFailure` with `api.IsRealFailure`. MODIFY (delete duplicate). |
| `internal/cli/gui_tray_state.go` | Replace duplicate failure logic with `api.IsRealFailure`. MODIFY (delete duplicate). |
| `docs/phase-3b-ii-verification.md` | D2.4 update + new D2.6 watchdog smoke. MODIFY. |
| `CLAUDE.md` | Watchdog architecture + state files + log paths + disable/install instructions + audit retention + reinstall-after-self-quarantine note + per-entry size cap. MODIFY. |

**Removed in v6 from v5:** `manifest_hash.go` extension and the `ComputeManifestHashFromDisk` mechanism (§30 in v5). Rationale: existing `Restart()` reads manifest only for port discovery (kill target), not for task XML reconstruction. The XML validator (§5) is the security boundary against task tampering. Manifest hash didn't add a defense layer the validator doesn't already provide and created coherence problems with embed-first per-server manifests.

---

## Critical Codex-review-driven design decisions (v6)

### 1. Recovery trigger state — `Ready` AND `Stopped` both qualify

```go
for row := range status:
  if isMaintenanceTaskName(row.TaskName)              { yield "maintenance"; continue }
  if !registry.IsManagedDaemon(row.TaskName)          { yield "orphan"; continue }
  if row.IsWorkspaceScoped && row.Lifecycle == LifecycleFailed: yield "lazy-proxy-failed-lifecycle"; continue
  if row.State == "Running"                           { continue }   // pure func skips silently
  if !(row.State == "Ready" || row.State == "Stopped" || row.State == "Failed"): continue
  if !IsRealFailure(row.LastResult)                    { continue }

  active, reason := intent.Tasks[row.TaskName].IsActiveStop(now)
  if active                                             { yield reason; continue }

  if cool.IsRestartPending(row.TaskName, now)           { yield "restart-pending-skipped"; continue }
  if !cool.Due(row.TaskName, now)                       { yield "cooldown"; continue }
  if cool.ChronicLimitReached(row.TaskName)             { yield "chronic-failure"; continue }
  if !validator.IsOwnedAndValid(row.TaskName)           { yield "suspicious-xml"; continue }

  yield "restart" with Server, Daemon, Attempt
```

Decision actions (returned by pure `RecoverStoppedDaemons`):

- `restart` — driver: `MarkRestartPending(name, now)` → `RecordAttempt(name, now)` → **`WriteWatchdogState(...)` to durably persist** (§31 fix) → `RestartContext(ctx, ...)` → log + `ClearRestartPending(name)`.
- `chronic-failure` — write intent + audit; do not modify cooldown
- `suspicious-xml` — high-priority watchdog.log; no other action
- `cooldown`, `user-stop`, `user-disabled`, `uninstalled`, `maintenance`, `orphan`, `lazy-proxy-failed-lifecycle`, `clock-skew-future-suspect`, `restart-pending-skipped`, `partial-bootstrap` — informational

The healthy-Running cooldown reset is a separate driver iteration (`for row.State == "Running": cool.RecordRunning(name, now)`).

### 2. Use existing `DaemonStatus`

### 3. Intent file three-state semantics + mixed-bootstrap

Identical to v5 §3. Quarantine cap 5 post-rename under flock; prune failure non-fatal (logged in `watchdog.log` as `quarantine-prune-failed-non-fatal`, retried next tick).

### 4. Intent file TTL with clock-skew detection

Identical to v5.

### 5. Task XML validation BEFORE `schtasks /Run` — hardened + structural ownership (Code Review #6 IMPORTANT + Security #6 MED)

**v7 closes the manifest-trust gap left by v6 §10:** the validator now also enforces a structural task-name ↔ ownership mapping that does NOT depend on the (potentially tampered) manifest.

The XML validator passes (returns nil) only if ALL of the following hold:

1. **Name pattern + structural ownership (v7-8):**
   - `\mcp-local-hub-{server}-{daemon}` (global): `{server}` MUST appear in `manifest.ListServers()` AND the loaded `config.ServerManifest.Daemons []DaemonSpec` slice MUST contain a `DaemonSpec.Name == {daemon}` entry. (Plan implements `manifestHasServerDaemon` by deriving a `map[string]bool` from the slice once per validation: `for _, d := range manifest.Daemons { byName[d.Name] = true }`.) The validated XML's `<Arguments>` MUST include `--server {server} --daemon {daemon}` for that exact pair.
   - `\mcp-local-hub-lsp-{wskey}-{lang}` (lazy proxy): `{wskey}-{lang}` MUST match an entry in the workspace registry (`internal/api/workspace_registry.go::Get(wskey, lang)`); the registry entry's `TaskName` field MUST equal the validated task's name byte-for-byte. The validated XML's `<Arguments>` MUST include `daemon workspace-proxy ...` shape.
   - `\mcp-local-hub-watchdog`, `\mcp-local-hub-workspace-weekly-refresh`: maintenance tasks; recovery filter (§21) skips them, but the structural validator still asserts the canonical exec args.
   - Anything else with `\mcp-local-hub-*` prefix → `ErrUnstructuredOwnership` (rejected).

2. Strict XML hardening (v6 §5 unchanged): 64KB+1 oversize, depth ≤32, DOCTYPE rejected, 2s ctx, Strict + Entity=nil + CharsetReader=nil.

3. Field assertions (v6 §5 unchanged): Command = `canonicalMcphubPath`, Args shape, UserId = current user, RunLevel=Limited, LogonType=Interactive.

This closes Code Review #6 IMPORTANT (manifest-trust gap): the XML validator alone is now sufficient to rule out tampered-manifest-driven misrouting. `Restart()`'s use of `manifestPortMap` for kill discovery still reads the manifest, BUT the watchdog has already verified — via the validator — that the task targeted for restart is structurally bound to a real manifest+registry entry. A fully-tampered manifest that adds a phantom server wouldn't pass the structural check (the task name's server/daemon parts wouldn't resolve).

`Status()` alone is NOT a trust source for `IsManagedDaemon`; the registry from `LoadDaemonRegistry()` (§32) AND the structural validator BOTH must agree.

```go
const (
    xmlSizeLimit    = 64 * 1024
    xmlDepthLimit   = 32
    schtasksTimeout = 2 * time.Second
)

func ValidateOwnedTaskXML(taskName string) error {
    // Structural ownership (v7): name MUST resolve before we even export XML.
    ownership, err := classifyOwnedTaskName(taskName)  // returns "global"|"lazy-proxy"|"maintenance"|err
    if err != nil { return err }   // not an mcp-local-hub-* prefix → ErrNotOwnedTask

    ctx, cancel := context.WithTimeout(context.Background(), schtasksTimeout)
    defer cancel()
    raw, err := schtasksQueryXML(ctx, taskName)
    if err != nil { return fmt.Errorf("schtasks query: %w", err) }

    buf := make([]byte, xmlSizeLimit+1)
    n, _ := io.ReadFull(io.LimitReader(bytes.NewReader(raw), int64(xmlSizeLimit)+1), buf)
    if n > xmlSizeLimit { return ErrXMLOversize }
    raw = buf[:n]

    if bytes.Contains(bytes.ToLower(raw), []byte("<!doctype")) { return ErrXMLDoctypeRejected }

    dec := xml.NewDecoder(bytes.NewReader(raw))
    dec.Strict = true
    dec.Entity = nil
    dec.CharsetReader = nil
    if err := decodeWithDepthLimit(dec, xmlDepthLimit); err != nil { return ErrXMLMalformed }

    cmd, args, userId, runLevel, logonType := extractFields(raw)
    if !filepath.SameFile(cmd, canonicalMcphubPath()) { return ErrCommandMismatch }
    if !sameWindowsUser(userId)                       { return ErrPrincipalMismatch }
    if runLevel != "Limited"                          { return ErrUnexpectedRunLevel }
    if logonType != "Interactive"                     { return ErrUnexpectedLogonType }

    // Structural arg verification (v7) — depends on ownership classification:
    switch ownership.Kind {
    case "global":
        // Args must contain --server X --daemon Y where X+Y match the parsed task name AND manifest entry
        if !globalArgsMatch(args, ownership.Server, ownership.Daemon) { return ErrArgsMismatch }
        if !manifestHasServerDaemon(ownership.Server, ownership.Daemon) { return ErrUnstructuredOwnership }
    case "lazy-proxy":
        // Args must contain "daemon workspace-proxy" shape AND wskey+lang must resolve in workspace registry
        if !lazyProxyArgsMatch(args) { return ErrArgsMismatch }
        if !workspaceRegistryHas(ownership.WorkspaceKey, ownership.Language, taskName) {
            return ErrUnstructuredOwnership
        }
    case "maintenance":
        if !maintenanceArgsMatch(args, ownership.MaintenanceKind) { return ErrArgsMismatch }
    }
    return nil
}
```

Tests for new ownership cases:
- Global task name with server NOT in manifest → `ErrUnstructuredOwnership`
- Lazy-proxy task name with wskey NOT in workspace registry → `ErrUnstructuredOwnership`
- Lazy-proxy task name with wskey in registry but `TaskName` field byte-mismatched → `ErrUnstructuredOwnership`
- Adversary registers `\mcp-local-hub-fake-default` as a real TS task pointing at attacker.exe → ownership classify passes (matches `\mcp-local-hub-{server}-{daemon}` regex) BUT `manifestHasServerDaemon("fake", "default")` returns false → `ErrUnstructuredOwnership`. Defense layer.

### 5b. Task XML validation legacy header (no cache, original)

Identical to v5. No cache; ~150 schtasks/hr per task accepted.

### 6. Backoff math — T+15 inclusive

Identical to v5.

### 7. Hidden=false

### 8. Stop fail-closed with audit

Identical to v5.

### 9. Log injection mitigation — JSON Lines + 16KB cap with identity preservation (Security #5 Part C + Code Review #7 stale-text fix)

`watchdog.log` and `intent-audit.log` JSON Lines. Each entry passed through `json.Marshal`. **Per-entry size cap = 16KB.** Identity fields (`task`, `task_name`, `caller_user`) are NEVER truncated (see §35). For oversized identity fields the entry is REJECTED with `ErrIdentityOversize`; for oversized non-identity fields (`err`, `note`) the longest non-identity string is truncated, marked with `_truncated:true` + `_truncated_field:<name>` + `_task_hash:<sha256-12hex>`. Defense vs disk-exhaustion via malicious task names; identity preservation maintains forensic correlation.

### 10. Bootstrap policy + orphan filter (NO manifest hash in v6)

1. Build managed daemon set from `Status()` + `manifest.go::ListServers()`.
2. Tasks not in managed set → `orphan-not-managed`.
3. **No manifest-hash mid-tick recheck (v6 removes v5 §30).** XML validator (§5) is the security gate against task tampering. The manifest is read only at registry-build time (snapshot once per `--once`); driver uses an immutable defensive copy of registry data (Security #5 LOW `LoadDaemonRegistry`).

### 11. Concurrency-safe intent re-read

Driver re-reads intent inside critical section before each restart. `mcphub stop` writes intent BEFORE kill.

### 12. Strict purity of `RecoverStoppedDaemons` + `IsRestartPending(name, now)` (Code Review #5 IMPORTANT 3)

```go
func RecoverStoppedDaemons(
    now       time.Time,
    status    []DaemonStatus,
    intent    DaemonIntentFile,
    cool      CooldownReader,
    validator OwnedXMLValidator,
    registry  DaemonRegistry,
) []RecoveryDecision   // PURE
```

```go
type CooldownReader interface {
    Due(taskName string, now time.Time) bool
    ChronicLimitReached(taskName string) bool
    AttemptsInWindow(taskName string) int
    IsRestartPending(taskName string, now time.Time) bool   // v6 fix: now injected, no time.Since
}

type Cooldown interface {
    CooldownReader
    RecordAttempt(taskName string, now time.Time)
    RecordRunning(taskName string, now time.Time)
    MarkRestartPending(taskName string, now time.Time)
    ClearRestartPending(taskName string)
}
```

`IsRestartPending(name, now)` returns:

- `false` if `RestartPendingAt.IsZero()`
- `false` if `now.Sub(RestartPendingAt) > 6*time.Minute` (stale — caller MUST clear via `ClearRestartPending` on next mutation; **also surfaces the stale-clear in `mcphub watchdog status` with `restart-pending-stale-cleared-by-{taskName}` event** per Security #5 MEDIUM 3)
- `true` otherwise

The driver's `WriteWatchdogState` ALSO clears stale pending (`now - RestartPendingAt > 6min`) before serializing, ensuring stale entries don't accumulate in the file.

### 13. Cooldown corrupt = fail-CLOSED

Identical to v5.

### 14. `--once` ctx deadline + restart-budget guard + verify-only + best-effort ctx propagation (Code Review #5 IMPORTANT 4)

OS-level `ExecutionTimeLimit=PT5M`; app-level `context.WithTimeout(ctx, 4*time.Minute)`.

**Restart-budget guard:** before each restart, check `time.Until(deadline) >= 60s`.

**Restart-verify is observation-only:** no `RecordRunning` from verify. §6 5-min reset rule preserved via healthy-Running iteration.

**Best-effort ctx propagation (v6 fix for code review #5 IMPORTANT 4):** `RestartContext`, `StatusContext`, `WaitDaemonRunning` all use the goroutine + ctx-select pattern:

```go
func StatusContext(ctx context.Context) ([]DaemonStatus, error) {
    type result struct {
        rows []DaemonStatus
        err  error
    }
    ch := make(chan result, 1)
    go func() {
        rows, err := api.Status()
        ch <- result{rows, err}
    }()
    select {
    case r := <-ch:
        return r.rows, r.err
    case <-ctx.Done():
        return nil, ctx.Err()
    }
}
```

Documented limitation: cancellation returns ctx err to caller but the underlying `Status()` / `Restart()` / `schtasks` invocation continues to completion (fire-and-forget on cancel). For v0.3.0 this is acceptable: the OS-level 5-min `ExecutionTimeLimit` ensures the watchdog process is killed if the underlying op hangs longer than ctx allows. Future improvement: refactor underlying scheduler ops to be context-aware (out of scope). Documented in Out-of-scope.

### 15. POSIX file permissions

Identical to v5.

### 16. State path sanity + Windows KnownFolder API — production fail-closed (Security #5 MEDIUM 1)

Windows production resolution uses ONLY `SHGetKnownFolderPath(FOLDERID_LocalAppData)`. **If the API fails in production, exit 8.** The env fallback (`LOCALAPPDATA`, `USERPROFILE\AppData\Local`) is gated behind a `_test.go` build tag — compiled into test binaries only. Production binary calls a non-fallback `daemonStateDirProduction()`; test binary overrides with `daemonStateDirWithEnvFallback()`.

```go
//go:build !test_state_path_env

func daemonStateDir() (string, error) {
    if runtime.GOOS == "windows" {
        return knownFolderLocalAppData()  // exit 8 on failure
    }
    // ... POSIX path
}
```

```go
//go:build test_state_path_env

func daemonStateDir() (string, error) {
    if runtime.GOOS == "windows" {
        if d, err := knownFolderLocalAppData(); err == nil { return d, nil }
        // Fallback only in test build:
        if d := os.Getenv("LOCALAPPDATA"); d != "" { return d, nil }
        if up := os.Getenv("USERPROFILE"); up != "" { return filepath.Join(up, "AppData", "Local"), nil }
        return "", errKnownFolderUnavailable
    }
    // ... POSIX path
}
```

Tests run with `-tags=test_state_path_env`. Production build excludes the fallback path entirely (compile-time guarantee).

### 17. Cross-platform path documentation

For v0.3.0 only Windows watchdog ships.

### 18. `IsRealFailure` exported (single source)

Identical to v5.

### 19. Lazy-proxy `Running + LifecycleFailed` skip

Identical to v5.

### 20. JSON Lines + control + invalid UTF-8 + 16KB cap

`watchdog.log` + `intent-audit.log`: 16KB per-entry. Truncation marker `_truncated:true`. Tests cover both control bytes AND oversize task names.

### 21. Maintenance task filter

```go
func isMaintenanceTaskName(name string) bool {
    return strings.HasSuffix(name, "-watchdog") || strings.HasSuffix(name, "-weekly-refresh")
}
```

### 22. Dependency injection summary

### 23. Quarantine retention cap — post-rename prune under flock; failure non-fatal (Security #5 LOW Prune failure)

Quarantine flow under intent's flock:

```go
// Under flock:
// 1. os.Rename(stateFile, quarantineName)  -- if this fails, abort entire quarantine
// 2. List ${stem}.corrupt-* sorted by mtime DESC
// 3. Keep 5 newest, delete rest
//    - Per-file delete failure is logged as `quarantine-prune-failed-non-fatal`
//      with file path + error; NOT propagated as fatal. Next tick's quarantine
//      attempt re-prunes (idempotent).
```

### 24. Audit-write failure semantic

Identical to v5.

### 25. Caller identity + per-OS conversion

Identical to v5 §25.

### 26. Audit retention + `audit-rotated` self-event with idempotent retry (Security #5 MEDIUM 2)

Same 10MB rotation. `audit-rotated` self-event behavior:

1. Write `intent-audit.log` reaches 10MB → `os.Rename(*.log, *.log.1)`.
2. Open fresh `intent-audit.log`.
3. Attempt to append `audit-rotated` self-event.
4. **If the self-event append fails (disk full, perms):** do NOT re-rename, do NOT retry rotation this tick. Log to `watchdog.log` as `audit-rotated-event-write-failed-non-fatal`. The fresh file exists empty; on the NEXT successful audit append, that append goes through normally. The `audit-rotated` event for THIS rotation is permanently lost, but rotation itself is recorded only in `watchdog.log` for that occurrence.

This avoids:

- Double-rotation (re-renaming a file that's now smaller than the threshold)
- Infinite retry loop on persistent disk-full
- Inconsistent state between filesystem and audit log

### 27. schtasks query rate — accept ~150/hr (no cache)

Identical to v5.

### 28. Corrupt-strike — 30-min sliding window + self-quarantine

Identical to v5.

### 29. Wall-clock jump detection — including missing-baseline-after-corrupt

Identical to v5.

### 30. Restart-pending durability (Code Review #5 IMPORTANT 2 + Security #5 MEDIUM 3)

**v5 issue:** `MarkRestartPending` / `RecordAttempt` were in-memory only until end-of-tick `WriteWatchdogState`. Process kill mid-restart loses the pending marker → next tick double-restarts.

**v6 fix:**

```go
// Driver flow per "restart" decision:
case "restart":
    deadline, ok := ctx.Deadline()
    if ok && time.Until(deadline) < 60*time.Second {
        watchdogLog.Append(d, "ctx-budget-exhausted")
        continue
    }
    if !api.IntentStillRunning(d.TaskName, time.Now().UTC()) {
        watchdogLog.Append(d, "stop-race-aborted")
        continue
    }
    coolR.Cool.MarkRestartPending(d.TaskName, time.Now().UTC())
    coolR.Cool.RecordAttempt(d.TaskName, time.Now().UTC())

    // PERSIST IMMEDIATELY — durability against mid-restart kill
    if err := api.WriteWatchdogState(coolR, time.Now().UTC()); err != nil {
        watchdogLog.Append(d, "pre-restart-state-write-failed: "+err.Error())
        continue
    }

    err := api.RestartContext(ctx, d.Server, d.Daemon)
    coolR.Cool.ClearRestartPending(d.TaskName)
    if err == nil {
        verifyCtx, vc := context.WithTimeout(ctx, 30*time.Second)
        running := api.WaitDaemonRunning(verifyCtx, d.TaskName)
        vc()
        if running {
            watchdogLog.Append(d, "restart-verified-running")
        } else {
            watchdogLog.Append(d, "restart-not-yet-running-after-30s")
        }
    } else {
        watchdogLog.Append(d, errString(err))
    }
```

If process is killed AFTER `WriteWatchdogState` but BEFORE `RestartContext` returns: next tick reads the file, sees `RestartPendingAt != 0` for this task, yields `restart-pending-skipped`. After 6-min TTL (within ~2 ticks at 5-min cadence) the entry ages out and a fresh restart attempt is allowed. Notable: this means a hung restart locks out the daemon from re-attempt for 5-10 minutes — acceptable trade-off for durability.

If `WriteWatchdogState` itself fails BEFORE Restart: do NOT call Restart (logged + skip). Next tick reads stale state and re-evaluates fresh — no double-restart.

### 31. Restart-pending guard with stale clear visibility (Security #5 MEDIUM 3)

`CooldownEntry.RestartPendingAt` zero = not pending. TTL 6 minutes (>= one 5-min cadence). When `IsRestartPending(name, now)` finds a stale (>6min) entry, it returns false; `WriteWatchdogState` clears stale entries during serialization AND **emits an event** to `watchdog.log`:

```json
{"ts":"...","action":"restart-pending-stale-cleared","task":"\\mcp-local-hub-X","pending_at":"...","cleared_at":"...","note":"likely process kill mid-restart on prior tick"}
```

`mcphub watchdog status` surfaces this event in its "recent events" tail — operators see the lockout window.

### 32. New API surfaces — Task 0 with goroutine ctx pattern (Code Review #5 IMPORTANT 4 + Security #5 LOW UninstallWatchdogTask)

| Helper | Existing? | Plan |
|---|---|---|
| `(*API).Status()` | YES (`install.go:368`) | wrap with goroutine + ctx-select pattern as `StatusContext(ctx)` |
| `(*API).Restart(server, filter)` | YES (`install.go:1486`) | wrap as `RestartContext(ctx, server, filter)` (general-purpose; reads manifest fresh) |
| `RestartContextWithSnapshot(ctx, server, filter, snap OwnershipSnapshot)` (v10/v11 §59) | NO | NEW — watchdog-only variant; consumes frozen snap (incl. PortMap) so kill-by-port discovery is tick-stable. **Documented as one-tick-scope-only**: callers must pass a fresh `LoadOwnershipSnapshot()` per tick; reusing a stale snapshot across ticks is incorrect. Tests assert behavior matches snapshot's PortMap, not live manifest. |
| `WaitDaemonRunning(ctx, name) bool` | NO | NEW — polls `StatusContext(ctx)` every 1s until row.State=="Running" or ctx.Done() |
| `IntentStillRunning(name, now) bool` | NO | NEW — wraps `ReadDaemonIntent` + `IsActiveStop`; true iff intent NOT actively stopped |
| `LoadDaemonRegistry() DaemonRegistry` | NO | NEW — reads `Status()` + `manifest.ListServers()` and **freezes a defensive copy** into the returned interface; subsequent `IsManagedDaemon` calls are pure lookups against the snapshot |
| `LoadOwnershipSnapshot() OwnershipSnapshot` (v9/v11 §47, §59) | NO | NEW — immutable snapshot of `{ManifestServers, ManifestDaemons, WorkspaceTasksByKey, PortMap, SnapshottedAt}`; consumed by `NewOwnedXMLValidatorFromSnapshot` and `RestartContextWithSnapshot` |
| `NewOwnedXMLValidatorFromSnapshot(snap OwnershipSnapshot) OwnedXMLValidator` (v10/v11) | NO | NEW — constructor wrapping `snap` so `IsOwnedAndValid(taskName)` does ownership/structural checks against the frozen snapshot, NOT a live manifest read. Defeats mid-tick rotation race. Plain `OwnedXMLValidator` constructed from a fresh manifest read still exists for non-watchdog callers (tests, future tools). |
| `UninstallWatchdogTask() error` | NO | NEW — public API: `schtasks /Delete /TN \\mcp-local-hub-watchdog /F`; idempotent. Used by `mcphub watchdog uninstall`. Per §64 v10/v11: interactive confirm + `--yes` flag + isatty fail-fast at exit 6. |
| `UninstallWatchdogTaskInternal(reason SelfQuarantineReason) error` (v10/v11 — typed enum) | NO | NEW — same operation but typed `reason`; called ONLY by self-quarantine path. Writes audit `Action="watchdog-self-quarantined"` (§63 canonical action filter; not `Action="self-quarantine-{reason}"` — the enum value goes into the `Reason` field instead). |
| `ManifestPath()` | n/a (v6 removed manifest-hash) | NOT NEEDED |

### 33. Singleton lock for manual `--once` with PID/timestamp stale-detection (Security #5 LOW Lock timeout + Part C)

`mcphub watchdog --once` acquires `<state-dir>/--once.lock` flock. Lock file content = JSON `{pid, started_at, hostname}`. Lock-acquire pseudocode:

```go
lock := flock.New(lockPath)
locked, err := lock.TryLock()
if err != nil { /* permission/io error → skip tick, log */ return 1 }
if locked {
    // Write owner info
    os.WriteFile(lockPath+".owner.json", ownerInfoJSON, 0600)
    defer func() { lock.Unlock(); os.Remove(lockPath+".owner.json") }()
} else {
    // Read owner info to log who's holding it
    ownerInfo := readOwnerInfoBestEffort(lockPath+".owner.json")
    watchdogLog.Append("already-running-skipped", ownerInfo)
    return 0
}
```

`mcphub watchdog status` reads `*.owner.json` (best-effort) and surfaces "Last flock skip: PID 1234 from HOST started 2026-05-07T15:20:00Z (4 min ago)" if a recent skip occurred. Operator can manually break a stale lock by deleting `--once.lock` if the recorded PID is dead.

### 34. `mcphub watchdog status` rich observability + audit redaction (Security #5 LOW + Part C)

Output includes:

- Scheduled task installed status (`installed` / `not-installed-self-quarantined`)
- Cadence (5 min)
- LastWallClockSeen with delta
- CorruptStrikeWindow count + age of oldest entry
- Self-quarantined: yes/no + last self-quarantine ts (from audit log)
- Recent events tail (last 20 from `watchdog.log`)
- Audit log tail (last 20 from `intent-audit.log`)

**Redaction rule (Security #5 LOW Audit tail exposure):** for each audit-tail entry, check if `caller_user` matches the OS-current user. If not, replace `caller_user` value with `"<redacted-non-owner>"` in display. The actual file content is NOT redacted — only the status display. (Single-user box: redaction is rarely triggered; multi-user safety net.)

`--json` flag: emits same data as JSON, with same redaction rules.

### 35. 16KB log entry cap (Security #5 Part C)

JSON Lines logs (`watchdog.log`, `intent-audit.log`) cap each entry at 16KB. If marshaled size exceeds:

1. Identify the longest string field (typically `task`, `err`, or `note`).
2. Truncate to fit within budget (allow ~256 bytes for fixed schema overhead).
3. Append `_truncated:true` to the JSON object.
4. Re-marshal and write.

If truncation alone can't fit (e.g., 16KB of fixed schema fields), drop the entry entirely and append a placeholder:

```json
{"ts":"...","action":"log-entry-dropped-oversize","reason":"original entry exceeded 16KB after truncation","_truncated":true}
```

**v7 fix (Code Review #6 MINOR — identity field truncation):** identity fields (`task`, `task_name`, `caller_user`) are NEVER truncated. Instead:

1. **Identity oversize rejection:** if `task` exceeds 1KB (real TS task names are <100 bytes), entry REJECTED with `ErrIdentityOversize`. Caller logs error and skips audit entry.
2. **Non-identity truncation:** if remaining oversize, truncate longest non-identity string field (`err`, `note`); add `_truncated:true` + `_truncated_field:"<field-name>"`.
3. **Always include `_task_hash:"<sha256-first-12-hex>"`** in any entry that gets truncated, so a truncated `err` chain still has a stable identity fingerprint for correlation.

Tests:

- 32KB `task` → REJECTED with `ErrIdentityOversize`.
- 32KB `err`, 100-byte `task` → entry written; `task` intact; `err` truncated; `_truncated:true` + `_truncated_field:"err"` + `_task_hash` present.
- Multi-field oversize → drop with placeholder + `_task_hash`.

### 36. `WriteWatchdogState` ordering — err first, events only on success (Code Review #6 MINOR)

**v6 §9 issue:** driver logged `staleEvents` before checking `WriteWatchdogState` err. If the write failed but events were returned, status would claim a stale clear that didn't persist.

**v7 fix:** `WriteWatchdogState` returns `staleEvents` ONLY on successful write. On failure: returns `(nil, err)`. Driver logs `staleEvents` ONLY after err-check.

```go
func (a *API) WriteWatchdogState(s WatchdogStateRead, now time.Time) (staleEvents []string, err error) {
    // Compute stale-clear events in-memory (mutate copy); do NOT return them yet
    candidateEvents := computeStaleClearEvents(s, now)
    if err := atomicWriteFile(...); err != nil {
        return nil, err   // events NOT returned on failure
    }
    return candidateEvents, nil
}
```

Driver:

```go
events, err := api.WriteWatchdogState(coolR, time.Now().UTC())
if err != nil {
    watchdogLog.Append(d, "pre-restart-state-write-failed: "+err.Error())
    continue
}
// events guaranteed nil if err != nil; safe to log here
for _, ev := range events { watchdogLog.Append("restart-pending-stale-cleared", ev) }
```

### 37. `system_entry` exemption for redaction (Security #6 LOW)

**v6 §34 issue:** redaction logic blindly replaces `caller_user` if not OS owner. The `audit-rotated` self-event has `caller_user="<rotation-system>"` (sentinel). Redacting that yields `<redacted-non-owner>` — meaningless and confusing.

**v7 fix:** add `SystemEntry bool` field to `IntentAuditEntry`. Set true for self-events (rotation, watchdog-self-quarantine). Redaction logic:

```go
func RedactIntentAuditEntryForNonOwner(e IntentAuditEntry) IntentAuditEntry {
    if e.SystemEntry { return e }   // never redact system entries
    if e.CallerUser != currentOSUser() {
        e.CallerUser = "<redacted-non-owner>"
    }
    return e
}
```

Tests: system entry with `caller_user="<rotation-system>"` → unchanged; non-system from another user → redacted.

### 38. `audit-degraded` high-priority log entry (Security #6 LOW audit disk-full)

When `AppendIntentAudit` fails (disk full, perms, etc.), the watchdog AND `mcphub stop` AND other callers (already write the failure to `watchdog.log` per v6 §24) NOW also append a high-priority `audit-degraded` event to `watchdog.log` IF the failure persists across consecutive calls (counter threshold = 3 failures within 30 minutes). The watchdog `status` command surfaces this:

```text
AUDIT DEGRADED: 4 audit-write failures in past 30 min. Last error: <error msg>. Investigate disk space / permissions.
```

This makes silent audit-broken state visible to operators. Counter persisted in `watchdog-state.json::AuditFailureWindow []time.Time` (same sliding-window semantics as `CorruptStrikeWindow`, separate field).

### 39. Reason enum (Security #6 LOW audit reason injection)

**v6 §32 issue:** `UninstallWatchdogTaskInternal(reason string)` accepts arbitrary string. JSON+16KB cap mitigates injection but doesn't constrain semantics.

**v7 fix:** `reason` becomes a typed enum:

```go
type SelfQuarantineReason string

const (
    QuarantineFourStrikes30Min SelfQuarantineReason = "4-strikes-30min"
    // (extension point for future reasons)
)

func (a *API) UninstallWatchdogTaskInternal(reason SelfQuarantineReason) error
```

Audit log writes `Action="watchdog-self-quarantined"` (canonical literal per §63 v11) with `Reason=<enum-value>` (e.g., `Reason="4-strikes-30min"`). The earlier text suggested `Action="self-quarantine-{reason}"` — v12 supersedes that with the canonical contract: action is ALWAYS the literal `"watchdog-self-quarantined"`, and the typed enum value carries semantics in the `Reason` field. **Invariant:** any future self-quarantine variant MUST keep `Action="watchdog-self-quarantined"` so §63 status filter continues to detect it; if a new action label is needed, the §63 status filter MUST be expanded in the same change. Compile-time-typed `SelfQuarantineReason` prevents injection of arbitrary content into `Reason`.

### 40. Owner-info sidecar staleness labeling (Security #6 LOW stale owner)

**v6 §33 issue:** if `os.Remove(lockPath+".owner.json")` fails after lock release, stale owner-info persists. Status display would show a fake "running" PID.

**v7 fix:** `mcphub watchdog status` reads `*.owner.json` AND checks if the underlying lock is currently held (via `flock.New(...).TryLock()` immediately followed by `Unlock()` on success — which would mean lock is NOT currently held, so owner-info is stale). Display:

```text
Last flock skip: PID 1234 from HOST started 2026-05-07T15:20:00Z (4 min ago) [STALE — lock not currently held]
```

vs.

```text
Last flock skip: PID 1234 from HOST started 2026-05-07T15:20:00Z (30 sec ago) [LOCK BUSY]
```

If `--once.lock.owner.json` exists AND lock is NOT busy (TryLock succeeds + Unlock), display includes `[STALE]` marker. Operator can manually break the lock by deleting both files if PID is dead. Tests cover both states.

### 41. POSIX flock advisory-only documentation (Security #6 PARTIALLY in Part A)

**v7 fix:** explicit text added to §33: "POSIX `flock` is advisory — only honored by callers that use the same locking convention. Non-cooperating processes can ignore the lock; this is acceptable since `mcphub` is the only legitimate caller. The lock protects against concurrent `mcphub watchdog --once` invocations only (TS-vs-TS via IgnoreNew, TS-vs-manual + manual-vs-manual via flock)."

### 42. Administrator install refusal (Security #6 Part C)

**v7 fix:** `mcphub watchdog install` (and the auto-install path in `mcphub setup`) refuses to proceed if invoked from a process running with elevated privileges (Administrator on Windows; root/effective-uid==0 on POSIX) UNLESS `--allow-elevated` flag is passed. Rationale: the watchdog runs as a per-user task; installing from elevated context could create a task with the wrong principal, opening privilege escalation surface.

```go
if isElevated() && !allowElevated {
    return fmt.Errorf("mcphub watchdog install must run un-elevated; use --allow-elevated to override")
}
```

Detection:

- Windows: `golang.org/x/sys/windows.GetTokenInformation` checking `TokenElevation`.
- POSIX: `os.Geteuid() == 0`.

### 43. CI must run both default AND `-tags=test_state_path_env` (Code Review #6 MINOR + Security #6 LOW build tag)

**v7 fix:** Task 1.4 explicitly notes that BOTH test runs must pass. Plus `.github/workflows/ci.yml` requires a parallel test step with `-tags=test_state_path_env`. Production `go build` (deployment) MUST run without the tag. Add an explicit assertion test that, when running with default tags, the env-fallback path is unreachable (e.g., a function pointer check or build-tag-conditional smoke).

### 44. Self-failure indicator in `mcphub watchdog status` (Security #6 Part C)

`mcphub watchdog status` reads its OWN scheduled task's `LastResult` via `schtasks /Query /XML /TN \mcp-local-hub-watchdog`. If non-zero AND not in informational range:

```text
WATCHDOG SELF-FAILURE INDICATOR: last run exited 0x1 at 2026-05-07T15:30:00Z. Recent watchdog.log tail:
  ...
```

This gives operators a single command to spot watchdog degradation.

### 45. Stale-clear strike alert (Security #6 LOW pre-restart pending DoS)

**v6 §31 issue:** repeated `restart-pending-stale-cleared` events suggest something is repeatedly killing the watchdog mid-restart (legitimate: machine sleep + wake; adversarial: deliberate DoS).

**v7 fix:** track `StaleClearWindow []time.Time` in `watchdog-state.json` (same sliding-window semantics as `CorruptStrikeWindow`). 4 stale-clears within 30 min → log `stale-clear-strike-alert` to `watchdog.log` (high-priority) AND surface in `mcphub watchdog status`:

```text
STALE-CLEAR STRIKE ALERT: 4 restart-pending-stale-cleared events in past 30 min — investigate process kills.
```

Does NOT trigger self-quarantine (unlike CorruptStrikeWindow); this is observability only.

### 46. Windows flock contention smoke test (Security #6 LOW)

Task 9.1 includes a Windows-specific test: spawn 5 concurrent `mcphub watchdog --once` processes; assert exactly 1 acquires the lock (others log `already-running-skipped`). Validates `gofrs/flock` Windows behavior with `LockFileEx`. Each test process gets a 30-second timeout; if flaky in CI, downgrade to a release-gate manual smoke (Security #7 LOW flock test framing).

### 47. XML validator one-tick snapshot (Security #7 MED 1)

**v7 §5 issue:** the structural ownership checks (`manifestHasServerDaemon`, `workspaceRegistryHas`) re-read manifest+registry on every validation call. If manifest is rotated mid-tick, two consecutive validations (e.g., orphan-filter + restart-time recheck) could see different states.

**v8 fix:** at `--once` start, build an immutable snapshot:

```go
type OwnershipSnapshot struct {
    ManifestServers     map[string]bool                  // server name → present
    ManifestDaemons     map[string]map[string]bool       // server → set of daemon names
    WorkspaceTasksByKey map[string]string                // wskey-lang → registered TaskName
}

func (a *API) LoadOwnershipSnapshot() OwnershipSnapshot { /* defensive copy of slice → map */ }
```

The validator constructor takes `OwnershipSnapshot`; all ownership checks use the snapshot. The snapshot is built once at `--once` start, passed through `RecoverStoppedDaemons` (now also takes the snapshot, alongside `OwnedXMLValidator` which already wraps it). This prevents mid-tick rotation from changing ownership semantics within one tick.

Implementation note: `LoadDaemonRegistry()` from §32 already builds an immutable snapshot for `IsManagedDaemon`; the new `LoadOwnershipSnapshot()` extends the same model to the manifest-daemon and workspace-registry-task-name maps.

### 48. `SystemEntry` sealed constructor (Security #7 MED 3)

**v7 §37 issue:** `IntentAuditEntry.SystemEntry bool` could be set by any caller, weakening redaction.

**v8 fix:** `SystemEntry` field is unexported (Go: `systemEntry bool`). Setters are package-private functions:

```go
// internal/api/intent_audit.go (package api)
type IntentAuditEntry struct {
    // ... public fields ...
    systemEntry bool   // ONLY set via newSystemAuditEntry()
}

// Sealed constructors (package-internal, unexported):
func newSystemAuditEntry(action string, ...) IntentAuditEntry {
    e := IntentAuditEntry{ ... }
    e.systemEntry = true
    return e
}

// Public constructor for normal entries:
func NewIntentAuditEntry(action string, ...) IntentAuditEntry {
    return IntentAuditEntry{ ... }   // systemEntry stays false
}

// Display predicate (used by status):
func (e IntentAuditEntry) IsSystemEntry() bool { return e.systemEntry }
```

Only the rotation path and self-quarantine path (in `internal/api/intent_audit.go` and `internal/api/api_surfaces.go::UninstallWatchdogTaskInternal`) call `newSystemAuditEntry`. External callers (audit log writers in `mcphub stop`, etc.) cannot set `systemEntry=true`. JSON serialization writes lowercased `system_entry` field on output for forensic visibility but cannot be set from JSON input (sealed at struct level via custom `UnmarshalJSON` that ignores `system_entry` — set explicitly in test fixtures only via the sealed constructor).

### 49. `audit-degraded` emergency stderr fallback (Security #7 MED 4)

**v7 §38 issue:** if `watchdog.log` itself is unwritable, `audit-degraded` event is silently lost.

**v8 fix:** on `audit-degraded` write attempt, the write order is:

1. Try `watchdog.log` append. Success → done.
2. Failure → fallback to `os.Stderr.WriteString("[mcphub-watchdog] AUDIT-DEGRADED: <details>\n")`. Stderr is OS-level resource always available.
3. Failure → fallback to `eventlog.Notify` on Windows (Application log, source `mcp-local-hub`); on POSIX, `syslog.LOG_USER|LOG_WARNING` via `log/syslog`.
4. All-paths failure → process state corrupt; exit 10 (new code: emergency-fallback-failed).

`mcphub watchdog status` reads recent stderr captures from the watchdog scheduled task's `LastTaskRun` output (TS captures stdout/stderr to `%LOCALAPPDATA%\Temp\mcp-local-hub-watchdog-*.log` by default) and surfaces any audit-degraded stderr lines.

### 50. Release pipeline env-fallback symbol verification (Security #7 MED 5 + Code Review #6 MINOR)

**v7 §43 issue:** CI runs both default and `-tags=test_state_path_env`, but production builds without symbol-presence verification could accidentally compile in the env fallback if a developer forgets `-tags` discipline.

**v8 fix:** the release pipeline (and CI) explicitly verifies the production binary does NOT contain the env-fallback symbols. Add a CI step:

```yaml
- name: Production binary symbol assertion
  run: |
    go build -o mcphub-prod ./cmd/mcphub
    if go tool nm mcphub-prod | grep -q 'daemonStateDirWithEnvFallback\|test_state_path_env'; then
      echo "FAIL: production binary contains test-only symbols"
      exit 1
    fi
```

Plus an automated test in `state_paths_test.go`:

```go
//go:build !test_state_path_env

func TestProduction_NoEnvFallbackSymbol(t *testing.T) {
    // Compile a binary; assert symbol absent.
    // Use `go list -f '{{.Dir}}'` + `go tool nm` via os/exec.
}
```

Release gate documented in `docs/release-checklist.md` (NEW) as a required pre-tag manual verification.

### 51. `ErrIdentityOversize` triggers `mcphub stop` fail-closed (Security #7 MED 6)

**v7 §35 issue:** when `AppendIntentAudit` returns `ErrIdentityOversize`, plan said "caller logs error and skips audit entry," but didn't define semantics for `mcphub stop` which has fail-closed-on-audit-failure (§24).

**v8 fix:** `ErrIdentityOversize` IS classified as an audit failure for §24 fail-closed purposes:

| Cmd | `ErrIdentityOversize` from audit | Outcome |
|---|---|---|
| `mcphub stop` (without `--force`) | counts as audit fail | fail-closed; do NOT kill |
| `mcphub stop --force` | counts as audit fail | fail-closed (per §8 v6 rule: both intent + audit must succeed) |
| `mcphub install` | counts as audit fail | **fail-closed (v10 §62):** install rejected with diagnostic; rollback any partially-created scheduler entry |
| watchdog driver chronic-failure | counts as audit fail | log + skip Restart (next tick re-attempts) |

Plan §8 + §24 + Task 11.1 tests must explicitly cover `ErrIdentityOversize` paths for `stop`/`stop --force`.

### 52. Stale text fixes — Task 3, Task 4, §32 (Code Review #7 IMPORTANT 2)

Code Review #7 caught stale plan sections inconsistent with v7 fix-blocks:

**Task 3 redaction** must include `IsSystemEntry()` exemption (per §37/§48):

```go
func RedactIntentAuditEntryForNonOwner(e IntentAuditEntry) IntentAuditEntry {
    if e.IsSystemEntry() { return e }   // §48 sealed constructor exemption
    if e.CallerUser != currentOSUser() {
        e.CallerUser = "<redacted-non-owner>"
    }
    return e
}
```

**Task 4 schema** (`watchdog_state.go`) v8 includes:

```go
type WatchdogState struct {
    Cooldowns           map[string]CooldownEntry `json:"cooldowns"`
    LastWallClockSeen   time.Time                `json:"last_wall_clock_seen"`
    CorruptStrikeWindow []time.Time              `json:"corrupt_strike_window"`
    AuditFailureWindow  []time.Time              `json:"audit_failure_window"`  // §38
    StaleClearWindow    []time.Time              `json:"stale_clear_window"`    // §45
}
```

These are explicitly distinct sliding windows: `CorruptStrikeWindow` triggers self-quarantine at ≥4/30min; `AuditFailureWindow` triggers `audit-degraded` event; `StaleClearWindow` triggers observability alert (no auto-quarantine). Task 4.1 tests cover each window separately. Backward compat: missing fields in old JSON unmarshal to nil slices → 0 strikes → safe defaults.

**§32 API table** (typed `UninstallWatchdogTaskInternal(reason SelfQuarantineReason)` per §39):

```go
type SelfQuarantineReason string

const (
    QuarantineFourStrikes30Min SelfQuarantineReason = "4-strikes-30min"
    // (extension point for future reasons; reserved: QuarantineAuditDegraded — Security #7 INFO note)
)

func (a *API) UninstallWatchdogTaskInternal(reason SelfQuarantineReason) error
```

### 53. `mcphub watchdog status` graceful self-quarantined render (Security #7 LOW + Code Review #7 MINOR)

**v7 §44 issue:** if watchdog scheduled task is uninstalled (self-quarantined), `schtasks /Query /TN \mcp-local-hub-watchdog` fails. Status command needs graceful handling.

**v8 fix:** `mcphub watchdog status` checks task existence first:

```text
WATCHDOG SELF-QUARANTINED: scheduled task not installed.
Last self-quarantine: 2026-05-07T15:30:00Z
Reason: 4-strikes-30min
Suggested action: verify state files in <abs-path-of-state-dir>; review .corrupt-* quarantines; then `mcphub watchdog install` to resume.
```

If task IS installed, normal output (with `LastResult` self-failure indicator per §44 if non-zero).

### 54. `_task_hash` collision bound documented (Security #7 LOW)

12 hex chars = 48 bits. For ~10k forensic entries, birthday collision probability is ~10^-7 (negligible). Adversarial collision requires ~16M targeted entries to reach 50% probability. Plan documents this as acceptable for v0.3.0 single-user forensic correlation. Out-of-scope: full SHA-256 (64 hex) for adversarial environments.

### 55. `--allow-elevated` soft-guard documentation + audit (Security #7 LOW + Part C)

**v7 §42 documentation:** `--allow-elevated` is a SOFT guard (refusal-to-install is informational; an admin script could pass it). The HARD security boundary is the per-user state dir (§16) which is enforced at every read/write.

**v8 fix:** `--allow-elevated` use writes a high-priority audit entry:

```json
{"ts":"...","action":"watchdog-install-elevated-override","caller_pid":...,"caller_user":"DESKTOP\\admin","caller_exe":"...","reason":"--allow-elevated flag","_priority":"high"}
```

The entry is ALSO surfaced in `mcphub watchdog status` recent-events tail. Operators see immediately when this guard was bypassed.

### 56. `SelfQuarantineReason` structured suggested-action (Security #7 Part C)

**v8 fix:** each `SelfQuarantineReason` carries a suggested-action string:

```go
func (r SelfQuarantineReason) SuggestedAction() string {
    switch r {
    case QuarantineFourStrikes30Min:
        return "verify state files clean; review .corrupt-* quarantines; then `mcphub watchdog install` to resume"
    default:
        return "manual investigation required"
    }
}
```

Used by §53 status output.

### 57. Absolute paths in `mcphub watchdog status` (Security #7 Part C)

`mcphub watchdog status` output prints absolute paths to:

- `daemon-intent.json`
- `watchdog-state.json`
- `intent-audit.log`
- `watchdog.log`
- `--once.lock`
- State dir root

```text
State dir: C:\Users\dima_\AppData\Local\mcp-local-hub
Files:
  daemon-intent.json    (intent)
  watchdog-state.json   (cooldown + windows)
  intent-audit.log      (audit log; rotates at 10MB → .log.1)
  watchdog.log          (decision log; rotates at 10MB → .log.1)
  --once.lock           (singleton lock)
```

### 58. Stale-driver-loop §9 ordering fix (Code Review #7 stale-text fix)

**v7 fix in §36 corrected the in-restart driver branch.** Code Review #7 noted the END-OF-TICK and CTX-DEADLINE branches in §9 still call `WriteWatchdogState` and use the result. v8 confirms ALL branches:

- In-restart pre-RestartContext branch: uses `if err != nil { ... continue }` BEFORE logging events. ✅ (already fixed in v7).
- Ctx-deadline early-return branch: same pattern.
- End-of-tick final write branch: same pattern.
- Self-quarantine path BEFORE `UninstallWatchdogTaskInternal`: write state to record `CorruptStrikeWindow` then proceed to uninstall (state not strictly needed after uninstall but coherent for forensics).

Verification: §9 driver pseudocode in v9 audited — ALL `WriteWatchdogState(...)` calls use `events, err := ...; if err != nil { ...; continue/return }; for _, ev := range events { ... }` pattern. v9 fixed the ctx-deadline branch (was `staleEvents, _ :=` in v8 — now `staleEvents, err :=` with explicit err handling).

### 59. `LoadOwnershipSnapshot()` covers `manifestPortMap` (Security #8 MED 1)

**v8 §47 issue:** snapshot covered manifest server/daemons + workspace registry, but NOT `manifestPortMap`. `Restart()` (which kills by port) reads manifest at runtime — race with mid-tick manifest swap could direct kill to wrong port.

**v9 fix:** `OwnershipSnapshot` includes `PortMap`:

```go
type OwnershipSnapshot struct {
    ManifestServers     map[string]bool
    ManifestDaemons     map[string]map[string]bool
    WorkspaceTasksByKey map[string]string
    PortMap             map[string]int           // task name → expected port (from manifest at snapshot time)
    SnapshottedAt       time.Time
}
```

`(*API).RestartContext(ctx, server, filter)` is wrapped to accept the snapshot:

```go
func (a *API) RestartContextWithSnapshot(ctx context.Context, server, filter string, snap OwnershipSnapshot) ([]RestartResult, error) {
    // Uses snap.PortMap for kill-by-port discovery instead of fresh manifestPortMap()
    // Uses snap.ManifestDaemons for serverIsWorkspaceScoped lookup instead of fresh manifest read
}
```

Driver in §9 calls `RestartContextWithSnapshot(ctx, d.Server, d.Daemon, ownership)`. The plain `RestartContext(ctx, ...)` keeps existing behavior (re-reads manifest) for non-watchdog callers (e.g., `mcphub restart` CLI command) — backward-compatible.

Tests in Task 0:
- `TestLoadOwnershipSnapshot_ImmutableAcrossManifestSwap`: build snapshot; mutate manifest; assert snapshot unchanged.
- `TestLoadOwnershipSnapshot_PortMapPresent`: snapshot includes per-task port mapping.
- `TestRestartContextWithSnapshot_UsesSnapshotPortMap`: stub PortMap returns port X; manifest swapped to port Y; assert kill goes to X (snapshot wins).

### 60. EventLog source registration in `mcphub setup` (Security #8 MED 4)

**v8 §49 issue:** `audit-degraded` cascade falls back to `eventlog.Notify` on Windows, but the source must be REGISTERED in Windows Application log. If not registered, write fails.

**v9 fix:** `mcphub setup` registers the EventLog source:

```go
import "golang.org/x/sys/windows/svc/eventlog"

func registerEventLogSource() error {
    const sourceName = "mcp-local-hub"
    err := eventlog.InstallAsEventCreate(sourceName, eventlog.Info|eventlog.Warning|eventlog.Error)
    if errors.Is(err, registry.ErrAlreadyExists) {
        return nil  // idempotent
    }
    return err
}
```

Called from `internal/cli/setup.go::Setup()` AFTER `installWatchdog()`. Registration failure is non-fatal (logged as `eventlog-source-registration-failed-non-fatal`); the cascade falls through to syslog/stderr which still work.

`mcphub uninstall` removes the source via `eventlog.Remove("mcp-local-hub")` (idempotent).

Per-OS implementation explicit:
- Windows: `golang.org/x/sys/windows/svc/eventlog`
- POSIX: `log/syslog` (`syslog.New(syslog.LOG_WARNING|syslog.LOG_USER, "mcphub-watchdog")`)
- Stderr: `os.Stderr.Write()` with explicit error check; partial-write retry once.

Test for stderr partial-write failure: stub stderr to a writer that returns `io.ErrShortWrite` → assert cascade proceeds to next layer.

### 61. `--allow-elevated` audit fail-closed (Security #8 MED 3)

**v8 §55 issue:** writing the `watchdog-install-elevated-override` audit entry could fail; install proceeded silently.

**v9 fix:** if `--allow-elevated` is used AND the audit entry write fails (any audit-write error, including `ErrIdentityOversize`), `mcphub watchdog install` returns error with exit code 11 (new — `audit-required-but-failed`). Operator must fix audit-log permissions before re-attempting elevated install. This makes the audit trail a HARD requirement for the override.

```go
if elevated && allowElevated {
    err := api.AppendIntentAudit(NewIntentAuditEntry(
        WithAction("watchdog-install-elevated-override"),
        WithPriority("high"),
        WithReason("--allow-elevated flag explicit override"),
    ))
    if err != nil {
        return fmt.Errorf("audit log unwritable; --allow-elevated requires audit trail (exit 11): %w", err)
    }
}
```

Test: stub audit writer to return error → assert install returns non-nil error AND scheduled task NOT installed.

### 62. `mcphub install` ErrIdentityOversize fail-closed (Code Review #8 IMPORTANT 5)

**v8 §51 issue:** `mcphub install` on `ErrIdentityOversize` proceeded with "log warning + continue" — no audit trace for an oversized malicious task identity.

**v9/v11 fix:** `mcphub install` follows the same fail-closed semantic as `mcphub stop`. If the audit append for the install event returns `ErrIdentityOversize` (or any other error), the install is REJECTED with error to user. The intent file is NOT written and the scheduled task is NOT created. Rationale: a 32KB task name in a manifest is itself a red flag (legitimate names are <100 bytes); failing closed prevents a malicious manifest from installing without trace.

**v11 rollback-timing clarification (Sec #10 MED):** the audit append happens AT ONE OF TWO POINTS:

1. **Before any mutation** (preferred): `mcphub install` first computes the intended task XML + intent + audit-entry, calls `AppendIntentAudit`, and ONLY on success proceeds with `schtasks /Create` + intent file write. Audit failure → no mutations occurred → no rollback needed; just return error.
2. **While the rollback stack is still live**: if the implementation places the audit append later (e.g., inside an existing transactional block), the rollback stack accumulated by `internal/api.Install` MUST still be live when the audit fails. On audit error: invoke the existing rollback (which deletes any partially-created scheduler entry, removes any partially-written intent), THEN return error. The `internal/api.Install` function already has rollback support (verify with `grep -n "rollback\|defer.*delete\|transaction" internal/api/install.go`).

Tests cover both timings: (a) audit-fail-pre-mutation → no scheduler/intent state; (b) audit-fail-mid-transaction → rollback executed → no scheduler/intent state. End state is identical: no partial install survives.

```go
// In install path (option 1: audit-first):
err := api.AppendIntentAudit(NewIntentAuditEntry(WithAction("server-installed"), ...))
if err != nil {
    return fmt.Errorf("install audit failed; refusing to proceed (manifest may have malicious oversized identifier): %w", err)
}
// Now proceed with mutations (sch.Create, WriteDaemonIntent, etc.)
```

Test: stub audit writer with `ErrIdentityOversize` for an install → assert install returns error, no task created, no intent file modified, end-state identical to never-attempted install.

### 63. Self-quarantine status requires both signals (Security #8 LOW)

**v8 §53 issue:** if `schtasks /Query` itself fails (Windows update etc.), status falsely reports "self-quarantined".

**v10 fix (was v9 §63 with 7-day cutoff — Sec #9 LOW):** `mcphub watchdog status` declares `WATCHDOG SELF-QUARANTINED` if BOTH:

1. `schtasks /Query /TN \mcp-local-hub-watchdog` returns "task not found" — specific error code (Windows TS exit 1 + stderr substring `"The system cannot find the file specified"` or `"ERROR: The specified task name ... does not exist"`); NOT a generic schtasks failure (e.g., access denied).
2. `intent-audit.log` contains AT LEAST ONE entry where `Action == "watchdog-self-quarantined"` (canonical action filter v11 — Sec #10 LOW). The `Reason` field holds the `SelfQuarantineReason` enum value (e.g., `Reason="4-strikes-30min"`). Status filter does NOT count generic `Priority="high"` system entries or other action types — only the canonical `Action="watchdog-self-quarantined"` literal. Regardless of age (no cutoff).

If exactly ONE signal is present (e.g., generic schtasks failure but audit shows quarantine; OR task missing but no audit entry — possibly someone hand-deleted the task), status reports `STATUS UNKNOWN` with diagnostic detail:

```text
WATCHDOG STATUS UNKNOWN: schtasks query returned generic error (exit 1, "Access denied"); audit log shows last self-quarantine 2026-04-15. Possible causes: schtasks permissions issue OR partial uninstall. Investigate manually.
```

```text
WATCHDOG STATUS UNKNOWN: scheduled task missing but no self-quarantine audit entry found. Possible causes: hand-deleted by operator without using mcphub OR audit log corrupted. Investigate manually.
```

Test: combinations of (task missing/present) × (audit shows quarantine recent/none/old) → expected status output.

### 64. Public `mcphub watchdog uninstall` interactive confirm + `--yes` (Security #8 Part C)

**v8 issue:** public uninstall could run accidentally (e.g., script typo). Operators wanted a confirm gate.

**v10 fix (was v9 §64; Sec/Code #9 IMPORTANT — non-TTY safety):** `mcphub watchdog uninstall` decision tree:

1. **`--yes` flag set:** proceed without prompt; write `watchdog-uninstalled-by-user` audit entry with `Priority="high"`. (Both interactive and non-interactive contexts.)
2. **`--yes` NOT set AND stdin is a TTY (`golang.org/x/term.IsTerminal(int(os.Stdin.Fd()))`):** prompt `"This will disable mcp-local-hub watchdog. Confirm? [y/N]"` and wait for `y` or `Y`. On `n`/`N`/EOF/empty → exit 0 with `uninstall cancelled` message; on `y`/`Y` → proceed + audit.
3. **`--yes` NOT set AND stdin is NOT a TTY (CI, scripted, redirected):** **fail-fast**. Print `"mcphub watchdog uninstall is interactive; use --yes flag in non-interactive contexts"` to stderr. Exit 6. Do NOT hang on stdin read.

**Exit code 6 — operator note (Sec #10 LOW):** `mcphub gui --force --kill` already uses exit 6 for "non-interactive shell with --kill but no --yes" (see CLAUDE.md). `mcphub watchdog uninstall` reuses the same exit code with the same semantic ("interactive command requires `--yes` in non-interactive contexts"). Exit codes are command-scoped — no conflict. Task 12.2 updates `CLAUDE.md` to document BOTH commands' exit-6 contexts side-by-side, and `mcphub watchdog uninstall --help` includes the same explanation, so operators don't confuse them.

Distinction from internal: `UninstallWatchdogTaskInternal(SelfQuarantineReason)` uses `Priority="high"` AND `IsSystemEntry()=true` (sealed), so it's not redacted in cross-user audit displays. Public uninstall audit entries have `IsSystemEntry()=false` and ARE subject to redaction display rules.

Tests:
- Interactive mode + stdin "n" → exits 0 with "cancelled".
- Interactive mode + stdin "y" → proceeds + audit entry.
- `--yes` mode (TTY or not) → proceeds + audit entry.
- **Non-TTY without `--yes`:** stub `term.IsTerminal` returns false; assert exit 6 + stderr message + audit entry NOT written + scheduled task NOT removed.

### 65. Acceptance criteria expanded for v9 gates

The "Acceptance criteria" block at the end of this plan is updated to include explicit gates for:

- `LoadOwnershipSnapshot` immutability + `PortMap` coverage (§47, §59)
- Sealed `systemEntry` constructor pattern; JSON unmarshal ignores input (§48)
- `audit-degraded` cascade per-OS (eventlog/syslog/stderr) with registration in setup (§49, §60)
- `_task_hash` 12-hex SHA-256 first 12 chars (§54)
- `Priority` field on `IntentAuditEntry` (§55, Task 3.1)
- `--allow-elevated` audit fail-closed at exit 11 (§61)
- `mcphub install` fail-closed on `ErrIdentityOversize` (§62)
- Self-quarantine status both-signals requirement (§63)
- Public uninstall interactive confirm + `--yes` (§64)
- Absolute paths in status (§57)
- Three sliding windows distinct triggers (§52, §28, §38, §45)

---

## Task 0: foundational API surfaces with ctx-select pattern

**Steps:**

- [ ] **0.1** Failing tests:
  - `TestStatusContext_RespectsCtxCancellation`: cancelled ctx → returns `ctx.Err()` even if underlying Status would return.
  - `TestStatusContext_NormalCompletion`: returns rows when underlying Status returns first.
  - `TestRestartContext_RespectsCtxCancellation`: cancelled ctx → ctx.Err.
  - `TestRestartContext_BestEffort`: comment-documented that underlying Restart continues after ctx-cancel; verify ctx-cancel returns immediately to caller (timing assertion within ~10ms).
  - `TestWaitDaemonRunning_ReturnsTrue`: stub StatusContext returning Running row → true.
  - `TestWaitDaemonRunning_ReturnsFalse`: never Running within ctx deadline → false (no panic; returns false on ctx-Done before any ok poll).
  - `TestWaitDaemonRunning_PollsAtOneSecond`: instrument poll counter; over 5s ctx, asserts ~5 polls.
  - `TestIntentStillRunning_TrueWhenNoStopIntent`.
  - `TestIntentStillRunning_FalseWhenUserStop`.
  - `TestIntentStillRunning_TrueWhenStopExpired`.
  - `TestLoadDaemonRegistry_ImmutableSnapshot`: build registry, mutate underlying `Status()` snapshot, assert returned registry's `IsManagedDaemon` reflects the original snapshot (defensive copy verified).
  - `TestLoadDaemonRegistry_IsManagedDaemon_TaskInStatus`.
  - `TestLoadDaemonRegistry_IsManagedDaemon_TaskInManifest`.
  - `TestLoadDaemonRegistry_IsManagedDaemon_OrphanTask`.
  - `TestUninstallWatchdogTask_Idempotent`.
  - `TestUninstallWatchdogTaskInternal_AuditReason` (v12 canonical filter): typed `SelfQuarantineReason` parameter; stub audit writer; assert audit entry has `Action == "watchdog-self-quarantined"` literal AND `Reason == <enum-string-value>` (e.g., `"4-strikes-30min"` for `QuarantineFourStrikes30Min`). Earlier draft test wording suggested `Action="self-quarantine-{enum}"`; v12 canonicalizes per §63 status filter contract.
  - **(v9 §47/§59) `TestLoadOwnershipSnapshot_ImmutableAcrossManifestSwap`:** build snapshot; mutate manifest on disk; assert `snap.ManifestServers` / `snap.ManifestDaemons` / `snap.PortMap` unchanged.
  - **(v9 §59) `TestLoadOwnershipSnapshot_PortMapPresent`:** snapshot includes per-task `PortMap`; matches manifest-time port discovery.
  - **(v9 §59) `TestRestartContextWithSnapshot_UsesSnapshotPortMap`:** stub PortMap returns port X; manifest swapped to port Y; assert kill goes to port X (snapshot wins).
  - **(v9 §61) `TestWatchdogInstall_Elevated_AuditFailFailClosed`:** stub audit writer with error; `mcphub watchdog install --allow-elevated` returns non-nil; scheduled task NOT installed.
  - **(v9 §62) `TestMcphubInstall_AuditFailFailClosed`:** stub audit writer with `ErrIdentityOversize`; `mcphub install` returns error, no task created, no intent file modified.
  - **(v9 §63) `TestStatus_BothSignalsRequiredForSelfQuarantined`:** matrix test (task missing/present × audit recent/none/old) → expected status output.
  - **(v9 §64) `TestPublicUninstall_InteractiveConfirm`:** stdin "n" → exits 0 with "cancelled"; stdin "y" → proceeds + audit entry.
  - **(v9 §64) `TestPublicUninstall_YesFlag`:** `--yes` → proceeds without prompt + audit entry.
- [ ] **0.2** Implement all surfaces in `internal/api/api_surfaces.go`. Use goroutine + ctx-select pattern for ctx-aware wrappers; document best-effort cancellation limitation in code comments. Implement `LoadOwnershipSnapshot()` returning immutable `OwnershipSnapshot` struct.
- [ ] **0.3** Commit `feat(api): foundational ctx-aware API surfaces + ownership snapshot + sealed audit constructors`.

---

## Task 1: `state_paths.go` — KnownFolder production fail-closed + sanity + perms (Security #5 MEDIUM 1)

**Steps:**

- [ ] **1.1** Failing tests:
  - `TestDaemonStateDir_Windows_KnownFolder` (build-tag `test_state_path_env`): stub returns path → assert.
  - `TestDaemonStateDir_Windows_KnownFolderFails_FallsBackToEnv` (build-tag `test_state_path_env`): stub fails; LOCALAPPDATA set → falls back.
  - `TestDaemonStateDir_Windows_KnownFolderFails_NoFallbackInProduction` (NO build-tag): stub fails → returns error immediately; env not consulted.
  - `TestDaemonStateDir_LinuxXDG`.
  - `TestDaemonStateDir_LinuxFallback`.
  - `TestDaemonStateDir_macOS`.
  - `TestDaemonStateDir_DirPermsPOSIX` (skip on Windows): dir = 0700.
  - `TestDaemonStateDir_FilePermsPOSIX`.
  - `TestDaemonStateDir_RejectWorldWritablePOSIX`.
  - `TestDaemonStateDir_RejectGroupWritablePOSIX`.
  - `TestDaemonStateDir_RejectNonOwnerPOSIX`.
- [ ] **1.2** Implement with build-tag-gated `daemonStateDir`. Production binary uses KnownFolder-only path; test binary uses env fallback.
- [ ] **1.3** Implement `OpenStateFile(name string) (*os.File, error)` wrapper with 0600 + post-Chmod.
- [ ] **1.4** Run `go test -count=1 ./internal/api/`. Run with both default tags AND `-tags=test_state_path_env`. Commit `feat(api): cross-platform state dir + KnownFolder-only-in-production + sanity check`.

---

## Task 2: `daemon_intent.go` — three-state + TTL + clock-skew + UTC + mixed-bootstrap + post-rename quarantine + 16KB cap

Schema same as v5.

**Steps:**

- [ ] **2.1** Failing tests (all v5 tests + v6 additions):
  - All v5 tests
  - **Quarantine prune failure non-fatal:** simulate disk-full on prune-step-2 (delete) → quarantine completes (rename succeeded), prune logged as `quarantine-prune-failed-non-fatal`, function returns success.
  - **16KB cap on intent file write:** write task name 32KB → fails with explicit `ErrEntryOversize`. Caller policy: refuse the write, log error, surface to user. (Don't silently truncate intent task names — those need to match TS task names exactly.)
- [ ] **2.2** Implement.
- [ ] **2.3** Inject path via API field for testability.
- [ ] **2.4** Commit `feat(api): daemon intent — 3-state, TTL, skew, UTC, post-rename quarantine + non-fatal prune + size cap`.

---

## Task 3: `intent_audit.go` — append-only + retention + caller fields + `audit-rotated` idempotent retry + sealed `SystemEntry` + identity-preserving 16KB cap + `Priority` field (v9 spec)

Schema (v9 — final):

```go
type IntentAuditEntry struct {
    TS              time.Time      `json:"ts"`              // UTC RFC3339Nano
    Who             string         `json:"who"`
    Action          string         `json:"action"`
    Task            string         `json:"task"`             // identity field — never truncated
    Before          *DaemonIntent  `json:"before,omitempty"`
    After           *DaemonIntent  `json:"after,omitempty"`
    CallerPID       int            `json:"caller_pid"`
    CallerExe       string         `json:"caller_exe"`
    CallerStartTime time.Time      `json:"caller_start_time"`
    CallerUser      string         `json:"caller_user"`      // identity field — never truncated
    Reason          string         `json:"reason,omitempty"`
    Priority        string         `json:"priority,omitempty"` // §55 v9 — "high" or "" (default low)
    systemEntry     bool           // §48 unexported; only set via newSystemAuditEntry()
}

// JSON marshal writes lowercased "system_entry" for forensic visibility:
func (e IntentAuditEntry) MarshalJSON() ([]byte, error) { /* writes system_entry as lower JSON field */ }

// JSON unmarshal IGNORES system_entry on input (sealed pattern). Persisted entries
// loaded from disk are treated as non-system unless a trusted package-private
// rehydration helper sets the flag — used only by audit-tail readers in the same
// package, never from external untrusted JSON. (v9 fix: explicit Code Review #8 IMPORTANT 2)
func (e *IntentAuditEntry) UnmarshalJSON(data []byte) error { /* discards system_entry input */ }

// Internal-only rehydrator for log-tail reading by status command (same-package trust):
func rehydrateSystemEntryFromTrustedSource(e *IntentAuditEntry, raw bool) {
    e.systemEntry = raw   // package-private; reachable only from internal/api
}

// IsSystemEntry predicate used by redaction.
func (e IntentAuditEntry) IsSystemEntry() bool { return e.systemEntry }

// Sealed constructors:
func newSystemAuditEntry(action string, /* ... */) IntentAuditEntry { /* sets systemEntry=true */ }
func NewIntentAuditEntry(action string, /* ... */) IntentAuditEntry { /* leaves systemEntry=false */ }

// Display-only redaction helper used by status command:
func RedactIntentAuditEntryForNonOwner(e IntentAuditEntry) IntentAuditEntry {
    if e.IsSystemEntry() { return e }   // §48 sentinel — never redact
    if e.CallerUser != currentOSUser() {
        e.CallerUser = "<redacted-non-owner>"
    }
    return e
}
```

**Steps:**

- [ ] **3.1** Failing tests:
  - Append → readable line.
  - Rotation: 11MB → `.log.1` exists; fresh `.log` first entry is `audit-rotated` self-event.
  - Control-char escape (`\n\t\x00`).
  - Invalid UTF-8 escape (replacement chars).
  - File perms 0600 on POSIX.
  - Caller fields populated; `caller_start_time` UTC ±2s.
  - **Audit-write failure surfaces error:** simulated disk-full → `AppendIntentAudit` returns non-nil error.
  - **`audit-rotated` idempotent retry:** simulate disk-full on the self-event append after rotation → no double-rotation; next successful append goes through normally; failure logged as `audit-rotated-event-write-failed-non-fatal` to `watchdog.log`.
  - **Identity field oversize REJECTION (v9 — Code Review #7 + #8 stale-fix):** entry with `task` >1KB → returns `ErrIdentityOversize`; NO truncation; caller logs error and skips audit. (NOT 32KB→truncate as in earlier stale text.)
  - **Identity oversize on `caller_user`:** same — `>1KB caller_user` → `ErrIdentityOversize`.
  - **Non-identity truncation (v9):** entry with 32KB `err` field, 100-byte `task` → output ≤16KB, `task` intact, `err` truncated, `_truncated:true`, `_truncated_field:"err"`, `_task_hash` (12-hex SHA256 of original task) present.
  - **Non-identity drop with placeholder:** if multi-field oversize even after truncation → `log-entry-dropped-oversize` placeholder + `_task_hash` present.
  - **Sealed `systemEntry` constructor:** `NewIntentAuditEntry(...)` produces `IsSystemEntry()=false`; `newSystemAuditEntry(...)` produces true.
  - **JSON unmarshal ignores `system_entry`:** load JSON `{"system_entry":true,...}` via `json.Unmarshal` → `IsSystemEntry()=false` (sealed pattern).
  - **Sentinel `<rotation-system>` literal:** entry created with `caller_user="<rotation-system>"` AND `systemEntry=true` (via constructor) → display rendering preserves the literal value (no redaction).
  - **`<rotation-system>` non-system rejection:** if test forges entry with `caller_user="<rotation-system>"` but `systemEntry=false` (no sealed constructor) → redaction applies → display shows `<redacted-non-owner>` (sentinel name alone is not trust).
  - **Owner match no-redaction:** entry with `caller_user=currentOSUser()` and `systemEntry=false` → returned unchanged.
  - **`Priority` field (§55 v9):** `NewIntentAuditEntry(...)` with `Priority="high"` → JSON output has `"priority":"high"`; default empty Priority → field omitted (`omitempty`).
- [ ] **3.2** Implement.
- [ ] **3.3** Commit `feat(api): intent audit — sealed SystemEntry, identity-preserving 16KB, Priority, idempotent rotation`.

---

## Task 4: `watchdog_state.go` — cooldown + restart-pending(now) + sliding-30min strikes + wall-clock + corrupt fail-CLOSED + stale-clear visibility (Code Review #5 IMPORTANT 3 + Security #5 MEDIUM 3)

Schema:

```go
type CooldownEntry struct {
    FirstAttemptAt   time.Time `json:"first_attempt_at"`
    AttemptsInWindow int       `json:"attempts_in_window"`
    LastRunningAt    time.Time `json:"last_running_at"`
    ChronicCycles    int       `json:"chronic_cycles"`
    RestartPendingAt time.Time `json:"restart_pending_at"`
}

type WatchdogState struct {
    Cooldowns           map[string]CooldownEntry `json:"cooldowns"`
    LastWallClockSeen   time.Time                `json:"last_wall_clock_seen"`
    CorruptStrikeWindow []time.Time              `json:"corrupt_strike_window"` // §28: ≥4 in 30min → self-quarantine (exit 9)
    AuditFailureWindow  []time.Time              `json:"audit_failure_window"`  // §38 v9: ≥3 in 30min → audit-degraded (no quarantine)
    StaleClearWindow    []time.Time              `json:"stale_clear_window"`    // §45 v9: ≥4 in 30min → observability alert (no quarantine)
}
```

The three sliding windows are **explicitly distinct** (Code Review #7 + Sec #6 explicit clarification):

| Window | Trigger threshold | Effect |
|---|---|---|
| `CorruptStrikeWindow` | ≥4 strikes in 30 min | Self-quarantine: uninstall watchdog task + exit 9 |
| `AuditFailureWindow` | ≥3 failures in 30 min | Emit `audit-degraded` high-priority log entry (no quarantine) |
| `StaleClearWindow` | ≥4 events in 30 min | Emit `stale-clear-strike-alert` (observability only, no quarantine) |

Backward-compat: missing fields in older state JSON unmarshal to nil → 0 strikes → safe defaults. v0.3.0 is fresh; schema-bump warning unnecessary now, documented for v0.4.x.

API:

```go
type CooldownReader interface {
    Due(name string, now time.Time) bool
    ChronicLimitReached(name string) bool
    AttemptsInWindow(name string) int
    IsRestartPending(name string, now time.Time) bool
}

type Cooldown interface {
    CooldownReader
    RecordAttempt(name string, now time.Time)
    RecordRunning(name string, now time.Time)
    MarkRestartPending(name string, now time.Time)
    ClearRestartPending(name string)
}

// Returns events list of stale-clears for caller to log (Security #5 MEDIUM 3)
func (a *API) WriteWatchdogState(s WatchdogStateRead, now time.Time) (staleClearEvents []string, err error)
```

`WriteWatchdogState` clears stale RestartPendingAt during serialization AND returns a list of cleared task names so the driver can log `restart-pending-stale-cleared` events to watchdog.log.

**Steps:**

- [ ] **4.1** Failing tests (v9 — comprehensive):
  - Window math (Due true at 1..4 inclusive of T+15, false at 5..6, true at 7 of new cycle, per §6).
  - Reset after Running ≥5min.
  - Chronic limit (4 cycles, no Running ≥5min) → ChronicLimitReached.
  - Persist + reload (FirstAttemptAt survives).
  - Corrupt JSON → `State="corrupt"` + quarantine + suppress-all `Cool.Due=false`.
  - Quarantine cap (6 fake `.corrupt-*` files → corrupt write quarantines + post-rename prune to 5 newest).
  - Persistent across `mcphub watchdog --once` invocations.
  - Wall-clock-jump persistence (LastWallClockSeen survives reads).
  - **`IsRestartPending(name, now)` injected clock:** stub `Cooldown` wrapper records calls; assert no ambient `time.Now()` invocations.
  - **Stale-clear events on Write:** `MarkRestartPending(t1)` + persist; `WriteWatchdogState(s, t1+7min)` → returns `[taskName]` in events list.
  - **`CorruptStrikeWindow` 30-min sliding:** 4 entries within 30min → trigger detected; 4 spread over 31min → no trigger; 5 entries → capped at 4 newest.
  - **`AuditFailureWindow` 30-min sliding (v9 §38):** 3 within 30min → trigger detected; 3 spread over 31min → no trigger.
  - **`StaleClearWindow` 30-min sliding (v9 §45):** 4 within 30min → trigger detected; same separation tests.
  - **All three windows independent:** 4 corrupt strikes does NOT trigger audit-degraded; 3 audit failures does NOT trigger self-quarantine; etc.
  - **Quarantine prune non-fatal:** simulate per-file delete error → quarantine completes (rename succeeded), prune logged as `quarantine-prune-failed-non-fatal`, function returns success.
  - **Backward-compat:** load older state JSON without `AuditFailureWindow` / `StaleClearWindow` → fields unmarshal to nil → no-trigger / safe defaults.
  - **`WriteWatchdogState` err-first contract (v9 §36):** simulate atomic-write failure → returns `(nil, err)`; assert events slice is nil (NOT partial events). Test EVERY branch that calls WriteWatchdogState in driver.
- [ ] **4.2** Implement.
- [ ] **4.3** Commit `feat(api): watchdog state — fail-CLOSED, 3 sliding windows (corrupt/audit/staleclear), restart-pending(now-injected), err-first Write`.

---

## Task 5: bug #2 partial — `StopExisting` commit

Already in working tree.

- [ ] **5.1** `git add internal/scheduler/scheduler_windows.go internal/scheduler/scheduler_windows_test.go && git commit -m "fix(scheduler): MultipleInstancesPolicy=StopExisting unblocks manual restart edge cases (bug #2 partial)"`

---

## Task 6: `watchdog_xml_validator.go` — hardened security gate (no cache)

**Steps:**

- [ ] **6.1** Failing tests (same as v5 — name/command/args/principal/RunLevel/LogonType mismatches + DOCTYPE + oversize via limit+1 + depth + malformed + billion-laughs + schtasks-timeout).
- [ ] **6.2** Implement.
- [ ] **6.3** Commit `feat(api): hardened owned-task XML validator`.

---

## Task 7: `recovery.go` — strictly pure state machine + IsRealFailure exported

```go
type RecoveryDecision struct {
    TaskName string; Server string; Daemon string; Action string; Reason string; Attempt int
}

type DaemonRegistry interface { IsManagedDaemon(taskName string) bool }
type OwnedXMLValidator interface { IsOwnedAndValid(taskName string) bool }

func RecoverStoppedDaemons(
    now       time.Time,
    status    []DaemonStatus,
    intent    DaemonIntentFile,
    cool      CooldownReader,
    validator OwnedXMLValidator,
    registry  DaemonRegistry,
) []RecoveryDecision
```

**Steps:**

- [ ] **7.1** Failing tests cover EVERY action vocabulary including `restart-pending-skipped` and mixed-bootstrap (per v5 list).
- [ ] **7.2** Implement decision tree per §1; uses existing `DaemonStatus`.
- [ ] **7.3** Move `isRealFailure` from `internal/tray/state.go` AND `internal/cli/gui_tray_state.go` to `api.IsRealFailure`.
- [ ] **7.4** Verify with `grep -rn "isRealFailure\|IsRealFailure" internal/` — ONE definition.
- [ ] **7.5** Commit `feat(api): watchdog recovery state machine (pure) + IsRealFailure exported`.

---

## Task 8: scheduled task install — `buildWatchdogXML` + install/uninstall

(v5 Task 8 about `manifest_hash.go` REMOVED in v6.)

```xml
<RegistrationInfo>
  <Description>mcp-local-hub watchdog: auto-recovery for daemons. Cadence 5 min. Disable: mcphub watchdog uninstall.</Description>
</RegistrationInfo>
<Triggers>
  <CalendarTrigger>
    <StartBoundary>2026-05-07T00:00:00</StartBoundary>
    <ScheduleByDay><DaysInterval>1</DaysInterval></ScheduleByDay>
    <Repetition>
      <Interval>PT5M</Interval>
      <StopAtDurationEnd>false</StopAtDurationEnd>
    </Repetition>
  </CalendarTrigger>
  <LogonTrigger><Enabled>true</Enabled></LogonTrigger>
</Triggers>
<Settings>
  <Hidden>false</Hidden>
  <Priority>9</Priority>
  <ExecutionTimeLimit>PT5M</ExecutionTimeLimit>
  <MultipleInstancesPolicy>IgnoreNew</MultipleInstancesPolicy>
  <DisallowStartIfOnBatteries>false</DisallowStartIfOnBatteries>
  <RunOnlyIfIdle>false</RunOnlyIfIdle>
  <AllowStartOnDemand>true</AllowStartOnDemand>
  <Enabled>true</Enabled>
</Settings>
<Actions Context="Author">
  <Exec>
    <Command>{canonicalMcphubPath}</Command>
    <Arguments>watchdog --once</Arguments>
    <WorkingDirectory>{workingDir}</WorkingDirectory>
  </Exec>
</Actions>
```

**Steps:**

- [ ] **8.1** Failing tests for `buildWatchdogXML` (Hidden, Priority, Interval, ExecutionTimeLimit, Description, both triggers, IgnoreNew).
- [ ] **8.2** Implement.
- [ ] **8.3** install/uninstall flow (idempotent).
- [ ] **8.4** Commit `feat(scheduler): watchdog scheduled task install/uninstall`.

---

## Task 9: `mcphub watchdog ...` CLI

Subcommands:

```
mcphub watchdog --once                   # singleton-locked (PID/ts info file), 4min ctx, restart-budget guard, restart-verify-only
mcphub watchdog enable [--server NAME]
mcphub watchdog disable [--server NAME]
mcphub watchdog install
mcphub watchdog uninstall                # public uninstall, audit reason "user-uninstall"
mcphub watchdog status [--json]
```

**`--once` driver flow (v6):**

```go
func runWatchdogOnce(ctx context.Context) int {
    // Singleton lock with PID/ts owner file (§33)
    lockPath := filepath.Join(stateDir, "--once.lock")
    lock := flock.New(lockPath)
    locked, err := lock.TryLock()
    if err != nil { /* perm/io */ return 1 }
    if !locked {
        owner := readOwnerInfoBestEffort(lockPath + ".owner.json")
        watchdogLog.Append("already-running-skipped", owner)
        return 0
    }
    ownerJSON, _ := json.Marshal(map[string]any{
        "pid": os.Getpid(),
        "started_at": time.Now().UTC().Format(time.RFC3339Nano),
        "hostname": hostname(),
    })
    os.WriteFile(lockPath+".owner.json", ownerJSON, 0600)
    defer func() { lock.Unlock(); os.Remove(lockPath+".owner.json") }()

    ctx, cancel := context.WithTimeout(ctx, 4*time.Minute)
    defer cancel()

    intentR := api.ReadDaemonIntent()
    coolR := api.ReadWatchdogState()

    now := time.Now().UTC()

    // Wall-clock jump check (§29)
    // ... [as in v5; including missing-baseline-after-corrupt suppression]

    // Corrupt-strike accumulation + self-quarantine (§28)
    if intentR.State == "corrupt" || coolR.State == "corrupt" {
        // append to CorruptStrikeWindow + drop >30min + cap at 4
        // if len >= 4 within 30min:
        //   api.UninstallWatchdogTaskInternal(api.QuarantineFourStrikes30Min)  // v12 typed enum per §39/§63 canonical
        //   return 9
        // else:
        //   api.WriteWatchdogState(coolR, now)  // also clears stale RestartPendingAt
        //   return 0
    }

    // Defensive-copy snapshots (Security #5 LOW + v9 §47/§59 + v10 §59 PortMap)
    registry := api.LoadDaemonRegistry()         // immutable registry snapshot
    ownership := api.LoadOwnershipSnapshot()     // §47 + §59: immutable {ManifestServers,ManifestDaemons,WorkspaceTasksByKey,PortMap}

    status, err := api.StatusContext(ctx)
    if err != nil { return 1 }

    // Validator wraps the ownership snapshot so structural checks are tick-stable.
    validator := api.NewOwnedXMLValidatorFromSnapshot(ownership)

    decisions := api.RecoverStoppedDaemons(now, status, intentR.File, coolR.Cool, validator, registry)

    // Healthy-Running cooldown reset (per §6)
    for _, row := range status {
        if row.State == "Running" {
            coolR.Cool.RecordRunning(row.TaskName, now)
        }
    }

    // Apply decisions
    for _, d := range decisions {
        if ctx.Err() != nil {
            watchdogLog.Append("ctx-deadline-exceeded", "")
            // v9 fix (Code Review #8 MISSED): err-first ordering on EVERY WriteWatchdogState call site
            staleEvents, err := api.WriteWatchdogState(coolR, now)
            if err != nil {
                watchdogLog.Append("ctx-deadline-state-write-failed", err.Error())
                return 2
            }
            for _, ev := range staleEvents { watchdogLog.Append("restart-pending-stale-cleared", ev) }
            return 2
        }
        switch d.Action {
        case "restart":
            deadline, ok := ctx.Deadline()
            if ok && time.Until(deadline) < 60*time.Second {
                watchdogLog.Append(d, "ctx-budget-exhausted")
                continue
            }
            if !api.IntentStillRunning(d.TaskName, time.Now().UTC()) {
                watchdogLog.Append(d, "stop-race-aborted")
                continue
            }
            coolR.Cool.MarkRestartPending(d.TaskName, time.Now().UTC())
            coolR.Cool.RecordAttempt(d.TaskName, time.Now().UTC())
            // §30 PERSIST IMMEDIATELY — durability against mid-restart kill
            staleEvents, err := api.WriteWatchdogState(coolR, time.Now().UTC())
            if err != nil {
                watchdogLog.Append(d, "pre-restart-state-write-failed: "+err.Error())
                continue   // staleEvents guaranteed nil on err per §36
            }
            for _, ev := range staleEvents { watchdogLog.Append("restart-pending-stale-cleared", ev) }
            // v10 fix (Code #9 + Sec #9 IMPORTANT): use snapshot-bound restart so PortMap is frozen
            err = api.RestartContextWithSnapshot(ctx, d.Server, d.Daemon, ownership)
            coolR.Cool.ClearRestartPending(d.TaskName)
            if err == nil {
                verifyCtx, vc := context.WithTimeout(ctx, 30*time.Second)
                running := api.WaitDaemonRunning(verifyCtx, d.TaskName)
                vc()
                if running {
                    watchdogLog.Append(d, "restart-verified-running")
                } else {
                    watchdogLog.Append(d, "restart-not-yet-running-after-30s")
                }
            } else {
                watchdogLog.Append(d, errString(err))
            }
        case "chronic-failure":
            // (per §24)
        case "suspicious-xml":
            watchdogLog.AppendHighPriority(d, "validator rejected")
        default:
            watchdogLog.Append(d, "")
        }
    }

    staleEvents, err := api.WriteWatchdogState(coolR, now)
    if err != nil {
        watchdogLog.Append("end-of-tick-state-write-failed", err.Error())
        return 1
    }
    for _, ev := range staleEvents { watchdogLog.Append("restart-pending-stale-cleared", ev) }
    return 0
}
```

**Steps:**

- [ ] **9.1** Failing tests:
  - Exit-code matrix: 0 success, 1 backend error, 2 ctx deadline, 8 perms-rejected, 9 self-quarantined.
  - **Singleton lock with owner JSON:** owner file exists during run; deleted on success; second `--once` reads owner JSON in log.
  - Corrupt intent → exits 0 + appends to `CorruptStrikeWindow`.
  - Corrupt cooldown → ditto.
  - **4 strikes within 30min** → self-quarantines via `UninstallWatchdogTaskInternal(QuarantineFourStrikes30Min)` (audit `Action="watchdog-self-quarantined"` + `Reason="4-strikes-30min"` per §39/§63 canonical) + exits 9.
  - **4 strikes spread over 31min** → no self-quarantine.
  - Bootstrap missing intent → restarts managed, skips orphan.
  - Mixed-bootstrap → restarts both intent-mentioned and intent-missing managed.
  - Stop-race → no restart + log.
  - **No manifest-hash check (v6 removed):** verify driver does NOT call manifest hash.
  - ctx-budget < 60s → skips with `ctx-budget-exhausted`.
  - **Pre-restart persist:** assert `WriteWatchdogState` called between `RecordAttempt` and `RestartContext`. Use spy/counter on Write to verify ordering.
  - **Pre-restart persist failure:** simulated WriteWatchdogState err → no `RestartContext` call; logged.
  - Restart-verify success → `restart-verified-running` log; **no `RecordRunning` call.**
  - Restart-verify fail → `restart-not-yet-running-after-30s`.
  - **`restart-pending-skipped` after prior tick crash:** simulate prior tick that wrote pending then crashed; this tick reads pending; emits `restart-pending-skipped`.
  - **`restart-pending-stale-cleared` event after 6+ minutes:** prior tick wrote pending at T0; tick at T0+7min sees stale on Write; logs `restart-pending-stale-cleared`.
  - Wall-clock-jump >24h → suppresses + persists + exits 0.
  - `wall-clock-baseline-missing-after-corrupt` → suppresses + exits 0.
  - `enable`/`disable`/`status` round-trip.
  - **JSON Lines + 16KB cap with identity preservation (v9/v10 §35 + Task 3 spec):** entry with 32KB **non-identity** field (e.g., `err`) and 100-byte `task` → output ≤16KB; `task` intact; `err` truncated; `_truncated:true` + `_truncated_field:"err"` + `_task_hash` present. Entry with `task >1KB` → REJECTED with `ErrIdentityOversize` (caller logs error and skips audit per §35; `mcphub stop`/`install` fail-closed per §51/§62). NEVER truncates `task`/`task_name`/`caller_user`.
  - **(v12 driver-level snapshot test — Code Review #11 MINOR):** `TestWatchdogDriver_UsesSnapshotRestartPath`: install fake watchdog driver run; assert `RestartContextWithSnapshot` is called (NOT plain `RestartContext`); intercept snapshot argument and verify `PortMap` matches the `LoadOwnershipSnapshot()` returned at tick start. Manifest mutation between snapshot-build and restart MUST NOT change kill-target port.
  - Log rotation at 10MB.
  - **`status --json`:** parseable JSON with all fields (CorruptStrikeWindow, LastWallClockSeen, recent events, audit tail).
  - **`status` audit-tail redaction:** entry from caller_user="someone-else" → display shows `<redacted-non-owner>`.
  - **`status` last flock skip:** prior tick had skip; status shows owner info (PID, hostname, age).
- [ ] **9.2** Implement.
- [ ] **9.3** Commit `feat(cli): mcphub watchdog command — full v6 driver flow`.

---

## Task 10: integrate intent + audit into existing commands

Same as v5.

| Cmd | Intent timing | Audit fail handling |
|---|---|---|
| `mcphub stop --server X` | BEFORE kill | fail-closed both ways (intent OR audit fail incl. `ErrIdentityOversize`) |
| `mcphub stop --server X --force` | Skip intent | fail-closed if audit fails (incl. `ErrIdentityOversize`) |
| `mcphub install <s>` | **AUDIT-FIRST (v12 §62 canonical timing): audit append BEFORE any scheduler/intent mutation; on audit failure, no rollback needed (no mutations performed). Alternative valid timing: audit-mid-transaction with live rollback stack — both are tested in Task 11.1.** | **fail-closed (v10 §62):** install rejected if audit fails (incl. `ErrIdentityOversize`); end-state identical to never-attempted install |
| `mcphub register <ws> <lang>` | AFTER PASS | log warning + continue (audit non-blocking; rationale: workspace registration is observable through registry state) |
| `mcphub restart` | AFTER /Run success | log warning + continue (restart already happened; audit gap recorded in `watchdog.log`) |
| `mcphub uninstall` | BEFORE deleting tasks | log + proceed (uninstall idempotent; audit fail tolerated because tasks must be deleted regardless) |
| `mcphub watchdog uninstall` (public) | n/a (writes audit `watchdog-uninstalled-by-user` Priority=high) | fail-closed: if audit fails, uninstall rejected with diagnostic |
| `mcphub watchdog install --allow-elevated` | n/a (writes audit `watchdog-install-elevated-override` Priority=high) | fail-closed (§61 v9): exit 11; install NOT performed |
| watchdog driver: chronic-failure | per §24 | log + skip Restart (next tick re-attempts) |

**Steps:**

- [ ] **10.1** Tests for each command.
- [ ] **10.2** `mcphub stop` fail-closed semantics.
- [ ] **10.3** Implement.
- [ ] **10.4** Commit `feat(api): intent + audit writes — fail-closed semantics`.

---

## Task 11: setup auto-install + uninstall ordering

Same as v5.

- [ ] **11.1** Tests.
- [ ] **11.2** Implement.
- [ ] **11.3** Commit `feat(cli): auto-install watchdog during setup; uninstall ordering`.

---

## Task 12: docs + bug-trackers + manual smoke

- [ ] **12.1** Update `docs/phase-3b-ii-verification.md` D2.4 + add D2.6 watchdog smoke (12 sub-cases per v5; plus restart-pending-stale-cleared log entry visible in `mcphub watchdog status`).
- [ ] **12.2** Update `CLAUDE.md`: watchdog architecture + state files + log paths + disable/install instructions + audit retention + post-self-quarantine recovery (verify state files clean) + per-entry size cap + best-effort ctx cancellation note + **(v12 — Sec/Code #10/#11) exit-code documentation**: side-by-side description of exit 6 contexts (`mcphub gui --force --kill` vs `mcphub watchdog uninstall` non-TTY without `--yes`); both use exit 6 with the same semantic ("interactive command requires `--yes` in non-interactive contexts") but operators reading exit-code docs need both contexts visible. Same content also goes into `mcphub watchdog uninstall --help`.
- [ ] **12.3** Update `work-items/bugs/2026-05-07-task-scheduler-restartonfailure-not-firing.md`.
- [ ] **12.4** Manual smoke (real machine):
  - All v5 sub-cases.
  - **Pre-restart persist:** kill `mcphub watchdog --once` mid-restart via Task Manager; next tick → `restart-pending-skipped`; tick after 6min → fresh restart.
  - **Owner JSON in flock:** start `--once` manually; concurrently invoke another `--once`; second run logs PID/ts of first.
  - **Status redaction:** trigger an audit entry (real or staged) with caller_user=test-user; verify status display redacts.
  - **16KB log cap:** install a synthetic daemon with 32KB task-name (testing only); verify log line ≤16KB.
- [ ] **12.5** Commit `docs: watchdog architecture + verification doc updates`.

---

## Self-review

### v1 → v5 deltas

(See plan history.)

### v12 → v13 deltas (Codex round 12)

**Round 12 verdict:** Security review **APPROVE** — no new security regression. Code review **REVISE** for ONE remaining straggler. v13 fixes that straggler + minor wording polish.

| v12 | v13 | Source |
|---|---|---|
| §9 driver pseudocode comment had `api.UninstallWatchdogTaskInternal("4-strikes-30min")` raw string | §9 v13: `api.UninstallWatchdogTaskInternal(api.QuarantineFourStrikes30Min)` typed enum per §39/§63 canonical contract | Code #12 IMPORTANT |
| Out-of-scope mixed `post-v0.3.0` and `v0.4.x` wording inconsistently | Out-of-scope v13: explicit "wording note" + per-item markers (`→ v0.4.x` or `defer indefinitely`) | Code #12 MINOR |

### v11 → v12 deltas (Codex round 11)

**Round 11 verdict summary:** Security review **APPROVE**. Code review REVISE for cross-section text inconsistencies only (no architecture concerns). v12 closes those text inconsistencies.

| v11 | v12 | Source |
|---|---|---|
| §39 said "Audit log writes `self-quarantine-{reason}`" — contradicts §63 canonical `Action="watchdog-self-quarantined"` | §39 v12: explicit canonical: `Action="watchdog-self-quarantined"` literal + `Reason=<enum-value>` + invariant ("future variants must keep this action label OR §63 filter must be expanded in same change") | Code #11 IMPORTANT 1 |
| Task 9.1 test "audit reason `self-quarantine-4-strikes-30min`" stale | Task 9.1 v12: `Action="watchdog-self-quarantined"` + `Reason="4-strikes-30min"` per canonical contract | Code #11 IMPORTANT 1 |
| Task 0 test `TestUninstallWatchdogTaskInternal_AuditReason` asserted `self-quarantine-{enum-value}` action | Task 0 v12 test asserts canonical: `Action == "watchdog-self-quarantined"` AND `Reason == <enum-string>` | Code #11 IMPORTANT 1 |
| Task 10 table: `mcphub install <s>` Intent timing said "AFTER successful install" — contradicts §62 audit-first / mid-rollback | Task 10 v12: explicit "AUDIT-FIRST canonical timing: audit BEFORE any mutation; alternative valid timing: audit-mid-transaction with live rollback stack — both tested" | Code #11 IMPORTANT 2 |
| Task 9.1 lacked driver-level test proving watchdog uses snapshot path | Task 9.1 v12 adds `TestWatchdogDriver_UsesSnapshotRestartPath` — asserts `RestartContextWithSnapshot` called (NOT plain `RestartContext`); intercepts snapshot arg; verifies PortMap matches tick-start `LoadOwnershipSnapshot()`; manifest mid-tick swap doesn't change kill-target port | Code #11 MINOR + Sec #10 MED 1 |
| Task 12.2 didn't explicitly list exit-6 + `--help` documentation | Task 12.2 v12 expands: side-by-side exit-6 contexts (`mcphub gui --force --kill` vs `mcphub watchdog uninstall`); same content in `mcphub watchdog uninstall --help` | Code #11 MINOR + Sec #10 LOW |
| Out-of-scope missing `schema_version`, `watchdog doctor`, elevation-source detail deferrals | Out-of-scope v12 adds explicit deferrals for all three with rationale + v0.4.x landing target | Code #11 ❌ MISSED + Sec #10 Part C |

### v10 → v11 deltas (Codex round 10)

| v10 | v11 | Source |
|---|---|---|
| `NewOwnedXMLValidatorFromSnapshot` used in §9 driver but undefined elsewhere | §32 API table + file structure header (line 51) document it explicitly | Code #10 IMPORTANT 1 + Sec #10 MED |
| `RestartContextWithSnapshot` referenced in §59/Task 0 but missing from §32 API table | §32 API table + file structure header document it; flagged as "watchdog-only / one-tick-scope-only" | Code #10 IMPORTANT 2 + Sec #10 MED |
| §32 API table still had untyped `UninstallWatchdogTaskInternal(reason string)` | §32 API table updated to typed `UninstallWatchdogTaskInternal(reason SelfQuarantineReason) error`; reason maps to audit `Reason` field; canonical `Action="watchdog-self-quarantined"` | Code #10 + Sec #10 LOW canonicalization |
| Self-review block had stale "Old-snippet ... through round 5" line | Removed; type-consistency block notes v11 audit of §51 + §32 sync | Code #10 MINOR + Sec #10 LOW |
| Acceptance criteria omitted explicit gates for `OwnershipSnapshot.PortMap` consumption + public uninstall non-TTY/`--yes` | Acceptance criteria block expanded with 9 new gates explicitly covering snapshot/PortMap/snapshot-validator + non-TTY + audit-canonical-action | Code #10 IMPORTANT 3 |
| §62 install rollback timing ambiguous | §62 v11 spells out two valid options: (1) audit-first before any mutation; (2) audit-mid-transaction with live rollback stack; tests cover both | Sec #10 MED Install rollback |
| §63 action filter said `watchdog-self-quarantined` but earlier text used `self-quarantine-{reason}` | §63 v11 canonical: status filter is `Action == "watchdog-self-quarantined"` literal; `Reason` carries the enum value | Sec #10 LOW |
| §64 exit-6 reuse not documented re: cross-command operator confusion | §64 v11 explicit operator note: exit codes are command-scoped; CLAUDE.md (Task 12.2) and `mcphub watchdog uninstall --help` document side-by-side | Sec #10 LOW |

### v9 → v10 deltas (Codex round 9)

| v9 | v10 | Source |
|---|---|---|
| §59 says watchdog uses `RestartContextWithSnapshot` but §9 driver still calls `RestartContext` | §9 driver fixed: `LoadOwnershipSnapshot()` called; `validator := NewOwnedXMLValidatorFromSnapshot(ownership)`; restart uses `RestartContextWithSnapshot(ctx, server, daemon, ownership)` | Code #9 IMPORTANT + Sec #9 MED 1 |
| §64 prompt could hang in scripts (no `--yes`, no isatty detection) | §64 v10: `golang.org/x/term.IsTerminal` check; non-TTY without `--yes` → exit 6 + stderr message; never hangs | Code #9 IMPORTANT + Sec #9 MED |
| Task 9.1 still had stale "task name 32KB → log line ≤16KB with `_truncated:true`" | Task 9.1 v10 reflects identity-preserving rejection: `task >1KB` → `ErrIdentityOversize`; non-identity 32KB → truncate with `_task_hash` | Code #9 MINOR + Sec #9 LOW install-oversize visibility |
| §63 self-quarantine status used 7-day cutoff for audit entry | §63 v10: any-age audit entry counts; both signals required (task-missing AND audit-present) for SELF-QUARANTINED; otherwise STATUS UNKNOWN with diagnostic | Sec #9 LOW |
| Task 10 install audit-fail handling table said "log warning" | Task 10 v10 table: `mcphub install` audit-fail → fail-closed (rollback partial scheduler entry); per-row table updated with `--allow-elevated` and public `watchdog uninstall` rows | Code #9 + Sec #9 LOW install-oversize + §62 cross-ref |
| Self-review block said "every Codex finding through round 5" | Self-review block v10 explicitly references rounds 1-9; type consistency block enumerates all 14+ types | Code #9 MINOR |
| Sec #8 Part C `run_id`/`seq` had no deferred note in Out-of-scope | Out-of-scope adds explicit deferred-to-v0.4.x note for `run_id`/`seq`, sink attempt counts, EventLog status field | Sec #9 ❌ MISSED |

### v8 → v9 deltas (Codex round 8)

| v8 | v9 | Source |
|---|---|---|
| Stale Task 3 redaction missing `IsSystemEntry()` exemption | Task 3 v9 redaction includes IsSystemEntry guard + sentinel test (literal `<rotation-system>` preserved when set via sealed constructor) | Code #8 IMPORTANT 2 + Sec #8 LOW |
| Stale Task 3 16KB tests said "32KB task → truncate" | Task 3 v9 tests use `ErrIdentityOversize` REJECTION at >1KB; non-identity 32KB → truncate with `_task_hash`/`_truncated_field` | Code #8 MINOR 1 |
| Stale Task 4 schema omitted `AuditFailureWindow` / `StaleClearWindow` | Task 4 v9 schema includes BOTH; backward-compat noted | Code #8 IMPORTANT 2 |
| Stale Task 4 tests omitted 3-windows independence | Task 4 v9 tests cover sliding-window behavior for ALL three windows + independence assertions | Code #8 IMPORTANT 2 |
| Stale §32 API table had untyped `UninstallWatchdogTaskInternal()` | §32 v9 uses typed `UninstallWatchdogTaskInternal(reason SelfQuarantineReason) error` | Code #8 IMPORTANT 2 |
| §9 ctx-deadline branch still used `staleEvents, _ :=` (ignored err) | §9 + §58 v9 uses `staleEvents, err := ...; if err != nil { ... return 2 }` consistently across ALL branches | Code #8 MINOR 2 |
| §47 `LoadOwnershipSnapshot()` lacked failing tests + Task 9 still loaded only registry | Task 0 v9 adds 3 tests for snapshot (immutability, PortMap, RestartContextWithSnapshot); Task 9 driver loads BOTH snapshot AND registry | Code #8 IMPORTANT new |
| §47 snapshot didn't cover `manifestPortMap` (Restart reads manifest at runtime) | §59 v9 extends snapshot with `PortMap`; `RestartContextWithSnapshot` consumes it | Sec #8 MED 1 |
| §48 sealed pattern unclear on JSON unmarshal | §48 v9 explicit `UnmarshalJSON` ignores `system_entry`; package-private `rehydrateSystemEntryFromTrustedSource` for trusted log-tail load | Code #8 IMPORTANT 2 |
| §49 audit-degraded fallback per-OS unspecified | §60 v9 explicit Windows EventLog (`golang.org/x/sys/windows/svc/eventlog`) with source registration in `mcphub setup`; POSIX `log/syslog`; stderr partial-write retry once | Code #8 IMPORTANT + Sec #8 MED 4 |
| §55 `_priority:"high"` without `Priority` field | §55 + Task 3 v9 add explicit `Priority string` field on `IntentAuditEntry` (JSON `priority`, omitempty) | Code #8 IMPORTANT |
| §51 `mcphub install` continues on `ErrIdentityOversize` | §62 v9: `mcphub install` fail-closed on audit error (rejected with diagnostic) | Code #8 IMPORTANT 5 |
| §55 `--allow-elevated` audit failure non-blocking | §61 v9: audit-fail → exit 11 (`audit-required-but-failed`); install rejected if audit can't record override | Sec #8 MED 3 |
| §53 self-quarantined could misclassify generic schtasks failure | §63 v9: requires BOTH signals (task missing AND audit shows recent quarantine); else `STATUS UNKNOWN` with diagnostic | Sec #8 LOW |
| Public uninstall no confirm gate | §64 v9: interactive prompt + `--yes` flag + audit entry visible in status | Sec #8 Part C |
| Acceptance criteria omitted §35-§58 v8 gates | §65 v9 acceptance block explicitly lists v9 gates (snapshot, sealed entry, cascade per-OS, _task_hash, Priority, fail-closed paths, both-signals self-quarantine, public uninstall confirm) | Code #8 MINOR |

### v7 → v8 deltas (Codex round 7)

| v7 | v8 | Source |
|---|---|---|
| §5 said manifest has `daemons` map; repo shape is `config.ServerManifest.Daemons []DaemonSpec` | Plan derives `map[string]bool` once per validation from the slice (§5 v8 update) | Code #7 IMPORTANT 1 |
| Stale Task 3 redaction pseudocode omitted `SystemEntry` exemption | Task 3 + §52 reference `IsSystemEntry()` predicate from sealed constructor | Code #7 IMPORTANT 2 |
| Stale Task 4 schema omitted `AuditFailureWindow`/`StaleClearWindow` | Task 4 + §52 schema includes both windows; backward-compat noted | Code #7 IMPORTANT 2 |
| Stale §32 API table had untyped `UninstallWatchdogTaskInternal()` | §32 + §52 uses typed `UninstallWatchdogTaskInternal(reason SelfQuarantineReason)` | Code #7 IMPORTANT 2 |
| §9 still mentioned truncating `task` (stale text) | §9 v8 update: identity preservation per §35; never truncates `task` | Code #7 MINOR 1 |
| Some §9 driver branches did not check err before logging events | §58 verification: ALL branches use err-first pattern | Code #7 MINOR 2 |
| §44 self-failure indicator didn't define behavior when self-quarantined | §53 graceful `WATCHDOG SELF-QUARANTINED` rendering | Code #7 MINOR 3 |
| Acceptance criteria didn't cover §35-§46 | Acceptance criteria + §52 stale-fix expanded for §35-§58 | Code #7 MINOR 4 |
| XML validator reads manifest+registry on each call (mid-tick rotation race) | `LoadOwnershipSnapshot()` once per `--once`; passed to validator (§47) | Sec #7 MED 1 |
| `_task_hash` 48-bit collision bound undocumented | §54 documents adversarial collision bound; OOS for full SHA-256 | Sec #7 LOW |
| `SystemEntry` field publicly settable | Sealed constructor pattern (`systemEntry` unexported + `newSystemAuditEntry` package-private + `IsSystemEntry()` predicate) (§48) | Sec #7 MED 3 |
| `audit-degraded` only logs to watchdog.log; cascading failure if watchdog.log unwritable | Emergency stderr → eventlog/syslog fallback chain; exit 10 on all-paths failure (§49) | Sec #7 MED 4 |
| `--allow-elevated` no audit trace | High-priority audit entry on use; visible in status (§55) | Sec #7 LOW + Part C |
| `SelfQuarantineReason` no suggested-action | `.SuggestedAction()` method for each reason; used in §53 status output (§56) | Sec #7 Part C |
| Status output didn't show absolute paths | §57 absolute paths for all state files | Sec #7 Part C |
| CI runs both tag sets but no symbol verification | Release pipeline `go tool nm` assertion + automated test that env-fallback symbol absent in production binary (§50) | Sec #7 MED 5 + Code Review #6 MINOR |
| `ErrIdentityOversize` semantics undefined for `mcphub stop` | Classified as audit failure → fail-closed for `stop`/`stop --force` (§51) | Sec #7 MED 6 |
| 5-process flock test framing | Each process 30s timeout; downgrade to release-gate manual smoke if flaky in CI (§46 v8 note) | Sec #7 LOW flock test |
| `StaleClearWindow` separate from `CorruptStrikeWindow` (designed but not explicit) | §52 explicit text: "explicitly distinct sliding windows" with separate triggers documented | Sec #7 LOW |

### v6 → v7 deltas (Codex round 6)

| v6 | v7 | Source |
|---|---|---|
| Manifest-hash removed; `Restart` still uses `manifestPortMap` for kill discovery → tampered-manifest-driven mis-port-kill | XML validator extended with **structural task-name ownership**: global names must resolve to `manifest.ListServers()` server+daemon; lazy names must match exact workspace registry entry's `TaskName`. `Status()` alone insufficient. (§5 + §5b legacy notes) | Code #6 IMPORTANT + Sec #6 MED |
| 16KB cap could truncate `task` (identity field) | Identity fields (`task`, `task_name`, `caller_user`) NEVER truncated; `task >1KB` → `ErrIdentityOversize` (rejected); non-identity truncation marks `_truncated:true` + `_truncated_field` + `_task_hash` (12-hex SHA256 of original task) (§35 v7 fix) | Code #6 MINOR |
| `WriteWatchdogState` returns events even on err → status claims stale clear that didn't persist | `WriteWatchdogState` returns `staleEvents=nil` on err; driver checks err FIRST before logging events (§36) | Code #6 MINOR |
| CI runs only default `go test` | Task 1.4 + CI job: BOTH default tags AND `-tags=test_state_path_env`; production `go build` MUST be without tag (compile-time exclusion verified) (§43) | Code #6 MINOR + Sec #6 LOW |
| §34 redaction blindly applies to `caller_user` | `IntentAuditEntry.SystemEntry bool` field; system entries (`<rotation-system>`, self-quarantine sentinel) skipped from redaction (§37) | Sec #6 LOW system audit entries |
| Audit failures log to watchdog.log silently | High-priority `audit-degraded` event after 3 failures in 30 min; surfaced in `status`; `AuditFailureWindow` in `watchdog-state.json` (§38) | Sec #6 LOW audit disk-full |
| `UninstallWatchdogTaskInternal(reason string)` arbitrary | Typed enum `SelfQuarantineReason` (compile-time constraint) (§39) | Sec #6 LOW audit reason |
| `--once.lock.owner.json` could persist stale on cleanup failure → fake "running" PID in status | Status reads owner-info AND probes lock-busy state; displays `[STALE — lock not currently held]` if ownerinfo present but lock not busy (§40) | Sec #6 LOW stale owner |
| §33 documents singleton lock mechanics but no explicit advisory-only text | Explicit text: "POSIX flock is advisory; only honored by callers using same convention" (§41) | Sec #6 PARTIALLY |
| `mcphub watchdog install` proceeds even if elevated (privilege escalation risk on per-user task) | Refuses to install if elevated (Administrator/root) UNLESS `--allow-elevated` flag (§42) | Sec #6 Part C |
| No self-failure indicator | `mcphub watchdog status` reads its own task `LastResult`; flags `WATCHDOG SELF-FAILURE INDICATOR` if non-zero (§44) | Sec #6 Part C |
| Repeated stale-clears unobserved (could be DoS) | `StaleClearWindow` sliding 30-min in `watchdog-state.json`; ≥4 → `stale-clear-strike-alert` log + status (no auto-quarantine) (§45) | Sec #6 LOW pre-restart pending DoS |
| No flock contention test on Windows | Task 9.1 adds 5-concurrent-`--once` Windows test; exactly 1 acquires (§46) | Sec #6 LOW gofrs/flock Windows |

### v5 → v6 deltas (Codex round 5)

| v5 | v6 | Source |
|---|---|---|
| `manifest_hash.go::ComputeManifestHashFromDisk` + §10/§30 mid-tick recheck | REMOVED (manifest-hash mechanism dropped). XML validator (§5) is the security boundary. v5 §30 removed; §10 simplified. | Code #5 IMPORTANT 1 (`ManifestPath()` unresolved); manifest hash didn't add a defense layer XML validator doesn't already provide. |
| Restart-pending in-memory only until end-of-tick `WriteWatchdogState` | **Pre-restart persist:** `WriteWatchdogState` immediately AFTER `MarkRestartPending+RecordAttempt`, BEFORE `RestartContext`. | Code #5 IMPORTANT 2 |
| `IsRestartPending(taskName) bool` (uses ambient `time.Since`) | `IsRestartPending(taskName, now time.Time) bool` (clock injected) | Code #5 IMPORTANT 3 |
| `StatusContext`/`RestartContext` "wrappers over existing" but underlying not ctx-aware | Goroutine + ctx-select pattern; documented best-effort cancellation; underlying op continues until OS-level kill | Code #5 IMPORTANT 4 |
| KnownFolder fallback to env in production | **Production fail-closed if KnownFolder fails**; env fallback gated behind `_test.go` build tag | Sec #5 MEDIUM 1 (Windows stubbing + KnownFolder fallback) |
| `audit-rotated` event no failure semantic | Idempotent retry: write failure → log `audit-rotated-event-write-failed-non-fatal` to watchdog.log, no double-rotation, next successful append goes through normally | Sec #5 MEDIUM 2 |
| Restart-pending stale clear silent | Stale-clear emits `restart-pending-stale-cleared` event to `watchdog.log` + visible in `mcphub watchdog status` | Sec #5 MEDIUM 3 |
| `LoadDaemonRegistry()` returns shared snapshot | Returns immutable defensive copy | Sec #5 LOW LoadDaemonRegistry |
| `UninstallWatchdogTask` no caller distinction | Public + Internal split: `UninstallWatchdogTask()` (public; user-uninstall reason); `UninstallWatchdogTaskInternal(reason)` (self-quarantine; reason→audit) | Sec #5 LOW UninstallWatchdogTask |
| `--once.lock` flock no owner info | Lock + `*.owner.json` with PID/ts/hostname; visible in `status` | Sec #5 LOW Lock timeout + Part C |
| Quarantine prune fatal on per-file delete | Per-file delete failure logged as `quarantine-prune-failed-non-fatal`; rename success → quarantine returns success | Sec #5 LOW Prune failure |
| Audit-tail in `status` shows raw `caller_user` | Status display redacts `caller_user` to `<redacted-non-owner>` if not owner; file content unchanged | Sec #5 LOW Audit tail exposure + Part C |
| No log entry size cap | 16KB per JSON line; longest string truncated with `_truncated:true`; oversize-after-truncation drops with placeholder | Sec #5 Part C 16KB |
| Manifest mid-tick mismatch verdict | (removed with manifest-hash mechanism) | superseded by v6 §10 simplification |
| 14 tasks (Task 0 + 13) | 13 tasks (Task 0 + 12); Task 8 (`manifest_hash.go` extension) removed | superseded |

**Spec coverage (v10):** every Codex code-review and security-review finding through round 9 has a concrete plan item — verified via the deltas tables for v1→v10. No outstanding CRITICAL/IMPORTANT/MED items from rounds 1-9.

**Placeholder scan (v10):** every step has explicit code or `grep -n` instruction; no TBD; no stub-only API references that lack a Task 0 entry.

**Type consistency (v11):** all types (`DaemonIntent`, `IntentReadResult`, `IntentAuditEntry` with `Priority` + sealed `systemEntry`, `WatchdogState` with three sliding windows, `CooldownEntry` with `RestartPendingAt`, `WatchdogStateRead`, `CooldownReader`, `Cooldown`, `RecoveryDecision`, existing `DaemonStatus`, `DaemonRegistry`, `OwnedXMLValidator`, `OwnershipSnapshot` with `PortMap`, `SelfQuarantineReason` enum, error sentinels `ErrIdentityOversize`/`ErrManifestRaced`/`ErrXMLOversize`/`ErrXMLDoctypeRejected`/`ErrXMLTooDeep`/`ErrXMLMalformed`/`ErrSchtasksTimeout`/`ErrUnstructuredOwnership`) consistent across Tasks 0-13. v11 audited the file structure header (line 51) AND §32 API table to ensure `RestartContextWithSnapshot` and `NewOwnedXMLValidatorFromSnapshot` are documented in BOTH places.

**Placeholder scan:** every step has explicit code or `grep -n` instruction; no TBD. `ManifestPath()` removed entirely (no longer needed).

**Type consistency:** all types consistent across Tasks.

---

## Acceptance criteria

- [ ] `mcphub watchdog --once` runs in <5s on a clean system; honors 4min ctx deadline; singleton-locked.
- [ ] Force-killed daemon recovers within 5 min via watchdog.
- [ ] `mcphub stop --server X` no revival within 24h.
- [ ] `mcphub stop --server X --force` (intent unwritable + audit OK) → audit + kill.
- [ ] `mcphub stop --server X --force` (both unwritable) → fail-closed.
- [ ] `mcphub watchdog disable --server X` → no revival ever.
- [ ] Chronic always-failing daemon auto-disabled after ~2h.
- [ ] `watchdog.log`, `intent-audit.log` rotate at 10MB; `audit-rotated` self-event written; idempotent on failure.
- [ ] Quarantines retained 5 newest, post-rename pruned under flock; **per-file delete failures non-fatal**.
- [ ] `<Hidden>false</Hidden>`.
- [ ] Validator rejects DOCTYPE, oversize (>64KB), depth >32, schtasks timeout. No XML cache.
- [ ] Maintenance/orphan/lazy-proxy-failed-lifecycle excluded.
- [ ] Bootstrap restarts managed; skips orphan.
- [ ] Mixed-bootstrap → default-running.
- [ ] Clock-skew future > 5min → fail-closed.
- [ ] Wall-clock jump > 24h → suppress.
- [ ] Wall-clock missing baseline after corrupt tick → suppress.
- [ ] Corrupt intent/state → quarantine + suppress + strike count.
- [ ] **CorruptStrikeWindow ≥4 in 30min → self-quarantine via `UninstallWatchdogTaskInternal` + exit 9.**
- [ ] **No manifest-hash check (v6 removed). XML validator is the security gate.**
- [ ] **Restart-pending durably persisted BEFORE `RestartContext` invocation.**
- [ ] Restart-budget guard: ctx remaining < 60s → skip.
- [ ] **Restart-verify is observation-only; NO `RecordRunning` call.**
- [ ] **`IsRestartPending(name, now)` accepts injected clock; pure-function contract preserved.**
- [ ] **`restart-pending-stale-cleared` event emitted to watchdog.log + visible in `mcphub watchdog status`.**
- [ ] All UTC RFC3339Nano timestamps.
- [ ] On POSIX: state dir 0700, files 0600.
- [ ] On POSIX: world/group-writable parent OR non-owner-uid → exit 8.
- [ ] **Production Windows binary uses `SHGetKnownFolderPath` ONLY; env fallback gated behind `_test.go` build tag.**
- [ ] **`StatusContext`/`RestartContext` use goroutine + ctx-select pattern; ctx cancellation returns to caller within ~10ms even though underlying op continues (best-effort, documented).**
- [ ] **`LoadDaemonRegistry()` returns immutable defensive copy.**
- [ ] **`UninstallWatchdogTaskInternal(reason SelfQuarantineReason)` writes audit `Action="watchdog-self-quarantined"` with `Reason=<enum-value>` (canonical action filter per §63 v11).**
- [ ] **(v11 §47/§59) `LoadOwnershipSnapshot()` returns immutable snapshot including `PortMap`; `RestartContextWithSnapshot(ctx, server, daemon, snap)` uses snap's PortMap for kill-by-port discovery (NOT live manifest).**
- [ ] **(v11 §47) `NewOwnedXMLValidatorFromSnapshot(snap)` constructor present; structural ownership checks use snapshot, not live manifest.**
- [ ] **(v11 §59) `RestartContextWithSnapshot` documented one-tick-scope-only; tests assert snapshot freezing across simulated mid-tick manifest swap.**
- [ ] **(v11 §62) `mcphub install` audit-fail → fail-closed; rollback any partially-created scheduler entry; end-state identical to never-attempted install.**
- [ ] **(v11 §63) Self-quarantine status filter is canonical: `Action == "watchdog-self-quarantined"` literal; ignores generic `Priority="high"` system entries.**
- [ ] **(v11 §64) `mcphub watchdog uninstall` is non-TTY-safe: `golang.org/x/term.IsTerminal(os.Stdin.Fd())` check; no `--yes` + non-TTY → exit 6 + stderr message + audit NOT written + scheduled task NOT removed.**
- [ ] **(v11 §64) `mcphub watchdog uninstall --yes` (TTY or not) → proceeds + audit entry `watchdog-uninstalled-by-user` Priority=high.**
- [ ] **(v11 §32) §32 API table documents `RestartContextWithSnapshot`, `NewOwnedXMLValidatorFromSnapshot`, `LoadOwnershipSnapshot`; typed `UninstallWatchdogTaskInternal(SelfQuarantineReason)`. File structure header (line 51) matches.**
- [ ] **`--once.lock` carries PID/ts/hostname owner-info file; `mcphub watchdog status` surfaces last flock skip.**
- [ ] **JSON Lines logs cap at 16KB per entry with `_truncated:true`; oversize-after-truncation drops with placeholder.**
- [ ] **`mcphub watchdog status` audit-tail redacts `caller_user` for non-owner entries.**
- [ ] All existing tests pass; new tests cover Task 0 surfaces + recovery + intent + audit + cooldown + XML validator + state paths + CLI.
- [ ] `RecoverStoppedDaemons` strictly pure.
- [ ] `IsRealFailure` exported; tray + cli/gui_tray_state import; `grep -rn` shows ONE definition.
- [ ] `DaemonStatus` is existing type from `internal/api/types.go`.
- [ ] Audit log captures `caller_exe`, `caller_pid`, `caller_start_time` (UTC), `caller_user`.
- [ ] Manual smoke D2.4 + D2.6 (16 sub-cases) PASS.

---

## Out of scope (defer to v0.4.x or later)

**Wording note (v13 — Code Review #12 MINOR):** "post-v0.3.0" and "v0.4.x" both mean the same thing in this plan ("after v0.3.0 ships"). Items below use whichever phrase is most natural; specific items with concrete v0.4.x landing target are marked with `→ v0.4.x`, items deferred indefinitely (no committed landing version) say `defer indefinitely`.

- Linux systemd timer + macOS launchd plist for cross-platform watchdog. → v0.4.x
- GUI Settings UI for watchdog enable/disable per daemon. → v0.4.x
- Auto-disable notification (toast/banner) when chronic-failure fires. → v0.4.x
- Intent file migration on schema bump. defer indefinitely (only needed if v0.4.x changes schema in incompatible way)
- Audit log signing / tamper detection (single-user 0600 boundary acceptable for v0.3.0). defer indefinitely
- **`run_id` + per-process `seq` in audit/log entries** (Sec #8 Part C deferred). Optional sequence-number scheme for detecting partial writes across process restarts. v0.3.0 uses single-line append + atomic `os.OpenFile(O_APPEND)`; tearing is unlikely below the OS write-buffer threshold (typically 4KB; entries cap at 16KB but typical entries are <2KB). → v0.4.x if forensic evidence shows torn writes.
- **Sink-attempt counts in `audit-degraded` entry** (Sec #9 Part C). Future enhancement to record exactly which fallback sinks were attempted/succeeded/failed within a single cascade. v0.3.0 logs each individual sink failure as a separate `watchdog.log` entry; aggregated view → v0.4.x.
- **EventLog source registration status in `mcphub watchdog status`** (Sec #9 Part C). Adds a line to status output like `EventLog source: registered ✓` / `not registered ✗ (admin required)`. Useful but cosmetic; deferred to v0.4.x.
- **`schema_version` field in `mcphub watchdog status --json`** (Sec #10/#11 Part C). Adds a versioning marker for forward-compat with future status-output schema changes. Cheap to add but no consumer needs it for v0.3.0; deferred to v0.4.x.
- **`mcphub watchdog doctor` command** (Sec #10 Part C). Standalone diagnostic command running all health checks (state file integrity, lock file owner-info, EventLog source registered, audit log writable, etc.) with green/yellow/red report. v0.3.0 surfaces enough operator diagnostics via `mcphub watchdog status` (CorruptStrikeWindow, AuditFailureWindow, StaleClearWindow, last events, abs paths); doctor command is consolidation/UX, not new capability. Deferred to v0.4.x if operator demand materializes.
- **`--allow-elevated` audit elevation-source detail** (Sec #10 Part C). Records the OS-detected privilege source (e.g., `TokenElevation=Full`, `Linked-token`, `LSA-elevated-token` on Windows; `setuid` / `geteuid==0` source on POSIX) inside the `watchdog-install-elevated-override` audit entry for forensic detail. Low-effort (single field) but not blocking v0.3.0 ship; deferred to v0.4.x.
- NTP-aware mid-process clock-skew detection. defer indefinitely
- Per-task watchdog-disabled custom cadence. → v0.4.x
- HMAC-protected XML validation cache (v5 removed cache; if AV problems materialize, revisit with HMAC). defer indefinitely (only revisit if AV problems materialize)
- **Deep ctx propagation in scheduler/Restart underlying ops.** v6 uses goroutine + ctx-select for caller-side cancellation; underlying schtasks/Status calls remain non-cancellable. OS-level `ExecutionTimeLimit=PT5M` guarantees the watchdog process is killed if underlying ops hang. Refactoring `internal/api.Restart` and `internal/scheduler.Run/Stop` to thread ctx through schtasks invocations is a larger refactor. → v0.4.x
- **Manifest-hash-based runtime tamper detection.** v6 relies entirely on XML validator at restart time. If a future threat model requires manifest-level tamper detection, a per-server manifest hash check (snapshot at task install time, recheck before restart) could be added. defer indefinitely

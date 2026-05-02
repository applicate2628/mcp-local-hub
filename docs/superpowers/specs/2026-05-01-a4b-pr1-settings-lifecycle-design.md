# Phase 3B-II A4-b PR #1 — Settings lifecycle (design memo, r6)

**Status:** **PASS** — Codex r5 verdict 2026-05-01. 5 review rounds (r1-r5), 22 fixes total applied, 0 blockers, ready for writing-plans skill.
**Author:** Claude Code (brainstorm 2026-05-01)
**Predecessors:**
- A4-a memo: [`2026-04-27-phase-3b-ii-a4-settings-design.md`](2026-04-27-phase-3b-ii-a4-settings-design.md) (registry, save flow, sections)
- C1 memo: [`2026-04-29-c1-force-takeover-design.md`](2026-04-29-c1-force-takeover-design.md) (Verdict struct, force-kill semantics)
- Master design: [`2026-04-17-phase-3-gui-installer-design.md`](2026-04-17-phase-3-gui-installer-design.md) §5.7
- Backlog: [`docs/superpowers/plans/phase-3b-ii-backlog.md`](../plans/phase-3b-ii-backlog.md) row 9b

## 1. Summary

A4-b PR #1 finishes the Settings lifecycle items that A4-a deferred behind `Deferred:true` flags **except** the two pure runtime mutators (tray show/hide, port live-rebind), which split into PR #2. PR #1 covers six sub-items:

1. **Per-workspace weekly-refresh membership UI** (table inside `SectionDaemons`) + opt-in default driven by a new `daemons.weekly_refresh_default` registry setting.
2. **Weekly schedule edit** — typed `ScheduleSpec` parser + dedicated `PUT /api/daemons/weekly-schedule` route with transactional YAML+scheduler swap.
3. **Retry policy edit** — `daemons.retry_policy` flips from `Deferred:true` to editable; `RetryPolicy` interface + unit tests ship in PR #1, runtime applier wires in PR #2.
4. **Clean-now confirm flow** — `backups.clean_now` Action wired to a new `<ConfirmModal>` component reused for kill-stuck (sub-item 6).
5. **Export config bundle** — `advanced.export_config_bundle` Action streams a `.zip` from `POST /api/export-config-bundle`.
6. **Recover stuck instance** — two-click flow: `POST /api/force-kill/probe` (read-only Verdict) → if `Class==Stuck` and identity gate passes, `<ConfirmModal>` → `POST /api/force-kill`.

Design priority for r2: **no surprises, maximum stability, future extensibility.** Recommendations that maximize one at the cost of another were rejected (e.g., breaking CLI default flips were replaced with a configurable knob; `window.confirm` was replaced by a small reusable modal). Where extensibility motivates a heavier seam (typed parser, dedicated daemon-routes, RetryPolicy interface), the seam ships in PR #1 even when its consumer ships in PR #2.

## 2. Locked decisions

### D1. Opt-in default driven by `daemons.weekly_refresh_default` setting

A new boolean key `daemons.weekly_refresh_default` (default `false`) is added to `internal/api/settings_registry.go`. Entry-points that create a new `WorkspaceEntry` from a fresh user action read this knob:

- CLI `mcphub register`: explicit `--weekly-refresh` / `--no-weekly-refresh` flags override; absence reads the knob.
- GUI add-workspace path (when added — not in this PR): same.

`internal/api/legacy_migrate.go:161` is **explicitly excluded** from the knob (Codex r1 D1): legacy migration is a one-time intent-known operation that imports a pre-existing user setup with established expectations. Flipping legacy entries from `true` to `false` would surprise users whose pre-A4-b workflow relied on every imported workspace getting weekly refresh by default. The hardcoded `WeeklyRefresh: true` stays in `legacy_migrate.go` with a clear comment: *"Legacy import preserves the pre-A4-b register-time default. New register operations honor `daemons.weekly_refresh_default`."*

This eliminates the breaking-change to CLI default while still making the new behavior the default for new register operations. Operators who relied on the implicit `true` set the knob once.

**Why:** centralizing the default behind one persisted knob means new register entry-points (auto-register, IDE-driven register, future "import from foo") all honor one source of truth without per-call-site policy. Legacy migration is a one-shot import path with known intent and stays explicitly hardcoded.

### D2. Existing entries are not migrated

Whatever `WeeklyRefresh` value lives in `workspaces.yaml` today is preserved on first read. The membership table renders that state as the current value. The user changes it manually.

**Why:** auto-flipping established behavior risks surprising users whose weekly refresh is currently working as intended. Visible state + manual change is safer.

### D3. Membership table inside `SectionDaemons`

Existing precedent: `SectionDaemons` already owns `daemons.weekly_schedule` and `daemons.retry_policy`. Membership table renders below the cron + retry fields. Co-locates "what runs weekly + when + how + which workspaces."

### D4. Membership writes share `useSectionSaveFlow` (multi-op save with explicit transaction boundaries)

One Save button at the bottom of `SectionDaemons` collects dirty state from cron edit + retry edit + membership toggles + the `weekly_refresh_default` knob. **The Save button orchestrates three independent transactional operations** (Codex r1 D3-D6/D8 — clarification: this is NOT one global atomic transaction across YAML + Task Scheduler + workspace registry):

1. **Settings save** (atomic per-key, via existing `PUT /api/settings/{key}`): writes `daemons.weekly_refresh_default` and `daemons.retry_policy` if dirty. Uses `settingsMu`. Each key is independently persisted.
2. **Weekly schedule swap** (transactional within its own boundary, via `PUT /api/daemons/weekly-schedule`): YAML write of `daemons.weekly_schedule` + Task Scheduler trigger replacement, with best-effort rollback (D8 details).
3. **Membership update** (atomic, via `PUT /api/daemons/weekly-refresh-membership`): one `registryMu` + one `Registry.Save` to `workspaces.yaml`.

These three operations execute **sequentially** in the order above. **Partial-failure semantics:**

- If op 1 fails: ops 2 and 3 are skipped; user sees a banner naming the failed key. Dirty state retained for retry.
- If op 2 fails: op 3 is skipped; user sees a banner with the schedule-error and rollback status. Dirty state retained for the schedule field; settings keys from op 1 are committed (visible). Membership stays dirty for the next Save.
- If op 3 fails: user sees a banner naming the failed entry; ops 1+2 are committed (visible). Membership stays dirty for the next Save.

The save flow does **not** roll back successful prior operations on a later op's failure. Rationale: each op is independently observable and recoverable from the GUI state, and rolling back op 1's persisted settings on a failed op 3 would surprise users who already see "settings saved" feedback.

**UI messaging contract** (Codex r2 D4 specific answer applied): the partial-failure banner must explicitly tell the user which prior ops committed and which remain dirty. Example copy: *"Settings saved. Schedule update failed (degraded restore — see Recovery). Membership not attempted; still dirty."* This makes "op 1 may already be committed if op 2/3 fails" visible to the user rather than implicit.

This contract is also documented in the section's help-line so the user understands one Save is one button + multiple ops, not one atomic write.

### D5. Membership endpoint shape — structured array body

```
PUT /api/daemons/weekly-refresh-membership
Content-Type: application/json
Body: [
  {"workspace_key": "D:/dev/proj-a", "language": "python", "enabled": true},
  {"workspace_key": "D:/dev/proj-a", "language": "rust",   "enabled": false}
]
Response: 200 + {"updated": <n>, "warnings": []}
         400 if any (workspace_key, language) pair is unknown
         500 on transactional save failure
```

**Why structured array, not map keyed by `key+lang`:** workspace keys can contain `:` and `/` which collide with naive separator schemes. An object array is RESTier and cannot mis-parse keys.

**Idempotent partial update:** entries listed in the body are updated to the given `enabled`. Entries **not** in the body are unchanged. UI sends only dirty cells.

**Atomicity:** one mutex acquire (`registryMu`), one `Registry.Save` call. Either all listed toggles persist or none do.

### D6. UI affordances above the membership table

- `Select all` / `Clear all` text links — bulk toggle, dirty-tracked the same way as individual cells.
- No pagination, no filter input. YAGNI for typical workspace×language counts.
- Row order: registry order from `workspaces.yaml`.
- Empty state: "No workspaces registered yet — `mcphub register` from a workspace folder to add one."

### D7. Cron parser — typed `ScheduleSpec` with one Kind today

A new file `internal/api/schedule_parser.go` exposes:

```go
type ScheduleKind string
const (
    ScheduleWeekly ScheduleKind = "weekly"
    // future: ScheduleDaily, ScheduleCron
)

type ScheduleSpec struct {
    Kind      ScheduleKind
    DayOfWeek int  // 0=Sunday..6=Saturday  (Kind==Weekly)
    Hour      int  // 0..23
    Minute    int  // 0..59
}

func ParseSchedule(s string) (*ScheduleSpec, error)
```

Today only `weekly DAY HH:MM` is accepted (DAY ∈ Mon|Tue|Wed|Thu|Fri|Sat|Sun, case-insensitive). Anything else returns a typed error with the canonical example. The existing regex pattern in `settings_registry.go` for `daemons.weekly_schedule` is tightened to match exactly what the parser accepts (no more permissive `daily` allowance that the parser doesn't understand).

**Why typed struct:** scheduler-update callsite (`internal/api/scheduler_swap.go`) consumes `*ScheduleSpec`, not raw string. Future Kind additions (`daily`, `cron`) extend the parser with new cases; callsites stay unchanged. No string-parsing duplicated in callsites.

### D8. Dedicated `PUT /api/daemons/weekly-schedule` with best-effort restore + preflight

(Codex r1 D8 applied: rollback restated as best-effort, preflight on snapshot failure, explicit `restore_status` field in response.)

```http
PUT /api/daemons/weekly-schedule
Body: {"schedule": "weekly Sun 03:00"}

Response on success (200):
  {"updated": true, "schedule": "weekly Sun 03:00", "restore_status": "n/a"}

Response on parse error (400, outside the rollback-envelope per Codex r3 D8/§4):
  {"error": "parse_error", "detail": "<parser message>", "example": "weekly Sun 03:00"}
  (Note: parse errors are 4xx — input rejected before any side-effect — and do NOT carry
   "updated" or "restore_status" because no transaction was attempted. The 5xx envelope
   below applies only to transactional failures past parse.)

Response on preflight failure (500):
  {"error": "snapshot_unavailable", "detail": "<scheduler error>",
   "updated": false, "restore_status": "n/a"}

Response on swap failure with successful restore (500):
  {"error": "scheduler_swap_failed", "detail": "<step error>",
   "updated": false, "restore_status": "ok"}

Response on swap failure with degraded restore (500):
  {"error": "scheduler_swap_failed", "detail": "<step error>",
   "updated": false, "restore_status": "degraded",
   "manual_recovery": "Run 'mcphub workspace-weekly-refresh-restore' or restart mcphub to re-create the task."}
```

Handler steps:

1. `ParseSchedule(body.schedule)` → `*ScheduleSpec` (or 400 with `error: "parse_error"`).
2. Acquire `settingsMu` + `schedulerMu`.
3. **Preflight: ExportXML the existing scheduler task.** If ExportXML returns `ErrTaskNotFound`, treat as a fresh-install case and proceed without a snapshot (the swap simply Creates the task; failure leaves no task, which is the same state as before — no rollback needed). If ExportXML returns any other error (transient access failure, scheduler service down, permission denied), **abort the entire swap** with `error: "snapshot_unavailable"`. Do **not** proceed to delete or write settings.
4. Snapshot the prior settings YAML value (in-memory) for potential rollback.
5. Write new value to `gui-preferences.yaml`.
6. Call `SwapWeeklyTrigger(spec, priorXML)` — Delete + Create with new trigger; helper does best-effort ImportXML(priorXML) on Create failure.
7. **On step 6 success** (helper returned `("n/a", nil)`): the new task is installed; respond 200 with `restore_status="n/a"`. No rollback work to do.

8. **On step 6 failure** (helper returned `(_, err)` with non-nil err): the helper has already attempted scheduler-XML rollback per its contract (handler does NOT redo ImportXML). Handler now restores settings YAML from the in-memory snapshot taken in step 4. Combine the helper's `restoreStatus` with the settings-rollback result into the final response `restore_status`:

   | helper restoreStatus | settings rollback | response `restore_status` | meaning |
   |---|---|---|---|
   | `"n/a"` (fresh-install Create failed) | ok | `"n/a"` | settings reverted; no task installed (same state as before swap) |
   | `"ok"` (had-prior-task Create failed, ImportXML restored) | ok | `"ok"` | settings reverted; prior task restored |
   | `"degraded"` (had-prior-task Create failed, ImportXML failed) | ok | `"degraded"` | settings reverted; prior task lost; manual recovery needed |
   | any value | **failed** | `"failed"` | settings rollback itself failed; system in unknown state |

9. Return the appropriate 5xx response carrying the original Create error in `detail` and the combined `restore_status`. If `restore_status == "degraded"` or `"failed"`, include the `manual_recovery` hint string.

This combination rule is documented as the same truth table at the call site in `internal/gui/settings_daemons_handlers.go` so future readers see the mapping next to the implementation.

**Why best-effort:** ImportXML can fail for reasons outside the handler's control (Task Scheduler service hiccup, XML format reject, transient permission issue). Promising "atomic rollback" when the rollback path itself can fail is a contract lie. Returning an explicit `restore_status` value lets the GUI surface accurate user-facing messaging: a `degraded` response shows the recovery hint; `ok` shows just the swap error; `failed` shows a stronger warning to restart mcphub.

**Why preflight:** if we cannot snapshot the prior XML, we cannot guarantee even best-effort recovery from a failed Create. Failing fast before the destructive Delete keeps the system in its prior good state. The fresh-install case is a documented exception (no prior snapshot needed because there is no prior task).

Settings handler stays a pure persistence layer; daemon-lifecycle routes own external-system side effects + best-effort atomicity.

**Helper ownership boundary** (Codex r2 D8 applied): the route handler owns the settings YAML rollback; the scheduler helper owns **only** the scheduler XML lifecycle. Boundary specified:

```go
// internal/api/scheduler_swap.go
//
// SwapWeeklyTrigger replaces the weekly refresh Task Scheduler trigger with
// the new schedule. It owns ONLY: Delete → Create → optional ImportXML(priorXML)
// rollback on Create failure. It does NOT call ExportXML and does NOT touch
// settings YAML — those belong to the caller.
//
// Caller responsibilities (NOT the helper's):
//   - Call ExportXML before invoking this helper to obtain priorXML (or
//     determine the fresh-install case where no prior task exists).
//   - Pass priorXML == nil for the fresh-install case (helper skips
//     ImportXML on Create failure because there is nothing to restore).
//   - Pass priorXML != nil for the had-prior-task case (helper attempts
//     ImportXML on Create failure as best-effort restore).
//   - Own settings YAML rollback if the caller wrote settings before
//     invoking this helper.
//
// Returns (restoreStatus, err) — disjoint, unambiguous combinations only:
//
//   ("n/a", nil)        Create succeeded; no rollback was needed. This is
//                       the success case for BOTH the fresh-install path
//                       (priorXML == nil) and the had-prior-task path
//                       (priorXML != nil) — in either, the new task is
//                       installed and no restore happened.
//
//   ("ok", err)         Create FAILED, priorXML != nil, ImportXML(priorXML)
//                       restored the prior task successfully. err carries
//                       the original Create error so the caller knows the
//                       new schedule did not take effect. Caller maps to
//                       response error code with restore_status="ok".
//
//   ("degraded", err)   Create FAILED, priorXML != nil, AND ImportXML
//                       FAILED. The prior task is lost; manual recovery
//                       needed. err carries the original Create error;
//                       caller may surface ImportXML error in detail.
//                       Caller maps to response with restore_status="degraded".
//
//   ("n/a", err)        Create FAILED, priorXML == nil (fresh-install case).
//                       No rollback was attempted because there was nothing
//                       to restore. State is the same as before the swap
//                       (no task exists). Caller still treats this as a
//                       failed swap and rolls back settings YAML; if that
//                       settings rollback succeeds, response carries
//                       restore_status="n/a"; if settings rollback fails,
//                       response carries restore_status="failed".
//
// All other combinations are disallowed by the contract. The caller's
// truth table in D8 step 8 is exhaustive over these four cases.
func SwapWeeklyTrigger(spec *ScheduleSpec, priorXML []byte) (restoreStatus string, err error)
```

The route handler in `internal/gui/settings_daemons_handlers.go` orchestrates the full transaction:

1. `ParseSchedule(body)` (handler).
2. ExportXML preflight (handler) — failure returns `snapshot_unavailable` 500.
3. Read prior settings value into in-memory snapshot (handler).
4. Write new settings value to YAML (handler).
5. Call `SwapWeeklyTrigger(spec, priorXML)` (helper).
6. On helper error: restore settings YAML from in-memory snapshot (handler). Combine the helper's `restoreStatus` with the settings rollback result into the response's `restore_status` field. Return appropriate 500.

This keeps the helper reusable for PR #2 (and any future task-mutating route) without baking settings concerns into the scheduler-XML helper.

### D9. Retry policy — preference editable in PR #1, timing-only interface staged for PR #2

(Codex r1 D9 applied: `RetryPolicy` is timing-only. Error retryability lives in a separate classifier. Non-retryable error classes documented.)

`daemons.retry_policy` flips `Deferred:true` → `false`. User can save the value via existing `PUT /api/settings/{key}`. Help string says: `"Retry policy on daemon failure. Edit value here; runtime applier ships in A4-b PR #2."`

PR #1 also ships two separate concerns:

**1. Timing/budget — `internal/api/retry.go`:**

```go
// RetryPolicy controls timing and attempt budget for retryable errors.
// It does NOT decide whether an error is retryable — that is the classifier's job.
type RetryPolicy interface {
    Backoff(attempt int) time.Duration  // delay before next attempt; attempt is 1-indexed
    MaxAttempts() int                    // total attempt budget (0 = no retries)
}

func PolicyFromString(s string) (RetryPolicy, error)  // "none"|"linear"|"exponential"
```

**2. Error classification — `internal/api/retry_classifier.go`:**

```go
// IsRetryableError returns false for errors that are non-retryable regardless
// of policy timing — retrying would waste the attempt budget on a guaranteed-fail.
//
// Non-retryable classes (Codex r1 D9):
//   - binary-not-found (LSP server / mcp-language-server / gopls-mcp executable absent)
//   - permission denied (EACCES, ERROR_ACCESS_DENIED on lock file or working dir)
//   - bad config (manifest validation failure, malformed YAML)
//   - unrecoverable lock state (lock file owned by foreign image; see C1 identity gate)
//
// All other errors (transient I/O, port-bind conflicts, scheduler hiccups, network)
// are classified retryable.
func IsRetryableError(err error) bool
```

**3. Compose at the callsite (PR #2):**

```go
// PR #2 callsite (illustrative; ships in PR #2):
for attempt := 1; ; attempt++ {
    err := startDaemon(...)
    if err == nil { return nil }
    if !IsRetryableError(err) { return err }       // immediate exit; no policy backoff
    if attempt >= policy.MaxAttempts() { return err }
    time.Sleep(policy.Backoff(attempt))
}
```

**Why split:** retryability is an error-domain property (fixed by the error type), not a timing-policy property. Putting `IsRetryable(err)` on `RetryPolicy` would be saying "exponential might consider X retryable but linear wouldn't" — that is not a sane shape. Keeping them orthogonal lets PR #2 add new non-retryable classes without recompiling every policy.

**Why ship both interfaces in PR #1:** PR #2 (callsite wiring) becomes a pure compose-at-callsite delta. Both contracts + their unit tests are decoupled from the integration risk. If PR #2 slips, PR #1 still delivers what the backlog promises ("retry policy edit") plus the foundation for the runtime applier.

**Unit tests:**

- `RetryPolicy`: backoff sequences for each policy ("linear" returns Δ·attempt, "exponential" returns 2^(attempt-1) bounded at 5min, "none" returns 0); `MaxAttempts` ("none"=0, "linear"=3, "exponential"=5 default; configurable later).
- `IsRetryableError`: documented non-retryable classes return false; common transient errors (port conflict, EAGAIN, network timeout) return true; `nil` returns false (degenerate but well-defined).
- `PolicyFromString`: known strings return matching policies; unknown returns error.

### D10. Reusable `<ConfirmModal>` component (replaces native `window.confirm` for destructive actions)

A new component:

```tsx
type ConfirmModalProps = {
  title: string;
  body: preact.ComponentChildren;     // can include counts, lists, danger callouts
  confirmLabel: string;
  danger?: boolean;                    // red confirm button when true
  onConfirm: () => void | Promise<void>;
  onCancel: () => void;
};
```

Used in PR #1 by:

- Clean-now action (sub-item 4): `title="Delete eligible backups?", body=<>Delete <b>{N}</b> backups across <b>{M}</b> client(s)? Originals are never cleaned.</>, confirmLabel="Delete", danger`.
- Force-kill confirm step (sub-item 6): `title="Kill stuck mcphub process?", body=<>PID {pid} ({image}, started {start}). The process will be terminated immediately.</>, confirmLabel="Kill PID {pid}", danger`.

Two callsites in one PR justify the component. `useUnsavedChangesGuard` keeps `window.confirm` — that path is navigation, not destructive action.

**Accessibility:** Esc cancels, click-outside cancels, focus trap inside modal, focus returns to triggering button on close. **Pattern is copied locally from A3-a's `AddSecretModal`** rather than extracted into a shared `useFocusTrap` hook (Codex r1 D10): hook extraction in PR #1 would require refactoring `AddSecretModal` to consume the new hook, expanding the regression surface beyond PR #1's scope. The focus-trap logic in `ConfirmModal` is ~15-20 LOC; a future refactor PR can extract `useFocusTrap` once both modals (and any third) prove the same lifecycle needs. If during implementation a no-behavior-change extraction emerges naturally (e.g., the implementer notices the two modals are byte-identical in trap logic), extraction is acceptable — but this spec does not require it.

### D11. Export bundle — `.zip` streamed from one POST

```
POST /api/export-config-bundle
Body: {}
Response:
  Content-Type: application/zip
  Content-Disposition: attachment; filename="mcphub-bundle-<YYYYMMDD-HHMMSS>.zip"
```

Bundle contents:

- `servers/<name>/manifest.yaml` — every manifest under `%APPDATA%\mcp-local-hub\servers\` (or platform equivalent).
- `secrets.json` — encrypted secrets store as-is (ciphertext, not decrypted).
- `gui-preferences.yaml` — full settings file.
- `workspaces.yaml` — full registry.
- `bundle-meta.json` — `{"export_time": "...", "mcphub_version": "...", "platform": "...", "hostname": "redacted"}`. The hostname field is the literal string `"redacted"` (not `<host>`, not omitted) so the bundle's JSON shape stays stable for downstream tooling. Codex r1 D11 applied.

Backup files (`*.bak.<ts>`) are **not** included. Out of scope for "config bundle"; would inflate size.

**Restore limitations** (Codex r1 D11): the bundle is intended for **support and configuration sharing**, not for cross-machine restore of a working installation. `secrets.json` contains ciphertext encrypted with the source machine's master password — without that password, secrets cannot be decrypted. The action's Help string and the GUI affordance description both state this explicitly so users do not mistake the bundle for a backup-and-restore feature: *"Bundle contains encrypted secrets only — restoring on a different machine requires the same master password used at export time."*

Streaming is direct: handler creates a `zip.Writer` over `http.ResponseWriter`, walks the file set, writes each entry, closes. No temp file. No two-step. No race window.

### D12. Force-kill button — two-click flow with separate endpoints

`SectionAdvanced` adds two affordances grouped under a "Diagnostics" subheading:

```
Advanced
────────
[Open mcp-local-hub data folder]
[Export config bundle]

Diagnostics
───────────
[Diagnose lock state]      ← first click; read-only Probe
  ↓ result strip renders here when probe completes
  ↓ if Class == Stuck AND identity gate passes:
[Kill stuck PID 1234]      ← second click; opens ConfirmModal → POST kill
```

Backend:

```
POST /api/force-kill/probe
Body: {}
Response: 200 + Verdict (Diagnose/Hint stripped per C1 contract)

POST /api/force-kill
Body: {}
Response: 200 + Verdict (post-kill state)
         403 if identity gate refuses
         412 if lock file changed mid-flight (race)
         500 on kill syscall failure
```

Two endpoints, not one. Probe is read-only; kill is destructive. Future "diagnostic dashboard" can reuse Probe alone.

The kill button only renders when ALL of the following hold (Codex r1 D12 applied: index-safe access + clock semantics):

- `verdict.Class === "Stuck"`, AND
- `verdict.PIDImage` matches the C1 identity gate (basename ∈ {`mcphub.exe`, `mcphub`}, case-insensitive on Windows), AND
- **Index-safe cmdline check:** `(verdict.PIDCmdline.length > 1 && verdict.PIDCmdline[1] === "gui") || verdict.PIDCmdline.length <= 1`. The explicit `length > 1` guard before the index access is mandatory; without it a Verdict with `PIDCmdline.length === 1` (Explorer/Start-menu double-click case, which `cmd/mcphub/main.go:32` defaults to gui internally) would short-circuit-evaluate to `undefined !== "gui"` → false, incorrectly suppressing the kill button. The guard makes the contract self-documenting and panic-proof.
- `verdict.PIDStart < verdict.Mtime` — see clock-semantics note below.

**Clock-semantics note** (Codex r1 D12 applied): `PIDStart` and `Mtime` come from different OS APIs and may have different resolution and timezone semantics. The Verdict struct documents the source explicitly:

- `verdict.PIDStart`: process creation time. Source: `/proc/<pid>/stat` field 22 jiffies (Linux) or `GetProcessTimes` (Windows). Both APIs report monotonic kernel times convertible to wall-clock UTC. Resolution: ≥1ms (Linux jiffies) / ≥100ns (Windows FILETIME). Verdict carries this as **UTC**.
- `verdict.Mtime`: lock file modification time. Source: `os.Stat(lockPath).ModTime()`. NTFS has 100ns resolution; most POSIX filesystems have ≥1s resolution. Verdict carries this as **UTC**.
- **Comparison semantics:** strict less-than (`<`) without tolerance. Identity gate reads "process started BEFORE the lock file was last touched." If the two timestamps are equal (rare; requires sub-resolution coincidence), the gate fails closed — no kill button — to avoid false positives. POSIX systems with ≥1s mtime resolution may hit this on a very fast cold start; the CLI `--force --kill` recovery path remains available in that case.

These checks happen client-side from the Verdict's structured fields. The server enforces them again on `POST /api/force-kill` via C1's `KillRecordedHolder` identity gate; client-side gating is UX-only — a malicious or out-of-date GUI cannot bypass the server check. The two implementations MUST use identical comparison semantics; the C1 memo §"PID identity" is the authoritative reference.

Verdict presentation: a compact result strip rendering Class as a label (`Healthy`, `Stuck (lock held by PID 1234)`, `Mismatched (image: cmd.exe)`, etc.), plus a `<details>` toggle showing PID/Port/Image/Cmdline/Start/PingMatch.

**Labels:**
- First button: `Diagnose lock state`
- Second button: `Kill stuck PID {pid}` (rendered with the actual PID baked into the label)

Two distinct labels per phase make the action surface unambiguous; user always sees what the click does.

### D13. macOS scope

`POST /api/force-kill` and `POST /api/force-kill/probe` return 501 Not Implemented on macOS in PR #1, mirroring the C1 CLI scope. The button renders but the click surfaces (Codex r1 D13 applied — UI copy is product-neutral, no `CLAUDE.md` reference): *"Lock recovery is not yet supported on macOS. As a workaround, run `mcphub gui --force` from Terminal for a diagnostic, or restart the system to clear stuck file handles."* Tracked as future scope in backlog.

### D14. Settings registry deltas with `RenderKind` discriminator (B-lite)

(Codex r1 D14 applied: B-lite chosen over A or C. Add a narrow `RenderKind` discriminator to `SettingDef` so registry remains the single ordering/help/source-of-truth, but section code can opt out of `FieldRenderer` for keys that need custom UI.)

**Registry shape extension** (`internal/api/settings_registry.go`):

```go
// RenderKind discriminates between default FieldRenderer rendering and
// section-owned custom rendering. Codex r1 D14: keeps the registry as the
// single ordering/help/source-of-truth surface while letting sections
// render Action keys (and future variants) with custom UI when the default
// "single button + Help line" affordance is insufficient.
type RenderKind string
const (
    RenderDefault RenderKind = ""              // omit field → default; FieldRenderer handles it
    RenderCustom  RenderKind = "custom"        // section code is responsible for rendering
)

// SettingDef gains one optional field:
type SettingDef struct {
    // ... existing fields ...
    RenderKind RenderKind  // default "" = FieldRenderer; "custom" = section bypasses FieldRenderer
}
```

`FieldRenderer` reads `def.RenderKind` and returns `null` when it is `RenderCustom`. Section components that own custom-rendered keys are responsible for finding them in the snapshot and rendering them. This keeps the registry as the single source of ordering, help-text, type, and discovery — while letting sections render keys whose UX exceeds a single button.

**Registry deltas:**

```go
// New: opt-in default knob.
{Key: "daemons.weekly_refresh_default", Section: "daemons", Type: TypeBool,
 Default: "false",
 Help: "When registering a new workspace, enroll it in weekly refresh by default. Existing workspaces are not affected."},

// daemons.weekly_schedule: Deferred:true → false; tighten Pattern to bounded HH:MM (Codex r1 D7/D14).
{Key: "daemons.weekly_schedule", Section: "daemons", Type: TypeString,
 Default: "weekly Sun 03:00",
 Pattern: `^weekly\s+(?i:Sun|Mon|Tue|Wed|Thu|Fri|Sat)\s+(?:[01]\d|2[0-3]):[0-5]\d$`,
 Help: "Weekly refresh schedule (format: weekly DAY HH:MM, 24-hour local time)."},

// daemons.retry_policy: Deferred:true → false.
{Key: "daemons.retry_policy", Section: "daemons", Type: TypeEnum,
 Default: "exponential", Enum: []string{"none", "linear", "exponential"},
 Help: "Retry policy on daemon failure. Edit value here; runtime applier ships in A4-b PR #2."},

// backups.clean_now: Deferred:true → false.
{Key: "backups.clean_now", Section: "backups", Type: TypeAction,
 Help: "Delete eligible timestamped backups. Originals are never cleaned. Confirms before deleting."},

// advanced.export_config_bundle: Deferred:true → false.
{Key: "advanced.export_config_bundle", Section: "advanced", Type: TypeAction,
 Help: "Download a .zip bundle of all manifests, encrypted secrets, settings, and registry. Hostname redacted; ciphertext only."},

// advanced.force_kill_diagnose: NEW Action with custom rendering (D14 RenderCustom).
{Key: "advanced.force_kill_diagnose", Section: "advanced", Type: TypeAction,
 RenderKind: RenderCustom,
 Help: "Diagnose the single-instance lock. Read-only — shows what holds the lock without killing it."},

// advanced.force_kill: NEW Action with custom rendering. Visible in registry for ordering/help/discovery,
// but the actual UI is owned by SectionAdvanced because the kill button only renders conditionally on probe state.
{Key: "advanced.force_kill", Section: "advanced", Type: TypeAction,
 RenderKind: RenderCustom,
 Help: "Kill the recorded mcphub process holding the lock. Only available when diagnostic shows Stuck."},
```

**Why B-lite over A:** option A (drop the entries, add a per-section "custom actions" array) creates a second ordering/discovery surface alongside the registry — CLI `mcphub settings list`, GUI Settings snapshot, future Help generators all have to read both. The single source-of-truth is the registry; B-lite preserves that.

**Why B-lite over C:** option C (just keep the keys without a discriminator) silently relies on section code knowing which keys to skip in `FieldRenderer` — the bypass is undocumented and the next person adding a custom-render key will repeat the silent skip. The discriminator makes the contract explicit and self-documenting.

**Why B-lite over full B (`CustomRenderer` component injection):** registering a Preact component reference inside the Go-side registry crosses a layer boundary and ties the registry to frontend implementation details. The `RenderKind` discriminator stays on the Go side; the frontend reads the boolean discriminator and dispatches in the section component itself.

This pattern generalizes to future custom-rendered keys (e.g., a Settings-screen "Reload manifests" Action with progress UI, a "Restart all daemons" Action with confirm + per-daemon status). Each adds the registry entry with `RenderKind: RenderCustom` and the owning section renders it.

## 3. Section delta to Settings UI (PR #1)

`SectionDaemons` after PR #1:

```
Daemons
─────────
Weekly schedule:                 [weekly Sun 03:00     ]
Retry policy:                    [exponential       ▼  ]
Default for new workspaces:      [✓] Enroll in weekly refresh

Workspaces in weekly refresh:
  [✓] D:\dev\mcp-local-hub × cpp
  [✓] D:\dev\mcp-local-hub × python
  [ ] D:\dev\proj × rust
  [Select all] [Clear all]

[Save]   [Reset]
```

`SectionBackups`:

```
Backups
───────
Keep N timestamped per client:   [5    ]
[Clean now eligible backups]      ← opens ConfirmModal
Backups list (per-client preview): [...]
```

`SectionAdvanced`:

```
Advanced
────────
[Open mcp-local-hub data folder]
[Export config bundle]            ← streams .zip

Diagnostics
───────────
[Diagnose lock state]             ← read-only probe
  └─ result strip
[Kill stuck PID N]                ← only when probe says Stuck + identity gate passes; opens ConfirmModal
```

## 4. Backend additions (consolidated)

| Endpoint | Method | Body | Response | Atomicity |
|---|---|---|---|---|
| `/api/daemons/weekly-refresh-membership` | PUT | `[{workspace_key, language, enabled}]` | `{updated, warnings}` | `registryMu` + atomic Registry.Save |
| `/api/daemons/weekly-schedule` | PUT | `{schedule}` | success (200): `{updated, schedule, restore_status: "n/a"}`; **transactional failure (5xx, post-parse)**: `{error, detail, updated:false, restore_status: "ok"\|"degraded"\|"failed"\|"n/a", manual_recovery?}`; **parse error (400)**: `{error: "parse_error", detail, example}` only — no `updated`, no `restore_status` (see D8) | `settingsMu` + `schedulerMu` + ExportXML preflight + best-effort ImportXML rollback |
| `/api/backups/clean-now` | POST | `{}` | `{cleaned, errors}` | existing — confirms scope from A4-a |
| `/api/export-config-bundle` | POST | `{}` | `application/zip` stream | snapshot read-only |
| `/api/force-kill/probe` | POST | `{}` | `Verdict` JSON (read-only) | none |
| `/api/force-kill` | POST | `{}` | `Verdict` JSON (post-kill) | C1 KillRecordedHolder identity gate + lock acquire-poll |

`PUT /api/settings/{key}` (existing from A4-a) handles all keys EXCEPT `daemons.weekly_schedule` (D8 dedicated route). It handles the new `daemons.weekly_refresh_default` knob, retry_policy edit, and the (currently unused) action keys.

## 5. New / modified Go files (file plan, locked)

**New:**
- `internal/api/schedule_parser.go` — `ScheduleSpec`, `ParseSchedule`. ~60 LOC.
- `internal/api/schedule_parser_test.go` — accepted forms + error cases. ~80 LOC.
- `internal/api/scheduler_swap.go` — `SwapWeeklyTrigger(spec *ScheduleSpec, priorXML []byte) (restoreStatus string, err error)` extracted from `weekly_refresh.go`. Owns Delete + Create + best-effort ImportXML(priorXML); does NOT call ExportXML (caller's responsibility) and does NOT touch settings YAML. ~70 LOC.
- `internal/api/scheduler_swap_test.go` — happy + rollback. ~100 LOC.
- `internal/api/retry.go` — `RetryPolicy` interface + `PolicyFromString`. ~80 LOC.
- `internal/api/retry_test.go` — three policy backoff sequences. ~80 LOC.
- `internal/api/membership.go` — `UpdateWeeklyRefreshMembership([]MembershipDelta) error`. ~50 LOC.
- `internal/api/membership_test.go` — atomicity, partial update, validation. ~80 LOC.
- `internal/api/export_bundle.go` — `WriteConfigBundle(w io.Writer) error`. ~120 LOC.
- `internal/api/export_bundle_test.go` — content composition + hostname redaction. ~80 LOC.
- `internal/gui/settings_daemons_handlers.go` — HTTP handlers for the three new daemon routes (membership, schedule, plus reuse of force-kill handlers from C1). ~120 LOC.
- `internal/gui/settings_daemons_handlers_test.go` — handler-level tests. ~150 LOC.
- `internal/gui/export_handler.go` — streaming zip handler. ~50 LOC.
- `internal/gui/force_kill_handler.go` — wraps C1's `KillRecordedHolder` in HTTP. ~80 LOC.
- `internal/gui/force_kill_handler_test.go` — handler tests with mocked C1 probe. ~100 LOC.

**Modified:**
- `internal/api/settings_registry.go` — D14 deltas (new key + 5 Deferred flips + 2 force-kill action keys + tightened Pattern).
- `internal/api/register.go` — `RegisterOpts.WeeklyRefresh` semantics: if caller explicitly sets, use that; if zero-value, read `daemons.weekly_refresh_default` from settings.
- `internal/api/legacy_migrate.go:161` — **NO CHANGE** (Codex r1 D1 + r2 D1 confirmation): hardcoded `WeeklyRefresh: true` is preserved; legacy import is exempt from the knob. Add an inline comment naming the exemption rationale.
- `internal/api/weekly_refresh.go` — `EnsureWeeklyRefreshTask` now consumes `*ScheduleSpec` from settings (parsed at task-creation time) instead of hardcoded `DayOfWeek:0, HourLocal:3, MinuteLocal:0`.
- `cmd/mcphub/register.go` (or wherever `--weekly-refresh` is parsed) — flag becomes tri-state: explicit `--weekly-refresh=true|false` overrides; absent means "read knob".

## 6. New / modified frontend files (file plan, locked)

**New:**
- `internal/gui/frontend/src/components/ConfirmModal.tsx` — D10 component. ~80 LOC.
- `internal/gui/frontend/src/components/ConfirmModal.test.tsx` — Vitest. ~60 LOC.
- `internal/gui/frontend/src/components/settings/WeeklyMembershipTable.tsx` — D3-D6 table. ~150 LOC.
- `internal/gui/frontend/src/components/settings/WeeklyMembershipTable.test.tsx` — Vitest dirty tracking + Select all/Clear all. ~120 LOC.
- `internal/gui/frontend/src/components/settings/SectionAdvancedDiagnostics.tsx` — D12 force-kill UX block. ~150 LOC.
- `internal/gui/frontend/src/components/settings/SectionAdvancedDiagnostics.test.tsx` — Vitest. ~120 LOC.
- `internal/gui/frontend/src/lib/api-daemons.ts` — typed wrappers for new endpoints. ~80 LOC.

**Modified:**
- `internal/gui/frontend/src/components/settings/SectionDaemons.tsx` — flip Deferred fields; render `<WeeklyMembershipTable>`; integrate cron-edit error path from `PUT /api/daemons/weekly-schedule`.
- `internal/gui/frontend/src/components/settings/SectionBackups.tsx` — wire `backups.clean_now` Action through `<ConfirmModal>`.
- `internal/gui/frontend/src/components/settings/SectionAdvanced.tsx` — flip `export_config_bundle` Deferred → trigger streaming download; render `<SectionAdvancedDiagnostics>`.
- `internal/gui/frontend/src/components/settings/useSectionSaveFlow.ts` — accept multi-endpoint save: persisted setting writes via existing path, schedule write via dedicated route, membership write via dedicated route. All dirty state surfaces in one Save click.

## 7. Test strategy

**Go (~12 new tests):**
- `daemons.weekly_refresh_default` knob is read at register time; explicit flag overrides.
- `legacy_migrate.go` exemption: hardcoded `WeeklyRefresh: true` preserved; new test asserts the comment is present and that legacy imports do NOT consult the knob (regression guard against accidental flip).
- Membership endpoint: happy path, unknown (key, lang) returns 400, atomic-failure rollback.
- Cron parser: 6 valid forms (Sun/Mon/.../Sat), 8 invalid forms (no time, bad time, daily, cron syntax, lowercase day OK per case-insensitive flag, missing weekly prefix, etc.).
- `SwapWeeklyTrigger`: happy path, Create-fails-rolls-back-to-prior-XML.
- RetryPolicy: linear backoff sequence Δ, 2Δ, 3Δ; exponential bounded at 5min; none returns Done immediately; unknown string error.
- Export bundle: contains all 4 file types, hostname redacted in meta, no `.bak.<ts>` files included.
- Force-kill handlers: probe returns Verdict; kill returns 403 when image mismatch; macOS returns 501.

**Vitest (~8 new tests):**
- `ConfirmModal`: Esc cancels, click-outside cancels, focus trap, danger styling.
- `WeeklyMembershipTable`: cell-toggle sets dirty, Select all toggles all, Clear all clears all, registry-empty empty state.
- `SectionAdvancedDiagnostics`: probe runs on first click, kill button hidden until Class==Stuck + gate passes, kill button shows PID in label.

**Playwright E2E (~10 new tests in `internal/gui/e2e/settings.spec.ts`):**
1. Seeded `workspaces.yaml` with mixed `WeeklyRefresh` values → table renders both states correctly.
2. Toggle one row + Save → reload → state persisted.
3. Select all + Save → all enrolled; Clear all + Save → all cleared.
4. Cron edit `weekly Tue 14:30` + Save → backend logs scheduler swap (mock-checked via probe endpoint or scheduler XML inspection).
5. Cron edit invalid string → inline error → no save.
6. Clean-now button → ConfirmModal opens; Cancel preserves backups; Confirm invokes POST.
7. Export bundle button → response stream → file download triggered with `Content-Disposition` filename.
8. Diagnose lock state → result strip renders Class label.
9. Probe returns Class=Stuck → Kill button appears; click → ConfirmModal → confirm → POST fires.
10. Probe returns Class=Healthy → Kill button does not render.

## 8. Acceptance criteria

1. ✅ `daemons.weekly_refresh_default` setting added; `RegisterOpts.WeeklyRefresh` honors it for new register operations. `legacy_migrate.go` is **exempt** from the knob and continues to hardcode `WeeklyRefresh: true`; regression test asserts the exemption.
2. ✅ Membership table renders correctly for `workspaces.yaml` with mixed states; existing entries are not migrated.
3. ✅ Single Save in `SectionDaemons` writes settings + schedule + membership **sequentially via three independent transaction boundaries** (per D4): op 1 = settings (per-key atomic via existing `PUT /api/settings/{key}`); op 2 = weekly-schedule swap (best-effort transactional via D8 contract); op 3 = membership (atomic per D5). Partial-failure behavior is documented per op; the UI banner explicitly distinguishes which prior ops committed and which remain dirty.
4. ✅ All five A4-a Deferred flags flip to functional: `weekly_schedule`, `retry_policy`, `clean_now`, `export_config_bundle`, plus the two new `force_kill_*` keys.
5. ✅ `ScheduleSpec` parser + `SwapWeeklyTrigger` helper + `RetryPolicy` interface ship in PR #1 with full unit-test coverage.
6. ✅ `<ConfirmModal>` component reused twice (clean-now + force-kill).
7. ✅ Export bundle streams `.zip` from one POST; hostname redacted.
8. ✅ Force-kill two-click flow: probe is read-only; kill button only appears when Class==Stuck + identity gate passes.
9. ✅ macOS gracefully returns 501 for force-kill endpoints with explanatory copy.
10. ✅ All Go unit tests, Vitest tests, and Playwright E2E in §7 pass on Windows runner.
11. ✅ `go generate ./internal/gui/...` regenerated bundle committed.
12. ✅ CLAUDE.md updated with A4-b PR #1 surface description + new E2E count.
13. ✅ `docs/superpowers/plans/phase-3b-ii-backlog.md` row 9b marked `✅ A4-b PR #1` with link to this memo and merge SHA.

## 9. Out-of-scope (reaffirmed)

- Tray show/hide runtime mutator (`gui_server.tray`) → **PR #2**.
- Port live-rebind (`gui_server.port` taking effect without restart) → **PR #2**.
- Retry policy runtime applier (D9 ships interface; callsite wiring) → **PR #2**.
- Daily cron / cron expression syntax → future scope (parser interface accommodates).
- macOS force-kill support → future scope (501 placeholder ships).

## 10. Extensibility notes (forward-looking, non-blocking)

These notes capture the seam shape so future PRs avoid re-litigating decisions. Not acceptance-blocking for PR #1.

- **`ScheduleSpec` Kind expansion** (D7): adding `daily HH:MM` requires a new ScheduleKind constant, a parse case, and a `SwapDailyTrigger` helper. Settings regex tightens to a union pattern. UI cron field remains a text input.
- **`RetryPolicy` callsite integration** (D9): PR #2 wires `policy.Backoff(attempt)` into `internal/api/install.go` daemon-spawn loop, `weekly_refresh.go::WeeklyRefreshAll` per-entry retry, and lazy-proxy materialization. Each callsite reads `daemons.retry_policy` once at startup or per-action.
- **Daemon-route pattern** (D8): future settings keys with external-system side effects (port live-rebind in PR #2, hypothetical "service hostname" if added) follow the same dedicated-route + transactional-swap pattern. Settings handler stays pure persistence.
- **Force-kill scope** (D12): the probe + kill split is preserved for future "stale daemon recovery" features. Probe endpoints can be aggregated into a future `/api/diagnostics/all` without affecting kill semantics.
- **Knob-driven defaults** (D1): the `weekly_refresh_default` precedent generalizes — future "register-time default" settings (e.g., `daemons.retry_policy_default` if per-daemon overrides become a thing) follow the same shape: registry key with `default`, callsite reads it, explicit override at call site.

## 11. Codex consult resolution log

All open items from prior revisions are resolved. This section is retained as an audit trail for the brainstorm → review iteration.

**Codex r1 (REVISE, 11 points) → r2:**

- D8 transactional rollback failure tree → resolved: explicit `restore_status` field, ExportXML preflight, `manual_recovery` hint in degraded responses (D8).
- D9 RetryPolicy interface shape → resolved: split into timing-only `RetryPolicy` and separate `IsRetryableError(err)` classifier; non-retryable classes documented (D9).
- D10 ConfirmModal focus-trap → resolved: copy AddSecretModal pattern locally; hook extraction deferred (D10).
- D14 force_kill_* registry surface → resolved: B-lite `RenderKind` discriminator added to `SettingDef` (D14).
- D1 legacy_migrate exemption → resolved: hardcoded `WeeklyRefresh: true` preserved with inline rationale comment (D1).
- D3-D6 atomicity overstatement → resolved: D4 rewritten as multi-op save with three independent transactions and per-op partial-failure semantics.
- D7 regex bounds → resolved: tightened to `(?:[01]\d|2[0-3]):[0-5]\d` (D14 deltas).
- D11 hostname inconsistency → resolved: literal `"redacted"` string + restore-limitations subsection (D11).
- D12 panic risk → resolved: explicit `length > 1` guard before index access (D12).
- D12 clock semantics → resolved: documented sources, comparison strict-less-than, fail-closed on equality (D12).
- D13 UI copy `CLAUDE.md` reference → resolved: product-neutral copy (D13).

**Codex r2 (REVISE, 4 contract-drift points) → r3:**

- D1 / §5 / §7 / §8 internal contradiction → resolved: file plan, tests, acceptance criteria all confirm legacy_migrate exemption.
- D4 / §8 acceptance criterion 3 atomicity → resolved: rewritten as sequential per-op transaction boundaries.
- D8 helper ownership ambiguity → resolved: handler owns settings rollback; helper owns scheduler XML lifecycle only.
- §4 endpoint table drift → resolved: aligned with D8 contract (`restore_status`, `manual_recovery`, structured errors).

**Codex r3 (REVISE, 5 D8/§11 nits) → r4 → r5:**

- D8 / §4 parse-error envelope → resolved: parse errors documented as outside the rollback envelope (D8).
- D8 helper boundary ambiguity → resolved: helper docstring rewritten to own only Delete + Create + ImportXML(priorXML); caller owns ExportXML preflight (D8).
- D8 / §5 file plan signature → resolved: §5 now lists `SwapWeeklyTrigger(spec *ScheduleSpec, priorXML []byte) (restoreStatus string, err error)`.
- D8 fresh-install vs handler restore_status → resolved: combination truth table documented in D8 step 7 (handler combines helper's restoreStatus + settings rollback result deterministically).
- §11 stale → resolved: this section.

---

**End of r6.** Codex r5 PASS achieved 2026-05-01. Ready for writing-plans skill handoff.

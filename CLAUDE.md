# mcp-local-hub — Claude Code notes

This file documents developer workflows and conventions for this repo that
are load-bearing enough to be worth surfacing to the agent by default. Add
new sections as they become necessary.

## PR review + merge workflow (MANDATORY)

This is the canonical PR workflow for this repo. Every step is a hard gate;
skipping any of them counts as a regression. The KOSYAK examples come from
PR #134 (2026-05-08 — bot bypassed; CI run when disabled; "ТЕБЕ НЕ ДАЛИ
PASS"). Read the kosyak summaries before each PR — they exist precisely to
prevent the same mistakes again.

### Step 1 — Pre-push local verification

Before `git push`:

```bash
go build ./... && go vet ./... && go test -count=1 -timeout 5m ./...
go test -tags=test_state_path_env -count=1 -timeout 5m ./internal/api/ ./internal/cli/
```

Both must be clean. If a subagent reported "all green" — do NOT trust;
re-run these commands yourself before reporting status to the user.

KOSYAK examples:
- `feedback_kosyak_subagent_summary_overstates.md` — subagent claims "all
  green" but build broken (or vice versa); always verify yourself.
- `feedback_kosyak_trusted_ide_diagnostics_blindly.md` — IDE diagnostics
  with `[darwin]`/`[linux]` markers + "undefined" after Edit = stale
  gopls cache; verify with command-line first.

### Step 2 — Sweep test processes

After local tests:

```powershell
Get-Process -Name 'mcphub' -ErrorAction SilentlyContinue | Stop-Process -Force
```

Tests spawn `mcphub.exe` daemons on real ports; failing to sweep blocks the
next test run with port-9128 collisions.

KOSYAK example: `feedback_clean_test_processes.md`.

### Step 3 — Push + open PR

```bash
git push -u origin <branch>
gh pr create --title "..." --body "..."
```

Bot auto-triggers on PR open (per `.github/workflows/ci.yml`-adjacent
Codex Cloud configuration). Wait for bot review on the HEAD commit.

### Step 4 — Codex Cloud bot review LOOP

Wait for bot to review the EXACT HEAD commit. Bot review is an inline
comment AND a review record:

```bash
# Get current HEAD SHA FIRST — every downstream query must filter
# to it. Stale reviews from earlier commits do NOT satisfy PASS.
HEAD=$(gh pr view <N> --json headRefOid --jq .headRefOid)

# IMPORTANT: `gh api`'s built-in `--jq` flag takes ONLY a single
# expression string; it does NOT accept `--arg key value` like
# standalone `jq` does. Pipe `gh api` output to external `jq` when
# you need `--arg`. (Codex bot caught the incorrect `gh api --jq
# --arg ...` form in an earlier revision of this doc; rewritten here
# to the correct pipe-to-jq form.)

# Bot review state — MUST filter to HEAD (avoid stale APPROVED
# from an earlier commit satisfying PASS condition 1)
gh api repos/<owner>/<repo>/pulls/<N>/reviews --paginate \
  | jq --arg sha "$HEAD" '.[]
      | select(.user.login == "chatgpt-codex-connector[bot]")
      | select(.commit_id == $sha)
      | {state, commit_id, submitted_at}'

# Inline comments — MUST extract `original_commit_id` AND paginate
# (GitHub auto-rebases inline-comment `commit_id` across pushes;
# `original_commit_id` is the immutable anchor to the commit the bot
# actually reviewed when it left the comment. Filtering by
# `original_commit_id == $HEAD` is the only correct stale-filter.
# `--paginate` is mandatory — default page is 30; older comments
# beyond that would be invisible to a single-page query, making
# the PASS-zero-inline check vacuous on long-lived PRs.)
gh api repos/<owner>/<repo>/pulls/<N>/comments --paginate \
  | jq --arg sha "$HEAD" '.[]
      | select(.user.login == "chatgpt-codex-connector[bot]")
      | select(.original_commit_id == $sha)
      | {original_commit_id, line, path, body}'
```

PASS verdict — **all three of the following must hold on the CURRENT
HEAD commit** (not any earlier commit on the PR):

1. Either the bot's most-recent review summary contains a "no major
   issues" phrase (variants: "Didn't find any major issues. Nice work!",
   "Didn't find any major issues. Chef's kiss.", or similar — the bot
   rotates the trailing flourish, the prefix is the load-bearing part);
   OR the bot reacted with 👍 on the PR's HEAD-commit review event;
   OR the bot review state = `APPROVED`.

2. **AND** zero inline comments filtered to the current HEAD commit.
   Verify with: `original_commit_id == $(gh pr view N --json headRefOid --jq .headRefOid)`.
   GitHub auto-rebases inline comment line numbers across pushes, so
   filter by `original_commit_id` not by line number alone.

3. **AND** no inline comment has been added on the HEAD commit AFTER
   the no-issues summary fired. The bot can post the summary first
   and then attach inline observations as a single review event; the
   inline observations still count as findings unless the summary
   was issued after the inline activity stopped.

This anti-bypass rule prevents a stale 👍 from an earlier commit
satisfying PASS for a later commit that the bot hasn't yet seen.

NOT PASS (continue the loop):
- bot review state = `COMMENTED` with inline suggestions on HEAD commit
- bot review state = `CHANGES_REQUESTED`
- any of the three PASS conditions above fails

If NOT PASS:

1. **Fix EVERY finding**, regardless of severity (P0/P1/P2/P3 all). Do NOT
   defer findings to "post-merge follow-up" without explicit user approval.
2. Commit fix with descriptive message.
3. `git push origin <branch>`.
4. `gh pr comment <N> --body "@codex review"` to retrigger.
5. Wait for bot re-review on new HEAD commit (≈3–5 min).
6. Repeat until PASS.

KOSYAK examples:
- `feedback_kosyak_bot_state_misclassified.md` — `COMMENTED` ≠ APPROVE.
  Never report "bot approved" when it's `COMMENTED` with suggestions.
- `feedback_bot_pass_required_before_merge.md` — never merge before bot
  PASS, even if internal reviewers approve.
- `feedback_kosyak_passive_polling_when_user_yells.md` — when user asks
  for status, run a direct `gh api ...` query NOW; do not say "жду
  уведомлений".

### Step 5 — Full local review + Codex deep security agents (parallel)

After bot PASS, run a deeper review pass to catch what the bot missed:

1. **Local diff read:** `git diff master..HEAD` — read every changed
   file end-to-end. Verify the change matches the PR description.
2. **Codex deep security agents in parallel** — write 3 separate review
   prompts under `.scratch/codex-pr<N>-deep-security-{topic}.md` covering
   different angles (e.g. race conditions, error propagation,
   regression). Dispatch each via:

   ```bash
   codex exec - -c model_reasoning_effort=xhigh \
     < .scratch/codex-pr<N>-deep-security-{topic}.md \
     > .scratch/codex-pr<N>-deep-security-{topic}.out.md 2>&1 &
   ```

   Use `run_in_background=true` if invoking through the agent harness so
   they execute in parallel.

3. Aggregate findings. Fix EVERY finding before merge.

KOSYAK examples:
- `feedback_kosyak_review_prompt_bias.md` — review prompts must be
  neutral. Never embed "user wants this merged" / "if APPROVE state so"
  in the prompt body — biases reviewers toward rubber-stamp.
- `feedback_codex_xhigh_default.md` — always pass
  `-c model_reasoning_effort=xhigh` to `codex exec`.
- `feedback_codex_file_prompt_only.md` — always feed prompts via
  `codex exec - < file.md`; never argv.

### Step 6 — CI is MANUAL-ONLY; do NOT auto-trigger

`.github/workflows/ci.yml` is `workflow_dispatch` only by design. The
project owner controls when CI runs to manage GH Actions minute budget.
Do NOT call `gh workflow run ci.yml` unless the user explicitly asks.

CI on master (post-merge) runs automatically per `push: branches: [master]`
and acts as the safety net.

KOSYAK example: `feedback_kosyak_ci_disabled_dont_trigger.md` — running
CI when user said "CI отключены чтоб ты их не гоняло".

### Step 7 — Merge

After bot PASS + deep security agents clean + every finding fixed:

```bash
gh pr merge <N> --squash --delete-branch
```

NEVER use `--admin` to bypass missing bot pass. `--admin` is reserved for
cases the user explicitly authorized for THE SPECIFIC PR.

After merge: `git fetch origin master && git checkout master && git pull`.

KOSYAK examples:
- `feedback_kosyak_admin_merge_bypass.md` — `--admin` only with explicit
  user authorization. Never as a default.
- `feedback_dont_split_tiny_prs.md` — bundle small related additions
  into the current PR; do not propose follow-up PRs unless the diff is
  genuinely independent.
- `feedback_build_then_install_order.md` — commit FIRST, then build.sh,
  then install. Otherwise the installed binary is stale.

### Step 8 — Surgical-edit consistency

When a multi-section doc (plan, spec, design) is edited across several
review rounds, every iteration MUST run a full consistency pass:

```bash
grep -n "<concept-name>\|<API-name>\|<canonical-string>" docs/.../plan.md
```

Surgical Edit calls leave stale text in the OTHER sections. Default to a
full Write rewrite when v(N+1) introduces ≥3 cross-cutting changes.

KOSYAK example: `feedback_kosyak_surgical_edits_leave_stale_text.md` —
v3→v13 watchdog plan: every Codex round flagged stale-text contradictions
because surgical Edit left old phrasing intact.

### Quick reference — KOSYAK index

These are the failure modes documented in
`C:\Users\dima_\.claude\projects\d--dev-mcp-local-hub\memory\` from
prior sessions. Read each file before starting work that hits the same
surface:

| File | Failure mode |
|---|---|
| `feedback_bot_pass_required_before_merge.md` | merged before bot 👍; used --admin; triggered disabled CI |
| `feedback_kosyak_admin_merge_bypass.md` | --admin without explicit user auth |
| `feedback_kosyak_ci_disabled_dont_trigger.md` | triggered manual-only CI |
| `feedback_kosyak_bot_state_misclassified.md` | reported COMMENTED state as "approved" |
| `feedback_kosyak_passive_polling_when_user_yells.md` | said "ждём" instead of direct query |
| `feedback_kosyak_subagent_summary_overstates.md` | trusted subagent "all green" without verifying |
| `feedback_kosyak_review_prompt_bias.md` | review prompts begged reviewers to APPROVE |
| `feedback_kosyak_surgical_edits_leave_stale_text.md` | Edit-tool patches left contradictory stale sections |
| `feedback_kosyak_trusted_ide_diagnostics_blindly.md` | reported IDE diagnostics as compile errors without command-line verify |

## GUI frontend (Phase 3B-II onward)

The web UI lives under `internal/gui/frontend/` (Vite + TypeScript +
Preact). Built artifacts land in `internal/gui/assets/` and are
committed — `go build` does not require Node.

### Day-to-day frontend dev (hot reload)

```bash
# Terminal 1: Go backend with a fixed port so Vite proxy can target it.
go run ./cmd/mcphub gui --no-browser --no-tray --port 9125

# Terminal 2: Vite dev server on 5173 with /api → 9125 proxy.
cd internal/gui/frontend
npm install  # once
npm run dev
# Browse http://localhost:5173
```

### Build + smoke against embedded assets (what ships)

```bash
cd internal/gui/frontend
npm run build
cd ../../..
go run ./cmd/mcphub gui --no-browser --no-tray --port 9125
# Browse http://127.0.0.1:9125
```

### Regenerate the embedded bundle (CI + commits)

```bash
go generate ./internal/gui/...
```

This calls `npm run build` under `internal/gui/frontend/` and
overwrites `internal/gui/assets/{index.html,app.js,style.css}`. Always
rebuild before committing frontend changes so the embedded bundle
matches the source.

### Tests

- Frontend unit tests (pure helpers): `cd internal/gui/frontend && npm run test`
- Type-check: `cd internal/gui/frontend && npm run typecheck`
- Go-side embed smoke: `go test ./internal/gui/`

## GUI E2E tests (Phase 3B-II onward)

End-to-end browser tests live under `internal/gui/e2e/` (Playwright +
TypeScript, headless Chromium). They spawn a real `mcphub gui`
binary per-test with `HOME`/`USERPROFILE` redirected to a temp dir
so tests never touch the developer's real config, and drive the
Preact UI against the live Go backend. The backend scheduler is
redirected to an empty-noop via `MCPHUB_E2E_SCHEDULER=none` so
/api/status returns [] regardless of the host's installed
mcp-local-hub-* tasks.

### One-time setup

```bash
# Frontend deps are required because global-setup.ts runs `npm run build`
# on the frontend before building the Go binary. Fresh clones need this
# step first.
cd internal/gui/frontend
npm ci

cd ../e2e
npm ci
npx playwright install chromium --with-deps
```

### Running

```bash
cd internal/gui/e2e
npm test                # headless
npm run test:headed     # see the browser
npm run test:debug      # Playwright Inspector step-through
```

The `global-setup.ts` rebuilds `internal/gui/assets/` then compiles
`cmd/mcphub` into `internal/gui/e2e/bin/` once per run. Each test
spawns that binary with `--port 0` so the OS picks a free port —
tests are parallel-safe.

### CI (Windows-only)

Run E2E as a separate job from `go test` on a Windows runner. The GUI's
`/api/status` route goes through the real scheduler; `scheduler.New()`
on Linux/macOS returns "not implemented" and the status route 500s, so
Dashboard/Logs tests would fail on non-Windows runners. Pin this job
to `windows-latest` until a scheduler-less test seam exists.

```yaml
jobs:
  e2e:
    runs-on: windows-latest
    steps:
      - uses: actions/checkout@v4
      - uses: actions/setup-go@v5
      - uses: actions/setup-node@v4
        with: { node-version: 20 }
      - run: cd internal/gui/frontend && npm ci
      - run: cd internal/gui/e2e && npm ci
      - run: cd internal/gui/e2e && npx playwright install chromium
      - run: cd internal/gui/e2e && npm test
```

### What's covered

- Shell: sidebar, eight nav links, hash routing, active-link highlight.
- Servers: matrix columns (Server + 4 clients + Port + State), empty-body state on clean tmpHome, Apply disabled with no dirty cells, uncheck-via-hub + Apply posts /api/demigrate narrowed to cell, mixed Apply dispatches demigrate-before-migrate ordering, demigrate failure always-reloads and retains failed entry for retry, via-hub tooltip describes Uncheck-and-Apply semantic (no more 'mcphub rollback --client' stale text), per-client gate + 3-outcome pruning (failed + gated retained, succeeded pruned) with second-Apply retry firing exactly the previously-gated migrate.
- Migration: h1, empty-state copy, group sections hidden on empty home, hashchange swap from Servers, full POST /api/dismiss → on-disk JSON → GET /api/dismissed round-trip, /api/scan-unfiltered regression guard (seed + dismiss + re-scan).
- Add server: empty-state + debounced YAML preview, live name-regex inline error, single-daemon flat bindings, cascade rename/delete with confirm, Save writes manifest, Save&Install port-conflict failure path with Retry Install banner, Paste YAML import, sidebar-intercept unsaved-changes guard, Advanced kind-toggle (workspace/global reveals/hides languages+port_pool), Advanced always-visible fields survive kind toggles.
- Edit server: #/edit-server?name= load from disk, name+kind locked, Save → Reinstall banner, Force Save with external-edit hash-mismatch preserving `_preservedRaw` top-level fields, nested-unknown read-only mode, load failure banner, sidebar-intercept when dirty, 4+-daemon matrix view, workspace-scoped Advanced (languages + port_pool), internal-ID cascade daemon rename, hashchange cancel/accept dirty-guard, Paste YAML → Save race (version-counter invariant).
- Dashboard: empty-cards state on fresh home, `/api/events` SSE connection opens on mount.
- Logs: picker + controls render, notice text on no-daemons state, controls disabled when no eligible entries.
- Secrets: empty-state init, Add modal, Used-by counts from manifest scan, ghost-refs for manifest-only keys, decrypt-failed degraded view, Rotate Save-without-restart with persistent CTA + Restart-now path via POST /restart, Rotate Save-and-restart with 207 partial-failure handling, Delete differential typed-confirm (single-click for unreferenced / typed DELETE for referenced) via D5 escalation flow, scan-incomplete fail-closed path, backend 409 guard verification, sidebar nav link, mcphub secrets edit banner.
- Add/Edit server env picker: affordance button, auto-open on `secret:` typing with substring-narrowing filter, sort-by-match with `matchTier`-based badge for exact-after-normalization, broken-ref inline marker (red `.broken` for missing, yellow `.unverified` for unverified), conditional compact summary line above env section when count > 1 or vault not ok, in-place AddSecretModal with snapshot revalidate-on-save (savedFiredRef dedup) and Retry-on-load-error, full ARIA combobox semantics with keyboard navigation (Tab/Esc/Arrow/Enter), create-contextual 409 conflict flow (modal stays open + Cancel triggers refresh + marker clears), `[data-action="create-disabled"]` rendering for vault not ok.
- Settings: sidebar link + 5 section headers + deep-link query-string (#/settings?section=backups) + sticky inner-nav active-on-scroll + Save Appearance round-trip to gui-preferences.yaml + port save validation (below min) + port pending-restart badge after Save (anchored to persisted, not draft — Codex r3+r4 P2.1) + Daemons read-only "Configured schedule (effective in A4-b)" wording + Backups 4-client groups + would-prune badge with locked Codex copy + disabled Clean-now tooltip + Open app-data folder POST (mocked, no real spawn — Codex r2 P2) + dirty-guard navigation prompt + per-section Save isolation + deferred tray field disabled + discard-key remount on intra-Settings confirmed-discard navigation (Codex r2 P1, memo §10.4).
- About (PR #22): /api/version network round-trip + heading + version/commit/build-date data-testids + external links carry target=_blank rel="noopener noreferrer" + sidebar nav highlights active.
- A4-b PR #1: SectionDaemons editable section with multi-op save (settings + schedule + membership) + WeeklyMembershipTable (mixed-state render, toggle + Save persistence, Select all/Clear all), cron parse-error inline, ConfirmModal-gated clean-now (cancel preserves backups), export bundle download trigger, force-kill Diagnose → Healthy no-kill-button.

103 smoke tests total (3 shell + 8 servers + 6 migration + 13 add-server + 17 edit-server + 2 dashboard + 3 logs + 14 secrets + 10 secret-picker + 16 settings + 3 about + 8 a4b-pr1), ~55s wall-time on a warm machine.

### What's NOT covered (future)

- Populated-row matrix tests (needs a client-config seed fixture — deferred to a follow-up plan item).
- Real migrate/restart flows (needs populated client configs).
- Dashboard SSE cleanup on screen swap — the `useEffect` return is the implementation, but Playwright's request API cannot observe connection close. A future CDP-based test could.
- Workspace-scoped daemons (Phase 3B-II D3 — manual smoke per `docs/phase-3b-ii-verification.md`).
- Tray icon rendering (4-state variants + toast notifications wired in PR #22; native surface Playwright can't reach — manual smoke per `docs/phase-3b-ii-verification.md` D2.1 + D2.4).
- Browser focus on activate-window (PR #22 wires SetForegroundWindow; manual smoke per D2.1).
- Linux/macOS (blocked on scheduler test seam).

## Watchdog (Phase 3B-II onward)

The watchdog is a per-user scheduled task (`\mcp-local-hub-watchdog`)
that runs `mcphub watchdog --once` every 5 min. Each tick walks the
daemon registry, classifies failures via the exported `IsRealFailure`
predicate, and restarts eligible daemons under a strictly-pure recovery
state machine. It exists because Task Scheduler `RestartOnFailure` does
not reliably fire for user-issued End Task / `Stop-Process -Force` kills
on Win11 24H2+ (see
`work-items/bugs/2026-05-07-task-scheduler-restartonfailure-not-firing.md`).
The full design lives in
`docs/superpowers/plans/2026-05-07-mcphub-watchdog.md` (v13). The manual
smoke checklist lives in `docs/phase-3b-ii-verification.md` D2.6.

### State files

All under `<state-dir>` (resolved at runtime — see "State path" below):

```text
<state-dir>/
  daemon-intent.json    # 3-state intent: Desired / Reason / UpdatedAt (UTC RFC3339Nano)
  watchdog-state.json   # cooldown + 3 sliding 30-min windows
                        # (CorruptStrikeWindow, AuditFailureWindow, StaleClearWindow)
  intent-audit.log      # JSON Lines audit log; rotates at 10 MB → .log.1
  watchdog.log          # JSON Lines decision log; rotates at 10 MB → .log.1
  --once.lock           # singleton flock for manual `--once` invocations
  --once.lock.owner.json # sidecar with PID/started_at/hostname of current holder
  .corrupt-*            # quarantined corrupt state files (5 newest retained)
```

Each `watchdog-state.json` field uses sliding-30-min windows over
`[]time.Time` slices; quarantine names match `.corrupt-<basename>-<RFC3339>`.

### State path

- **Windows (production):** `SHGetKnownFolderPath(FOLDERID_LocalAppData)`
  → `%LOCALAPPDATA%\mcp-local-hub\`. The env-var fallback path is
  excluded from production builds via the `test_state_path_env`
  build tag (CI runs the full test matrix BOTH with and without that
  tag; release `go build` runs WITHOUT it). Plan §50 includes a
  release-pipeline `go tool nm` assertion that the env-fallback symbol
  is absent from the shipped binary.
- **POSIX (test only for v0.3.0):** `$XDG_STATE_HOME/mcp-local-hub` or
  `~/.local/state/mcp-local-hub`; dir mode 0700, files 0600. Sanity
  check rejects world-writable parent or non-owner uid → exit 8.
- Linux systemd-timer + macOS launchd shipping is deferred to v0.4.x.

The state dir is a single-user 0600/0700 boundary (per-user on POSIX,
per-user `%LOCALAPPDATA%` ACL on Windows). `mcphub watchdog status`
prints absolute paths to every state file (plan §57) so operators can
inspect / quarantine / restore them directly.

### Subcommands

```bash
mcphub watchdog --once                  # one-shot recovery tick
                                        # (5-min cadence; usually scheduled
                                        # via the watchdog task, not run by hand)
mcphub watchdog enable [--server NAME]  # clear stop intent (per server or all)
mcphub watchdog disable [--server NAME] # write Desired=stopped, Reason=user-disabled
                                        # (permanent; never auto-revived)
mcphub watchdog install [--allow-elevated]
                                        # install scheduled task (idempotent)
                                        # refuses if shell is elevated unless
                                        # --allow-elevated; uses fail-closed audit
mcphub watchdog uninstall [--yes]       # remove scheduled task
                                        # interactive confirm in TTY;
                                        # non-TTY without --yes → exit 6
mcphub watchdog status [--json]         # rich observability output
                                        # (cooldown + 3 windows + abs paths +
                                        # recent events + audit tail w/ redaction)
```

`mcphub setup` auto-installs the watchdog scheduled task (idempotent;
Task 11). Manual `mcphub watchdog install` is only needed after a
self-quarantine recovery (sub-case D2.6.11).

### Exit codes (cross-command summary)

```text
0   — success
1   — backend error (Status, manifest, scheduler, etc.)
2   — ctx deadline exceeded (4-min app-level deadline; plan §14)
6   — `mcphub watchdog uninstall` invoked non-interactively without `--yes`
      (plan §64). Side-by-side with `mcphub gui --force --kill` exit 6
      ("non-interactive shell with --kill but no --yes"; see
      "Stuck-instance recovery" below). Both commands share the SAME
      semantic — "interactive command requires --yes in non-interactive
      contexts" — so operators reading exit-code docs see both
      contexts. The `mcphub watchdog uninstall --help` text repeats
      this note. Exit codes are command-scoped: there is no conflict.
8   — state path sanity rejected (POSIX world-writable parent OR
      Windows KnownFolder failed in production; plan §16)
9   — self-quarantined (4 corrupt-state strikes within 30 min;
      `UninstallWatchdogTaskInternal(QuarantineFourStrikes30Min)`
      removed the scheduled task; plan §28)
10  — emergency-fallback failed (audit-degraded cascade exhausted —
      watchdog.log → stderr → eventlog/syslog all unreachable; plan §49)
11  — `--allow-elevated` requested but the audit override entry could
      not be written; install is rejected and no scheduler entry is
      created (plan §61)
```

`mcphub watchdog --once` returns these codes via a typed
`forceExitError` (no `os.Exit` from inside command bodies); the parent
process picks them up. `mcphub install` and `mcphub watchdog install`
share exit 11 for the audit-required-but-failed path.

### Audit + log retention

- **`intent-audit.log` and `watchdog.log` rotate at 10 MB → `.log.1`.**
  After rotation, an `audit-rotated` self-event is written to the new
  active file (`SystemEntry=true`, `caller_user="<rotation-system>"`).
  Rotation is idempotent on retry; partial writes during rotation
  retain a placeholder until the next successful append.
- Two log files × 10 MB each = ~20 MB ceiling per log family
  (~130 k events at typical entry size before the oldest .log.1 is
  overwritten on the next rotation).
- Quarantines (`.corrupt-*`) keep the **5 newest** under flock; older
  entries pruned after rename. Per-file delete failures are non-fatal.

### Per-entry size cap (16 KB)

JSON Lines entries cap at 16 KB. Identity fields — `task`, `task_name`,
`caller_user` — are NEVER truncated:

- `task` exceeds 1 KB (legitimate Task Scheduler names are <100 bytes)
  → entry rejected with `ErrIdentityOversize`. Caller treats this as
  audit failure and fails closed (plan §51 — applies to `mcphub stop`,
  `mcphub stop --force`, `mcphub install`, `mcphub watchdog install
  --allow-elevated`).
- Non-identity oversize → truncate longest non-identity string field;
  set `_truncated:true` + `_truncated_field:"<name>"` + a 12-hex
  `_task_hash` (first 12 chars of SHA-256 over the original `task`)
  for forensic correlation.
- If even truncation can't fit 16 KB → drop with placeholder entry
  `{"action":"log-entry-dropped-oversize", ...}`.

### Best-effort ctx cancellation

`StatusContext` and `RestartContext` (plan §32) use a goroutine +
ctx-select pattern: cancellation returns to the caller within ~10 ms,
but the underlying `schtasks` / status syscall continues until it
completes. The OS-level `<ExecutionTimeLimit>PT5M</ExecutionTimeLimit>`
on the watchdog scheduled task guarantees the watchdog process is
killed if an underlying op hangs. Deep ctx propagation through
`internal/api.Restart` and `internal/scheduler` is deferred to v0.4.x.

### Post-self-quarantine recovery

If `mcphub watchdog status` shows:

```text
WATCHDOG SELF-QUARANTINED: scheduled task not installed.
Last self-quarantine: 2026-05-07T15:30:00Z
Reason: 4-strikes-30min
Suggested action: verify state files clean; review .corrupt-* quarantines; then `mcphub watchdog install` to resume.
```

then:

1. Open the state-dir path (printed at the top of status output) and
   review the `.corrupt-*` quarantine files. They are the four (or
   more) corrupt snapshots that drove the 30-min strike count.
2. Verify `daemon-intent.json` and `watchdog-state.json` are clean
   JSON (or absent — they will be re-created on the next tick).
3. Run `mcphub watchdog install` to re-create the scheduled task.
   The next tick (within 5 min) restores normal operation.

If the corruption was caused by an external process (AV scanner,
backup tool, etc.) the loop will likely re-quarantine. Add a
sufficient exception in the offending tool before re-installing.

### Path exposure note (§57)

`mcphub watchdog status` displays absolute paths to every state file
because operators routinely need to inspect / break the singleton
lock / quarantine corrupt state. The state dir is a per-user 0600
boundary on POSIX and is `%LOCALAPPDATA%`-ACL'd to the current user
on Windows; absolute paths in status do not weaken that boundary.
Audit-tail entries displayed by `status` redact `caller_user` for
non-owner entries (plan §34 + §37 — `SystemEntry=true` entries are
exempt).

### Singleton lock recovery

`mcphub watchdog --once` acquires `<state-dir>/--once.lock` via flock.
The sidecar `--once.lock.owner.json` carries `{pid, started_at, hostname}`.

If a stale lock blocks a manual run:

1. Run `mcphub watchdog status` — the "Last flock skip" line shows the
   recorded PID and start time.
2. If the PID is dead (e.g., `Get-Process -Id <pid>` returns nothing),
   delete `--once.lock` and the sidecar `--once.lock.owner.json` from
   the state dir.
3. Re-run `mcphub watchdog --once`.

POSIX `flock` is advisory; only honored by callers that use the same
convention. The watchdog binary always honors it.

## Marketplace (G5, v0.3.0)

`mcphub marketplace {search,show,generate,refresh}` lets an operator
discover MCP servers from a curated catalog. Default registry URL:
`https://raw.githubusercontent.com/applicate2628/mcp-local-hub/master/marketplace/v1/catalog.json`.

- `search [query]` — table of catalog entries matching query (empty = list all).
- `show <id>` — metadata block + `Readme URL:` line (operator opens the URL).
- `generate <id>` — draft YAML to stdout. **Operator MUST edit before**
  `manifest create`: rename `name:`, pick a real port, **do NOT
  persist raw tokens / passwords / API keys** — replace verbatim
  `${env:*}` placeholders with a `secret:<key>` reference (from the
  local vault — see `mcphub secrets`) or the operator-meaningful
  literal when the variable is non-secret. Sensitive env names match
  a broad classifier (suffixes `*_TOKEN`/`*_SECRET`/`*_PASSWORD`/
  `*_KEY`/`*_API_KEY`/`*_AUTH`/`*_DSN`; prefixes `AWS_`/`AZURE_`/
  `GCP_`/`GITHUB_`/`GOOGLE_`/`OAUTH_`; substrings `TOKEN`/`SECRET`/
  `PASSWORD`/`CREDENTIAL`/`BEARER`/`PRIVATE_KEY`; exact names
  `DATABASE_URL`/`CONNECTION_STRING`/`DSN`/`AUTHORIZATION`/`OAUTH`/
  `GOOGLE_APPLICATION_CREDENTIALS`); each match is LEFT VERBATIM
  with a stderr warning per occurrence. Workspace strings using
  `${workspaceFolder}/..` (parent-directory escape) are also surfaced
  as warnings before the draft prints.
- `refresh` — force re-fetch (bypass TTL + ETag).

Cache: `<state-dir>/marketplace-cache.json` (routed through
`writeHubMcpStateFile` — atomic tempfile + rename + post-rename DACL re-verify (best-effort cache, no cross-process flock — see Architecture intro)), 24h
TTL, ETag revalidate. Cache entries carry the `source_url` they were
fetched from; a `--registry` switch forces a fresh fetch instead of
serving the prior registry's body. HTTPS-only; downgrade redirects
rejected; gzip disabled. Catalog display fields (name, summary,
categories, etc.) are stripped of C0/C1/ESC bytes before reaching
stdout so a hostile registry cannot inject terminal control sequences.
Credential-bearing extra HTTP headers (Authorization, Cookie,
Proxy-Authorization) are refused at the fetch helper. Native-http
(`transport: "http"`) entries refuse to generate; the CLI prints a
G6-deferral error to stderr with the entry URL and explicit operator
guidance: **wait for G6 (Remote MCP manifests, v0.4.x)** is the
supported path. Operators who cannot wait may hand-author a local
stdio wrapper that proxies to the remote URL; both options are surfaced
in the CLI error message.

## Hardened client-config writes + corp-policy posture

Every adapter write goes through `api.SecureWriteClientConfig`
(`internal/api/secure_write_client_config.go`), which uses a
handle-relative pipeline (Windows: `NtCreateFile` relative to a
parent `dirHandle` with `WRITE_DAC` access; POSIX: `openat` relative
to a parent `dirFd` with `O_CREAT|O_EXCL|O_NOFOLLOW|0600`). The
restrictive DACL/mode is installed on the file HANDLE/FD BEFORE any
bytes hit disk, then the atomic rename happens across the held
handle. This pattern closes all classic temp-file races.

The pipeline includes a parent-dir DACL/mode gate (Windows: reject
non-allowlisted ACEs like Domain Users, Authenticated Users,
CodexSandboxUsers, AppContainer SIDs, orphan AD SIDs; POSIX: reject
group/world permission bits or non-owner uid). The gate is
controlled by two env vars:

- **Default (v0.4.0+, no env vars set):** if the parent-dir gate
  rejects, the pipeline RE-RUNS with the parent-dir gate disabled.
  Everything else — symlink refusal, handle-bound DACL/mode at
  temp-create, atomic rename, post-rename re-verify — still applies.
  The new file is owner-only regardless of how broad the parent's
  ACL is.
- **`MCPHUB_REQUIRE_SINGLE_USER_HOME=1`:** strict gate. Parent-dir
  rejection becomes a hard error; no relax fallback. Use on
  corp-managed/shared hosts where the parent-dir ACL is the
  authoritative boundary.
- **`MCPHUB_ALLOW_UNHARDENED_CLIENT_WRITE=1`:** legacy explicit
  opt-in to the relax path. Pre-v0.4.0 this was required to bypass
  the strict-by-default gate; v0.4.0+ it is a no-op vs the default
  (relax fires either way), kept for backward compatibility so
  operators with the env var already set in their shell profile see
  identical behavior. Distinguished in the audit log so operators
  can grep their profile and remove the now-redundant setting.

When the relax lane fires (default OR legacy opt-in), a structured
**warn** event `client-write-unhardened-fallback` is emitted through
the hub-mcp event log with the destination path, the reason
(`default-relax-on-solo-host` or `legacy opt-in via MCPHUB_ALLOW_UNHARDENED_CLIENT_WRITE`),
and the original gate error.

Strict-mode error wording (when `MCPHUB_REQUIRE_SINGLE_USER_HOME=1`
is set):

```text
secure write: parent directory not single-user safe (path C:\Users\...): <details>;
MCPHUB_REQUIRE_SINGLE_USER_HOME=1 is set, so the strict parent-dir
gate is enforced (unset that env var, or tighten the parent's DACL
to remove the offending principal, to proceed)
```

All other secure-write failures (open temp, write, rename,
post-rename DACL/mode re-verify, pre-existing symlink/reparse-point
at destination) propagate unchanged regardless of either env var.

**Residual co-resident risk (relax lane only):** the parent-dir gate
exists to detect when the parent is broadened to other principals
beyond the file owner. The relax lane writes through anyway because
on solo-developer Windows hosts the broadening principals
(CodexSandboxUsers, AppContainer SIDs, orphan AD SIDs) are
typically not under operator control and pose no realistic threat.
The new file's DACL still denies content/object access to those
principals (the file is owner-only with PROTECTED_DACL blocking
inherited ACEs), and on Windows the restrictive DACL is installed
at NtCreateFile time via `OBJECT_ATTRIBUTES.SecurityDescriptor` so
there is no pre-DACL window during which the file could be opened.

**What the relax lane does NOT protect against:** parent-directory
namespace rights. If a co-resident principal has been granted
`FILE_DELETE_CHILD` on the parent directory (one of the more
permissive ACEs Group Policy / SCCM can apply to shared profile
paths), they can still delete the entry from the directory or
replace it with an attacker-controlled file. They cannot read or
modify the original file's contents through its own DACL — that is
denied by the file's allowlist — but the directory entry itself
sits outside the file's security boundary. Operators on genuinely
multi-tenant or admin-managed hosts with broad parent-directory
write permissions should set `MCPHUB_REQUIRE_SINGLE_USER_HOME=1` to
enforce the strict gate or tighten the parent's DACL to remove
namespace rights for non-allowlisted principals; the relax lane is
not designed for that posture.

## Stuck-instance recovery

If `mcphub gui` exits with the structured "Cannot acquire mcphub gui
single-instance lock" block, run `mcphub gui --force` for the
diagnostic — it also opens the lock folder in your file manager so
the offending files are visible.

To recover automatically:

```bash
mcphub gui --force --kill              # prompts before killing
mcphub gui --force --kill --yes        # no prompt; for scripts
```

`--kill` only terminates the recorded PID after a three-part identity
gate: (1) image basename is `mcphub.exe` (Windows) or `mcphub` (POSIX);
(2) `argv[1]` (cobra subcommand token) equals `gui` exactly OR the
process was launched with no args (Explorer/Start-menu double-click,
which `cmd/mcphub/main.go:32` defaults to gui internally); (3) process
start time precedes pidport mtime. If any gate fails (e.g. PID
recycled to a `mcphub.exe daemon` Task Scheduler child), `--kill`
refuses with exit 7.

Manual recovery when `--kill` refuses:

```text
Windows: download Sysinternals `handle.exe`, then
         `handle.exe -a "<lock-path>"` (REQUIRES ELEVATED shell).
Linux:   `lsof "<lock-path>"` or `fuser "<lock-path>"` (use `sudo`
         for cross-user holders).
```

Then `kill -9 <pid>` (Linux) or Task Manager → End Task on that
PID (Windows). DO NOT delete the lock file — deleting under a live
holder splits ownership. If admin tooling isn't available, reboot
is the universally available recovery (stuck file handles survive
user-mode cleanup only across a session reset).

**macOS:** `--force --kill` is unsupported on macOS in this PR (the
identity probe relies on `/proc`, which is Linux-only). `mcphub gui
--force` for the diagnostic still works, but kill recovery on macOS
is not yet implemented; reboot is the recovery path. Tracked as
follow-up in phase-3b-ii-backlog.md.

Exit codes:

```text
0 — success
1 — non-force startup error
2 — bare --force exited after diagnostic
3 — race lost or pidport changed mid-prompt
4 — kill failed / pidport unrecoverable
5 — RESERVED (not emitted)
6 — non-interactive shell with --kill but no --yes
7 — --kill refused by identity gate
```

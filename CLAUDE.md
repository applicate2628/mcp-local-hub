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
`C:\Users\<you>\.claude\projects\d--dev-mcp-local-hub\memory\` from
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

## npm publish — CANONICAL pipeline (tag-push CI, NEVER local `npm publish`)

The ONLY supported publish path is a **git tag push** that triggers
`.github/workflows/npm-publish.yml`. Do NOT `npm publish` from a local
shell — see "Why not local" below. Every prior release (`v0.4.6`,
`v0.4.7`, `v0.4.8`) went out this way.

### The 4 steps

1. **Bump the version.** `npm/package.json` is the SINGLE version authority.
   Set `version` + every `optionalDependencies` entry to the new `X.Y.Z`,
   then propagate:

   ```bash
   V=0.4.9   # must be > the current `npm view mcp-local-hub version` (latest)
   node -e "const f='npm/package.json';const p=require('./'+f);p.version='$V';for(const k in p.optionalDependencies)p.optionalDependencies[k]='$V';require('fs').writeFileSync(f,JSON.stringify(p,null,2)+'\n')"
   node npm/generate-platform-packages.js   # rewrites the 6 npm/packages/*/package.json
   node npm/sync-version.js --inject         # pushes $V into build.sh + build.ps1
   ```

2. **Commit** the bump (`npm/package.json`, the 6 `npm/packages/*/package.json`,
   `build.sh`, `build.ps1`) and `git push origin master`.

3. **Tag + push the tag:**

   ```bash
   git tag v$V && git push origin v$V
   ```

4. **Watch + verify:**

   ```bash
   gh run watch "$(gh run list --workflow=npm-publish.yml --limit 1 --json databaseId --jq '.[0].databaseId')" --exit-status
   npm view mcp-local-hub version dist-tags   # latest must == $V
   ```

The workflow cross-builds the 6 platform binaries (win32/darwin/linux ×
x64/arm64) with `-s -w -X main.version=$V`, drops them into each
`@applicate2628/mcp-local-hub-<plat>-<arch>` sub-package's `bin/`, asserts
the binary version, then `npm publish --provenance --access public`-es the
6 scoped platform packages FIRST (so the meta's `optionalDependencies`
resolve) and the `mcp-local-hub` meta LAST. Prereleases (`vX.Y.Z-beta.N`)
auto-route to the `beta` dist-tag, never `latest`.

### Why NOT local `npm publish`

- The npm account has **2FA enabled**, so a local `npm publish` hits
  `EOTP` ("This operation requires a one-time password") on EVERY package.
  The web-auth OTP flow (`Open this URL …`) needs an interactive CLI
  session an automated run cannot complete; a 6-digit TOTP would have to
  be re-pasted within its short window for all 7 publishes.
- The local `~/.npmrc` `_authToken` periodically **expires** (`npm whoami`
  → `E401`). Do not "fix" this by pasting a value into `_authToken` — a
  64-hex OTP/auth-code is NOT a token and will only `E401` AND clobber a
  working interactive-login session token.
- The CI workflow uses the repository's npm automation credential
  `NPM_TOKEN`, which bypasses the OTP prompt, and `--provenance` (Sigstore
  attestation) only works from CI (needs the OIDC id-token permission set to
  write), not locally.

KOSYAK (2026-06-17): wasted a round fumbling local publish (EOTP →
mis-set the OTP as `_authToken` → E401 → clobbered the session token)
before remembering the canonical tag-push path that `v0.4.6`/`v0.4.7`
already used. Tag-push first; never local-publish.

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
mcp-local-hub-* tasks. The supervisor spawn block (added in PR #212
"GUI owns supervisor lifecycle") is suppressed via
`MCPHUB_E2E_SUPERVISOR=none` — fixtures run under temp HOME with no
`supervisor-intent.json`, so without the seam every test would wait
the full 15-second IPC-readiness timeout. Both env vars are set in
`internal/gui/e2e/fixtures/hub.ts` and are NOT for production use;
GUI emits a stderr warning when `MCPHUB_E2E_SUPERVISOR=none` is set
so an accidental production set is operator-visible.

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
- Add/Edit server env picker: affordance button, auto-open on vault-reference prefix typing with substring-narrowing filter, sort-by-match with `matchTier`-based badge for exact-after-normalization, broken-ref inline marker (red `.broken` for missing, yellow `.unverified` for unverified), conditional compact summary line above env section when count > 1 or vault not ok, in-place AddSecretModal with snapshot revalidate-on-save (savedFiredRef dedup) and Retry-on-load-error, full ARIA combobox semantics with keyboard navigation (Tab/Esc/Arrow/Enter), create-contextual 409 conflict flow (modal stays open + Cancel triggers refresh + marker clears), `[data-action="create-disabled"]` rendering for vault not ok.
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

## Watchdog — REMOVED in v0.6 (Phase D)

The v0.4.x watchdog (the `\mcp-local-hub-watchdog` scheduled task +
`mcphub watchdog` command + the `recovery.go` recovery state machine)
is **DELETED**. It existed to revive scheduler-task daemons every 5
min, a job the v0.5.0 supervisor now owns directly via its Job-Object
reaper + reconcile loop. With the supervisor model the watchdog only
fought it (re-spawning daemons the supervisor deliberately stopped,
spamming `suspicious-xml` warnings against task XML its v0.4.x
validator could not parse). The full removal is in the v0.6 redesign
spec [`docs/superpowers/specs/2026-06-10-clean-architecture-redesign.md`](docs/superpowers/specs/2026-06-10-clean-architecture-redesign.md)
§5 Phase C/D.

**What replaced it.** Two distinct concerns the single watchdog used
to (partly) cover are now owned by purpose-built mechanisms:

- **Daemon revival** — the supervisor's own Job-Object reaper +
  restart-policy state machine (see "Supervisor (v0.5.0)" below). A
  crashed daemon is respawned by the supervisor in real time, not on a
  5-min scheduler poll.
- **Supervisor/GUI-owner-death recovery** — the **supervisor-liveness
  task** (`\mcp-local-hub-liveness`, installed by `mcphub setup`),
  added in v0.6 Phase 3a. It runs `mcphub supervise --ensure-alive`
  every ~1 min; the action probes the flock-authoritative
  `SupervisorRunningUnderStateDir` and relaunches the owner via the
  autostart task when no live lock holder exists. This is a NEW
  capability the watchdog never had (the watchdog only revived
  daemons, never the supervisor/GUI owner).

**Operator-visible changes.** The `mcphub watchdog ...` subcommands
(`--once`, `enable`, `disable`, `install`, `uninstall`, `status`) are
gone. `mcphub setup` no longer installs `\mcp-local-hub-watchdog` and
best-effort-removes any leftover one on existing hosts; `mcphub
uninstall` removes it on the last-server teardown. After `mcphub
setup`, `schtasks /Query /TN \mcp-local-hub-watchdog` returns "not
found". The `watchdog.log` / `intent-audit.log` watchdog-side entries
are no longer written (`intent-audit.log` itself stays — the
supervisor's `SupervisorEventLog` is the v0.6 audit/event surface).
`IsRealFailure` (the Task Scheduler LastResult classifier) survived
the deletion — it migrated to `internal/api/task_classifiers.go` and
is still consumed by the tray + GUI tray-state.

## Supervisor (v0.5.0)

v0.5.0 introduces a long-lived `mcphub supervise` parent process per user
that replaces v0.4.x's N-per-daemon Task Scheduler model. The supervisor
owns every MCP daemon as a child process under an OS-appropriate lifecycle
primitive (Windows Job Object with `KILL_ON_JOB_CLOSE`, Linux
`PR_SET_PDEATHSIG`, macOS preview process-group spawn; kqueue observation is
a v0.6 follow-up), observes child exits
in real time, applies a persisted restart-policy state machine, and
exposes a local-only owner-bound IPC for control commands. The full
design lives in
[`docs/superpowers/specs/2026-05-16-v0.5.0-supervisor-architecture.md`](docs/superpowers/specs/2026-05-16-v0.5.0-supervisor-architecture.md).
Release scope is **Windows GA / Linux beta / macOS preview** per spec Q9.
(The v0.4.x watchdog command + engine were DELETED in v0.6 Phase D — see
"Watchdog — REMOVED in v0.6 (Phase D)" above; supervisor/GUI-owner-death
recovery is now the `\mcp-local-hub-liveness` task.)

### State files

All under `<state-dir>` (resolved at runtime — see "State path" below;
shared with the v0.4.x watchdog state dir for byte-symmetric rollback):

```text
<state-dir>/
  supervisor-intent.json              # NEW: {version, updated_at, daemons[], maintenance_timers[], strict_mode}
                                      # canonical truth for `strict_mode`; mutated only via `mcphub strict-mode {enable, disable}`
                                      # task_name keys are canonical leading-backslash form
  supervisor-state.json               # NEW: {version, daemons{state, current_pid, pid_generation, started_at,
                                      #        orphan_pid, job_protection},
                                      #        transient_pids[], maintenance_fired_at{}}
                                      # NOTE: state is durable-only (`running`, or neutral `idle` when no
                                      # verified child is running). Restart-policy runtime state (the 30-min
                                      # crash sliding window, backoff deadline, quarantine timestamp, queued
                                      # post-exit action, and backoff/quarantine sub-states) is IN-MEMORY ONLY
                                      # in the supervisor (DaemonRuntimeTracker + SMContext) and
                                      # RESETS on every cold restart by design — pre-restart crashes are not
                                      # relevant to runtime respawn decisions. Earlier revisions carried
                                      # vestigial restart_history / backoff_until / quarantine_since /
                                      # queued_action fields here, but no production path ever wrote a non-empty
                                      # value; they were removed (2026-06-20 supervisor audit P3) so the
                                      # persisted schema matches what the code actually writes.
  supervisor-events.log               # NEW: JSONL audit trail; 16 KB per-entry cap; 10 MB rotation → .log.1 (schema below)
  supervisor.lock                     # NEW: supervisor singleton flock; sidecar JSON carries {pid, start_time}
                                      # IPC clients read this BEFORE opening the named pipe / unix socket for handshake
  # migration-journal-<UTC-ts>/       # REMOVED in v0.6 Phase F (the forward-migration engine that wrote it is deleted)
  daemon-intent.json                  # preserved exactly (byte-symmetric for v0.4.x rollback)
  managed-entries.json                # preserved exactly
  watchdog-state.json                 # preserved unchanged; v0.5.0 supervisor does NOT extend or write
```

The shared state-file helper inherits the v0.4.0+ relax-on-rejection
parent-dir DACL posture unless the operator sets
`MCPHUB_REQUIRE_SINGLE_USER_HOME=1` — see "Hardened state-file writes"
below.

### State path

- **Windows (GA):** `SHGetKnownFolderPath(FOLDERID_LocalAppData)` →
  `%LOCALAPPDATA%\mcp-local-hub\`. Production builds exclude the env-var
  fallback path via the `test_state_path_env` build tag (same gate the
  v0.4.x watchdog uses; the release-pipeline `go tool nm` assertion at
  watchdog plan §50 covers both).
- **Linux (beta):** `$XDG_STATE_HOME/mcp-local-hub` or
  `~/.local/state/mcp-local-hub`; dir mode 0700, files 0600. Sanity check
  rejects world-writable parent or non-owner uid (same exit 8 as
  watchdog).
- **macOS (preview):** same POSIX layout. Build-only Go cross-compile plus
  process-group spawn only; no kqueue lifecycle watcher or automated
  lifecycle tests in v0.5.0; v0.6 reevaluates.

`mcphub supervise --help` and the IPC `status` reply both print absolute
paths to every state file so operators can inspect / quarantine / restore
them directly.

GUI Dashboard status is now sourced through the supervisor IPC status seam:
`internal/cli/gui.go` wires `api.SupervisorIPCStatusFn =
api.DialSupervisorIPCStatus` before `gui.NewServer`, and `/api/status`
continues through `internal/api/health.go`'s `DaemonStatusSnapshot` cache.
The IPC handler reads `<state-dir>/supervisor-intent.json` for daemon
descriptors and `<state-dir>/supervisor-state.json` for runtime PID/state.
The legacy scheduler scan remains only the nil-seam fallback for hosts running
without this wiring; once wired, an unreachable or mismatched supervisor fails
loud as `STATUS_FAILED` instead of silently returning the deleted v0.4.x task
view.

### Subcommands

```bash
mcphub supervise                       # long-lived supervisor; idempotent via supervisor.lock
                                       # Hosts FIFO event loop, reconcile driver, IPC listener, child-exit reaper
                                       # Reads supervisor-intent.json + daemon-intent.json as canonical; --strict-mode CLI
                                       # arg is a SEED applied only when intent.updated_at predates the supervisor binary mtime

mcphub strict-mode enable              # Canonical mutation. Universal lock order: migration.lock → --once.lock
mcphub strict-mode disable             # Two-resource atomic write (supervisor-intent.json + autostart shim args)
mcphub strict-mode --recover           # Recovers from STRICT_MODE_REVERT_FAILED (exit 10) breadcrumb.
                                       # Acquires migration.lock; reads <state-dir>/strict-mode-mutation-incomplete.json;
                                       # prompts operator with two and only two branches: (A) drive both to `intended`,
                                       # (B) drive both to `actual_intent_state`. No third "manual override" branch.

mcphub autostart enable                # Per-OS shim install (Windows: Task Scheduler LogonTrigger; Linux managed:
mcphub autostart disable               # systemd user service; macOS managed: LaunchAgent; unmanaged Linux/macOS: none)
mcphub autostart status                # Per-backend probe semantics

mcphub install --upgrade               # Cold-restart upgrade flow (see "Cold-restart upgrade flow" below).
                                       # This is a binary-replacement + IPC-handoff flow, NOT a migration.
```

> **v0.6 Phase F removed the v0.4.x→v0.5.0 forward-migration engine.** The
> `internal/migration` package is deleted, and with it `mcphub install
> --rollback-to-legacy` (the legacy-demotion path), the
> `mcphub install --upgrade --reset-failure-windows` flag, and the
> `migration-journal-<UTC-ts>/` per-install journal. The remaining
> `mcphub install --upgrade` is the **cold-restart binary-replacement**
> flow (rename-aside + IPC handoff), which never wrote a migration journal
> and is unaffected. The `migration.lock` flock SURVIVES — it was migrated
> out of the deleted package into `internal/api/state_dir_locks.go` and is
> now the GENERIC universal-lock-order primitive (basename preserved for
> byte-symmetry); it is no longer migration-engine-specific. `mcphub
> migrate-legacy` (a SEPARATE command that converts disabled
> mcp-language-server client-config entries into managed workspace
> registrations — M4 Task 14) is unaffected and still ships.

`mcphub restart` and `mcphub status` are **IPC-only commands** — they do
NOT acquire `migration.lock`, only mutate supervisor in-memory + IPC
state. This exempts them from the universal lock-acquire order and
prevents them from deadlocking against a `quiesce-timers` drain.

### Exit codes

Exit codes are command-scoped. The surviving v0.6 supervisor / strict-mode
surfaces are below. **v0.6 Phase F removed the migration/rollback exit
codes** — `13 ROLLBACK_TOKEN_MISMATCH`, `14 MIGRATION_POWERSHELL_LOCKED`,
and the named abort codes `MIGRATION_PORT_LOOKUP_INCONSISTENT`,
`ROLLBACK_ORPHAN_DAEMONS_REMAIN`, `SUPERVISOR_REFUSING_ROLLBACK_IN_PROGRESS`
are gone with the forward-migration engine. The old `8 INSTALL_BUSY`
(migration.lock held by another `mcphub install`) no longer fires either —
`mcphub install --upgrade` is the cold-restart binary swap, not a
migration. Codes 9 / 10 below survive (strict-mode + the generic
`migration.lock` primitive in `state_dir_locks.go`):

```text
0   — success
1   — generic backend error
8   — exitSetupStatePathRejected (`mcphub setup` state-path rejected; NOT the
      removed install-migration INSTALL_BUSY code)
9   — STRICT_MODE_BUSY (`migration.lock` held when `mcphub strict-mode
      {enable,disable,--recover}` tried to acquire). Universal lock order:
      migration.lock BEFORE --once.lock; refuse-if-held with explicit
      exit code beats silent blocking.
10  — STRICT_MODE_REVERT_FAILED. Set when the two-resource atomic write
      (supervisor-intent.json + autostart shim args) failed AND the revert
      of the first-step write also failed. Supervisor writes
      <state-dir>/strict-mode-mutation-incomplete.json carrying
      {intended, actual_intent_state, actual_shim_state, step1_error,
      step2_error, revert_error, ts}; emits body to stderr; exits 10.
      Subsequent `strict-mode` invocations refuse-if-held on the breadcrumb
      until `mcphub strict-mode --recover` runs or operator deletes manually.
```

### Migration journal layout + retention — REMOVED in v0.6 Phase F

> **This entire mechanism is gone.** v0.6 Phase F deleted the
> `internal/migration` package and with it the per-install
> `migration-journal-<UTC-ts>/` directory, all its forward-progress
> markers, the resume/rollback classification, and the
> `--rollback-to-legacy` demotion path. The surviving `mcphub install
> --upgrade` is a cold-restart binary-replacement flow (see "Cold-restart
> upgrade flow" below) that NEVER wrote a migration journal. Global daemons
> now spawn from `supervisor-intent.json` reconcile, not from a migrated
> per-daemon scheduler-task set. The historical layout below is RETAINED FOR
> REFERENCE ONLY (e.g. reading an old journal left on a pre-Phase-F host);
> no current code writes or reads it.

Historical (pre-Phase-F) layout — `mcphub install --upgrade` (the deleted
forward-migration engine) wrote a per-install journal under
`<state-dir>/migration-journal-<UTC-timestamp>/` with forward-progress
markers:

```text
<state-dir>/migration-journal-<UTC-ts>/
  prepared                              # touched AFTER intent derived + classification done; before any OS mutation
  pre-os-mutating                       # touched AFTER the FIRST successful kill (so resume distinguishes "no kills yet" from "some kills committed")
  os-mutating-complete                  # touched AFTER all legacy schtasks /Delete + shim install
  committed                             # touched AFTER supervisor reconcile-ready confirmed within 30s via IPC `status`
  rollback-in-progress                  # ONLY present during rollback execution; deleted at rollback step 12
  legacy-tasks/<task>.xml ...           # raw XML snapshots — re-registered verbatim on rollback
  legacy-tasks-classification.json      # deviation-only classification verdict per task
  canonical-template-snapshot.xml       # rendered by v0.4.x-pinned renderer (NOT v0.5.0 install code)
                                        # Cross-version resume always uses this verbatim; never re-renders
  pre-migration-strict-mode             # marker file recording pre-migration strict_mode value (for autostart shim seed)
  derived-supervisor-intent.json        # the intent file the migration will write — preserves operator edits on resume
  killed-daemons.json                   # per-daemon kill verdicts (PID, port, basename, CommandLine, createdUnix, gate result)
  netstat-cache.json                    # snapshot used by lookupProcessIdentity; refreshed after every kill attempt
```

**Retention:** after `committed` AND step 14 supervisor reconcile-ready
confirmed, the migration driver under the held `migration.lock` keeps
the **5 newest** `migration-journal-*/` directories and deletes the
rest using two-phase crash-atomic pruning (rename to
`.pruning-<original-basename>/` first; then `os.RemoveAll`). On crash
mid-prune, any `.pruning-*` prefix is unambiguously failed-prune debris
and is finished by the next migration driver invocation BEFORE resume
classification scans for journals. Per-dir delete failures are
non-fatal (logged warn). Operator visibility: `mcphub install status`
prints the retained-journal count + oldest-retained timestamp.

**Resume classification** (markers `prepared` → `pre-os-mutating` →
`os-mutating-complete` → `committed`; rollback marker
`rollback-in-progress`):

- `prepared` only → safe to abort; delete journal.
- `pre-os-mutating` no `os-mutating-complete` → forward-resume re-uses
  `derived-supervisor-intent.json` (preserves operator edits).
- `os-mutating-complete` no `committed` → operator picks forward retry
  or rollback.
- `rollback-in-progress` present at supervisor cold start → supervisor
  refuses to reconcile, exits
  `SUPERVISOR_REFUSING_ROLLBACK_IN_PROGRESS` with operator guidance
  ("complete `mcphub install --rollback-to-legacy` or `mcphub install
  --upgrade` first").

### `supervisor-events.log` schema

JSONL envelope follows the same 16 KB per-entry cap, 10 MB rotation,
and flock discipline as `internal/api/gui_event_log.go:19-25`. Field
set differs — supervisor envelope uses `event` discriminator + adds
`task_name`; `gui_event_log` uses `type` and has no `task_name`:

```json
{
  "schema_version": "1",
  "ts": "RFC3339Nano",
  "severity": "debug|info|warn|error",
  "source": "ipc|lifecycle|restart-policy|migration|autostart|reconcile",
  "event": "ipc-command|child-exit-observed|reconcile-tick|...",
  "task_name": "\\mcp-local-hub-memory-default",
  "body": { "...arbitrary structured payload..." },
  "_truncated": false
}
```

Per-entry size cap follows the `AuditIdentityFieldByteCap` posture in
`internal/api/intent_audit.go`: 16 KB hard cap; identity fields
(`event`, `task_name`, `source`) NEVER truncated; oversize identity →
entry rejected with `ErrIdentityOversize` and caller fails closed.
10 MB rotation with single `.log.1` backfile. After rotation, an
`audit-rotated` self-event is written to the new active file.
Flock-protected appends. (The v0.4.x `watchdog_log.go` that first
established this shape was deleted in v0.6 Phase D; the supervisor
event log carries the pattern forward.)

Cross-channel routing: structured `warn`-severity events like
`severity: warn, event: supervisor-state-relax-lane-active` and
`severity: warn, event: unknown-maintenance-kind` go to this log; the
operator-visible stderr line is the human-readable shadow of the same
event.

Supervisor lost-child / first-bind events (PR-1, P1a+P1b):

- `daemon-stale-exit-ignored` (`severity: info`, `source: lifecycle`) —
  a late `cmd.Wait` exit of a SUPERSEDED child (an older `pid_generation`
  than the tracker's current one for the task) was dropped instead of
  clearing the CURRENT child's tracking / driving an SM transition. Two
  emit sites, distinguishable by body: the wait goroutine (body carries
  `pid`, `pid_generation`, `exit_code`) and the controller processing-time
  guard (body adds `current_generation`, `sm_state`). This is the
  generation-stamped exit attribution (P1a) that stops the supervisor from
  manufacturing forgotten port squatters.
- `daemon-bind-timeout` (`severity: warn`, `source: liveness`) — a
  freshly-spawned daemon never bound its port before its first-bind
  deadline (P1b); body carries `pid`, `port`, `deadline_seconds`,
  `waited_seconds`. Emitted before the existing `daemon-running-state-stale`
  + `EvManualRestart`. Deadline is per-descriptor
  (`startup_bind_deadline_seconds`, default 60s global / 120s serena-proxy);
  it replaces the flat 5s grace ONLY for the pre-first-bind window, so a
  slow-starting daemon is no longer killed mid-startup.

### Hardened state-file writes — corp-policy posture

Every supervisor state file (`supervisor-intent.json`,
`supervisor-state.json`, `supervisor-events.log`, `supervisor.lock`,
plus migration journal files) goes through the same shared state-file
helper that consumes the relax-on-rejection parent-dir DACL gate
documented in the "Hardened client-config writes + corp-policy
posture" section above. The supervisor state files inherit the same
default posture: parent-dir gate rejects → pipeline re-runs with the
parent-dir gate disabled; the new file is owner-only regardless of
how broad the parent ACL is.

**Residual co-resident attacker risk on multi-tenant Windows hosts.**
On corp-managed environments where `%LOCALAPPDATA%` parent grants
`FILE_DELETE_CHILD` to `Domain Users` (a common corporate Group
Policy ACE), a co-resident user cannot read or modify supervisor
state file **content** (file-level DACL still denies that), but they
CAN delete the directory entry and replace it with an
attacker-crafted file. Combined with v0.5.0's new attack surface
(IPC commands + strict-mode mutation + maintenance timer scheduler),
this gives a co-resident user the ability to:

- flip `strict_mode` posture via swapped `supervisor-intent.json`,
- inject attacker-controlled daemon descriptors.

A swapped `supervisor-state.json` cannot by itself inject a persisted
`"state":"quarantined"` row from a current supervisor writer to suppress
a legitimate daemon on the next supervisor cold start. Current writes
collapse restart-policy sub-states to neutral `idle`; the state file is
still parsed into the runtime tracker, but cold-start controller state is
seeded only for verified running PIDs; a not-running daemon reaches the
initial reconcile as idle/default state and running intent spawns it.
The restart-policy internals (crash history, backoff deadline,
quarantine timestamp, queued action, and backoff/quarantine sub-states)
are also in-memory-only and no longer attacker-primable through
persisted fields after the 2026-06-20 audit.

**Operators on such hosts MUST set
`MCPHUB_REQUIRE_SINGLE_USER_HOME=1` to extend the strict parent-dir
gate to supervisor state files.** The strict gate is the only
mitigation; supervisor itself cannot detect or refuse a parent-dir
replacement attack because it would have already trusted the swapped
file on read. `mcphub install` emits an explicit `severity: warn,
event: supervisor-state-relax-lane-active` entry to
`supervisor-events.log` (and a one-line stderr message) on every
install when strict mode is NOT set on a Windows host, naming the
residual risk and the env var that mitigates it. No silent downgrade.

For IPC trust boundary specifics, the per-resource SDDL allowlists
diverge intentionally: file SDDL is
`D:P(A;;GA;;;<currentUserSID>)(A;;GA;;;SY)(A;;GA;;;BA)` (matches
v0.4.x precedent, BA retained for installer/admin-managed write
paths); pipe SDDL is `D:P(A;;GRGW;;;<currentUserSID>)(A;;GRGW;;;SY)`
(BA **dropped** — defense-in-depth: any admin token on the box would
otherwise be able to issue `exit`/`restart`/`quiesce-timers` commands
without owner consent; LocalSystem retained for legitimate
service-host scenarios). A post-`ListenPipe` smoke test asserts the
effective pipe DACL via `GetSecurityInfo` on the listener handle and
is a required merge gate.

### Cold-restart upgrade flow

**Windows binary replacement (rename-aside).** The atomic-rename
trick used elsewhere on POSIX is not available for a live `.exe` on
Windows because the kernel holds the executable image:

1. Write new binary to `<install-dir>/mcphub.exe.new`.
2. `MoveFileExW(target, target + ".old-<ts>", REPLACE_EXISTING)`.
3. `MoveFileExW(target + ".new", target, 0)`.

**`.old-<ts>` cleanup.** On every `mcphub install --upgrade` AND
every `mcphub supervise` startup, glob
`<install-dir>/mcphub.exe.old-*` and `os.Remove` each whose name
parses as `.old-<RFC3339>` form AND whose mtime is older than 7 days.
`os.Remove` failures (file still mapped, AV scan, ACL flip) logged
warn + retried on next pass. Bounded accumulation; no admin rights
required because supervisor runs as same user that wrote the file.

POSIX uses atomic `rename(2)`.

**IPC quiesce + exit sequence.**

1. Replace binary atomically (per above).
2. Connect to supervisor IPC; client reads `supervisor.lock` for
   expected `{pid, start_time}` FIRST.
3. Issue IPC `quiesce-timers{timeout_ms: 30000}` — drains
   maintenance-timer transients on a **separate goroutine** (does
   not block the FIFO event loop). Two-frame response: immediate
   `{accepted: true}`, then `{drained, still_running, final: true}`.
4. Issue IPC `exit{graceful: true, timeout_ms: 5000}`. Supervisor
   posts `request-graceful-exit` events into FIFO for each running
   daemon, waits for all to reach `idle`, then exits 0.
5. **Force-kill fallback on IPC timeout:** `taskkill /F /T /PID
   <supervisor>` (the `/T` ensures any non-Job-Object transients in
   `transient_pids` die alongside the supervisor); POSIX `kill -KILL
   -<pgid>` (process-group kill, not single-PID). After force-kill,
   verify each expected daemon port is unbound via
   `lookupProcessIdentity`; if any port still bound after a bounded
   retry (10 s), abort with `ROLLBACK_ORPHAN_DAEMONS_REMAIN` plus
   manual cleanup guidance.
6. `mcphub install` explicitly starts the new supervisor (Windows:
   `schtasks /Run /TN \mcp-local-hub-supervisor` if shim installed,
   else detached `windows.CreateProcess` with `DETACHED_PROCESS |
   CREATE_NEW_PROCESS_GROUP`; Linux managed: `systemctl --user
   restart mcphub-supervisor.service`; etc.).
7. New supervisor reads intent + daemon-intent → reconcile →
   respawns daemons.

Note: the `DETACHED_PROCESS | CREATE_NEW_PROCESS_GROUP` flag combo
makes the new Windows supervisor unable to receive `CTRL_C_EVENT`
from any other process per Microsoft docs; graceful shutdown is
**exclusively via IPC `exit`**. Task Manager → End Task →
ungraceful Job Object close; `KILL_ON_JOB_CLOSE` reaps every child.

### Post-migration / post-rollback recovery — REMOVED in v0.6 Phase F

> **The migration/rollback recovery flow below is gone.** v0.6 Phase F
> deleted the forward-migration engine, the `migration-journal-<UTC-ts>/`
> directory, `mcphub install --rollback-to-legacy`, and the
> `rollback-warnings.json` artifact. There is no migration to recover from
> anymore — global daemons spawn from `supervisor-intent.json` reconcile.
> The historical recovery procedure below is RETAINED FOR REFERENCE ONLY
> (e.g. an operator on a pre-Phase-F host with a stale journal). The
> `STRICT_MODE_REVERT_FAILED` recovery further down is UNAFFECTED — it
> concerns strict-mode + the surviving `migration.lock` primitive, not the
> deleted migration engine.

Historical (pre-Phase-F): after `mcphub install --upgrade` (the deleted
migration engine) returned success, the journal at
`<state-dir>/migration-journal-<UTC-ts>/` carried `committed` and the
classification + kill-verdict artifacts. Operators investigating
incidents preserved the directory (retention kept the 5 newest; older
were pruned at the next migration).

Historical (pre-Phase-F): after `mcphub install --rollback-to-legacy`,
operators read `<state-dir>/migration-journal-<UTC-ts>/rollback-warnings.json`
if present (schema v1):

```json
{"version": 1, "warnings": [
  {"task": "\\mcp-local-hub-memory-default",
   "port": 9128,
   "reason": "port-not-bound-after-60s",
   "observed_at": "RFC3339Nano"}
]}
```

Rollback **exits 0** even with warnings present (success-with-warnings
semantic). Non-zero exit is reserved for "rollback itself failed".
Recovery steps:

1. For each warning, manually inspect why the legacy daemon didn't
   bind the port (port collision with a new process started during
   the rollback window, missing executable, manifest mismatch, etc.).
2. Re-run `schtasks /Run /TN <task>` for each unbound task once the
   blocker is cleared.
3. After all legacy daemons are healthy, optionally re-attempt
   `mcphub install --upgrade` on the supervisor model.

**`STRICT_MODE_REVERT_FAILED` (exit 10) recovery.** If the breadcrumb
`<state-dir>/strict-mode-mutation-incomplete.json` is present, run
`mcphub strict-mode --recover`. It acquires `migration.lock` (refuses
if held → `STRICT_MODE_BUSY` exit 9), reads the breadcrumb, prompts
operator with two and only two branches: drive both `intent` + shim
to the original `intended` value, or drive both to
`actual_intent_state` (what's on disk now). No third "manual
override" branch — those two are exhaustive. Once both writes succeed
under the held lock, breadcrumb is deleted. If either write fails
during recovery, breadcrumb is re-asserted with updated
`step1_error` / `step2_error` / `revert_error` and exit 10 fires
again.

**Resume classification cheat-sheet** — REMOVED in v0.6 Phase F. The
markers below lived under the deleted `migration-journal-<UTC-ts>/`
directory; the forward-migration engine that wrote and resumed them is
gone. Retained for reference only (reading a pre-Phase-F host's stale
journal):

| Markers present | Operator action (historical, pre-Phase-F) |
|---|---|
| `prepared` only | Safe to abort; delete journal. |
| `pre-os-mutating` no `os-mutating-complete` | `mcphub install --upgrade` resumed from `derived-supervisor-intent.json`. |
| `os-mutating-complete` no `committed` | Operator picked forward retry or rollback. |
| `committed` | Migration succeeded. Older journals beyond the 5 newest were pruned automatically. |
| `rollback-in-progress` | Re-run `mcphub install --rollback-to-legacy` to finish; supervisor cold start refused to reconcile until cleared. |

### `mcphub watchdog` — removed (v0.6 Phase D)

The `mcphub watchdog` command and all its subcommands (`--once`,
`enable`, `disable`, `install`, `uninstall`, `status`) are DELETED —
see "Watchdog — REMOVED in v0.6 (Phase D)" above. The canonical
management surface is `mcphub supervise --help` plus the IPC `status`
command issued via `mcphub status`. Supervisor/GUI-owner-death
recovery is the `\mcp-local-hub-liveness` task (`mcphub supervise
--ensure-alive`).

### Job Protection field operator runbook (PR #242)

When the GUI Dashboard renders **Job Protection: UNPROTECTED ⚠** on
a daemon card, or `mcphub status --json` returns
`"job_protection": false` for a daemon, the per-spawn Windows Job
Object allocation failed on the supervisor's last spawn attempt for
that daemon. The supervisor proceeded via the documented non-fatal
fallback (ADR #239 Step 1): plain `cmd.Start` without
`StartWithJob`, so the daemon's descendant tree no longer has
`KILL_ON_JOB_CLOSE` orphan-protection. If the supervisor crashes
(`taskkill /F mcphub.exe`, OS reboot, OOM kill), this daemon's
sub-processes (e.g. `uvx` → `python`, `npx` → `node` wrappers) may
survive as orphans and continue holding TCP ports — `mcphub`
respawn will then hit port-in-use until manual cleanup.

The field is tri-state (`*bool` in Go, `boolean | undefined` in
TypeScript). `nil`/`undefined` means "unknown / legacy state file /
not yet probed" and renders no badge (default-trust). `true` means
"per-spawn Job allocated; orphan-protection invariant holds" and
also renders no badge. `false` is the only state that renders the
warning badge.

**Realistic underlying causes** for `false`, in order of frequency:

1. **AppLocker / WDAC publisher allowlist** — corporate endpoint
   management policy denying `CreateJobObjectW`. The supervisor
   process itself is allowed, but the Job-creation syscall is
   blocked by group policy. Verify with `Get-AppLockerPolicy
   -Effective -Xml | findstr CreateJob`; if AppLocker is enforcing
   on the host, the policy owner (typically the endpoint-management
   team in a corporate environment) must add an exception.
2. **Nested Job constraints** (Windows 7 / Server 2008 R2 era hosts
   without nested-Job support). Win8+ supports
   `JOB_OBJECT_LIMIT_BREAKAWAY_OK` transparently per Microsoft
   docs; pre-Win8 hosts fail `AssignProcessToJobObject` when the
   supervisor is itself in a Job. Mitigation: upgrade the host OS
   or run the supervisor outside the parent Job.
3. **Handle exhaustion** — the supervisor (or its parent session)
   exceeded the per-process kernel handle quota (default ~16M, but
   environments with handle-leaking AV / monitoring agents can
   reach saturation). Verify with `Get-Process mcphub | Select
   Handles`. Mitigation: restart the offending leaker; restart the
   supervisor.
4. **Permission denied** on `OpenProcess(PROCESS_SET_QUOTA |
   PROCESS_TERMINATE)` — extremely rare in single-user mode but
   possible on hosts where the spawned daemon's binary is owned
   by a different SID than the supervisor (e.g. binaries installed
   via an admin-owned MSI then exec'd from a non-admin session).
   Mitigation: chown the daemon binary to the same SID as the
   supervisor process owner.

**Operator action when the badge fires**:

- *Single-user solo-dev host (not corp-managed)*: investigate which
  of causes 2-4 applies. The fallback is rare on solo-dev hosts;
  the most likely cause is handle exhaustion from a transient
  leak. Restart the supervisor (`mcphub supervise` → Ctrl+C →
  restart, or `taskkill /F /IM mcphub.exe && start mcphub supervise`)
  to retry Job allocation on the next spawn. If the badge clears
  after restart, the underlying leak was transient. If it persists
  across restarts, file a bug with the supervisor-events.log
  `per-spawn-job-create-failed` entries attached.
- *Corp-managed host with AppLocker / WDAC*: the underlying cause is
  policy you cannot fix yourself. Confirm with `mcphub status
  --json` that the field stays `false` across supervisor restarts
  (single-user-level diagnostic). Then escalate to the
  endpoint-management policy owner with the `severity: warn` event
  body from `supervisor-events.log` (path printed by `mcphub
  supervise --help`) — they need to add a publisher-allowlist
  exception for the mcphub binary's `CreateJobObjectW` syscall path.
  Until policy is updated, the daemon DOES still run (the fallback
  is non-fatal by design), but operator must accept the
  orphan-protection regression as a known tradeoff. Document the
  exception in your team's runbook so the badge stops being noise.
- *Pre-restart `nil` value*: a daemon entry that's been running since
  before the supervisor binary was upgraded to PR #242's surface
  reads as nil (unknown). The supervisor never re-probes mid-run;
  the field flips on the NEXT spawn. To force a re-probe, restart
  the supervisor (in-place via `mcphub supervise` Ctrl+C + restart,
  or via the IPC `restart` command).

**Strict job-protection (fail-closed) — SHIPPED.** The
`mcphub supervise --strict-job-protection` flag (env equivalent:
`MCPHUB_STRICT_JOB_PROTECTION=1`, truthy `1`/`true`) flips the
documented non-fatal fallback into a refusal. With it set, a per-spawn
Job-Object allocation failure makes the supervisor REFUSE the spawn
(no `cmd.Start`, no child) and quarantine the daemon directly — it does
NOT churn through backoff (a Job-create failure is a recurring
host-policy condition, so backoff would never recover). The daemon
stays Quarantined until the operator clears the underlying cause
(AppLocker/WDAC publisher allowlist, handle exhaustion, etc. — see the
field runbook above) and restarts the supervisor. The same
`per-spawn-job-create-failed` event is emitted (now with
`strict_job_protection: true` + a fail-closed `action`, at `error`
severity), and a `daemon-quarantined` event records the SM transition.
DEFAULT is unchanged (flag/env unset → non-fatal fallback, daemon
spawns without orphan-protection + `severity: warn
per-spawn-job-create-failed` with `strict_job_protection: false`). The
env var is the host-level config knob that survives supervisor restart
via the autostart shim's inherited environment, mirroring the
`MCPHUB_REQUIRE_SINGLE_USER_HOME` DACL-gate posture; there is no
GUI-spawn flag chain for it. The flag is resolved ONCE at supervisor
startup (`runSupervise` → `strictJobProtectionEnabled`) and threaded
into the per-spawn closure. ROADMAP §11.3.

**Future v0.5.x followups** (still NOT scoped):

- Auto-remediation: retry Job allocation on a bounded schedule
  (e.g. exponential backoff via the existing supervisor restart
  policy) so a transient cause clears without operator action.
- Metrics export (Prometheus / OpenTelemetry counter for
  `mcphub_supervisor_job_create_failures_total` by cause) so
  fleet-wide trends are visible.
- Alerting integration: piping `severity: warn event:
  per-spawn-job-create-failed` to PagerDuty / Slack / etc.

### Serena migrate supervisor-lock interlock (v0.5.x)

`mcphub migrate serena legacy-to-dynamic-pool` reaps the running
supervisor, writes the new `runtime_spec`-bearing
`supervisor-intent.json`, then starts the successor. Across that whole
reap→write→start window the migrate HOLDS `supervisor.lock` as a
critical-section mutex so no other actor can START a supervisor inside
the window (every supervisor-starter acquires the same lock first). The
spec-bearing write gate is bypassed via a typed capability token
(`InstallParsedManifestBypass`, mintable ONLY by
`(*SupervisorLock).AllowSpecBearingWriteBypass()`) whose identity the
gate re-verifies (lock still held AND lock path == the gate's own
`supervisor.lock`), so a foreign supervisor can never trip the gate
against the migrate's own held lock. The serena auto-register INTRODUCE
cutover takes the SAME lock before its own reap, so the two reaping
flows are mutually exclusive and neither can force-kill the other's
lock-holding PID. Full design:
[`docs/superpowers/specs/2026-05-29-serena-migrate-redesign-descriptor-proxy.md`](docs/superpowers/specs/2026-05-29-serena-migrate-redesign-descriptor-proxy.md)
§7.1.1.

**Operator-sequencing constraint (Starter B).** Do NOT run `mcphub
install --upgrade` concurrently with `mcphub migrate serena
legacy-to-dynamic-pool`. The two are NOT co-serialized — they share no
lock (the v5-upgrade path does not take `migration.lock`, and folding
the generic `RunInstallUpgrade` contract into the serena interlock seam
is out of scope). Both reap-then-start and both read the same
`supervisor.lock.owner.json`, so a concurrent `install --upgrade` can
force-kill the migrate's lock-holding process. The collision is
SAFE-but-noisy, not corrupting: the migrate's intent write is atomic
temp+rename, so a death before the rename leaves legacy serena intact
(recoverable) and a death after the rename leaves the committed
dynamic-pool intent which the upgrade's freshly-started new-binary
supervisor reconciles correctly. Either way there is no split-brain —
the residual is a failed/odd-looking migrate run the operator re-runs.
Sequence the two commands one after the other; do not overlap them.

**New `supervisor-events.log` events (both `info`).** These join the
Cross-channel routing set documented under "`supervisor-events.log`
schema" above:

- `supervisor-interlock-handoff-window` (`severity: info`, `source:
  migration`) — fired when the known-benign release→child-acquire
  hand-off window actually exercised its tolerance. `body.phase` is
  `reconcile-ready-retry` (the pre-bind IPC-pipe race materialized but
  resolved after >1 poll) or `duplicate-spawn-exit` (a racing duplicate
  supervisor exited via the singleton). It exists so an operator can
  tell this benign window apart from a recurrence of the original
  bare-30 s-IPC-timeout bug; emit-failure is silently non-fatal.
- `serena-auto-register-deferred-on-interlock` (`severity: info`,
  `source: reconcile`) — fired when a serena auto-register INTRODUCE
  cutover could NOT acquire `supervisor.lock` because a concurrent
  serena migrate/cutover holds it. It fires on BOTH spec-bearing
  introduce sub-paths: (a) the introduce-WHILE-RUNNING case
  (`needReap`) — the auto-register acquires the interlock IMMEDIATELY
  AFTER its reap (bot PR #276 r2 P1 — the running supervisor holds the
  lock, so a pre-reap acquire could never succeed), so a defer there is
  POST-reap and the cutover drives its `failPreCommit` recovery restart
  (the race winner restarts the supervisor); and (b) the
  NO-supervisor introduce (`needStart && !needReap && !priorHasSpec`,
  bot PR #276 r4 P2) — the spec-bearing `runtime_spec` write also holds
  the interlock across its write→start window, and a defer there is
  PRE-reap (nothing was reaped, so `failPreCommit` owes no recovery
  restart — it just rolls back the registry row). Either way the
  cutover rolls back its registry row and returns an honest error the
  `/serena/mcp` router maps to 503 → the client retries (by which time
  the winner has settled). It is deliberately a DISTINCT event, NOT a
  misleading `spec-bearing-install-refused` / "supervisor running"
  (there is no supervisor — a CLI peer holds the lock).

## Marketplace (G5, v0.3.0)

`mcphub marketplace {search,show,generate,refresh}` lets an operator
discover MCP servers from a curated catalog. Default registry URL
(**v2 is the current default** — the Tier-1 desktop-app rows + the
additive D-2/D-3 metadata live here):
`https://raw.githubusercontent.com/applicate2628/mcp-local-hub/master/marketplace/v2/catalog.json`.

**v1 is FROZEN** (`marketplace/v1/catalog.json`) — kept in-tree only so
an OLDER released client hard-coded to the v1 URL still resolves; do NOT
edit it (old-client contract — it must stay `schema_version: "1"` with
zero D-2/D-3 keys). The default-URL string is **single-owned in
`internal/api`** (`api.DefaultMarketplaceRegistryURL`, in
`marketplace_catalog.go`); `internal/cli/marketplace.go`
(`DefaultMarketplaceRegistryURL`) and `internal/gui/marketplace.go`
(`defaultMarketplaceRegistryURL`) both RE-EXPORT from it (both already
import `internal/api`, and the GUI still never imports the CLI package) —
so a catalog-version change is **one bump in `internal/api`**, no more
bump-both drift (was tracked + now closed:
`work-items/bugs/2026-06-24-marketplace-url-duplication.md`). The v2
catalog's three shapes (S1 local-stdio / S2 remote-http / S3 docs-only
OAuth connector) are decided in
`work-items/decisions/2026-06-24-d1-three-catalog-shapes.md`.

- `search [query]` — table of catalog entries matching query (empty = list all).
- `show <id>` — metadata block + `Readme URL:` line (operator opens the URL).
- `generate <id>` — draft YAML to stdout. **Operator MUST edit before**
  `manifest create`: rename `name:`, pick a real port, **do NOT
  persist raw tokens / passwords / API keys** — replace verbatim
  `${env:*}` placeholders with a vault reference from `mcphub secrets` or the operator-meaningful
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

### Init-time stubs (v0.4.5+ Initialize button)

The Servers matrix "Initialize" affordance writes an empty per-client
stub (`{"servers":{}}`, `{"mcpServers":{}}`, or `[mcp_servers]`) when
the operator clicks the per-column button. That write goes through
`clients.EnsureClientConfigStub`
([internal/clients/write.go](internal/clients/write.go)) which uses
an atomic `O_CREAT|O_EXCL` open with `O_NOFOLLOW` (POSIX) plus an
explicit `os.Lstat` symlink/non-regular pre-check. It does NOT
route through the production `SecureWriteClientConfig` pipeline
because that pipeline uses `FILE_RENAME_REPLACE_IF_EXISTS` semantics
which would be vulnerable to the deep-sec Lane A race (Init clobbers
a concurrent migrate write) and the Lane C symlink TOCTOU
(default-relax symlink resolution follows attacker-planted symlink).

**Residual Windows symlink window.** On POSIX the create is fully
atomic — `O_NOFOLLOW` refuses symlinks at kernel level. On Windows
the Go runtime's `os.OpenFile` cannot map to `NtCreateFile +
FILE_FLAG_OPEN_REPARSE_POINT`, so the Lstat pre-check is the
defense and there is a microsecond window between Lstat and
`CreateFileW` during which an attacker who has write rights on the
parent directory could plant a fresh symlink and redirect the
empty-stub write to an attacker-chosen target. Impact is bounded
(arbitrary-path file create/clobber with predictable empty-stub
content). The proper fix — a `SecureCreateClientConfigIfMissing`
helper using NtCreateFile with reparse-point refusal — is tracked
for v0.4.6+. On a single-user solo-dev host with `MCPHUB_REQUIRE_SINGLE_USER_HOME=1`,
the parent-dir DACL gate already prevents any co-resident principal
from having parent-dir write rights, so the race is unreachable.

## Stuck-instance recovery

### Quarantined daemon with a port squatter — `mcphub daemon recover <task>`

Symptom: a daemon (typically `serena`) is quarantined
(`10+ failures in 30-min sliding window; automatic respawns suspended —
run 'mcphub daemon recover <task>' …`) while a **forgotten own-child**
(a live mcphub daemon the supervisor lost track of) still squats the
daemon's TCP port, so the hub looks "red" but a working server is
answering on the port. This is the supervisor lost-child class
(`work-items/bugs/2026-07-02-supervisor-lost-child-quarantine-class.md`).

**When to use.** A quarantined daemon whose port is held by a squatter,
OR any daemon you want to force-respawn after clearing a port squatter,
WITHOUT a full supervisor restart (which would reap the whole fleet and
mask the defect).

**What it does.** Resolves the descriptor from `supervisor-intent.json`,
checks who owns the port, and:

1. If a **verified-own** squatter holds it (our binary at the configured
   path, our argv naming THIS task exactly, start-time-proven via a held
   handle), reaps it with `TerminatePIDWithIdentity` (confirm prompt
   unless `--yes`). A **foreign or unverifiable** owner is REFUSED —
   never killed (fail-closed). Windows only in v1; on other platforms a
   bound foreign owner is refused (no kill).
2. Forces a respawn through the supervisor (`force=true`), which resets
   the quarantine window.

Every verdict/kill is audited to `supervisor-events.log`
(`daemon-port-squatter-reaped` / `-foreign` / `-unverified` /
`-reap-failed`, with `source:"recover"` + `actor`).

```bash
mcphub daemon recover \mcp-local-hub-serena-b133f336        # prompts before killing
mcphub daemon recover \mcp-local-hub-serena-b133f336 --yes  # no prompt; for scripts
```

Exit codes:

```text
0 — recovered (reaped if needed, force respawn accepted)
2 — unknown task (not in supervisor-intent.json) OR intent unreadable
3 — refused: the port owner is foreign / unverifiable (no kill), or the
    operator declined the confirmation prompt
4 — force respawn returned a non-success supervisor code
5 — supervisor unreachable (start `mcphub supervise` / enable autostart)
```

`POST /api/daemon/respawn {force:true}` remains the GUI/programmatic
equivalent of step 2 (without the port-squatter reap).

### Secret daemons exit 1 / quarantine on sandbox-broadened `%LOCALAPPDATA%`

Symptom: secret-using daemons such as `wolfram` or `paper-search`
exit 1 quickly and may quarantine with an error like:
`daemon <server>/<daemon>: vault state-file DACL refused for <path>.
offending SID <SID>. Remediate: tighten this file's DACL to owner-only
(your account + SYSTEM + Administrators); see the "secret daemons exit 1
on a sandbox-broadened %LOCALAPPDATA%" runbook in CLAUDE.md for the exact
icacls / chmod command. Cause: vault exists but unreadable: read identity:
file <path>\.age-key not single-user safe: hub-mcp state file DACL grants
read to a SID outside {current-user, LocalSystem, BuiltinAdministrators}:
SID <SID> grants access ...`.

Cause: `.age-key` or `secrets.age` inherited a non-owner ACE from a
broadened profile/state root, commonly `Wave\CodexSandboxUsers` or an
orphan SID with Modify rights. The read hardening must fail closed here:
a swapped `.age-key` is an attacker-substituted X25519 identity.

Fix the refused file named in the error. On Windows PowerShell 5.1, run
these as two separate commands for that file:

```powershell
icacls "<path>" /inheritance:r
icacls "<path>" /grant:r "*<your-SID>:F" "*S-1-5-18:F" "*S-1-5-32-544:F" /remove:g "*<the-OBSERVED-offending-SID-from-the-error>"
```

Substitute the exact `<path>` and observed offending SID printed by the
daemon error. Use SID literals, including the leading `*`, because they
are locale-proof; display names such as `SYSTEM` or `Administrators` can
vary by language. If the other vault file fails next, repeat the same two
commands for that file and its observed SID.

The built-in repair command is path-only: run
`mcphub repair-state-dacl --path "<path>"` for the one refused state file
named by the error. There is no directory-wide scan mode.

POSIX equivalent on Linux/macOS:

```bash
chmod 600 '<path>'
```

POSIX `chmod` does not revoke write access from a process that already
holds the file open. Before manual `chmod` or `mcphub repair-state-dacl`
on Linux/macOS, stop `mcphub` and any other process that may already hold
the file open for writing.

On Windows, `mcphub repair-state-dacl` first uses a strong, content-read-free
open (`FILE_WRITE_DATA | DELETE | WRITE_DAC | READ_CONTROL |
FILE_READ_ATTRIBUTES`, with no sharing) so a concurrent writer causes a
sharing-violation refusal and the DACL is left unchanged. If the owner's DACL has
only metadata repair rights (`WRITE_DAC` plus security-read / attributes), the
strong open returns access denied and the command refuses; apply the manual
`icacls` commands above for that exotic case.

`MCPHUB_REQUIRE_SINGLE_USER_HOME=1` additionally makes broadened
parent directories fail hard. Unsetting it can allow the default
parent-dir relax on solo-dev hosts, but it does not bypass a broadened
secret-bearing state file; repair the file DACL/mode.

If `mcphub gui` exits with the structured "Cannot acquire mcphub gui
single-instance lock" block, run `mcphub gui --force` for the
diagnostic — it prints the lock-folder path. By default `--force` is
PRINT-ONLY: it no longer auto-opens the lock folder in the file manager
(bug `work-items/bugs/2026-06-22-explorer-folder-window-orphan-flood.md`
— the one-shot CLI process cannot track/close the `explorer.exe` window
it spawns, so repeated `--force` recoveries on a host with the Windows
"Launch folder windows in a separate process" option leaked unbounded
persistent `explorer.exe` windows). Add `--reveal` to also open the
folder: `mcphub gui --force --reveal`. With `--reveal` the operator
accepts one un-reapable persistent `explorer.exe` window per invocation:
an empirical probe proved `explorer.exe /select` HANDS OFF (the launched
process exits within seconds and the surviving window is a different,
handed-off PID), so no reliable reaper exists — the print-only default is
the durable mitigation. On a host with the separate-process option set,
the GUI also emits a one-time
`severity: warn, event: explorer-separate-process-detected` entry to
`hub-mcp.log` naming the behavior.

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
8 — --reset-port refused: a client is gate-ON (hub-aggregate mode).
    Resetting the port would orphan every gated client URL. Gate OFF
    first, OR after the reset run `mcphub install --reconcile-hub-mode`.
    See "Hub aggregate (gate-ON) mode + port reset" below.
```

## Groups (/g/ namespaces)

A **group** is a named subset of MCP servers exposed at the gate-ON hub
route `/g/<group>/mcp` — the fix for tool-context bloat (point a client
at a group URL to give it only that group's tools). Groups live in
`<state-dir>/groups.yaml`, owned by the GUI **Groups** screen (or
hand-edited). Decision:
[`work-items/decisions/2026-06-18-groups-namespaces-tool-visibility.md`](work-items/decisions/2026-06-18-groups-namespaces-tool-visibility.md).

### `groups.yaml` schema (v1)

```yaml
version: 1                        # only 1 (or absent/0) is accepted; an
                                  # unknown version is a hard parse error
groups:
  - name: "frontend"             # single URL path segment: ^[A-Za-z0-9._-]+$,
                                  # no ':' (it is the "g:" scope-key separator),
                                  # not "."/"..", max 64 chars
    description: "JS/TS tools"    # optional, free-form
    servers:                      # member server names (manifest .Name);
      - "serena"                  # only LOCALLY-ROUTABLE servers (a local
      - "mcp-language-server"     # daemon ref) can be bound
    tools_hidden:                 # OPTIONAL per-server HIDE-list (deny only)
      mcp-language-server:        # key MUST be a member of `servers`
        - "delete_file"           # raw tool names to drop from the /g/ view
```

### Route shape + required headers

- Route: `http://127.0.0.1:<hubport>/g/<group>/mcp` (the hub port is the
  same listener the `/clients/` routes use; re-bound on each start).
- Scope key: `g:<group>` — a kind-namespaced subspace disjoint from the
  bare-client keys in the shared Bindings/Tokens maps (a group and a
  client of the same name never collide).
- Auth headers (same as the client routes): `X-Mcphub-Hub-Token` (the
  `g:<group>` row from the hub token table) + `X-Mcphub-Instance-Id`
  (the hub endpoint InstanceID). The GUI Groups screen surfaces the
  URL + both header values as copyable fields when the hub is live (B4);
  it shows a "start the hub" placeholder when gate-OFF.
- Gate-OFF host → inert (no listener, no `/g/` routes).
- A declared-but-EMPTY group (no members) is KNOWN and returns an empty
  `tools/list` success — NOT a 404 and NOT a `-32000`.
- A DELETED group's route returns an empty-body 404 after the snapshot
  republishes (`isKnownGroup` false) — repoint or restart the client.

### `tools_hidden` is NOT a security boundary

Hiding a tool reduces the surface EXPOSED AT THE HUB; it is not access
control. Daemon ports remain directly reachable; at gate-OFF the hub
filter is not in the path. HIDING a tool revokes existing sessions at
`tools/call` time (the call target is revalidated against the live
snapshot — PR #374, so a freshly-hidden tool returns -32601 to an
already-open session), but UN-hiding / adding a tool still takes effect
only on the session's next reconnect; granularity is per-SERVER (cannot
hide a tool on daemon A but keep it on daemon B of the same server).
Treat it as context-bloat reduction, never as a fence.

## Hub aggregate (gate-ON) mode + port reset

**What gate-ON is.** When `gui_server.hub_endpoint_enabled` is `true`
(Settings → "Expose a single aggregated hub URL"), mcphub runs a
**single aggregated hub listener** inside the GUI process. Instead of
each client config pointing at N per-daemon URLs (`http://localhost:<daemonport>/mcp`),
the gate-ON reconciler (`mcphub install --reconcile-hub-mode`) rewrites
every client's config to ONE aggregate entry named `mcphub-hub` whose
URL is `http://127.0.0.1:<hubport>/clients/<client>/mcp`. The hub then
fans each client's MCP traffic out to the per-daemon backends. The
hub's `instance_id` is the long-lived identity (persisted across
restarts in `<state-dir>/hub-mcp.endpoint.json`); the hub PORT is NOT —
it is re-bound on each start.

**The port-reset footgun (B2, guarded since this PR).** The hub port is
baked into every gate-ON client URL. `mcphub gui --reset-port` (and the
internal listener-rollback path) clear the persisted port to 0, so the
NEXT hub bind grabs a fresh OS-assigned ephemeral port. Every gated
client URL then points at the OLD port → `connection refused` for ALL
aggregated servers at once. The symptom ("connection refused")
misdirects diagnosis toward the daemons, not the config.

**The guard.** `mcphub gui --reset-port` now REFUSES (exit 8) while any
client is gate-ON — detected by reading each supported client's config
for the reserved `mcphub-hub` aggregate entry. The refusal message
names the gated clients and tells the operator to gate-OFF first OR to
re-run `mcphub install --reconcile-hub-mode` after the reset.

**If you DID reset the hub port while clients were gate-ON** (e.g. via
an older binary, or the internal rollback path fired on a reload-handler
failure): re-run `mcphub install --reconcile-hub-mode`. That rewrites
every gated client's `mcphub-hub` URL to the new bound port. A hub port
reset REQUIRES this re-reconcile whenever clients are gate-ON;
otherwise the gated URLs stay orphaned.

**Groups `/g/` routes share the hub port (C7).** A group's
`http://127.0.0.1:<hubport>/g/<group>/mcp` URL bakes in the SAME bound
hub port. A port reset orphans every `/g/` client URL too — but UNLIKE
the `mcphub-hub` client entries, NO `reconcile-hub-mode` path rewrites
group URLs into client configs (the operator copies them from the
Groups GUI by hand — B4). After a hub port reset, re-open the Groups
screen and re-copy each group's URL into the client that points at it.
The `--reset-port` exit-8 gate keys on the `mcphub-hub` client entry, so
it does NOT fire for a host that ONLY uses `/g/` group routes — reset
those at will, but re-copy the group URLs afterward.

## Hub listener hang — observability (B1, partial)

The gate-ON hub aggregate listener is a fire-and-forget goroutine inside
the GUI process. A serve-loop death (fatal accept error) already logs
`hub-listener-down` to `hub-mcp.log` and flips the live badge. A HANG
(wedged accept loop, stuck handler, deadlock) with the GUI still alive
was previously SILENT — `\mcp-local-hub-liveness` probes the supervisor
lock, not the hub listener's responsiveness, so a live GUI with a hung
listener passes the liveness probe and all aggregated MCP dies with no
automatic recovery.

**What ships now (observability only).** A self-watchdog goroutine in
the GUI periodically TCP-dials the bound hub port. When the socket is
unreachable for a bounded number of consecutive probes, it emits a
structured `severity: warn, event: hub-listener-unresponsive` entry to
`hub-mcp.log` (and `hub-listener-probe-recovered` info on recovery), so
the previously-silent failure is observable in the same log stream as
bind/lifecycle events. It does NOT auto-restart the listener.

**Deferred (full recovery).** Auto-restart of a hung listener
(ShutdownHubListener + startHubMcpListener) and handler-deadlock
detection (a full authed round-trip rather than a TCP dial) need
careful Server-lifecycle integration and are out of scope here to avoid
destabilizing the running hub. Until then, the runbook recovery for "ALL
aggregated MCP dies at once under gate-ON" is: **restart the GUI**
(close the tray/window and relaunch, or `mcphub gui --force --kill --yes`
then relaunch). Tracked in
`work-items/backlog/2026-06-16-hub-listener-hang-no-recovery.md`.

**Groups `/g/` routes ride the same listener (C7).** A hung or dead hub
listener takes the `/g/<group>/mcp` group routes down alongside the
`/clients/` routes — they share the one aggregate listener. The
`hub-listener-unresponsive` warn + the "restart the GUI" recovery above
apply identically to groups; there is no separate group-listener health
signal.

## LSP router — cold-start contract (P2c; requests await, notifications 202)

The GUI LSP router (`127.0.0.1:<guiport>/lsp/<lang>/mcp`) fans forwarded
methods (`tools/call` and other non-handshake methods) to a per-
(workspace, language) lazy-proxy daemon that materializes its heavy
backend (gopls / mcp-language-server) on first use. Handshake methods
(`initialize`, `tools/list`, `resources/list`, `prompts/list`, `ping`,
`notifications/*`) are answered SYNTHETICALLY (no backend touch) and stay
fast — so `claude mcp list` never sees a 503 from this router (its own
slowness is claude-side npx/remote-sweep cost, NOT this router; do not
re-file that as a router bug).

**Requests AWAIT (they do not 503 after delivery); notifications 202.** A
forwarded method is classified by whether the client needs its response:

- **REQUESTS** (`tools/call`, generic forwards) are AWAITED, NOT 503-ed after
  delivery. `StdioHost.SendRPC` writes the request to the backend stdin BEFORE
  awaiting a reply, so fast-failing with a retry-503 after delivery would make the
  client retry a FRESH, DUPLICATE side-effecting call (`edit_file`,
  `rename_symbol`, …). The request is awaited under `min(client-ctx,
  ColdRequestHoldCeiling)` — default **120s**, sized above the slowest cold LSP
  index (rust-analyzer / clangd / large-TS routinely exceed 60s; gopls-only
  optimism is wrong). On ceiling expiry the call WAS delivered, so the proxy
  returns a **NON-retryable controlled error**: `HTTP 500`, NO `Retry-After`,
  message `"language backend still indexing after Ns; the call was delivered and
  may have partially executed — do not auto-retry mutating calls"` — never a retry
  hint. A `lsp-cold-forward-held` warn event fires once a request holds past
  `MaterializeWaitBudget` (~15s): the fleet-visible signal for a long cold hold.
- **NOTIFICATIONS** (`textDocument/didOpen`/`didClose`) keep the round-1 contract:
  bounded by `MaterializeWaitBudget` (15s) while cold; on the budget deadline the
  notification (already written to the backend) is treated as DELIVERED → `HTTP
  202`, refcount retained, no retry. Fire-and-forget: there is no result to await.

**Pre-delivery materialize refusals are still retryable 503s.** These fire BEFORE
anything is written to the backend (spawn + handshake + singleflight dedup only),
so a retry is safe:

- **Materialize in progress** → `503`, `Retry-After: 15`, message `language
  backend cold start in progress (...); retry in ~15s`. The caller's
  `MaterializeWaitBudget` (15s) elapsed while the shared materialize keeps running
  detached; the retry joins it. The row stays `starting` — NOT `active` — until the
  first successful forwarded response, so `mcphub status` / GUI truthfully show the
  backend is not yet usable.
- **Cold-start slots busy** → `503`, `Retry-After: 30`, message `LSP cold-start
  slots busy (<n> backends warming); retry in ~30s`. At most `ColdStartConcurrency`
  (default 2) OTHER backends may be cold-starting at once; further colds are refused
  WITHOUT spawning. A retry that merely joins THIS proxy's own already-running
  materialize is exempt (spawns nothing).

**Timeout ordering (enforced).** `ColdStartMaxProbation (5m) > LSP-forward upstream
timeout (150s, DECOUPLED from serena's 60s) > ColdRequestHoldCeiling (120s) >
MaterializeWaitBudget (15s)`. `daemon.NewLazyProxy` clamps + warns a misordered
config; a gui test asserts the cross-component ordering against the router
constant. The proxy request ceiling therefore always fires BEFORE the router
upstream timeout (client sees the controlled error, never a raw router 504). The
probation>ceiling ordering bounds a SINGLE request's own hold only: a request
started at publish is severed by its own 120s ceiling long before the 5m watchdog.
The watchdog MAY still sever a LATE-ARRIVING in-flight request near the probation
boundary (started at e.g. publish+4:30, only 30s into its ceiling at the 5:00
reap) — by design, since the never-warmed backend is presumed wedged; the severed
request receives the same controlled non-retryable 500 (never a retryable-looking
error). A backend wedged past `ColdStartMaxProbation` is torn down by the
probation watchdog on the idle-reaper tick (which runs whenever idle-reaping OR
probation is configured — `--idle-backend-ttl=0` does not disable it, and neither
does disabling the cold-start gate via `ColdStartConcurrency < 0`), freeing its
slot.

**Registry lifecycle is single-owner.** The `Configured`/`Starting`/`Active`
running-state column is written by ONE authoritative reconcile
(`reconcileRegistryLifecycleLocked`, gen-guarded + shadow-idempotent, called at
every endpoint acquisition under `p.mu`), which fixed a stuck-`Starting` class
where a concurrent slot-reserve downgraded a warmed `Active` row and nothing
restored it. `Failed`/`Missing` stay owned by the teardown / fn-error paths.

**SLO.** Cold first LSP `tools/call` first-byte: p50 ≈ backend cold-index time
(gopls ~35-45s), p99 ≤ `ColdRequestHoldCeiling` (120s), beyond which a controlled
non-retryable error — never a silent hang, never a raw 504. Notifications: 15s →
202-delivered, unchanged. `claude mcp list` never sees any of this (the handshake
surface is synthetic-200). Config defaults live in `daemon.NewLazyProxy`; no
manifest/GUI knobs this round.

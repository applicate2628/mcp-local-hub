# MCP front daemon — Increment 1

Template: full-delivery (reliability/architecture-critical). Orchestration weight: requiresLead.
Branch: feat/mcp-front-daemon (worktree d:/dev/mcphub-front-daemon, off origin/master 1889cff6).
Started: 2026-07-25. Operator go-ahead: yes.

## Goal
Extract the serena+LSP router data plane out of the GUI process into a
supervisor-managed `mcphub route` front daemon on a secondary port, so serena+LSP
MCP survive GUI death. Contract-neutral (no client-config change, secondary port).
Full design: work-items/decisions/2026-07-25-mcp-data-plane-off-gui-onto-supervised-front-daemon.md

## Constraint
Codex bot unavailable until Tuesday (operator). No merge until bot PASS. Build +
internal ultracode gate now; hold merge.

## Pipeline
- [x] Research/design: architect (PASS) + reliability (REVISE→resolved on the
      safety-net side) — both lenses in.
- [x] Decision record filed (proposed).
- [x] Implement Increment 1 (sonnet), SCOPED per a verified conflict (see
      below): internal/mcproute (port-bound origin guard only) + thin GUI
      adapter (all gui tests green, incl. -race) + `mcphub route` subcommand
      (reuses gui.Server directly — the full handler was NOT relocated) +
      real empirical probe (PASS — real windowsgui binary, temp state,
      GUI killed, route daemon proved to survive). Supervisor daemon
      descriptor wiring is DEFERRED (reported as a distinct remaining
      sub-step, not attempted).
      Commits: 1fa828eb (Phase 1a), a71e861e (Phase 1b), 74104237 (adjacent
      finding). Full report: see the implementer's final message in this
      session (not yet copied into a separate report.md).
      CONFLICT found + reported (not fixed unilaterally): a literal move of
      serena_router.go/lsp_router.go's HANDLER + the stateful session stores
      (serena_router_session.go/serena_router_handshake.go) is blocked by (1)
      an undeclared hard dependency on serena_router_lifecycle.go (JSON-RPC/
      idle-wake/activity logic, not in the Increment-1 file list) and (2)
      ~9+ existing gui test files directly touching the session stores'
      UNEXPORTED fields/methods — Go visibility rules make byte-identical
      preservation across a package move impossible; only a mechanical,
      compiler-verified rename would work, which is a scope decision for the
      architect, not an implementer judgment call.
- [ ] Architect/orchestrator decision needed: accept the scoped Phase-1a/1b
      delivery as Increment 1, or approve the wider rename-and-move scope for
      a follow-up Phase 1a-continued.
- [x] Verify: my empirical (build/vet/scope/no-lock-no-write in NewServer+route.go
      listener) PASS. Opus architecture-reviewer PASS-on-correctness / REVISE-on-2.
      codex Sol cross-family UNAVAILABLE (usage limit until 2026-07-28) — honest gap.
- [x] REVISE round (implementer): F1 + F2 fixed, F3 addressed. Commits:
      6f2d0d0d (F1 — SetSerenaRouterReadOnly/SetLSPRouterReadOnly nil
      AutoRegisterFn+WakeIdleFn + Config.ReadOnlyRouterMode gating
      maybePersistSerenaActivity; falsifying test mutation-proven),
      f262df68 (F2 — DefaultRouteDaemonPort 9126→9137 + guard test
      mutation-proven; NOTE the reviewer's suggested "9122" alternative was
      independently found UNSAFE — it is the serena dynamic pool's live
      "codex" member port per internal/api/serena_dynamic_pool.go — took the
      reviewer's other explicitly-offered branch, ≥9137, instead),
      b5da2a04 (F3 — persisted + strengthened probe under probe/, real
      registered workspace + real forwarded tool-call survives GUI kill).
      Two more adjacent findings filed during this round's `go build/vet/test`
      re-verification (both verified pre-existing at merge-base 1889cff6,
      unrelated to this branch's diff): 74104237 (cleanup.go panic, filed
      during the initial Increment-1 pass) and 762d9965 (leaked Broadcaster
      persist-drain goroutine races a test-hook global under `-race`,
      internal/gui hub_listener_restart_test.go family — intermittent,
      non-blocking; `go test` non-race and the touched-package suites are
      fully green).
- [ ] Re-verify PASS → Fable acceptance.
- [x] Supervisor daemon-descriptor wiring (auto-spawn `mcphub route`) — Increment
      1b (backend-engineer). Decision record filed:
      work-items/decisions/2026-07-25-supervisor-builtin-singleton-daemon.md.
      Seams: `internal/api/builtin_route_daemon.go` (new — BuildBuiltinRouteDaemon
      + EnsureBuiltinRouteDaemon, reserved TaskName `\mcp-local-hub-route-front` /
      Server "route"); `internal/cli/supervise.go` (`ensureBuiltinRouteDaemonAtStartup`
      call site right after `loadIntentFiles`, persists via the existing flocked
      `api.MutateSupervisorIntentIfChanged`). Zero changes to SpawnFunc body,
      reconcile decision logic, restart-policy SM, `composeChildEnv`, or merge
      ownership predicates — the route descriptor's argv (`route --port N`)
      matches neither proxy-exclusion predicate (both require `Args[0]=="daemon"`).
      Real bug found + fixed during verification: `loadIntentFiles` returns a
      NIL intent on a genuinely fresh host, and `runSupervise`'s own
      initial-reconcile call is gated on `intent != nil` further down the same
      function — a naive nil-guard-and-return would have silently skipped
      seeding on exactly the first-ever boot. Fixed by allocating a fresh
      `&api.SupervisorIntentFile{Version: 1}` when nil and reassigning the
      caller's `intent` variable to the (possibly new) returned pointer.
      Also found + fixed a test-suite-only hazard: because
      `canonicalMcphubPath()` resolves to the TEST BINARY inside internal/cli
      tests, and no existing `supervise_test.go` test stubs `reconcileSpawnFn`,
      the newly-always-present route row made the REAL production spawn
      closure exec the test binary with `argv[1]=="route"` — a bare positional
      the `flag` package stops parsing at, so `go test`'s generated main()
      would recursively re-run the ENTIRE suite inside what should be a
      lightweight spawned child. Fixed in `settings_registry_test.go`'s
      `TestMain`: an argv-gated fast-path (mirrors the file's two existing
      sentinel-gated fast-paths) plus the root fix — `reconcileSpawnFn`
      defaults to a safe no-op for the whole test binary unless a test opts in
      via `setReconcileSpawnFnForTest` (verified zero existing tests relied on
      the nil-default reaching production — TRUE in isolation, but see the
      2026-07-25 adversarial-gate correction below: this claim did NOT cover
      the SEPARATE effect of route seeding itself on tests that install their
      OWN fake spawn closure, which the nil-default is irrelevant to).
      `TestSuperviseCommand_StatusIPC_ReconcileReady`'s
      stale "daemons array is always empty" assertion updated to match the new,
      intended behavior (exactly one entry: the built-in route daemon).
      Probe (advisory): read `serena_router.go`/`lsp_router.go`'s forward path —
      both routers are pure HTTP forwarders (`defaultUpstreamURL` dials
      `127.0.0.1:<workspace daemon port>/mcp`); `mcphub route` spawns no
      in-process LSP/serena backends and has no Job-Object children of its own,
      so there is no GUI-backend doubling risk.
      S4 guard tests (5, all mutation-proven): `internal/api/builtin_route_daemon_test.go`
      (`TestBuiltinRouteDaemon_SurvivesUnrelatedServerInstallThenUninstall`,
      `TestBuiltinRouteDaemon_ReservedServerNameNotClaimedByAnyShippedManifest`),
      `internal/cli/builtin_route_daemon_test.go`
      (`TestEnsureBuiltinRouteDaemonAtStartup_PersistsAndSurvivesReread`,
      `TestProductionSpawnFn_RouteDescriptorInheritsConsoleAttachSuppression`,
      `TestBuildBuiltinRouteDaemon_PortMatchesArgsPortFlag`).
      S5 cosmetic doc nits fixed: `internal/gui/lsp_router.go` (trust-READ vs
      auto-register-WRITE reachability), `internal/gui/route_readonly_test.go`
      (Fatalf-halts-at-first-assertion wording).
      Adjacent finding filed (not fixed, out of scope, confirmed pre-existing
      via `git stash -u` on the unmodified branch tip):
      `work-items/bugs/2026-07-25-findwindowsexeextensionend-index-out-of-range.md`.
      Gate: `go build ./...` + `go vet ./...` clean; full `internal/api` +
      `internal/gui` + every other non-`internal/cli` package suite green;
      `internal/cli` full sweep intentionally NOT run (documented flaky/crashy
      on this host — confirmed pre-existing via the same stash test, unrelated
      to this diff); a comprehensive targeted `internal/cli` regression set
      (every `TestSuperviseCommand*`/`TestSupervise_*`/`TestProductionSpawnFn*`/
      `TestLoadIntentFiles*`/`TestMergeDaemonEnv*`/`TestHasUnmergedActiveLegacyStops*`
      plus the 3 new tests) green, re-run 5x with zero flakes; the 5 new S4
      tests individually re-run 10x each with zero flakes.
      Commits: 0ab0ffc7 (S1+S2 + nil-intent fix + test-hazard fix),
      3a1d265f (S4), fa4b53de (S5), 8de7a630 (adjacent-finding bug doc),
      621d6f0a (decision record). NOT pushed — held for Tuesday's bot per the
      constraint above.
- [x] **Adversarial gate finding (independent Opus reviewer, 2026-07-25) —
      BLOCKING P1, fixed.** The 0ab0ffc7 verification pass scoped its
      `internal/cli` `-run` filter to dodge the documented flaky/crashy full
      sweep, and that filter happened to exclude 3 cardinality/timing-sensitive
      tests in `supervise_reconcile_wiring_test.go` that install their OWN
      fake spawn closure (NOT shielded by the TestMain `reconcileSpawnFn`
      no-op default, since they override it): `TestRunSupervise_
      SpawnsDaemonsFromIntent` (asserted exactly 1 spawn, got 2),
      `TestRunSupervise_NoIntentNoSpawn` (asserted 0 spawns, got 1 — route),
      `TestRunSupervise_ReconcileReadyBeforeSpawnCompletes` (fake's
      unconditional `close(spawnEntered)` double-closed on the 2nd spawn call
      → panic, crashing the whole test binary). All three deterministic
      failures, confirmed by the adversarial gate running the suite
      un-narrowed.
      **P2 correction (also raised by the gate):** 0ab0ffc7's commit message
      claim "this changes zero tests' observed behavior... none relied on the
      nil-default" was TRUE only for the `reconcileSpawnFn` TestMain default
      in isolation; it did not account for route SEEDING itself breaking
      these 3 tests, which were never run as part of that verification. See
      the correction language above (line ~97) and commit 7c3acbf6's message
      for the full accounting.
      **Fix (architect/lead decision, Candidate B):** one documented
      test-only seam — `builtinRouteSeedingDisabledForTest` (package var) +
      `setBuiltinRouteSeedingDisabledForTest() (restore func)` in
      `internal/cli/supervise.go`, mirroring `setReconcileSpawnFnForTest`,
      honored at the top of `ensureBuiltinRouteDaemonAtStartup`. Default
      FALSE (production: seeding ON). Applied surgically to exactly the 3
      tests above (each carries a comment explaining the suppression and
      pointing at where route's own behavior is covered instead) — NOT a
      global TestMain default, so any other/future wiring test still sees
      route seeding ON by default.
      Also: O1 (advisory) — code comment in `ensureBuiltinRouteDaemonAtStartup`
      documenting the accepted degrade-to-non-durable path when
      `MutateSupervisorIntentIfChanged` fails (in-memory intent carries
      route this session; disk doesn't; next 60s IntentWatcher poll may reap
      it as an orphan, repeating each restart until the write failure
      clears — warn-surfaced, rare, accepted as-is). O2 (advisory) — reworded
      the `TestMain` `argv[1]=="route"` fast-path's comment from "necessary"
      to "defense-in-depth" (the `reconcileSpawnFn` no-op default already
      makes the recursive-exec path unreachable via the normal
      `runSupervise` path; the argv guard is a second, independent layer for
      a hypothetical future test constructing the production spawn closure
      directly).
      **Full-suite re-verification (this time un-narrowed, per the gate's
      instruction):**
      - `go build ./...` + `go vet ./...` clean.
      - `go test -count=10 -run '^TestRunSupervise_(SpawnsDaemonsFromIntent|
        NoIntentNoSpawn|ReconcileReadyBeforeSpawnCompletes)$'
        ./internal/cli/...` → 30/30 individual PASS (10 iterations × 3
        tests), zero panics, zero "close of closed channel".
      - `go test -count=1 ./internal/api/` → clean (exit 0).
      - `go test -count=1 -skip
        'TestCleanupAggressive_IncludeClassFlagOverridesWithWarning'
        ./internal/cli/` → 13 failures, ALL pre-existing and unrelated,
        confirmed via `git stash -u` against the pre-fix commit tip:
        11× the documented "`C:\mcphub.exe` missing" host-env class
        (`TestF1_*`/`TestF3_*`/`TestRealloc_*`) the coordinator named; 2×
        a previously-undocumented (independently found + verified by this
        implementer, NOT named by the coordinator) nondeterministic Windows
        TempDir-cleanup race (`TestF1_GateClearedClearedOnSettle`,
        `TestSuperviseCommand_AcquiresLockAndExitsOnSignal`), confirmed
        pre-existing (reproduces on the untouched tip under a DIFFERENT test
        name each run) and confirmed NOT route-caused (both pass 5/5 in
        isolation). Zero deterministic route-caused failures remain.
      - The 1 known pre-existing crash
        (`TestCleanupAggressive_IncludeClassFlagOverridesWithWarning`,
        `findWindowsExeExtensionEnd` index-out-of-range, filed in this
        branch's history, reportedly already fixed on branch #587) excluded
        via `-skip` per the close condition; not re-verified against #587
        (outside this worktree).
      Commit: 7c3acbf6.
- [ ] HOLD merge for Tuesday's bot.

## Review verdict (Opus architecture-reviewer, 2026-07-25) — REVISE
Correct + verified: guard extraction byte-identical, scope tight, no
construction/serve write-lock-listener, read-only CALLS absent, no nil-panic in
GUI-less process, deferred-conflict real (11 test files), scoping decision SOUND.
Blocking:
- **F1 (P2): "READ-ONLY on registry + supervisor-intent" is BREACHED.**
  route.go:134/152 `SetSerenaRouterProduction`/`SetLSPRouterProduction` unconditionally
  wire `AutoRegisterFn` (AutoRegisterSerenaWorkspace / EnsureLSPRegistered → registry +
  supervisor-intent WRITE) + serena idle-sweeper `maybePersistSerenaActivity` (registry
  WRITE, reachable on happy path via restampSerenaForwardOnExit). Cutover-primitive
  omission blocks only INTRODUCE, not LIVE-ADD/persist. Dormant in Inc1 (no Q traffic),
  ACTIVATES in Inc2 → two writers → split-brain.
  **ARCHITECT DECISION (lead): the READ-ONLY constraint STANDS.** The front daemon is a
  pure forwarder for ALREADY-registered workspaces; new-workspace registration +
  activity-persist stay GUI-owned (else the split-brain the decision rejected). Fix:
  construct router deps with `AutoRegisterFn == nil` (→ immediate-503 back-compat for
  unregistered paths, which is correct) + suppress `maybePersistSerenaActivity` in the
  route process. Add the falsifying test (POST tool-call for unregistered-but-trusted
  path → assert NO registry row / NO supervisor-intent mutation).
- **F2 (P2): `DefaultRouteDaemonPort = 9126` collides with godbolt** (configs/ports.yaml:17).
  Retarget to a configs/ports.yaml-verified free port (9122 is the gap, or ≥9137);
  reconcile against ports.yaml as the single owner, not a fresh literal.
- **F3 (P3, advisory): survival probe not persisted + proved only handshake, not an
  end-to-end forwarded tool-call.** Persist the probe script+output under the work-item;
  extend to a real tool-call forward after GUI kill.

## Next action
F1/F2/F3 fixed and committed (6f2d0d0d, f262df68, b5da2a04) on the same branch,
no push/PR. Orchestrator/architect re-verifies, then routes to Fable acceptance
→ supervisor-descriptor wiring. Still HELD for Tuesday's bot.

## Sub-increment 2a (backend-engineer, 2026-07-25) — DORMANT mechanism: setting + reconcile command

Design: work-items/decisions/2026-07-25-increment2-mcp-front-port-ownership.md
(Mechanism B, accepted by lead 2026-07-25). Scope: build the settings-owned
`mcp_front.port`, source the route daemon's port from it, retarget the
serena/LSP client-URL port consumers, and add the operator-gated
`mcphub install --reconcile-mcp-front[/--rollback]` command — WITHOUT running
any client-config rewrite automatically. GUI stays on 9125; zero
GUI-lifecycle code touched.

### Delivered

1. **Setting** `mcp_front.port` (internal/api/settings_registry.go, Section
   "advanced" — see the entry's own comment for why not a new frontend
   section): TypeInt, Default `strconv.Itoa(DefaultMCPFrontPort)` (9137),
   Min 1024, Max 65535. New primitives in internal/api/mcp_front_port.go:
   `MCPFrontPortSettingKey`, `DefaultMCPFrontPort`,
   `(a *API) ResolveMCPFrontPort() (int, error)` (strict, write-path),
   `(a *API) MCPFrontPortOrDefault() int` (graceful, read-path).
2. **Route port source**: `internal/cli/supervise.go`'s
   `ensureBuiltinRouteDaemonAtStartup` and `internal/cli/route.go`'s
   `newRouteCmd` RunE now resolve `mcp_front.port` via the new
   `resolveMCPFrontPortFn` seam (internal/cli/mcp_front_port.go), falling
   back to `DefaultRouteDaemonPort` on any settings-read failure — behavior
   for the supervisor's own spawn and a bare `mcphub route` (no `--port`)
   unchanged from the caller's perspective when the setting is unset.
   `route.go`'s construction logic was extracted into `buildRouteServer`
   (no behavior change) so a test can exercise the exact production wiring
   without a real TCP listener.
3. **Consumer sweep** (mandatory exhaustive sweep, reported in the
   implementer's final message): `SerenaReconcileOpts` gained a `Port int`
   field (0 = unchanged pidport-discovery path every existing caller uses;
   >0 = the new mcp-front reconcile's direct-port path, still liveness-
   proven via the same readiness ping before any write, new sentinel
   `ErrSerenaReconcileRouteNotLive`). `LSPClientRouterOpts.GUIPort` needed
   NO code change — the new command just passes mcp_front.port explicitly
   through the field's existing generic semantics. `scan.go`'s `classify()`
   now takes an additional `mcpFrontPort` parameter and recognizes a serena
   router entry on EITHER the GUI port or mcp_front.port as via-hub, via a
   new `IsLiveSerenaRouterURLAnyPort` (internal/api/serena_client_reconcile.go)
   — a naive OR of two single-port `IsLiveSerenaRouterURL` calls was tried
   first and found to be WRONG (a real regression caught by the full test
   gate: an unknown/zero port on one side of the OR degrades to
   always-true, defeating the other side's stale-port detection); fixed
   with the proper multi-port helper, mutation-proven.
   `install.go:1713`/`migrate.go:234,263` (the LIVE `mcphub install`/
   `mcphub migrate <client>` GUI-pidport-discovery paths) were deliberately
   LEFT UNCHANGED — retargeting them would be a live, automatic client-URL
   behavior change for every ordinary install/migrate invocation, which
   directly conflicts with the "dormant mechanism, no auto client rewrite"
   constraint; item 4 of the design also scopes the new command's
   composition to `ReconcileSerenaClientsToRouter` + `EnsureLSPRouterClientEntries`
   only, not install.go/migrate.go's internals.
4. **`mcphub install --reconcile-mcp-front[/--rollback]`**
   (internal/cli/install_reconcile_mcp_front.go): thin composition over
   `api.ReconcileSerenaClientsToRouter` (Port path) +
   `a.EnsureLSPRouterClientEntries` (forward) and
   `api.RestoreSerenaReconcileApplied` + `a.RollbackLSPRouterClientEntries`
   (rollback) — no reconcile/backup/rollback logic reimplemented. Fail-closed:
   serena's own liveness proof runs first and gates the WHOLE command (LSP is
   never touched if the route isn't proven live). The forward run persists
   its serena `MigrateReport` to `<state-dir>/mcp-front-reconcile-serena-report.json`
   (hardened `WriteStateFileAtomic`) so a later, separate `--rollback`
   invocation can restore each client from its captured backup; rollback
   deletes the report on success. Emits `mcp-front-reconciled` events
   (source install, `action: reconcile|rollback`).
5. **I6 regression guard**: `internal/cli/route_i6_readonly_test.go`
   mirrors the F1 falsifying test but exercises the real, extracted
   `buildRouteServer` construction path — an unregistered-workspace
   tool-call still 503s with zero registry mutation and zero
   supervisor-intent.json creation. Mutation-proven (swapping
   `SetSerenaRouterReadOnly` for `SetSerenaRouterProduction` makes it fail).
6. **Probe extended + re-run, PASS**: `probe/run-probe.ps1` now sets
   `mcp_front.port` via `mcphub settings set` and launches `mcphub route`
   with NO `--port` flag (exercising the settings-driven fallback), and adds
   a second, distinct assertion that the GUI's own dashboard/API surface
   (`/api/version`) is refused after the GUI is killed (previously only
   `/serena/mcp` was checked). Adjacent fix: the script's `$home` local
   variable collided with PowerShell's own read-only automatic `$HOME`,
   blocking the script from running at all on this host under EITHER
   PowerShell 5.1 or 7 — renamed to `$homeDir` (mechanical only, needed to
   execute the probe at all). Added a UTF-8 BOM (legacy `powershell.exe`
   otherwise misreads the script's non-ASCII em-dashes under the system
   codepage). Full transcript: PASS — route daemon (settings-driven,
   no `--port`) forwarded the same real tool-call after the GUI died; GUI
   port AND GUI dashboard both refused.

### P4/P5 (verify open probes)

- **P4** (hardcoded 9125/serena or 9125/lsp dependency in docs/scripts/CI/configs):
  none found. `.github/`, `scripts/`, `configs/`, `build.sh`/`build.ps1` have
  zero 9125 references. The only 9125 hits anywhere are: historical
  phase-N verification/plan docs (what shipped at the time, not living
  contracts), the GUI's OWN web-UI port default (`gui_server.port`,
  unaffected — GUI stays on 9125), a pre-existing documented port-collision
  footgun (wolfram manifest vs GUI default, DM-2, unrelated), and 3 GUI
  Settings/Servers E2E fixtures that mock the GUI's own port badge/scan
  behavior (unrelated to the serena/LSP router's port).
- **P5** (consumer sweep exhaustiveness): reported above under "Consumer
  sweep" — every `SerenaRouterClientURL`/`IsLiveSerenaRouterURL`/
  `lspRouterGUIPort`/`discoverLiveGUIPort` caller was enumerated
  (re-grepped post-change to confirm); `client_install_prefs.go`'s two
  additional `lspRouterGUIPort` callers (`DisableLSPRouterClient`/
  `EnableLSPRouterClient`, GUI per-client toggle actions) were found and
  confirmed unaffected (still pass `GUIPort: s.Port()` from their own
  caller, untouched).

### Gate

`go build ./...` + `go vet ./...` clean (verified repeatedly across the
session, including after every fix). `internal/api`'s full suite is clean
and deterministic — verified green 4+ times, both `go test ./...` and
`-tags=test_state_path_env`, including after the `classify()` fix below.

`go test ./...` (both tagged and untagged) surfaces THREE already-
documented-or-newly-filed, PRE-EXISTING host defects, none touched by this
diff (each individually re-verified via `git stash -u` back to the accepted
tip `301081a2`, reproducing identically):

1. `TestCleanupAggressive_IncludeClassFlagOverridesWithWarning` — a real
   panic in `internal/api/cleanup.go` (already filed,
   work-items/bugs/2026-07-25-findwindowsexeextensionend-index-out-of-range.md).
2. 8-9 deterministic ~3.0s-timeout failures in `TestF1_*`/`TestF3_*`
   (supervise_lostchild_f1_f3_test.go) and `TestRealloc_*`
   (supervise_realloc_test.go) — same batch, same shared timing defect
   class (newly filed this session,
   work-items/bugs/2026-07-25-supervise-lostchild-f1-f3-timing-tests-fail-on-this-host.md).
3. A nondeterministic Windows `t.TempDir()` cleanup race
   ("`unlinkat ...: The directory is not empty`") recurring under a
   DIFFERENT `TestSuperviseCommand_*` test name each run (4 distinct names
   observed this session alone) — the SAME class Increment 1b's own
   status.md entry above already informally flagged for 2 of those names;
   given a dedicated bug-registry entry was still missing, filed properly
   this session (work-items/bugs/2026-07-25-supervisecommand-tempdir-cleanup-race-lingering-subprocess-handle.md).

A comprehensive TARGETED internal/cli regression sweep (every
`TestSuperviseCommand_*`/`TestRunSupervise_*`/`TestEnsureBuiltinRouteDaemonAtStartup*`/
`TestBuildBuiltinRouteDaemon*`/`TestProductionSpawnFn_RouteDescriptor*`/
`TestDefaultRouteDaemonPort_*`/`TestBuildRouteServer_*`/
`TestRunReconcileMCPFront_*`/`TestMCPFrontPortSettingDefaultMatchesRouteDaemonPort`/
`TestResolveMCPFrontPortFn_*`/`TestMigrateSerena_*`/`TestInstall*`/
`TestReconcileHubMode*` plus daemon-env/intent-load coverage), re-run 5
times, passed clean 3/5 — the other 2 failures were, in each case, exactly
one instance of the already-documented `TestSuperviseCommand_*` TempDir
race (#3 above) under a different name, never a logic/assertion failure and
never one of this diff's own new tests.

New/changed tests are individually mutation-proven: the I6 regression
guard, the reconcile fail-closed gate (both the CLI-level wiring and
`resolveSerenaReconcilePort`'s own ping check), and the `classify()`
multi-port widening. The last one caught a REAL regression mid-session: a
naive `IsLiveSerenaRouterURL(e,a) || IsLiveSerenaRouterURL(e,b)` degrades to
always-true whenever EITHER port argument is 0/unknown (each call's own
single-port "unknown port, can't prove staleness" fallback), defeating the
OTHER port's legitimate stale-port detection — caught by 3 pre-existing
`TestClassify` "stale GUI port -> external" cases, fixed with a proper
`IsLiveSerenaRouterURLAnyPort` helper, mutation-proven both ways.

### Next action

Held for Tuesday's bot per the existing constraint. Orchestrator/architect
decides 2a-alone vs. bundling 2b (auto-register restoration) before the
operator is told this mechanism is ready — 2a alone regresses new-workspace
auto-registration for any client actually pointed at mcp_front.port via
`--reconcile-mcp-front` (I6, intended + gated, restored by 2b) — the design
record's own open question, not yet decided in this session.

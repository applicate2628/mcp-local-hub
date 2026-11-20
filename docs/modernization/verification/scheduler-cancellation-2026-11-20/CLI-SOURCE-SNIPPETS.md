# Точные фрагменты CLI и изоляции

Источник всех фрагментов: `applicate2628/mcp-local-hub@f5fb00370a833255eacb992a7cd32f2e68da7399`. Получены чтением GitHub; ниже указан исходный диапазон строк, содержимое внутри блоков не переписано. Код не исполнялся. Интерпретация и ограничения — в `CONTRACT-REVIEW.md`.

## Оглавление

1. [Публичный потребитель и классификация отмены](#consumer)
2. [Тестовая fixture и два адресных теста](#tests)
3. [CLI TestMain](#testmain)
4. [Защищённая тестовая подмена state root](#testhook)
5. [Обычный resolver](#prod-resolver)
6. [Resolver с тестовым тегом](#tagged-resolver)
7. [Изоляция конфигурационных каталогов](#client-sandbox)
8. [Выбор пути registry](#registry)

<a id="consumer"></a>

## internal/cli/supervise_reconcile_ipc.go — строки 56–69

[Исходник](https://github.com/applicate2628/mcp-local-hub/blob/f5fb00370a833255eacb992a7cd32f2e68da7399/internal/cli/supervise_reconcile_ipc.go#L56-L69)

```go
func reconcileSchedulerListFnDefault(ctx context.Context) ([]scheduler.TaskStatus, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	sch, err := reconcileSchedulerNewFn()
	if err != nil {
		return nil, fmt.Errorf("scheduler.New: %w", err)
	}
	if lister, ok := sch.(scheduler.ContextLister); ok {
		tasks, err := lister.ListContext(ctx, "mcp-local-hub-")
		if err != nil {
			return nil, fmt.Errorf("scheduler.List: %w", err)
		}
		return tasks, nil
```

## internal/cli/supervise_reconcile_ipc.go — строки 179–183

[Исходник](https://github.com/applicate2628/mcp-local-hub/blob/f5fb00370a833255eacb992a7cd32f2e68da7399/internal/cli/supervise_reconcile_ipc.go#L179-L183)

```go
	writeTimeout := func(cause error) error {
		if cause == nil {
			cause = context.DeadlineExceeded
		}
		return writeFrame(api.IPCResponse{ID: req.ID, Error: &api.IPCErr{Code: "RECONCILE_TIMEOUT", Message: cause.Error(), Retryable: true}, Final: true}, cause)
```

## internal/cli/supervise_reconcile_ipc.go — строки 365–383

[Исходник](https://github.com/applicate2628/mcp-local-hub/blob/f5fb00370a833255eacb992a7cd32f2e68da7399/internal/cli/supervise_reconcile_ipc.go#L365-L383)

```go
	// (3) Scheduler snapshot.
	schedTasks, schedErr := reconcileSchedulerListFn(ctx)
	if errors.Is(schedErr, context.DeadlineExceeded) || errors.Is(schedErr, context.Canceled) {
		return writeTimeout(schedErr)
	}
	if schedErr != nil {
		if api.ReconcileSchedulerUnavailableError(schedErr) {
			schedTasks = nil
		} else {
			return writeFrame(api.IPCResponse{
				ID: req.ID,
				Error: &api.IPCErr{
					Code:    "RECONCILE_SCHEDULER_LIST_FAILED",
					Message: schedErr.Error(),
				},
				Final: true,
			}, schedErr)
		}
	}
```

<a id="tests"></a>

## internal/cli/supervise_reconcile_ipc_test.go — строки 183–211

[Исходник](https://github.com/applicate2628/mcp-local-hub/blob/f5fb00370a833255eacb992a7cd32f2e68da7399/internal/cli/supervise_reconcile_ipc_test.go#L183-L211)

```go
func newReconcileTestFixture(t *testing.T, intent *api.SupervisorIntentFile) *reconcileTestFixture {
	t.Helper()
	tmpHome := apitest.HardenedTempDir(t)
	// handleReconcile's apply-mode branch now calls
	// RepairSerenaIntentFromRegistry(deps.stateDir), which resolves the
	// WORKSPACE REGISTRY through api.DefaultRegistryPath() — a resolver
	// that is completely independent of deps.stateDir and instead reads the
	// LOCALAPPDATA (Windows) / XDG_STATE_HOME (POSIX) env vars directly.
	// Without this redirect, EVERY apply-mode test using this fixture would
	// silently touch the real developer's live workspaces.yaml (mkdir +
	// lock-file create at minimum) the first time it runs on this machine —
	// exactly the live-state-touch this fix must never cause. Point both env
	// vars at the SAME tmpHome the fixture already uses for
	// supervisor-intent.json so both resolvers agree.
	t.Setenv("LOCALAPPDATA", tmpHome)
	t.Setenv("XDG_STATE_HOME", tmpHome)
	intentPath := filepath.Join(tmpHome, "supervisor-intent.json")
	if intent == nil {
		intent = &api.SupervisorIntentFile{Version: 1}
	}
	if err := api.WriteSupervisorIntent(intentPath, intent); err != nil {
		t.Fatalf("seed supervisor-intent.json: %v", err)
	}

	events, err := api.OpenSupervisorEventLog(filepath.Join(tmpHome, "supervisor-events.log"))
	if err != nil {
		t.Fatalf("OpenSupervisorEventLog: %v", err)
	}
	t.Cleanup(func() { events.Close() })
```

## internal/cli/supervise_reconcile_ipc_test.go — строки 228–301

[Исходник](https://github.com/applicate2628/mcp-local-hub/blob/f5fb00370a833255eacb992a7cd32f2e68da7399/internal/cli/supervise_reconcile_ipc_test.go#L228-L301)

```go
	var ctrl *supervisorController
	loop := api.NewEventLoop(32)
	postedCh := make(chan api.LoopEvent, 64)
	var postedCount atomic.Int32
	loop.RegisterHandler(func(ev api.LoopEvent) {
		switch ev.Kind {
		case evReapScan:
			prev, _ := ev.Body[reapScanPreviousNamesBodyKey].(map[string]struct{})
			updated, _ := ev.Body[reapScanIntentBodyKey].(*api.SupervisorIntentFile)
			stops, _ := ev.Body[reapScanStopsBodyKey].(*api.DaemonIntentFile)
			ctrl.handleReapScan(prev, updated, stops)
			if doneCh, ok := ev.Body[reapScanDoneBodyKey].(chan struct{}); ok && doneCh != nil {
				close(doneCh)
			}
			return
		case evReapFollowup:
			generation, _ := ev.Body[reapFollowupGenerationBodyKey].(int)
			ctrl.handleReapFollowup(ev.TaskName, generation)
			return
		case evControllerBarrier:
			if done, ok := ev.Body[controllerBarrierResultBodyKey].(chan struct{}); ok {
				close(done)
			}
			return
		}
		postedCount.Add(1)
		select {
		case postedCh <- ev:
		default:
		}
	})
	ctx, cancel := context.WithCancel(context.Background())
	loopDone := make(chan struct{})
	go func() {
		defer close(loopDone)
		loop.Run(ctx)
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case <-loopDone:
		case <-time.After(2 * time.Second):
			t.Error("reconcile fixture event loop did not settle")
		}
	})

	ctrl = &supervisorController{
		intentCache:         newIntentCache(),
		eventLoop:           loop,
		events:              events,
		daemonIntent:        newDaemonIntentCache(),
		tracker:             NewDaemonRuntimeTracker(),
		statePath:           filepath.Join(tmpHome, "supervisor-state.json"),
		ctx:                 ctx,
		failureWindow:       respawnFailureWindow,
		quarantineThreshold: respawnQuarantineThreshold,
	}
	ctrl.intentCache.Refresh(intent)

	deps := ipcDispatchDeps{
		stateDir:           tmpHome,
		events:             events,
		runtimeTracker:     ctrl.tracker,
		reconcileReady:     &reconcileReady,
		controllerProvider: func() *supervisorController { return ctrl },
	}
	return &reconcileTestFixture{
		deps:        deps,
		ctrl:        ctrl,
		loop:        loop,
		postedCh:    postedCh,
		postedCount: &postedCount,
	}
}
```

## internal/cli/supervise_reconcile_ipc_test.go — строки 1172–1282

[Исходник](https://github.com/applicate2628/mcp-local-hub/blob/f5fb00370a833255eacb992a7cd32f2e68da7399/internal/cli/supervise_reconcile_ipc_test.go#L1172-L1282)

```go
// TestReconcileIPC_HandlerTimeoutCancelsSchedulerList verifies the production
// scheduler-list path hands the reconcile context to a context-aware scheduler
// implementation instead of abandoning an uncancellable goroutine after the
// handler deadline fires.
func TestReconcileIPC_HandlerTimeoutCancelsSchedulerList(t *testing.T) {
	taskName := `\mcp-local-hub-foo-default`
	intent := &api.SupervisorIntentFile{
		Version: 1,
		Daemons: []api.SupervisorDaemon{
			{TaskName: taskName, Server: "foo", Daemon: "default"},
		},
	}
	fx := newReconcileTestFixture(t, intent)

	origTimeout := reconcileHandlerTimeout
	reconcileHandlerTimeout = 25 * time.Millisecond
	t.Cleanup(func() { reconcileHandlerTimeout = origTimeout })

	ctxCanceled := make(chan error, 1)
	fakeScheduler := &reconcileContextSchedulerForTest{
		listContext: func(ctx context.Context, prefix string) ([]scheduler.TaskStatus, error) {
			if prefix != "mcp-local-hub-" {
				t.Errorf("scheduler prefix = %q, want mcp-local-hub-", prefix)
			}
			<-ctx.Done()
			ctxCanceled <- ctx.Err()
			return nil, ctx.Err()
		},
	}
	uninstall := setReconcileSchedulerNewFnForTest(func() (scheduler.Scheduler, error) {
		return fakeScheduler, nil
	})
	defer uninstall()

	req := api.IPCRequest{
		ID:   25,
		Cmd:  "reconcile",
		Args: map[string]any{"apply": false},
	}
	conn := newFakeIPCConn()
	if err := handleReconcile(conn, req, fx.deps); err != nil {
		t.Fatalf("handleReconcile: %v", err)
	}
	resp := conn.lastResponse(t)
	if resp.Error == nil {
		t.Fatalf("expected timeout response, got %+v", resp)
	}
	if resp.Error.Code != "RECONCILE_TIMEOUT" {
		t.Fatalf("response code = %q, want RECONCILE_TIMEOUT", resp.Error.Code)
	}

	select {
	case err := <-ctxCanceled:
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("scheduler ListContext saw ctx err %v, want DeadlineExceeded", err)
		}
	case <-time.After(1 * time.Second):
		t.Fatal("scheduler ListContext did not observe reconcile context cancellation")
	}
}

// TestReconcileIPC_HandlerTimeout verifies that handleReconcile creates the
// handler-side 25-second deadline and propagates it to long-running scheduler
// work through the reconcile scheduler seam.
func TestReconcileIPC_HandlerTimeout(t *testing.T) {
	if reconcileHandlerTimeout != 25*time.Second {
		t.Fatalf("reconcileHandlerTimeout = %s, want 25s", reconcileHandlerTimeout)
	}
	taskName := `\mcp-local-hub-foo-default`
	intent := &api.SupervisorIntentFile{
		Version: 1,
		Daemons: []api.SupervisorDaemon{
			{TaskName: taskName, Server: "foo", Daemon: "default"},
		},
	}
	fx := newReconcileTestFixture(t, intent)

	deadlineDelta := make(chan time.Duration, 1)
	uninstall := setReconcileSchedulerListFnForTest(func(ctx context.Context) ([]scheduler.TaskStatus, error) {
		deadline, ok := ctx.Deadline()
		if !ok {
			t.Fatal("scheduler list context had no deadline")
		}
		deadlineDelta <- time.Until(deadline)
		return []scheduler.TaskStatus{{Name: taskName, State: "Ready"}}, nil
	})
	defer uninstall()

	req := api.IPCRequest{
		ID:   23,
		Cmd:  "reconcile",
		Args: map[string]any{"apply": false},
	}
	conn := newFakeIPCConn()
	if err := handleReconcile(conn, req, fx.deps); err != nil {
		t.Fatalf("handleReconcile: %v", err)
	}
	_, body := decodeReconcileResponse(t, conn)
	if body.DriftCount != 1 {
		t.Fatalf("DriftCount = %d, want 1; drift=%+v", body.DriftCount, body.Drift)
	}

	select {
	case got := <-deadlineDelta:
		if got <= 24*time.Second || got > 25*time.Second {
			t.Fatalf("scheduler context deadline delta = %s, want within (24s, 25s]", got)
		}
	case <-time.After(1 * time.Second):
		t.Fatal("scheduler fake did not observe reconcile context")
	}
}
```

<a id="testmain"></a>

## internal/cli/settings_registry_test.go — строки 414–449

[Исходник](https://github.com/applicate2628/mcp-local-hub/blob/f5fb00370a833255eacb992a7cd32f2e68da7399/internal/cli/settings_registry_test.go#L414-L449)

```go
	reconcileSpawnFn = func(api.SupervisorDaemon) error { return nil }

	api.EnableSupervisorIPCTestPipeIsolation()

	// stateDirFunc ships env-free in production (productionStateDir →
	// api.DaemonStateDir, NOT reading MCPHUB_STATE_DIR_OVERRIDE — bug
	// 2026-06-03-cli-supervise-statedir-override-ungated). Restore the env read
	// for the whole test package so per-test MCPHUB_STATE_DIR_OVERRIDE redirects
	// still reach supervisor state. The env-read exists ONLY here (a _test.go),
	// so it is absent from the production binary (mirrors
	// EnableSupervisorIPCTestPipeIsolation).
	stateDirFunc = func() (string, error) {
		if override := os.Getenv("MCPHUB_STATE_DIR_OVERRIDE"); override != "" {
			return override, nil
		}
		return api.DaemonStateDir()
	}

	tmp, err := os.MkdirTemp("", "mcphub-cli-test-state-*")
	if err != nil {
		panic("internal/cli TestMain: create global test-state temp dir: " + err.Error())
	}
	// Protect the empty root before materializing the sandbox. Replacing its
	// DACL afterward can remove inherited access from existing descendants.
	if err := apitest.HardenedDirForTestMain(tmp); err != nil {
		panic(errors.Join(err, apitest.RemoveTestMainRoot(tmp)))
	}
	restore := api.SetDaemonStateRootForTest(tmp)
	// Install/adopt transaction fixtures deliberately use non-runnable
	// canonical descriptor paths to isolate their later mutation stages. The
	// API package's focused admission tests own the real contained probe; keep
	// this broad CLI fixture hermetic.
	restoreStdioAdmission := api.SetFrozenStdioBridgeAdmissionForTest(func(context.Context, []api.FrozenStdioBridgeAdmissionRequest) error { return nil })
	// Redirect every client-adapter path input before installing the audit. The
	// descriptor is shared with API and GUI package test setup.
	restoreClientEnv, err := clients.ApplyClientConfigSandboxEnvironment(tmp)
```

## internal/cli/settings_registry_test.go — строки 469–510

[Исходник](https://github.com/applicate2628/mcp-local-hub/blob/f5fb00370a833255eacb992a7cd32f2e68da7399/internal/cli/settings_registry_test.go#L469-L510)

```go
	// Default subprocess state-env safety net (see doc comment above).
	// MCPHUB_STATE_DIR_OVERRIDE is the authoritative redirect: it routes the
	// daemon state dir (env-fallback build, BEFORE the resolver) AND feeds the
	// supervisor IPC test-pipe discriminator (EnableSupervisorIPCTestPipeIsolation
	// derives the test pipe name from it — PR #300 r2 P2). Without it, a Windows
	// in-process supervisor test that forgets its own MCPHUB_STATE_DIR_OVERRIDE
	// would bind the PRODUCTION SID pipe \\.\pipe\mcphub-supervisor-<SID> and
	// could collide with the live fleet's supervisor IPC even though its files
	// were redirected. LOCALAPPDATA/XDG redirect the GUI pidport + log base dir.
	restoreEnv := setEnvWithRestore(map[string]string{
		"MCPHUB_STATE_DIR_OVERRIDE": tmp,
		// Global browser kill-switch for the whole cli test binary AND any real
		// `mcphub gui` child a test spawns (inherited env) — no test flashes a
		// browser window even when it spawns a GUI without an explicit
		// --no-browser. Matches gui.SuppressBrowserLaunchEnv (browser.go).
		"MCPHUB_SUPPRESS_BROWSER_LAUNCH": "1",
	})

	// Client-config sandbox audit. Fails any test in this package whose admitted
	// adapters resolve to a config path outside the test sandbox — including
	// adapters constructed by the PRODUCTION CODE under test, which is the shape
	// that let `withHermeticHome` reach the operator's real configs after
	// `mcpFrontPR588Env` had supposedly closed the class. Contract, rationale and
	// the report-mode knob: internal/clients/config_path_sandbox_audit.go.
	auditRestore := clients.EnforceSandboxedConfigPaths(tmp)

	code := m.Run()

	if escapes := auditRestore(); escapes > 0 && code == 0 {
		code = 1
	}
	restoreEnv()
	restoreClientEnv()
	restoreStdioAdmission()
	restore()
	if cleanupErr := apitest.RemoveTestMainRoot(tmp); cleanupErr != nil {
		if code == 0 {
			code = 1
		}
		_, _ = os.Stderr.WriteString("internal/cli TestMain cleanup: " + cleanupErr.Error() + "\n")
	}
	os.Exit(code)
```

<a id="testhook"></a>

## internal/api/testhooks.go — строки 227–242

[Исходник](https://github.com/applicate2628/mcp-local-hub/blob/f5fb00370a833255eacb992a7cd32f2e68da7399/internal/api/testhooks.go#L227-L242)

```go
func SetDaemonStateRootForTest(root string) (restore func()) {
	if !testing.Testing() {
		panic("api.SetDaemonStateRootForTest called outside a test binary — this is a programming error or attack vector; the helper exists exclusively for test hygiene")
	}
	orig := daemonStateRootOverride
	daemonStateRootOverride = root
	// FIX-5: the serena stop-read cache is keyed by the supervisor-intent path
	// (which changes with the state root) AND the file (mtime,size). Reset it on
	// every state-root switch so a test that reuses a previously-seen path (or a
	// fast-mtime-tick filesystem) never reads a stale cached classification from
	// a prior test. Cheap and test-only.
	resetSerenaStopReadCache()
	return func() {
		daemonStateRootOverride = orig
		resetSerenaStopReadCache()
	}
```

<a id="prod-resolver"></a>

## internal/api/state_paths_prod.go — строки 38–48

[Исходник](https://github.com/applicate2628/mcp-local-hub/blob/f5fb00370a833255eacb992a7cd32f2e68da7399/internal/api/state_paths_prod.go#L38-L48)

```go
func daemonStateDir() (string, error) {
	if daemonStateRootOverride != "" {
		// Test escape hatch — never set in production.
		return ensureStateRoot(daemonStateRootOverride)
	}
	if runtime.GOOS == "windows" {
		root, err := resolveKnownFolderProduction()
		if err != nil {
			return "", err
		}
		return ensureStateRoot(joinStateRoot(root))
```

<a id="tagged-resolver"></a>

## internal/api/state_paths_envfallback.go — строки 53–75

[Исходник](https://github.com/applicate2628/mcp-local-hub/blob/f5fb00370a833255eacb992a7cd32f2e68da7399/internal/api/state_paths_envfallback.go#L53-L75)

```go
func daemonStateDir() (string, error) {
	if daemonStateRootOverride != "" {
		return ensureStateRoot(daemonStateRootOverride)
	}
	// Cross-process env override (test build ONLY — this file is compiled
	// solely under -tags=test_state_path_env, never into a production
	// binary). It mirrors the in-memory daemonStateRootOverride above but
	// survives an exec boundary: a subprocess-spawned `mcphub` child cannot
	// inherit the parent process's in-memory package var, so cli subprocess
	// tests (gui_integration_test, daemon_reliability_test) set this env on
	// the child's cmd.Env to fence it off the live %LOCALAPPDATA%\mcp-local-hub
	// fleet. It MUST be checked BEFORE the GOOS switch / resolver chain so it
	// wins on a real Windows host where SHGetKnownFolderPath succeeds and the
	// LOCALAPPDATA fallback below would otherwise never fire (PR #300 r2 P1:
	// the resolver-first chain made LOCALAPPDATA inert on real hosts). Same env
	// name the supervisor IPC test-pipe discriminator reads
	// (EnableSupervisorIPCTestPipeIsolation), so ONE env redirects state dir
	// AND IPC pipe together. The existing resolveKnownFolderWithEnvFallback
	// LOCALAPPDATA → USERPROFILE chain is left UNCHANGED below so the
	// resolver-failure fallback unit tests keep passing.
	if v := os.Getenv("MCPHUB_STATE_DIR_OVERRIDE"); v != "" {
		return ensureStateRoot(v)
	}
```

<a id="client-sandbox"></a>

## internal/clients/config_path_sandbox_testenv.go — строки 28–53

[Исходник](https://github.com/applicate2628/mcp-local-hub/blob/f5fb00370a833255eacb992a7cd32f2e68da7399/internal/clients/config_path_sandbox_testenv.go#L28-L53)

```go
func ClientConfigSandboxEnvironment(root string) []ClientConfigSandboxEnv {
	redirect := func(key, relative string) ClientConfigSandboxEnv {
		value := root
		if relative != "" {
			value = filepath.Join(root, relative)
		}
		return ClientConfigSandboxEnv{Key: key, Value: value}
	}
	return []ClientConfigSandboxEnv{
		redirect("HOME", ""),
		redirect("USERPROFILE", ""),
		redirect("LOCALAPPDATA", filepath.Join("AppData", "Local")),
		redirect("APPDATA", filepath.Join("AppData", "Roaming")),
		redirect("XDG_CONFIG_HOME", ".config"),
		redirect("XDG_DATA_HOME", filepath.Join(".local", "share")),
		redirect("XDG_STATE_HOME", filepath.Join(".local", "state")),
		redirect("ProgramData", "ProgramData"),
		redirect("MIMOCODE_TEST_MANAGED_CONFIG_DIR", filepath.Join("ProgramData", "opencode")),
		{Key: "CODEX_HOME", Unset: true},
		{Key: "COPILOT_HOME", Unset: true},
		{Key: "KIMI_CODE_HOME", Unset: true},
		{Key: "MIMOCODE_HOME", Unset: true},
		{Key: "MIMOCODE_CONFIG", Unset: true},
		{Key: "MIMOCODE_CONFIG_DIR", Unset: true},
		{Key: "MIMOCODE_CONFIG_CONTENT", Unset: true},
	}
```

## internal/clients/config_path_sandbox_testenv.go — строки 59–92

[Исходник](https://github.com/applicate2628/mcp-local-hub/blob/f5fb00370a833255eacb992a7cd32f2e68da7399/internal/clients/config_path_sandbox_testenv.go#L59-L92)

```go
func PrepareClientConfigSandboxDirectories(root string) error {
	if root == "" || !filepath.IsAbs(root) {
		return fmt.Errorf("client config sandbox requires an existing absolute fixture root")
	}
	info, err := os.Stat(root)
	if err != nil {
		return fmt.Errorf("client config sandbox root: %w", err)
	}
	if !info.IsDir() {
		return fmt.Errorf("client config sandbox root is not a directory")
	}
	normalRoot := normalizeSandboxPath(root)
	seen := make(map[string]bool)
	directories := make([]string, 0)
	// Validate the entire descriptor before any directory creation.
	for _, entry := range ClientConfigSandboxEnvironment(root) {
		if entry.Unset {
			continue
		}
		key := normalizeSandboxPath(entry.Value)
		if !filepath.IsAbs(entry.Value) || !withinSandboxRoot(key, normalRoot) {
			return fmt.Errorf("client config sandbox directory %s escapes its fixture root", entry.Key)
		}
		if !seen[key] {
			seen[key] = true
			directories = append(directories, entry.Value)
		}
	}
	for _, directory := range directories {
		if err := os.MkdirAll(directory, 0o700); err != nil {
			return fmt.Errorf("prepare client config sandbox directory: %w", err)
		}
	}
	return nil
```

<a id="registry"></a>

## internal/api/workspace_registry.go — строки 168–185

[Исходник](https://github.com/applicate2628/mcp-local-hub/blob/f5fb00370a833255eacb992a7cd32f2e68da7399/internal/api/workspace_registry.go#L168-L185)

```go
func DefaultRegistryPath() (string, error) {
	if defaultRegistryPathFn != nil {
		return defaultRegistryPathFn()
	}
	if runtime.GOOS == "windows" {
		if v := os.Getenv("LOCALAPPDATA"); v != "" {
			return filepath.Join(v, "mcp-local-hub", "workspaces.yaml"), nil
		}
	}
	if v := os.Getenv("XDG_STATE_HOME"); v != "" {
		return filepath.Join(v, "mcp-local-hub", "workspaces.yaml"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".local", "state", "mcp-local-hub", "workspaces.yaml"), nil
}
```


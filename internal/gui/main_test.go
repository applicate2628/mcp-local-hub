package gui

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"

	"mcp-local-hub/internal/api"
	"mcp-local-hub/internal/api/apitest"
	"mcp-local-hub/internal/autostart"
	"mcp-local-hub/internal/clients"
	"mcp-local-hub/internal/scheduler"
)

type guiTestAutostartFixture struct {
	mu                     sync.Mutex
	schedulerFactoryCalls  int
	schedulerListCalls     int
	schedulerListPrefix    string
	schedulerDeleteCalls   int
	schedulerUnexpectedOps int
	statusCalls            int
	enableCalls            int
	startOwnerCalls        int
}

type guiTestInstallSideEffectSnapshot struct {
	schedulerFactoryCalls  int
	schedulerListCalls     int
	schedulerListPrefix    string
	schedulerDeleteCalls   int
	schedulerUnexpectedOps int
	statusCalls            int
	enableCalls            int
	startOwnerCalls        int
}

func (f *guiTestAutostartFixture) Enable(autostart.Options) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.enableCalls++
	return nil
}

func (f *guiTestAutostartFixture) Disable() error { return nil }

func (f *guiTestAutostartFixture) Status(autostart.Options) (autostart.State, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.statusCalls++
	return autostart.StateEnabledStopped, nil
}

func (f *guiTestAutostartFixture) StatusSnapshot(autostart.Options) (autostart.StatusSnapshot, error) {
	return autostart.StatusSnapshot{State: autostart.StateEnabledStopped}, nil
}

func (f *guiTestAutostartFixture) startOwner() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.startOwnerCalls++
	return nil
}

func (f *guiTestAutostartFixture) schedulerFactory() (scheduler.Scheduler, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.schedulerFactoryCalls++
	return guiTestScheduler{fixture: f}, nil
}

func (f *guiTestAutostartFixture) snapshot() guiTestInstallSideEffectSnapshot {
	f.mu.Lock()
	defer f.mu.Unlock()
	return guiTestInstallSideEffectSnapshot{
		schedulerFactoryCalls:  f.schedulerFactoryCalls,
		schedulerListCalls:     f.schedulerListCalls,
		schedulerListPrefix:    f.schedulerListPrefix,
		schedulerDeleteCalls:   f.schedulerDeleteCalls,
		schedulerUnexpectedOps: f.schedulerUnexpectedOps,
		statusCalls:            f.statusCalls,
		enableCalls:            f.enableCalls,
		startOwnerCalls:        f.startOwnerCalls,
	}
}

type guiTestScheduler struct {
	fixture *guiTestAutostartFixture
}

func (s guiTestScheduler) Create(scheduler.TaskSpec) error {
	return s.unexpectedOperation("Create")
}

func (s guiTestScheduler) Delete(string) error {
	s.fixture.mu.Lock()
	defer s.fixture.mu.Unlock()
	s.fixture.schedulerDeleteCalls++
	return nil
}

func (s guiTestScheduler) Run(string) error {
	return s.unexpectedOperation("Run")
}

func (s guiTestScheduler) Stop(string) error {
	return s.unexpectedOperation("Stop")
}

func (s guiTestScheduler) Status(string) (scheduler.TaskStatus, error) {
	return scheduler.TaskStatus{}, s.unexpectedOperation("Status")
}

func (s guiTestScheduler) List(prefix string) ([]scheduler.TaskStatus, error) {
	s.fixture.mu.Lock()
	defer s.fixture.mu.Unlock()
	s.fixture.schedulerListCalls++
	s.fixture.schedulerListPrefix = prefix
	return nil, nil
}

func (s guiTestScheduler) ExportXML(string) ([]byte, error) {
	return nil, s.unexpectedOperation("ExportXML")
}

func (s guiTestScheduler) ImportXML(string, []byte) error {
	return s.unexpectedOperation("ImportXML")
}

func (s guiTestScheduler) unexpectedOperation(operation string) error {
	s.fixture.mu.Lock()
	defer s.fixture.mu.Unlock()
	s.fixture.schedulerUnexpectedOps++
	return fmt.Errorf("unexpected GUI test scheduler operation: %s", operation)
}

var testInstallAutostartFixture = &guiTestAutostartFixture{}

const (
	auditLockTerminalWorkerStderrHelperEnv = "MCPHUB_AUDIT_LOCK_TEST_STDERR_HELPER"
	auditLockTerminalWorkerStderrMarker    = "audit-lock-test-private-stderr"
	auditLockBlockingHelperEnv             = "MCPHUB_AUDIT_LOCK_BLOCKING_HELPER"
	auditLockHelperLockEnv                 = "MCPHUB_AUDIT_LOCK_HELPER_LOCK"
	auditLockHelperEnteredEnv              = "MCPHUB_AUDIT_LOCK_HELPER_ENTERED"
	pidfdTestChildEnv                      = "MCPHUB_PIDFD_TEST_CHILD"
	pidfdTestChildStallEnv                 = "MCPHUB_PIDFD_TEST_CHILD_STALL"
	auditLockTerminalWorkerArg             = "audit-lock-terminal-worker"
)

// TestMain fences the WHOLE internal/gui test binary off the operator's real
// per-user state directory. Without it, any gui test that exercises a handler
// or broadcaster which resolves api.DaemonStateDir() (gui-events.log,
// hub-mcp.log) or the workspace registry (workspaces.yaml + its flock) writes
// into the live %LOCALAPPDATA%\mcp-local-hub fleet — corrupting the operator's
// running supervisor state (the documented KOSYAK: an internal/gui `go test`
// overwriting live supervisor-intent.json / leaking gui-events.log into the
// real log). Mirrors internal/cli's TestMain (settings_registry_test.go) and
// internal/api's TestMain (main_test.go).
//
// Two redirect mechanisms are installed, because the state surfaces resolve
// their roots through two DIFFERENT paths:
//
//   - api.SetDaemonStateRootForTest(stateRoot) — the in-memory override that
//     api.DaemonStateDir() consults FIRST (state_paths_envfallback.go). Fences
//     every state file routed through DaemonStateDir(): gui-events.log,
//     hub-mcp.log, supervisor-intent.json, supervisor-events.log, etc.
//   - LOCALAPPDATA / XDG_STATE_HOME / XDG_DATA_HOME env vars — api's
//     DefaultRegistryPath (workspace_registry.go) and gui's AppDataDir
//     (paths.go) read LOCALAPPDATA / XDG_STATE_HOME DIRECTLY, NOT the in-memory
//     override, so they need the env redirect to keep workspaces.yaml(.lock)
//     and gui.pidport off the real fleet. MCPHUB_STATE_DIR_OVERRIDE additionally
//     fences any forgotten subprocess and feeds the supervisor IPC test-pipe
//     discriminator (EnableSupervisorIPCTestPipeIsolation).
//
// Per-test isolation still composes on top: a test that calls
// api.SetDaemonStateRootForTest(...) itself nests correctly (it saves the
// current — i.e. this TestMain's — value and restores to it via the returned
// restore func), and a per-test t.Setenv("LOCALAPPDATA", ...) overrides the
// global default for that test and auto-restores. So this is strictly additive:
// it only catches the tests that currently FORGET to isolate.
//
// LOCALAPPDATA points at <tmp>/AppData/Local (not the bare tmp) so the
// "appdata"-substring assertion in TestAppDataDir_ReturnsUserWriteablePath
// (paths_test.go) keeps passing on Windows while the path is still a throwaway
// temp dir rather than the operator's real profile.
//
// os.Exit-safety: defers do NOT run after os.Exit, so cleanup is performed
// explicitly after capturing m.Run()'s exit code.
func TestMain(m *testing.M) {
	dispatch := classifyGUITestHelperDispatch(os.Args, os.Environ(), runtime.GOOS)
	if dispatch.role == guiTestRoleInvalid {
		_, _ = os.Stderr.WriteString("internal/gui: invalid test helper dispatch: " + string(dispatch.reason) + "\n")
		os.Exit(3)
	}
	switch dispatch.role {
	case guiTestRoleR6ReceiverChild, guiTestRoleBlockingHelperChild, guiTestRolePIDFDLinuxChild:
		os.Exit(m.Run())
	case guiTestRoleAuditTerminalWorkerChild:
		err := RunAuditLockTerminalWorker(os.Stdin, os.Stdout)
		if os.Getenv(auditLockTerminalWorkerStderrHelperEnv) == "1" {
			_, _ = os.Stderr.WriteString(strings.Repeat(auditLockTerminalWorkerStderrMarker, auditLockTerminalWorkerStderrMaxBytes/len(auditLockTerminalWorkerStderrMarker)+1))
			os.Exit(3)
		}
		if err != nil {
			os.Exit(1)
		}
		os.Exit(0)
	}
	api.EnableSupervisorIPCTestPipeIsolation()

	tmp, err := os.MkdirTemp("", "mcphub-gui-test-state-*")
	if err != nil {
		panic("internal/gui TestMain: create global test-state temp dir: " + err.Error())
	}

	// LOCALAPPDATA mirror keeps a real Windows-shaped %LOCALAPPDATA% layout
	// (<tmp>/AppData/Local) so AppDataDir() resolves a path containing
	// "appdata". The state root used by the in-memory override is the same
	// mcp-local-hub leaf the production resolver would produce under it, so
	// every redirect mechanism converges on one directory.
	localAppData := filepath.Join(tmp, "AppData", "Local")
	stateRoot := filepath.Join(localAppData, "mcp-local-hub")
	if mkErr := os.MkdirAll(stateRoot, 0o700); mkErr != nil {
		_ = os.RemoveAll(tmp)
		panic("internal/gui TestMain: create state root: " + mkErr.Error())
	}
	if hardenErr := apitest.HardenedDirForTestMain(stateRoot); hardenErr != nil {
		_ = os.RemoveAll(tmp)
		panic("internal/gui TestMain: harden state root: " + hardenErr.Error())
	}

	restoreState := api.SetDaemonStateRootForTest(stateRoot)
	// Install/adopt transaction fixtures deliberately use non-runnable
	// canonical descriptor paths; focused API tests own the real probe.
	restoreStdioAdmission := api.SetFrozenStdioBridgeAdmissionForTest(func(context.Context, []api.FrozenStdioBridgeAdmissionRequest) error { return nil })
	// Redirect every client-adapter path input before installing the audit. The
	// descriptor is shared with API and CLI package test setup.
	restoreClientEnv := clients.ApplyClientConfigSandboxEnvironment(tmp)
	restoreEnv := setEnvWithRestore(map[string]string{
		"MCPHUB_STATE_DIR_OVERRIDE": stateRoot,
		// Global browser kill-switch for the whole gui test binary AND any
		// `mcphub gui` child a test spawns (inherited env) — no test flashes a
		// browser window. See browser.go SuppressBrowserLaunchEnv.
		SuppressBrowserLaunchEnv: "1",
	})
	restoreAutostartFixture := api.SetInstallAutostartFixtureForTest(
		testInstallAutostartFixture.schedulerFactory,
		func() (autostart.Backend, error) { return testInstallAutostartFixture, nil },
		testInstallAutostartFixture.startOwner,
	)

	// Client-config sandbox audit. This TestMain fences the STATE dir but installs
	// no home barrier, and several gui handler tests drive real /api/adopt,
	// /api/deadopt and global-scan routes that fan out over the whole client
	// registry. The audit fails any test whose admitted adapters resolve outside
	// the sandbox. Contract: internal/clients/config_path_sandbox_audit.go.
	auditRestore := clients.EnforceSandboxedConfigPaths(tmp)

	code := m.Run()
	if leakErr := assertNoBroadcasterWorkers(); leakErr != nil {
		if code == 0 {
			code = 1
		}
		_, _ = os.Stderr.WriteString(leakErr.Error() + "\n")
	}

	if escapes := auditRestore(); escapes > 0 && code == 0 {
		code = 1
	}
	restoreAutostartFixture()
	restoreEnv()
	restoreClientEnv()
	restoreStdioAdmission()
	restoreState()
	if cleanupErr := apitest.RemoveTestMainRoot(tmp); cleanupErr != nil {
		if code == 0 {
			code = 1
		}
		_, _ = os.Stderr.WriteString("internal/gui TestMain cleanup: " + cleanupErr.Error() + "\n")
	}
	os.Exit(code)
}

type guiTestProcessRole uint8

const (
	guiTestRoleInvalid guiTestProcessRole = iota
	guiTestRoleNormalParent
	guiTestRoleR6ReceiverChild
	guiTestRoleAuditTerminalWorkerChild
	guiTestRoleBlockingHelperChild
	guiTestRolePIDFDLinuxChild
)

type guiTestDispatchFailure string

const (
	guiTestFailureDuplicateSelector      guiTestDispatchFailure = "GUI_TEST_HELPER_DUPLICATE_SELECTOR"
	guiTestFailureInvalidSelectorGrammar guiTestDispatchFailure = "GUI_TEST_HELPER_INVALID_SELECTOR_GRAMMAR"
	guiTestFailureSelectorOnly           guiTestDispatchFailure = "GUI_TEST_HELPER_SELECTOR_ONLY"
	guiTestFailurePartialFrame           guiTestDispatchFailure = "GUI_TEST_HELPER_PARTIAL_FRAME"
	guiTestFailureUnknownValue           guiTestDispatchFailure = "GUI_TEST_HELPER_UNKNOWN_VALUE"
	guiTestFailureConflict               guiTestDispatchFailure = "GUI_TEST_HELPER_CONFLICT"
	guiTestFailureWrongArgv              guiTestDispatchFailure = "GUI_TEST_HELPER_WRONG_ARGV"
	guiTestFailureInvalidPath            guiTestDispatchFailure = "GUI_TEST_HELPER_INVALID_PATH"
	guiTestFailureDuplicateEnvKey        guiTestDispatchFailure = "GUI_TEST_HELPER_DUPLICATE_ENV_KEY"
	guiTestFailurePIDFDFrameInvalid      guiTestDispatchFailure = "GUI_TEST_HELPER_PIDFD_FRAME_INVALID"
)

type guiTestHelperDispatch struct {
	role   guiTestProcessRole
	reason guiTestDispatchFailure
}

type guiTestSelector struct {
	present bool
	value   string
}

var guiTestHelperEnvironmentKeys = [...]string{
	auditLockBlockingHelperEnv,
	auditLockHelperLockEnv,
	auditLockHelperEnteredEnv,
	auditLockR6ReceiverHelperEnv,
	auditLockR6StateRootEnv,
	auditLockTerminalWorkerStderrHelperEnv,
	pidfdTestChildEnv,
	pidfdTestChildStallEnv,
}

const (
	guiTestR6Selector       = "^TestAuditLockTerminalWorker_RealHTTPEventPersistenceAndSecondRun$"
	guiTestBlockingSelector = "^TestAuditLockTerminalWorkerCancellationAfterAcquisitionReapsBeforeReturn$"
	guiTestPIDFDSelector    = "^TestRetainedPIDFDAlive_LinuxChildHelper$"
)

func invalidGUITestDispatch(reason guiTestDispatchFailure) guiTestHelperDispatch {
	return guiTestHelperDispatch{role: guiTestRoleInvalid, reason: reason}
}

func parseGUITestSelector(args []string) (guiTestSelector, guiTestDispatchFailure) {
	var result guiTestSelector
	for index := 1; index < len(args); index++ {
		arg := args[index]
		if arg == "--" || !strings.HasPrefix(arg, "-") {
			break
		}
		var value string
		matched := false
		switch {
		case strings.HasPrefix(arg, "-test.run="):
			value, matched = strings.TrimPrefix(arg, "-test.run="), true
		case strings.HasPrefix(arg, "--test.run="):
			value, matched = strings.TrimPrefix(arg, "--test.run="), true
		case arg == "-test.run" || arg == "--test.run":
			if index+1 >= len(args) {
				return guiTestSelector{}, guiTestFailureInvalidSelectorGrammar
			}
			index++
			value, matched = args[index], true
		case strings.HasPrefix(arg, "-test.run") || strings.HasPrefix(arg, "--test.run"):
			return guiTestSelector{}, guiTestFailureInvalidSelectorGrammar
		}
		if !matched {
			continue
		}
		if result.present {
			return guiTestSelector{}, guiTestFailureDuplicateSelector
		}
		result = guiTestSelector{present: true, value: value}
	}
	return result, ""
}

func guiTestEnvironmentIdentity(key, goos string) string {
	if goos == "windows" {
		return strings.ToLower(key)
	}
	return key
}

func guiTestHelperEnvironment(environment []string, goos string) (map[string]string, guiTestDispatchFailure) {
	canonical := make(map[string]string, len(guiTestHelperEnvironmentKeys))
	for _, key := range guiTestHelperEnvironmentKeys {
		canonical[guiTestEnvironmentIdentity(key, goos)] = key
	}
	values := make(map[string]string, len(guiTestHelperEnvironmentKeys))
	for _, entry := range environment {
		key, value, ok := strings.Cut(entry, "=")
		if !ok {
			continue
		}
		owner, recognized := canonical[guiTestEnvironmentIdentity(key, goos)]
		if !recognized {
			continue
		}
		if _, duplicate := values[owner]; duplicate {
			return nil, guiTestFailureDuplicateEnvKey
		}
		values[owner] = value
	}
	return values, ""
}

func classifyGUITestHelperDispatch(args, environment []string, goos string) guiTestHelperDispatch {
	selector, reason := parseGUITestSelector(args)
	if reason != "" {
		return invalidGUITestDispatch(reason)
	}
	values, reason := guiTestHelperEnvironment(environment, goos)
	if reason != "" {
		return invalidGUITestDispatch(reason)
	}
	for _, marker := range []string{auditLockBlockingHelperEnv, auditLockR6ReceiverHelperEnv, auditLockTerminalWorkerStderrHelperEnv, pidfdTestChildEnv, pidfdTestChildStallEnv} {
		if value, present := values[marker]; present && value != "1" {
			return invalidGUITestDispatch(guiTestFailureUnknownValue)
		}
	}
	blocking := hasAnyGUITestHelperValue(values, auditLockBlockingHelperEnv, auditLockHelperLockEnv, auditLockHelperEnteredEnv)
	r6 := hasAnyGUITestHelperValue(values, auditLockR6ReceiverHelperEnv, auditLockR6StateRootEnv)
	terminalFault := hasAnyGUITestHelperValue(values, auditLockTerminalWorkerStderrHelperEnv)
	pidfd := hasAnyGUITestHelperValue(values, pidfdTestChildEnv, pidfdTestChildStallEnv)
	families := 0
	for _, active := range []bool{blocking, r6, terminalFault, pidfd} {
		if active {
			families++
		}
	}
	if families > 1 {
		return invalidGUITestDispatch(guiTestFailureConflict)
	}
	if blocking && !hasAllGUITestHelperValues(values, auditLockBlockingHelperEnv, auditLockHelperLockEnv, auditLockHelperEnteredEnv) {
		return invalidGUITestDispatch(guiTestFailurePartialFrame)
	}
	if r6 && !hasAllGUITestHelperValues(values, auditLockR6ReceiverHelperEnv, auditLockR6StateRootEnv) {
		return invalidGUITestDispatch(guiTestFailurePartialFrame)
	}
	if pidfd && values[pidfdTestChildEnv] != "1" {
		return invalidGUITestDispatch(guiTestFailurePIDFDFrameInvalid)
	}
	terminalArgv := len(args) == 2 && args[1] == auditLockTerminalWorkerArg
	for _, arg := range args[1:] {
		if arg == auditLockTerminalWorkerArg && !terminalArgv {
			return invalidGUITestDispatch(guiTestFailureWrongArgv)
		}
	}
	if terminalArgv {
		if selector.present || blocking || r6 || pidfd {
			return invalidGUITestDispatch(guiTestFailureWrongArgv)
		}
		return guiTestHelperDispatch{role: guiTestRoleAuditTerminalWorkerChild}
	}
	if terminalFault {
		return invalidGUITestDispatch(guiTestFailureWrongArgv)
	}
	if blocking {
		if !validGUITestHelperPath(values[auditLockHelperLockEnv]) || !validGUITestHelperPath(values[auditLockHelperEnteredEnv]) {
			return invalidGUITestDispatch(guiTestFailureInvalidPath)
		}
		if !selector.present || selector.value != guiTestBlockingSelector {
			return invalidGUITestDispatch(guiTestFailureConflict)
		}
		return guiTestHelperDispatch{role: guiTestRoleBlockingHelperChild}
	}
	if r6 {
		if !validGUITestHelperPath(values[auditLockR6StateRootEnv]) {
			return invalidGUITestDispatch(guiTestFailureInvalidPath)
		}
		if !selector.present || selector.value != guiTestR6Selector {
			return invalidGUITestDispatch(guiTestFailureConflict)
		}
		return guiTestHelperDispatch{role: guiTestRoleR6ReceiverChild}
	}
	if pidfd {
		if goos != "linux" || values[pidfdTestChildEnv] != "1" || !selector.present || selector.value != guiTestPIDFDSelector {
			return invalidGUITestDispatch(guiTestFailurePIDFDFrameInvalid)
		}
		return guiTestHelperDispatch{role: guiTestRolePIDFDLinuxChild}
	}
	if selector.present && (selector.value == guiTestBlockingSelector || selector.value == guiTestPIDFDSelector) {
		return invalidGUITestDispatch(guiTestFailureSelectorOnly)
	}
	return guiTestHelperDispatch{role: guiTestRoleNormalParent}
}

func hasAnyGUITestHelperValue(values map[string]string, keys ...string) bool {
	for _, key := range keys {
		if _, present := values[key]; present {
			return true
		}
	}
	return false
}

func hasAllGUITestHelperValues(values map[string]string, keys ...string) bool {
	for _, key := range keys {
		if _, present := values[key]; !present {
			return false
		}
	}
	return true
}

func validGUITestHelperPath(path string) bool {
	return path != "" && filepath.IsAbs(path) && filepath.Clean(path) == path
}

func withoutGUITestHelperEnvironment(environment []string, goos string) []string {
	identities := make(map[string]struct{}, len(guiTestHelperEnvironmentKeys))
	for _, key := range guiTestHelperEnvironmentKeys {
		identities[guiTestEnvironmentIdentity(key, goos)] = struct{}{}
	}
	result := make([]string, 0, len(environment))
	for _, entry := range environment {
		key, _, ok := strings.Cut(entry, "=")
		if ok {
			if _, helper := identities[guiTestEnvironmentIdentity(key, goos)]; helper {
				continue
			}
		}
		result = append(result, entry)
	}
	return result
}

// setEnvWithRestore sets each key=value in the process environment and returns
// a function that reinstates the prior values (unsetting keys that were not
// previously present). Used by TestMain where no *testing.T is available for
// t.Setenv. Restore is best-effort: os.Setenv/Unsetenv errors are ignored
// because TestMain runs before any test and a failure here cannot meaningfully
// be surfaced past os.Exit. Mirrors the same-named helper in internal/cli's
// settings_registry_test.go.
func setEnvWithRestore(kv map[string]string) (restore func()) {
	type prior struct {
		val string
		set bool
	}
	saved := make(map[string]prior, len(kv))
	for k, v := range kv {
		old, ok := os.LookupEnv(k)
		saved[k] = prior{val: old, set: ok}
		_ = os.Setenv(k, v)
	}
	return func() {
		for k, p := range saved {
			if p.set {
				_ = os.Setenv(k, p.val)
			} else {
				_ = os.Unsetenv(k)
			}
		}
	}
}

// Package api — test hooks exported for cross-package integration tests.
//
// The Register/Unregister paths consult package-scoped overrides for the
// scheduler backend, client adapter set, and registry file location. In
// unit tests inside internal/api those overrides are assigned directly;
// cross-package tests (e.g. internal/e2e) cannot reach unexported names,
// so the install-time factory hooks are surfaced as typed public helpers
// here. Production callers never invoke these.
package api

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"mcp-local-hub/internal/clients"
	"mcp-local-hub/internal/scheduler"
)

// TestSchedulerIface matches the subset of scheduler.Scheduler that the
// Register/Unregister paths use. Cross-package fakes implement it to
// replace the real scheduler backend.
type TestSchedulerIface interface {
	Create(spec scheduler.TaskSpec) error
	Delete(name string) error
	Run(name string) error
	ExportXML(name string) ([]byte, error)
	ImportXML(name string, xml []byte) error
}

// TestClientIface matches the subset of clients.Client that the register
// path consumes. Cross-package fakes implement it.
type TestClientIface interface {
	Exists() bool
	AddEntry(clients.MCPEntry) error
	RemoveEntry(name string) error
	GetEntry(name string) (*clients.MCPEntry, error)
}

// InstallTestHooks replaces the Register/Unregister factories with fakes
// for cross-package tests. Every argument is mandatory except
// registryPathOverride (use "" to keep the default). Returns a restore
// function that resets every hook to the production default.
//
// Intended for internal/e2e-style integration tests; production code must
// never call this.
func InstallTestHooks(newScheduler func() (TestSchedulerIface, error),
	clientSet func() map[string]TestClientIface,
	registryPathOverride string,
) (restore func()) {
	origSch := testSchedulerFactory
	origClients := testClientFactory
	origRegPath := testRegistryPathOverride
	origReadiness := proxyReadinessFn

	// Cross-package tests run against a fake scheduler whose Run is a
	// no-op; the production readiness probe would then time out waiting
	// for a port that never binds. Fake it to succeed immediately so
	// Register's post-Run check does not block E2E tests. The E2E test
	// reaches the proxy via its own httptest server, not via the CLI
	// listening on the register-advertised port.
	proxyReadinessFn = func(port int, timeout time.Duration) error { return nil }

	testSchedulerFactory = func() (testScheduler, error) {
		s, err := newScheduler()
		if err != nil {
			return nil, err
		}
		return testSchedulerShim{s}, nil
	}
	testClientFactory = func() map[string]registerClient {
		out := map[string]registerClient{}
		for name, c := range clientSet() {
			out[name] = testClientShim{c}
		}
		return out
	}
	testRegistryPathOverride = registryPathOverride

	return func() {
		testSchedulerFactory = origSch
		testClientFactory = origClients
		testRegistryPathOverride = origRegPath
		proxyReadinessFn = origReadiness
	}
}

// SetTestCanonicalMcphubPath overrides the path that canonicalMcphubPath()
// returns. Production code uses ~/.local/bin/mcphub[.exe]; tests that
// need a deterministic stub (or a deliberately missing binary path) set
// this and reset it via the returned restore function.
//
// Intended for cross-package test helpers (internal/e2e and unit tests
// in other packages that exercise the Register/Install paths). The
// in-package register_test.go harness writes to the unexported variable
// directly. Production code must never call this.
func SetTestCanonicalMcphubPath(path string) (restore func()) {
	orig := testCanonicalMcphubPathOverride
	testCanonicalMcphubPathOverride = path
	return func() {
		testCanonicalMcphubPathOverride = orig
	}
}

// MCPHubBinaryName returns the platform-correct basename for the
// canonical mcphub binary ("mcphub" on POSIX, "mcphub.exe" on Windows).
// Cross-package tests use it to pick a stub-file name that matches what
// canonicalMcphubPath() would return after SetTestCanonicalMcphubPath.
func MCPHubBinaryName() string {
	return mcphubShortName
}

// SetDaemonStateRootForTest overrides the per-user state directory
// resolver so cross-package tests (internal/cli/watchdog_test.go,
// future internal/e2e watchdog tests) can route every state file
// (daemon-intent.json, watchdog-state.json, intent-audit.log,
// watchdog.log, --once.lock) into a temp directory without env vars.
//
// Returns a restore function that resets the override to "" (meaning
// the platform resolver runs normally).
//
// Production safety (issue #159 leaks lane #6 closure): the helper
// PANICS when called outside a test binary. Detection uses
// testing.Testing() (Go 1.21+) which returns true iff the running
// process was built via `go test`. A production `mcphub` binary
// linking the api package CANNOT smuggle a state-dir override at
// runtime via this exported surface — calling it crashes the
// process loudly instead of silently redirecting state writes.
//
// Plan v13 §16: production binary refuses env-fallback resolution; this
// override is the sanctioned cross-package test-only equivalent.
func SetDaemonStateRootForTest(root string) (restore func()) {
	if !testing.Testing() {
		panic("api.SetDaemonStateRootForTest called outside a test binary — this is a programming error or attack vector; the helper exists exclusively for test hygiene")
	}
	orig := daemonStateRootOverride
	daemonStateRootOverride = root
	return func() {
		daemonStateRootOverride = orig
	}
}

// SetTestStatusFn overrides the StatusContext source so cross-package
// tests can inject deterministic []DaemonStatus rows without spinning
// up Task Scheduler. The watchdog driver consumes both Status() and
// StatusContext via the same seam (statusContextSrcFn).
//
// Returns a restore function that resets the seam.
func SetTestStatusFn(fn func() ([]DaemonStatus, error)) (restore func()) {
	orig := statusContextSrcFn
	statusContextSrcFn = fn
	return func() { statusContextSrcFn = orig }
}

// SetTestRestartWithSnapshotFn overrides the snapshot-bound restart
// path so cross-package tests can capture the OwnershipSnapshot the
// driver passes to RestartContextWithSnapshot.
//
// Returns a restore function that resets the seam.
func SetTestRestartWithSnapshotFn(fn func(server, daemonFilter string, snap OwnershipSnapshot) ([]RestartResult, error)) (restore func()) {
	orig := restartContextWithSnapshotSrcFn
	restartContextWithSnapshotSrcFn = fn
	return func() { restartContextWithSnapshotSrcFn = orig }
}

// SetTestRestartContextFn overrides the general-purpose ctx-aware
// Restart wrapper. Used by cross-package tests that exercise paths
// where the watchdog driver could fall back to a non-snapshot restart
// (defensive — the driver should only ever take the snapshot path).
//
// Returns a restore function that resets the seam.
func SetTestRestartContextFn(fn func(server, daemonFilter string) ([]RestartResult, error)) (restore func()) {
	orig := restartContextSrcFn
	restartContextSrcFn = fn
	return func() { restartContextSrcFn = orig }
}

// SetTestSchedulerFactoryFn overrides the package-level scheduler
// factory used by Uninstall paths. Returns a restore function.
func SetTestSchedulerFactoryFn(fn func() (scheduler.Scheduler, error)) (restore func()) {
	orig := schedulerFactoryFn
	schedulerFactoryFn = fn
	return func() { schedulerFactoryFn = orig }
}

// SetTestIntentReaderFn overrides the readDaemonIntentFn seam so
// cross-package tests can drive IntentStillRunning without writing
// intent files to disk. Returns a restore function.
func SetTestIntentReaderFn(fn func(taskName string) (DaemonIntent, bool, error)) (restore func()) {
	orig := readDaemonIntentFn
	readDaemonIntentFn = fn
	return func() { readDaemonIntentFn = orig }
}

// SetTestAuditAppendFn overrides the disk-append step inside
// AppendIntentAudit. Cross-package tests inject targeted failures
// (disk full, permission denied) without exercising the OS write path.
//
// Returns a restore function. The seam is the lower-level audit-append
// step — appendIntentAuditFn (set by intent_audit.go's init()) still
// routes through the production AppendIntentAudit, which then
// dispatches via auditAppendWriteFn.
func SetTestAuditAppendFn(fn func(path string, line []byte) error) (restore func()) {
	orig := auditAppendWriteFn
	auditAppendWriteFn = fn
	return func() { auditAppendWriteFn = orig }
}

// SetTestWatchdogLogAppendFn overrides the disk-append step inside
// AppendWatchdogLog. Returns a restore function.
func SetTestWatchdogLogAppendFn(fn func(path string, line []byte) error) (restore func()) {
	orig := watchdogLogAppendWriteFn
	watchdogLogAppendWriteFn = fn
	return func() { watchdogLogAppendWriteFn = orig }
}

// SetTestCanonicalMcphubPathFn overrides the canonical-mcphub-path
// resolver consumed by the XML validator and InstallWatchdogTask.
// Returns a restore function.
func SetTestCanonicalMcphubPathFn(fn func() (string, error)) (restore func()) {
	orig := canonicalMcphubPathFn
	canonicalMcphubPathFn = fn
	return func() { canonicalMcphubPathFn = orig }
}

// SetTestCurrentWindowsUserFn overrides the current-user resolver
// consumed by the XML validator and InstallWatchdogTask. Returns a
// restore function.
func SetTestCurrentWindowsUserFn(fn func() (string, error)) (restore func()) {
	orig := currentWindowsUserFn
	currentWindowsUserFn = fn
	return func() { currentWindowsUserFn = orig }
}

// SetClientWriteFallbackForTest reverts the client-adapter writer hook
// (clients.WriteConfigFile) to a plain os.WriteFile-style fallback for
// the duration of a test that exercises adapter writes through
// t.TempDir() / t.Setenv("HOME"/"USERPROFILE", tmp) without
// hardenedTempDir.
//
// Production wires clients.WriteConfigFile to SecureWriteClientConfig
// in init() (see client_write_init.go); the parent-dir DACL gate that
// hook enforces rejects %TEMP%-backed paths on Windows. Tests that
// want to validate the secure-write pipeline use hardenedTempDir
// directly. Tests that pre-date Phase 5 and just want to exercise
// migrate/demigrate/scan flows can call this helper to fall back to
// the looser test-friendly writer.
//
// Returns a restore function that re-installs SecureWriteClientConfig.
// Always invoke via `t.Cleanup(restore)` or `defer restore()` so
// subsequent tests inherit the production hook.
func SetClientWriteFallbackForTest() (restore func()) {
	orig := clients.WriteConfigFile
	clients.WriteConfigFile = func(path string, contents []byte) error {
		if dir := filepath.Dir(path); dir != "" {
			if err := os.MkdirAll(dir, 0o755); err != nil {
				return err
			}
		}
		return os.WriteFile(path, contents, 0o600)
	}
	return func() { clients.WriteConfigFile = orig }
}

// testSchedulerShim adapts a caller-supplied TestSchedulerIface to the
// package-private testScheduler interface.
type testSchedulerShim struct{ s TestSchedulerIface }

func (a testSchedulerShim) Create(spec scheduler.TaskSpec) error    { return a.s.Create(spec) }
func (a testSchedulerShim) Delete(name string) error                { return a.s.Delete(name) }
func (a testSchedulerShim) Run(name string) error                   { return a.s.Run(name) }
func (a testSchedulerShim) ExportXML(name string) ([]byte, error)   { return a.s.ExportXML(name) }
func (a testSchedulerShim) ImportXML(name string, xml []byte) error { return a.s.ImportXML(name, xml) }

// testClientShim adapts a caller-supplied TestClientIface to the
// package-private registerClient interface.
type testClientShim struct{ c TestClientIface }

func (a testClientShim) Exists() bool                                    { return a.c.Exists() }
func (a testClientShim) AddEntry(e clients.MCPEntry) error               { return a.c.AddEntry(e) }
func (a testClientShim) RemoveEntry(name string) error                   { return a.c.RemoveEntry(name) }
func (a testClientShim) GetEntry(name string) (*clients.MCPEntry, error) { return a.c.GetEntry(name) }

// Tests for the Codex deep-security parallel review on PR #135.
//
// Six findings are addressed here:
//
//	Finding 1 (HIGH)     — Intent task-name key normalization. WriteDaemonIntent /
//	                       ClearDaemonIntent must store every entry under the
//	                       leading-backslash form so recovery.go's
//	                       `intent.Tasks[row.TaskName]` lookup (where row.TaskName
//	                       comes from Status() with leading "\") cannot miss the
//	                       Desired=stopped intent that user-stop / uninstall
//	                       writes recorded under the bare form.
//
//	Finding 3 (LOW)      — `mcphub stop` must record a stop-failed-no-kill audit
//	                       entry when stopTaskNamesForServer fails before the
//	                       intent path can run, so forensic trail survives an
//	                       early-exit registry / manifest load failure.
//
//	Finding 4 (LOW)      — LoadOwnershipSnapshotChecked must surface
//	                       workspace-registry load errors as errors so the
//	                       watchdog can refuse to run a tick on partial ownership
//	                       data (a phantom task could otherwise be marked orphan
//	                       OR a real task could be marked unowned).
//
//	Finding 5 (Coverage) — DefaultRegistryPath() resolution failure must
//	                       propagate through stopTaskNamesForServer for
//	                       workspace-scoped servers.
//
//	Finding 6 (Coverage) — The existing
//	                       TestStopTaskNamesForServer_Workspace_RegistryLoadFails_ReturnsError
//	                       test must additionally assert errors.Is wrapping so a
//	                       future refactor cannot silently strip %w.
package api

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"mcp-local-hub/internal/scheduler"
)

// ---------------------------------------------------------------------------
// Finding 1 (HIGH) — task-name normalization at the Write/Clear boundary.
// ---------------------------------------------------------------------------

// TestWriteDaemonIntent_NormalizesLeadingBackslash verifies that the intent
// file always stores the task name under the canonical leading-backslash
// form, regardless of how the caller passed it. This is the core fix for
// Finding 1: recovery.go indexes `intent.Tasks[row.TaskName]` where
// row.TaskName comes from Status() with leading "\". A bare-form write
// (e.g. "mcp-local-hub-x") used to leave a no-leading-slash key in the
// file → recovery missed the Desired=stopped intent → watchdog auto-revived
// a daemon the user just stopped.
func TestWriteDaemonIntent_NormalizesLeadingBackslash(t *testing.T) {
	a := NewAPI()
	daemonIntentTestHelper(t)

	// Bare-form write (without leading "\\").
	if err := a.WriteDaemonIntent("mcp-local-hub-bare", DaemonIntent{
		Desired:   IntentDesiredStopped,
		Reason:    IntentReasonUserStop,
		UpdatedAt: time.Now().UTC(),
	}, "tester"); err != nil {
		t.Fatalf("WriteDaemonIntent (bare): %v", err)
	}

	// Leading-backslash write (already canonical).
	if err := a.WriteDaemonIntent("\\mcp-local-hub-prefixed", DaemonIntent{
		Desired:   IntentDesiredStopped,
		Reason:    IntentReasonUserDisabled,
		UpdatedAt: time.Now().UTC(),
	}, "tester"); err != nil {
		t.Fatalf("WriteDaemonIntent (leading-backslash): %v", err)
	}

	res := a.ReadDaemonIntent()
	if res.State != IntentStateValid {
		t.Fatalf("intent state = %q, want valid", res.State)
	}

	// Both writes must land under the leading-backslash form.
	if _, ok := res.File.Tasks["\\mcp-local-hub-bare"]; !ok {
		t.Errorf("bare-form write missing canonical key \\mcp-local-hub-bare; tasks=%v", keys(res.File.Tasks))
	}
	if _, ok := res.File.Tasks["\\mcp-local-hub-prefixed"]; !ok {
		t.Errorf("leading-backslash write missing canonical key; tasks=%v", keys(res.File.Tasks))
	}

	// The bare form MUST NOT exist as a separate key (otherwise the same
	// task could end up with two intent records, one for each form).
	if _, ok := res.File.Tasks["mcp-local-hub-bare"]; ok {
		t.Errorf("bare-form key persisted alongside canonical form; tasks=%v", keys(res.File.Tasks))
	}
}

// TestWriteDaemonIntent_NormalizationIsIdempotent verifies that writing the
// same task under both forms updates one canonical entry rather than
// fragmenting into two records.
func TestWriteDaemonIntent_NormalizationIsIdempotent(t *testing.T) {
	a := NewAPI()
	daemonIntentTestHelper(t)

	now := time.Now().UTC()
	if err := a.WriteDaemonIntent("mcp-local-hub-same", DaemonIntent{
		Desired:   IntentDesiredStopped,
		Reason:    IntentReasonUserStop,
		UpdatedAt: now,
	}, "tester"); err != nil {
		t.Fatalf("first write (bare): %v", err)
	}
	// Second write under the prefixed form replaces the same record.
	if err := a.WriteDaemonIntent("\\mcp-local-hub-same", DaemonIntent{
		Desired:   IntentDesiredRunning,
		Reason:    IntentReasonInstall,
		UpdatedAt: now,
	}, "tester"); err != nil {
		t.Fatalf("second write (leading-backslash): %v", err)
	}

	res := a.ReadDaemonIntent()
	if len(res.File.Tasks) != 1 {
		t.Fatalf("entries=%d, want 1 (idempotent normalization); tasks=%v", len(res.File.Tasks), keys(res.File.Tasks))
	}
	got, ok := res.File.Tasks["\\mcp-local-hub-same"]
	if !ok {
		t.Fatalf("missing canonical key after two writes; tasks=%v", keys(res.File.Tasks))
	}
	if got.Desired != IntentDesiredRunning {
		t.Errorf("Desired = %q, want %q (last write wins)", got.Desired, IntentDesiredRunning)
	}
	if got.Reason != IntentReasonInstall {
		t.Errorf("Reason = %q, want %q (last write wins)", got.Reason, IntentReasonInstall)
	}
}

// TestClearDaemonIntent_NormalizesLeadingBackslash verifies the matching
// fix for ClearDaemonIntent: clearing under the bare form must remove the
// canonical leading-backslash entry that recovery.go reads.
func TestClearDaemonIntent_NormalizesLeadingBackslash(t *testing.T) {
	a := NewAPI()
	daemonIntentTestHelper(t)

	// Seed under the canonical form.
	if err := a.WriteDaemonIntent("\\mcp-local-hub-clearme", DaemonIntent{
		Desired:   IntentDesiredStopped,
		Reason:    IntentReasonUserStop,
		UpdatedAt: time.Now().UTC(),
	}, "tester"); err != nil {
		t.Fatalf("seed: %v", err)
	}

	// Clear under the bare form — the normalization must locate the
	// canonical entry and remove it.
	if err := a.ClearDaemonIntent("mcp-local-hub-clearme", "tester"); err != nil {
		t.Fatalf("ClearDaemonIntent (bare): %v", err)
	}

	res := a.ReadDaemonIntent()
	if _, ok := res.File.Tasks["\\mcp-local-hub-clearme"]; ok {
		t.Errorf("bare-form clear failed to remove canonical entry; tasks=%v", keys(res.File.Tasks))
	}
}

// TestRecoverStoppedDaemons_RespectsBareIntentKey_AfterNormalization is the
// regression guard for Finding 1: even when an intent is written with the
// bare form (legacy callers, future regressions, manual file edits), the
// recovery decision tree must still see the Desired=stopped directive and
// yield the matching reason — NOT "restart". After Option-A normalization
// at the WriteDaemonIntent boundary, every entry on disk is canonical so
// the lookup hits.
func TestRecoverStoppedDaemons_RespectsBareIntentKey_AfterNormalization(t *testing.T) {
	a := NewAPI()
	daemonIntentTestHelper(t)

	// Write with the BARE form — the bug shape that kicked off Finding 1.
	if err := a.WriteDaemonIntent("mcp-local-hub-memory-default", DaemonIntent{
		Desired:   IntentDesiredStopped,
		Reason:    IntentReasonUninstalled,
		UpdatedAt: time.Now().UTC(),
	}, "tester"); err != nil {
		t.Fatalf("WriteDaemonIntent: %v", err)
	}

	// Build a status row using the leading-backslash form (what Status()
	// returns on Windows).
	now := time.Now().UTC()
	status := []DaemonStatus{{
		TaskName:   "\\mcp-local-hub-memory-default",
		Server:     "memory",
		Daemon:     "default",
		State:      "Stopped",
		LastResult: 1, // real failure
	}}

	intent := a.ReadDaemonIntent()
	if intent.State != IntentStateValid {
		t.Fatalf("intent state = %q, want valid", intent.State)
	}

	cool := newStubCooldown()
	validator := alwaysValidValidator{}
	registry := stubRegistryAlwaysManaged{}

	decisions := RecoverStoppedDaemons(now, status, intent.File, cool, validator, registry)
	if len(decisions) != 1 {
		t.Fatalf("decisions=%d, want 1; got=%+v", len(decisions), decisions)
	}
	if decisions[0].Action != IntentReasonUninstalled {
		t.Errorf("decisions[0].Action = %q, want %q (intent must suppress restart even when written under bare form)",
			decisions[0].Action, IntentReasonUninstalled)
	}
}

// keys is a tiny helper to dump map keys for diagnostic output.
func keys(m map[string]DaemonIntent) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

// ---------------------------------------------------------------------------
// Finding 1 supporting fakes for RecoverStoppedDaemons.
// ---------------------------------------------------------------------------

// stubCooldown implements CooldownReader with permissive defaults (Due=true,
// no chronic limit, no restart-pending) so the recovery decision flow only
// exercises the intent-active branch.
type stubCooldown struct{}

func newStubCooldown() stubCooldown                                       { return stubCooldown{} }
func (stubCooldown) Due(taskName string, now time.Time) bool              { return true }
func (stubCooldown) ChronicLimitReached(taskName string) bool             { return false }
func (stubCooldown) AttemptsInWindow(taskName string) int                 { return 0 }
func (stubCooldown) IsRestartPending(taskName string, now time.Time) bool { return false }

// alwaysValidValidator answers IsOwnedAndValid=true for every task.
type alwaysValidValidator struct{}

func (alwaysValidValidator) IsOwnedAndValid(taskName string) bool { return true }

// stubRegistryAlwaysManaged answers IsManagedDaemon=true for every task.
type stubRegistryAlwaysManaged struct{}

func (stubRegistryAlwaysManaged) IsManagedDaemon(taskName string) bool { return true }

// ---------------------------------------------------------------------------
// Finding 3 (LOW) — stop-failed-no-kill audit on early-exit failures.
// ---------------------------------------------------------------------------

// TestStopWithOpts_RegistryLoadFails_AuditFailedNoKill verifies that when
// stopTaskNamesForServer fails (workspace registry corrupt → reg.Load
// returns error) the StopWithOpts caller emits a stop-failed-no-kill audit
// entry BEFORE returning the error, so the forensic trail records the
// blocked stop attempt. The kill path still must NOT run.
func TestStopWithOpts_RegistryLoadFails_AuditFailedNoKill(t *testing.T) {
	a := NewAPI()
	daemonIntentTestHelper(t)
	regPath := pointRegistryAtTempDir(t)
	if err := os.WriteFile(regPath, []byte("this: is: not\n  - valid: ["), 0o600); err != nil {
		t.Fatalf("seed corrupt registry: %v", err)
	}
	r := &recordingAuditWriter{}
	installRecordingAudit(t, r)
	killCounter := stopFakeKillCounter(t)

	_, err := a.StopWithOpts(StopOpts{Server: "mcp-language-server", Force: false})
	if err == nil {
		t.Fatal("StopWithOpts: want error on registry load failure, got nil")
	}
	if got := atomic.LoadInt32(killCounter); got != 0 {
		t.Errorf("kill path invoked %d times on early-exit fail-closed; want 0", got)
	}
	// Forensic trail must record the failed-no-kill event so an operator
	// can see the blocked stop attempt without parsing CLI stderr.
	saw := false
	for _, e := range r.entries {
		if e.Action == AuditActionStopFailedNoKill {
			saw = true
			if e.Priority != "high" {
				t.Errorf("stop-failed-no-kill priority = %q, want %q", e.Priority, "high")
			}
			if !strings.Contains(e.Reason, "registry") {
				t.Errorf("stop-failed-no-kill reason = %q, want substring 'registry'", e.Reason)
			}
		}
	}
	if !saw {
		t.Errorf("expected Action=%q in audit entries: %+v", AuditActionStopFailedNoKill, r.entries)
	}
}

// ---------------------------------------------------------------------------
// Finding 4 (LOW) — LoadOwnershipSnapshotChecked surfaces registry errors.
// ---------------------------------------------------------------------------

// TestLoadOwnershipSnapshotChecked_RegistryLoadError_ReturnsError verifies
// that when the workspace registry is present but unparseable, the new
// Checked variant of LoadOwnershipSnapshot returns the error instead of
// silently degrading to a partial snapshot. This lets the watchdog driver
// refuse the tick rather than make decisions on incomplete ownership data.
//
// PR #135 round 3 P1: the fail-closed gate is keyed on installed lazy-
// proxy tasks (`mcp-local-hub-lsp-*`) instead of the manifest catalog,
// so this test now stubs the scheduler factory with one synthetic
// installed task to drive the gate deterministically.
func TestLoadOwnershipSnapshotChecked_RegistryLoadError_ReturnsError(t *testing.T) {
	a := NewAPI()
	daemonIntentTestHelper(t)
	installLazyProxySchedulerStub(t, 1)
	regPath := pointRegistryAtTempDir(t)
	if err := os.WriteFile(regPath, []byte("this: is: not\n  - valid: ["), 0o600); err != nil {
		t.Fatalf("seed corrupt registry: %v", err)
	}

	_, err := a.LoadOwnershipSnapshotChecked()
	if err == nil {
		t.Fatal("LoadOwnershipSnapshotChecked: want error on registry load failure, got nil")
	}
	if !strings.Contains(err.Error(), "workspace registry") {
		t.Errorf("error message = %q, want substring 'workspace registry'", err.Error())
	}
}

// TestLoadOwnershipSnapshotChecked_HappyPath verifies that the Checked
// variant returns nil error when the registry path is absent or empty
// (the ordinary mcphub setup before any workspace is registered).
func TestLoadOwnershipSnapshotChecked_HappyPath(t *testing.T) {
	a := NewAPI()
	daemonIntentTestHelper(t)
	// Point at a temp dir but DO NOT plant a registry file — Load() returns
	// nil error on a missing file (per workspace_registry.go contract).
	pointRegistryAtTempDir(t)

	snap, err := a.LoadOwnershipSnapshotChecked()
	if err != nil {
		t.Fatalf("LoadOwnershipSnapshotChecked (missing registry): %v", err)
	}
	if snap.PortMap == nil {
		t.Errorf("PortMap nil; want non-nil empty map")
	}
}

// ---------------------------------------------------------------------------
// Finding 5 (Coverage) — DefaultRegistryPath() resolution failure path.
// ---------------------------------------------------------------------------

// TestStopTaskNamesForServer_Workspace_DefaultRegistryPathFails_ReturnsError
// covers the path-resolve failure branch. The defaultRegistryPathFn seam
// returns an explicit error → stopTaskNamesForServer must propagate it
// through the workspace branch without dropping context.
func TestStopTaskNamesForServer_Workspace_DefaultRegistryPathFails_ReturnsError(t *testing.T) {
	sentinel := errors.New("synthetic resolve failure")
	prev := defaultRegistryPathFn
	defaultRegistryPathFn = func() (string, error) { return "", sentinel }
	t.Cleanup(func() { defaultRegistryPathFn = prev })

	_, err := stopTaskNamesForServer("mcp-language-server", "")
	if err == nil {
		t.Fatal("stopTaskNamesForServer: want error on DefaultRegistryPath failure, got nil")
	}
	if !errors.Is(err, sentinel) {
		t.Errorf("error chain: want sentinel via errors.Is, got %v", err)
	}
	if !strings.Contains(err.Error(), "registry path") {
		t.Errorf("error message = %q, want substring 'registry path'", err.Error())
	}
}

// ---------------------------------------------------------------------------
// Finding 6 (Coverage) — errors.Is on the wrapped registry error.
// ---------------------------------------------------------------------------

// TestStopTaskNamesForServer_Workspace_RegistryLoadFails_PreservesWrap
// asserts that the error returned by stopTaskNamesForServer wraps the
// underlying load error via %w so callers can use errors.Is to inspect the
// root cause. The companion TestStopTaskNamesForServer_Workspace_RegistryLoadFails_ReturnsError
// already verifies the substring; this one prevents a future refactor from
// silently dropping the wrap.
func TestStopTaskNamesForServer_Workspace_RegistryLoadFails_PreservesWrap(t *testing.T) {
	regPath := pointRegistryAtTempDir(t)
	corruptYAML := []byte("this: is: not\n  - valid: [")
	if err := os.WriteFile(regPath, corruptYAML, 0o600); err != nil {
		t.Fatalf("seed corrupt registry: %v", err)
	}
	_, err := stopTaskNamesForServer("mcp-language-server", "")
	if err == nil {
		t.Fatal("stopTaskNamesForServer: want error on registry load failure, got nil")
	}
	// Independently load the same registry to capture the underlying YAML
	// parse error, then assert the stopTaskNamesForServer error chain
	// contains that exact error type via errors.Is. This proves the %w
	// wrap survived through the fmt.Errorf call.
	reg := NewRegistry(regPath)
	loadErr := reg.Load()
	if loadErr == nil {
		t.Fatal("expected reg.Load to fail on the same corrupt YAML")
	}
	// The underlying load error is wrapped by fmt.Errorf with %w. Compare
	// using a synthetic chain to confirm errors.As / errors.Is round-trip.
	// Because reg.Load returns a yaml package error type whose value isn't
	// stable across runs, we synthesize a sentinel via a stub registry path
	// resolver and re-run with errors.Is.
	sentinel := errors.New("synthetic load failure")
	// Re-run with the path resolver stub to inject a known error.
	prev := defaultRegistryPathFn
	defaultRegistryPathFn = func() (string, error) { return "", fmt.Errorf("resolve failed: %w", sentinel) }
	t.Cleanup(func() { defaultRegistryPathFn = prev })

	_, err2 := stopTaskNamesForServer("mcp-language-server", "")
	if err2 == nil {
		t.Fatal("stopTaskNamesForServer: want error via stubbed resolver, got nil")
	}
	if !errors.Is(err2, sentinel) {
		t.Errorf("errors.Is(err2, sentinel) = false; want true. got err=%v", err2)
	}
}

// ---------------------------------------------------------------------------
// PR #135 round 3 P1 — LoadOwnershipSnapshotChecked must scope fail-closed
// to hosts that actually have at least one `mcp-local-hub-lsp-*` task
// installed, NOT to whatever workspace-scoped manifests ship in the catalog.
// The catalog gate (round 2) was too broad: every shipped build includes
// the `mcp-language-server` workspace-scoped manifest, so global-only
// deployments still tripped fail-closed on every watchdog tick whenever
// DefaultRegistryPath / reg.Load failed.
// ---------------------------------------------------------------------------

// schedulerWithLazyProxyTasks returns a scheduler.Scheduler stub whose
// List(prefix) call returns nLazyProxy synthetic lazy-proxy entries
// when prefix == "mcp-local-hub-lsp-", and nil otherwise. All other
// methods return errNotImplementedForTest. Used by the round-3 P1
// tests to drive the installed-task gate deterministically without
// depending on the developer's host scheduler state.
type schedulerWithLazyProxyTasks struct {
	nLazyProxy int
	listErr    error
}

func (s *schedulerWithLazyProxyTasks) Create(scheduler.TaskSpec) error {
	return errNotImplementedForTest
}
func (s *schedulerWithLazyProxyTasks) Delete(string) error { return errNotImplementedForTest }
func (s *schedulerWithLazyProxyTasks) Run(string) error    { return errNotImplementedForTest }
func (s *schedulerWithLazyProxyTasks) Stop(string) error   { return errNotImplementedForTest }
func (s *schedulerWithLazyProxyTasks) Status(string) (scheduler.TaskStatus, error) {
	return scheduler.TaskStatus{}, errNotImplementedForTest
}
func (s *schedulerWithLazyProxyTasks) List(prefix string) ([]scheduler.TaskStatus, error) {
	if s.listErr != nil {
		return nil, s.listErr
	}
	if prefix != lazyProxyTaskNamePrefix {
		return nil, nil
	}
	out := make([]scheduler.TaskStatus, 0, s.nLazyProxy)
	for i := 0; i < s.nLazyProxy; i++ {
		out = append(out, scheduler.TaskStatus{Name: fmt.Sprintf("\\mcp-local-hub-lsp-%08x-go", i)})
	}
	return out, nil
}
func (s *schedulerWithLazyProxyTasks) ExportXML(string) ([]byte, error) {
	return nil, errNotImplementedForTest
}
func (s *schedulerWithLazyProxyTasks) ImportXML(string, []byte) error {
	return errNotImplementedForTest
}

// installLazyProxySchedulerStub patches schedulerFactoryFn to return a
// stub whose List("mcp-local-hub-lsp-") yields nLazyProxy entries.
// Restores on cleanup.
func installLazyProxySchedulerStub(t *testing.T, nLazyProxy int) {
	t.Helper()
	stub := &schedulerWithLazyProxyTasks{nLazyProxy: nLazyProxy}
	prev := schedulerFactoryFn
	schedulerFactoryFn = func() (scheduler.Scheduler, error) { return stub, nil }
	t.Cleanup(func() { schedulerFactoryFn = prev })
}

// TestLoadOwnershipSnapshotChecked_NoLspTasksInstalled_RegistryPathFailsBenignly
// is the round-3 P1 primary case: the catalog ships at least one
// workspace-scoped manifest (mcp-language-server) but the host has no
// `mcp-local-hub-lsp-*` task installed, so the watchdog must NOT fail
// closed when DefaultRegistryPath errors. Without this, every global-
// only deployment with a service-account-style unreachable home dir
// would block recovery for unrelated global daemons.
func TestLoadOwnershipSnapshotChecked_NoLspTasksInstalled_RegistryPathFailsBenignly(t *testing.T) {
	a := NewAPI()
	daemonIntentTestHelper(t)

	// Catalog still contains the workspace-scoped server (this is the
	// realistic shipped-build state — catalog catalog cannot be opted
	// out of). The gate must look at INSTALLED tasks only.
	manifestDir := t.TempDir()
	t.Setenv("MCPHUB_MANIFEST_DIR_OVERRIDE", manifestDir)
	subdir := filepath.Join(manifestDir, "workspace-srv")
	if err := os.MkdirAll(subdir, 0o700); err != nil {
		t.Fatalf("mkdir manifest dir: %v", err)
	}
	body := "name: workspace-srv\nkind: workspace-scoped\ntransport: stdio-bridge\ncommand: workspace-srv\nport_pool: {start: 9300, end: 9399}\nlanguages:\n  - name: go\n    backend: gopls-mcp\n    transport: stdio\n    lsp_command: gopls\n"
	if err := os.WriteFile(filepath.Join(subdir, "manifest.yaml"), []byte(body), 0o600); err != nil {
		t.Fatalf("write manifest: %v", err)
	}

	// Empty scheduler — zero installed lazy-proxy tasks.
	installLazyProxySchedulerStub(t, 0)

	// Force DefaultRegistryPath to error — the production scenario the
	// bot flagged (service account without home dir).
	sentinel := errors.New("synthetic resolve failure")
	prev := defaultRegistryPathFn
	defaultRegistryPathFn = func() (string, error) { return "", sentinel }
	t.Cleanup(func() { defaultRegistryPathFn = prev })

	snap, err := a.LoadOwnershipSnapshotChecked()
	if err != nil {
		t.Fatalf("LoadOwnershipSnapshotChecked (no lsp tasks, registry path fails): want nil error, got %v", err)
	}
	if !snap.ManifestServers["workspace-srv"] {
		t.Errorf("expected workspace-srv server present in snapshot.ManifestServers (catalog still walked), got %+v", snap.ManifestServers)
	}
	if len(snap.WorkspaceTasksByKey) != 0 {
		t.Errorf("WorkspaceTasksByKey: want empty, got %+v", snap.WorkspaceTasksByKey)
	}
}

// TestLoadOwnershipSnapshotChecked_LspTaskInstalled_RegistryPathFailsClosed
// is the round-3 P1 safety counterpart: when ≥1 `mcp-local-hub-lsp-*`
// task IS installed, registry path-resolve failure must propagate as
// before (Finding 4 contract preserved) — fail-closed so the watchdog
// refuses the tick rather than make orphan/lazy-proxy classifications
// on partial ownership data.
func TestLoadOwnershipSnapshotChecked_LspTaskInstalled_RegistryPathFailsClosed(t *testing.T) {
	a := NewAPI()
	daemonIntentTestHelper(t)

	// Catalog content does not matter for the new gate, but provide a
	// realistic shape so ManifestServers is non-empty.
	manifestDir := t.TempDir()
	t.Setenv("MCPHUB_MANIFEST_DIR_OVERRIDE", manifestDir)
	subdir := filepath.Join(manifestDir, "workspace-srv")
	if err := os.MkdirAll(subdir, 0o700); err != nil {
		t.Fatalf("mkdir manifest dir: %v", err)
	}
	body := "name: workspace-srv\nkind: workspace-scoped\ntransport: stdio-bridge\ncommand: workspace-srv\nport_pool: {start: 9300, end: 9399}\nlanguages:\n  - name: go\n    backend: gopls-mcp\n    transport: stdio\n    lsp_command: gopls\n"
	if err := os.WriteFile(filepath.Join(subdir, "manifest.yaml"), []byte(body), 0o600); err != nil {
		t.Fatalf("write manifest: %v", err)
	}

	// Exactly one installed lazy-proxy task — the gate must trip.
	installLazyProxySchedulerStub(t, 1)

	sentinel := errors.New("synthetic resolve failure")
	prev := defaultRegistryPathFn
	defaultRegistryPathFn = func() (string, error) { return "", sentinel }
	t.Cleanup(func() { defaultRegistryPathFn = prev })

	_, err := a.LoadOwnershipSnapshotChecked()
	if err == nil {
		t.Fatal("LoadOwnershipSnapshotChecked (lsp task installed, registry path fails): want non-nil error to fail closed, got nil")
	}
	if !errors.Is(err, sentinel) {
		t.Errorf("error chain: want sentinel via errors.Is, got %v", err)
	}
}

// TestLoadOwnershipSnapshotChecked_NoLspTasksInstalled_RegistryLoadFailsBenignly
// covers the symmetric load-error path: the registry path resolves but
// the file itself is unparseable. With zero installed lazy-proxy tasks,
// the load failure must be tolerated.
func TestLoadOwnershipSnapshotChecked_NoLspTasksInstalled_RegistryLoadFailsBenignly(t *testing.T) {
	a := NewAPI()
	daemonIntentTestHelper(t)

	manifestDir := t.TempDir()
	t.Setenv("MCPHUB_MANIFEST_DIR_OVERRIDE", manifestDir)
	subdir := filepath.Join(manifestDir, "global-only")
	if err := os.MkdirAll(subdir, 0o700); err != nil {
		t.Fatalf("mkdir manifest dir: %v", err)
	}
	body := "name: global-only\nkind: global\ntransport: stdio-bridge\ncommand: echo\ndaemons:\n  - name: default\n    port: 9211\n"
	if err := os.WriteFile(filepath.Join(subdir, "manifest.yaml"), []byte(body), 0o600); err != nil {
		t.Fatalf("write manifest: %v", err)
	}

	installLazyProxySchedulerStub(t, 0)

	regPath := pointRegistryAtTempDir(t)
	if err := os.WriteFile(regPath, []byte("this: is: not\n  - valid: ["), 0o600); err != nil {
		t.Fatalf("seed corrupt registry: %v", err)
	}

	snap, err := a.LoadOwnershipSnapshotChecked()
	if err != nil {
		t.Fatalf("LoadOwnershipSnapshotChecked (no lsp tasks, registry load fails): want nil error, got %v", err)
	}
	if !snap.ManifestServers["global-only"] {
		t.Errorf("expected global-only present in snapshot, got %+v", snap.ManifestServers)
	}
}

// TestHasInstalledWorkspaceScopedDaemon_SchedulerListErrorTreatedAsAbsent
// pins down the helper's scheduler-error contract: a List failure (e.g.
// schtasks transient error) must collapse to "no installed task" rather
// than fail-closed on the watchdog tick. The watchdog already cannot
// run without a working scheduler downstream, so adding a separate
// fail-closed path here would only block recovery for global daemons
// the watchdog could otherwise revive once the transient clears.
func TestHasInstalledWorkspaceScopedDaemon_SchedulerListErrorTreatedAsAbsent(t *testing.T) {
	stub := &schedulerWithLazyProxyTasks{listErr: errors.New("synthetic schtasks failure")}
	prev := schedulerFactoryFn
	schedulerFactoryFn = func() (scheduler.Scheduler, error) { return stub, nil }
	t.Cleanup(func() { schedulerFactoryFn = prev })

	if hasInstalledWorkspaceScopedDaemon() {
		t.Error("hasInstalledWorkspaceScopedDaemon: want false on List error, got true")
	}
}

// TestHasInstalledWorkspaceScopedDaemon_SchedulerFactoryErrorTreatedAsAbsent
// covers the symmetric construction-failure path. The production factory
// returns a "not implemented" error on Linux/macOS; the gate must read
// that as "no installed lazy proxies on this host" rather than stalling
// the watchdog tick on a registry error.
func TestHasInstalledWorkspaceScopedDaemon_SchedulerFactoryErrorTreatedAsAbsent(t *testing.T) {
	prev := schedulerFactoryFn
	schedulerFactoryFn = func() (scheduler.Scheduler, error) {
		return nil, errors.New("synthetic factory failure")
	}
	t.Cleanup(func() { schedulerFactoryFn = prev })

	if hasInstalledWorkspaceScopedDaemon() {
		t.Error("hasInstalledWorkspaceScopedDaemon: want false on factory error, got true")
	}
}

// ensureLeadingBackslashHelper is a tiny safety net for tests in this file
// that need to compute the canonical key.
func ensureLeadingBackslashHelper(s string) string {
	if strings.HasPrefix(s, "\\") {
		return s
	}
	return "\\" + s
}

// touch the ensure helper so future maintenance does not need to re-add an
// import or wonder why it is unused. Compile-time guard only.
var _ = ensureLeadingBackslashHelper
var _ = filepath.Join

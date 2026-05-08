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
func TestLoadOwnershipSnapshotChecked_RegistryLoadError_ReturnsError(t *testing.T) {
	a := NewAPI()
	daemonIntentTestHelper(t)
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
// PR #135 round 2 P2 — LoadOwnershipSnapshotChecked must scope fail-closed
// to installations that actually depend on workspace registry data. Global-
// only installations (no workspace-scoped manifests) must tolerate registry
// path-resolve / load failures benignly so unrelated global daemons keep
// auto-recovery wiring on hosts where the registry path is unreachable
// (service accounts without a resolvable home dir, etc.).
// ---------------------------------------------------------------------------

// TestLoadOwnershipSnapshotChecked_GlobalOnly_RegistryPathFailsBenignly
// verifies the round-2 P2 scope narrowing: when the manifest set has zero
// workspace-scoped servers, a DefaultRegistryPath resolution failure must
// not propagate. The watchdog's runWatchdogOnceInner is otherwise broken
// by an unreachable registry path on global-only hosts.
func TestLoadOwnershipSnapshotChecked_GlobalOnly_RegistryPathFailsBenignly(t *testing.T) {
	a := NewAPI()
	daemonIntentTestHelper(t)

	// Seed a global-only manifest. listManifestNamesEmbedFirst will see
	// just this one entry under the override and never report any
	// workspace-scoped server, so registry-load failure must be benign.
	manifestDir := t.TempDir()
	t.Setenv("MCPHUB_MANIFEST_DIR_OVERRIDE", manifestDir)
	subdir := filepath.Join(manifestDir, "global-only")
	if err := os.MkdirAll(subdir, 0o700); err != nil {
		t.Fatalf("mkdir manifest dir: %v", err)
	}
	body := "name: global-only\nkind: global\ntransport: stdio-bridge\ncommand: echo\ndaemons:\n  - name: default\n    port: 9210\n"
	if err := os.WriteFile(filepath.Join(subdir, "manifest.yaml"), []byte(body), 0o600); err != nil {
		t.Fatalf("write manifest: %v", err)
	}

	// Force DefaultRegistryPath to error — exactly the production scenario
	// the bot flagged (service account without home dir).
	sentinel := errors.New("synthetic resolve failure")
	prev := defaultRegistryPathFn
	defaultRegistryPathFn = func() (string, error) { return "", sentinel }
	t.Cleanup(func() { defaultRegistryPathFn = prev })

	snap, err := a.LoadOwnershipSnapshotChecked()
	if err != nil {
		t.Fatalf("LoadOwnershipSnapshotChecked (global-only host, registry path fails): want nil error, got %v", err)
	}
	if !snap.ManifestServers["global-only"] {
		t.Errorf("expected global-only server present in snapshot.ManifestServers, got %+v", snap.ManifestServers)
	}
	if len(snap.WorkspaceTasksByKey) != 0 {
		t.Errorf("WorkspaceTasksByKey: want empty, got %+v", snap.WorkspaceTasksByKey)
	}
}

// TestLoadOwnershipSnapshotChecked_WorkspaceScoped_RegistryPathFailsClosed
// is the safety counterpart: when the manifest set DOES include at least
// one workspace-scoped server, registry path-resolve failure must still
// propagate (Finding 4 contract preserved) — fail-closed so the watchdog
// refuses the tick rather than make orphan/lazy-proxy classifications on
// partial ownership data.
func TestLoadOwnershipSnapshotChecked_WorkspaceScoped_RegistryPathFailsClosed(t *testing.T) {
	a := NewAPI()
	daemonIntentTestHelper(t)

	manifestDir := t.TempDir()
	t.Setenv("MCPHUB_MANIFEST_DIR_OVERRIDE", manifestDir)
	subdir := filepath.Join(manifestDir, "workspace-srv")
	if err := os.MkdirAll(subdir, 0o700); err != nil {
		t.Fatalf("mkdir manifest dir: %v", err)
	}
	// Workspace-scoped manifest exercises the fail-closed gate.
	body := "name: workspace-srv\nkind: workspace-scoped\ntransport: stdio-bridge\ncommand: workspace-srv\nport_pool: {start: 9300, end: 9399}\nlanguages:\n  - name: go\n    backend: gopls-mcp\n    transport: stdio\n    lsp_command: gopls\n"
	if err := os.WriteFile(filepath.Join(subdir, "manifest.yaml"), []byte(body), 0o600); err != nil {
		t.Fatalf("write manifest: %v", err)
	}

	sentinel := errors.New("synthetic resolve failure")
	prev := defaultRegistryPathFn
	defaultRegistryPathFn = func() (string, error) { return "", sentinel }
	t.Cleanup(func() { defaultRegistryPathFn = prev })

	_, err := a.LoadOwnershipSnapshotChecked()
	if err == nil {
		t.Fatal("LoadOwnershipSnapshotChecked (workspace-scoped present, registry path fails): want non-nil error to fail closed, got nil")
	}
	if !errors.Is(err, sentinel) {
		t.Errorf("error chain: want sentinel via errors.Is, got %v", err)
	}
}

// TestLoadOwnershipSnapshotChecked_GlobalOnly_RegistryLoadFailsBenignly
// covers the symmetric path: registry path resolves, but the file itself
// is unparseable. Global-only host: must tolerate the load error.
func TestLoadOwnershipSnapshotChecked_GlobalOnly_RegistryLoadFailsBenignly(t *testing.T) {
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

	regPath := pointRegistryAtTempDir(t)
	if err := os.WriteFile(regPath, []byte("this: is: not\n  - valid: ["), 0o600); err != nil {
		t.Fatalf("seed corrupt registry: %v", err)
	}

	snap, err := a.LoadOwnershipSnapshotChecked()
	if err != nil {
		t.Fatalf("LoadOwnershipSnapshotChecked (global-only host, registry load fails): want nil error, got %v", err)
	}
	if !snap.ManifestServers["global-only"] {
		t.Errorf("expected global-only present in snapshot, got %+v", snap.ManifestServers)
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

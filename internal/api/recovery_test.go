// Package api — Task 7 unit tests for the strictly-pure
// RecoverStoppedDaemons decision tree + IsRealFailure exported predicate
// (watchdog plan v13 §1, §11, §12, §16, §18, §19, §21, §22).
//
// recovery_test.go covers EVERY action vocabulary entry the decision
// tree can yield ("maintenance", "orphan", "lazy-proxy-failed-lifecycle",
// "user-stop", "user-disabled", "chronic-failure", "uninstalled",
// "clock-skew-future-suspect", "restart-pending-skipped", "cooldown",
// "suspicious-xml", "restart") plus the no-decision skip cases (Running,
// success, never-run sentinel, TS info codes, non-failure exit codes).
// The IsRealFailure table tests guard the full LastResult range
// classification (0, -1, TS info, user-program 1..0xFFFF, HRESULT bit
// 31 set, out-of-range positive past 0xFFFF).
//
// Strict-purity assertion: the tests construct fake CooldownReader /
// OwnedXMLValidator / DaemonRegistry implementations that record method
// calls. RecoverStoppedDaemons must never invoke the mutating methods
// (RecordAttempt, RecordRunning, MarkRestartPending, ClearRestartPending)
// on Cooldown — those belong to the driver. The fakes only implement
// CooldownReader so a compile-time check enforces the constraint.
package api

import (
	"testing"
	"time"
)

// ---------------------------------------------------------------------------
// Test fakes (CooldownReader / OwnedXMLValidator / DaemonRegistry).
// ---------------------------------------------------------------------------

// fakeCoolReader is the minimum CooldownReader the recovery decision
// tree requires. Per-task-name maps so tests can express "task A is in
// cooldown but task B is due" in one struct.
type fakeCoolReader struct {
	due                 map[string]bool
	chronicLimitReached map[string]bool
	attemptsInWindow    map[string]int
	restartPending      map[string]bool

	dueDefault                 bool
	chronicLimitReachedDefault bool
	attemptsInWindowDefault    int
	restartPendingDefault      bool
}

func (f *fakeCoolReader) Due(name string, _ time.Time) bool {
	if v, ok := f.due[name]; ok {
		return v
	}
	return f.dueDefault
}

func (f *fakeCoolReader) ChronicLimitReached(name string) bool {
	if v, ok := f.chronicLimitReached[name]; ok {
		return v
	}
	return f.chronicLimitReachedDefault
}

func (f *fakeCoolReader) AttemptsInWindow(name string) int {
	if v, ok := f.attemptsInWindow[name]; ok {
		return v
	}
	return f.attemptsInWindowDefault
}

func (f *fakeCoolReader) IsRestartPending(name string, _ time.Time) bool {
	if v, ok := f.restartPending[name]; ok {
		return v
	}
	return f.restartPendingDefault
}

// fakeXMLValidator mocks OwnedXMLValidator. Default returns true (most
// tests want validation to pass); per-name overrides flip individual
// rows to "suspicious-xml".
type fakeXMLValidator struct {
	ownedAndValid        map[string]bool
	ownedAndValidDefault bool
}

func newFakeXMLValidatorPass() *fakeXMLValidator {
	return &fakeXMLValidator{ownedAndValidDefault: true}
}

func (f *fakeXMLValidator) IsOwnedAndValid(name string) bool {
	if v, ok := f.ownedAndValid[name]; ok {
		return v
	}
	return f.ownedAndValidDefault
}

// fakeRegistry mocks DaemonRegistry. Default-true is convenient for
// tests where every row is managed; per-name false flips a row to
// "orphan".
type fakeRegistry struct {
	managed        map[string]bool
	managedDefault bool
}

func newFakeRegistryAllManaged() *fakeRegistry {
	return &fakeRegistry{managedDefault: true}
}

func (f *fakeRegistry) IsManagedDaemon(name string) bool {
	if v, ok := f.managed[name]; ok {
		return v
	}
	return f.managedDefault
}

// ---------------------------------------------------------------------------
// IsRealFailure — table tests for every documented LastResult range.
// ---------------------------------------------------------------------------

func TestIsRealFailure_Table(t *testing.T) {
	cases := []struct {
		name       string
		lastResult int32
		want       bool
	}{
		// success / sentinels
		{"0 success → false", 0, false},
		{"-1 placeholder (never run sentinel) → false", -1, false},

		// TS info codes — NOT failures
		{"0x41300 (ready to run) → false", 0x41300, false},
		{"0x41301 (currently running) → false", 0x41301, false},
		{"0x41303 (task has not yet run) → false", 0x41303, false},
		{"0x4130F (top of TS info range) → false", 0x4130F, false},

		// boundaries around TS info range. Both values fall outside the
		// user-program range (1..0xFFFF = 65535) AND outside the TS info
		// range; conservative classification is "not a failure".
		{"0x412FF (just below TS info range, > 0xFFFF) → false (conservative)", 0x412FF, false},
		{"0x41310 (just above TS info range, > 0xFFFF) → false (conservative)", 0x41310, false},

		// user-program exit codes 1..0xFFFF
		{"1 (typical exit failure) → true", 1, true},
		{"2 → true", 2, true},
		{"127 → true", 127, true},
		{"0xFFFE → true", 0xFFFE, true},
		{"0xFFFF (top of user-program range) → true", 0xFFFF, true},

		// out-of-range positive (above 0xFFFF, not in TS info range) → conservative false
		{"0x10000 (just above 0xFFFF, conservative false)", 0x10000, false},
		{"0x100000 (large positive, conservative false)", 0x100000, false},

		// HRESULT/NTSTATUS — bit 31 set, negative when read as int32 → true
		{"-2 (negative) → true", -2, true},
		{"-2147467259 (E_FAIL HRESULT) → true", -2147467259, true},
		{"INT32_MIN (1<<31) → true", -1 << 31, true},
		{"-2 != -1 → true", -2, true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := IsRealFailure(tc.lastResult)
			if got != tc.want {
				t.Errorf("IsRealFailure(%d / 0x%X) = %v, want %v",
					tc.lastResult, uint32(tc.lastResult), got, tc.want)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// RecoverStoppedDaemons — decision-tree action vocabulary.
// ---------------------------------------------------------------------------

// findDecision returns the (one and only) decision in `decs` whose
// TaskName matches `name`. Test helper. nil if no match.
func findDecision(decs []RecoveryDecision, name string) *RecoveryDecision {
	for i := range decs {
		if decs[i].TaskName == name {
			return &decs[i]
		}
	}
	return nil
}

// expectAction is a fluent assertion: there is exactly one decision
// for the given task name, and it carries the expected Action / Reason
// pair. Empty Reason expectation means "do not assert Reason".
func expectAction(t *testing.T, decs []RecoveryDecision, taskName, action, reason string) {
	t.Helper()
	d := findDecision(decs, taskName)
	if d == nil {
		t.Fatalf("expected decision for %q action=%q; got %d decisions: %+v",
			taskName, action, len(decs), decs)
	}
	if d.Action != action {
		t.Errorf("decision[%q].Action = %q, want %q (full: %+v)",
			taskName, d.Action, action, *d)
	}
	if reason != "" && d.Reason != reason {
		t.Errorf("decision[%q].Reason = %q, want %q (full: %+v)",
			taskName, d.Reason, reason, *d)
	}
}

// expectNoDecision asserts no decision was emitted for the task name.
// Healthy-Running rows skip silently per plan §1.
func expectNoDecision(t *testing.T, decs []RecoveryDecision, taskName string) {
	t.Helper()
	if d := findDecision(decs, taskName); d != nil {
		t.Errorf("expected no decision for %q; got %+v", taskName, *d)
	}
}

// emptyIntent is a clean DaemonIntentFile (no entries — every task
// defaults to running).
func emptyIntent() DaemonIntentFile {
	return DaemonIntentFile{Tasks: map[string]DaemonIntent{}}
}

func TestRecoverStoppedDaemons_Restart_ReadyClean(t *testing.T) {
	now := time.Date(2026, 5, 7, 12, 0, 0, 0, time.UTC)
	taskName := "\\mcp-local-hub-memory-default"
	rows := []DaemonStatus{
		{Server: "memory", Daemon: "default", TaskName: taskName,
			State: "Ready", LastResult: 1},
	}
	cool := &fakeCoolReader{dueDefault: true}
	v := newFakeXMLValidatorPass()
	reg := newFakeRegistryAllManaged()

	decs := RecoverStoppedDaemons(now, rows, emptyIntent(), cool, v, reg)
	expectAction(t, decs, taskName, "restart", "")

	d := findDecision(decs, taskName)
	if d.Server != "memory" || d.Daemon != "default" {
		t.Errorf("Server/Daemon mismatch: got %s/%s want memory/default", d.Server, d.Daemon)
	}
	// Attempt = AttemptsInWindow + 1 (zero-default + 1 = 1 on first)
	if d.Attempt != 1 {
		t.Errorf("Attempt = %d, want 1 (default 0 + 1)", d.Attempt)
	}
}

func TestRecoverStoppedDaemons_Restart_StoppedClean(t *testing.T) {
	now := time.Date(2026, 5, 7, 12, 0, 0, 0, time.UTC)
	taskName := "\\mcp-local-hub-memory-default"
	rows := []DaemonStatus{
		{Server: "memory", Daemon: "default", TaskName: taskName,
			State: "Stopped", LastResult: 1},
	}
	cool := &fakeCoolReader{dueDefault: true}
	decs := RecoverStoppedDaemons(now, rows, emptyIntent(), cool,
		newFakeXMLValidatorPass(), newFakeRegistryAllManaged())
	expectAction(t, decs, taskName, "restart", "")
}

func TestRecoverStoppedDaemons_Restart_FailedClean(t *testing.T) {
	now := time.Date(2026, 5, 7, 12, 0, 0, 0, time.UTC)
	taskName := "\\mcp-local-hub-memory-default"
	rows := []DaemonStatus{
		{Server: "memory", Daemon: "default", TaskName: taskName,
			State: "Failed", LastResult: 1},
	}
	cool := &fakeCoolReader{dueDefault: true}
	decs := RecoverStoppedDaemons(now, rows, emptyIntent(), cool,
		newFakeXMLValidatorPass(), newFakeRegistryAllManaged())
	expectAction(t, decs, taskName, "restart", "")
}

func TestRecoverStoppedDaemons_AttemptCountReflectsCool(t *testing.T) {
	now := time.Date(2026, 5, 7, 12, 0, 0, 0, time.UTC)
	taskName := "\\mcp-local-hub-memory-default"
	rows := []DaemonStatus{
		{Server: "memory", Daemon: "default", TaskName: taskName,
			State: "Ready", LastResult: 1},
	}
	cool := &fakeCoolReader{
		dueDefault:       true,
		attemptsInWindow: map[string]int{taskName: 3},
	}
	decs := RecoverStoppedDaemons(now, rows, emptyIntent(), cool,
		newFakeXMLValidatorPass(), newFakeRegistryAllManaged())
	d := findDecision(decs, taskName)
	if d == nil {
		t.Fatalf("expected restart decision for %q", taskName)
	}
	if d.Attempt != 4 {
		t.Errorf("Attempt = %d, want 4 (AttemptsInWindow=3 + 1)", d.Attempt)
	}
}

func TestRecoverStoppedDaemons_Running_NoDecision(t *testing.T) {
	now := time.Date(2026, 5, 7, 12, 0, 0, 0, time.UTC)
	taskName := "\\mcp-local-hub-memory-default"
	rows := []DaemonStatus{
		{Server: "memory", Daemon: "default", TaskName: taskName,
			State: "Running", LastResult: 0},
	}
	decs := RecoverStoppedDaemons(now, rows, emptyIntent(),
		&fakeCoolReader{dueDefault: true},
		newFakeXMLValidatorPass(),
		newFakeRegistryAllManaged())
	expectNoDecision(t, decs, taskName)
}

func TestRecoverStoppedDaemons_Running_LazyProxyFailedLifecycle(t *testing.T) {
	now := time.Date(2026, 5, 7, 12, 0, 0, 0, time.UTC)
	taskName := "\\mcp-local-hub-lsp-abc123-go"
	rows := []DaemonStatus{
		{Server: "lsp", Daemon: "go", TaskName: taskName,
			State: "Running", IsWorkspaceScoped: true,
			Lifecycle: LifecycleFailed, LastResult: 0},
	}
	decs := RecoverStoppedDaemons(now, rows, emptyIntent(),
		&fakeCoolReader{dueDefault: true},
		newFakeXMLValidatorPass(),
		newFakeRegistryAllManaged())
	expectAction(t, decs, taskName, "lazy-proxy-failed-lifecycle", "")
}

func TestRecoverStoppedDaemons_Running_LazyProxyActive_NoDecision(t *testing.T) {
	now := time.Date(2026, 5, 7, 12, 0, 0, 0, time.UTC)
	taskName := "\\mcp-local-hub-lsp-abc123-go"
	rows := []DaemonStatus{
		{Server: "lsp", Daemon: "go", TaskName: taskName,
			State: "Running", IsWorkspaceScoped: true,
			Lifecycle: LifecycleActive, LastResult: 0},
	}
	decs := RecoverStoppedDaemons(now, rows, emptyIntent(),
		&fakeCoolReader{dueDefault: true},
		newFakeXMLValidatorPass(),
		newFakeRegistryAllManaged())
	expectNoDecision(t, decs, taskName)
}

func TestRecoverStoppedDaemons_Running_LazyProxyConfigured_NoDecision(t *testing.T) {
	now := time.Date(2026, 5, 7, 12, 0, 0, 0, time.UTC)
	taskName := "\\mcp-local-hub-lsp-abc123-go"
	rows := []DaemonStatus{
		{Server: "lsp", Daemon: "go", TaskName: taskName,
			State: "Running", IsWorkspaceScoped: true,
			Lifecycle: LifecycleConfigured, LastResult: 0},
	}
	decs := RecoverStoppedDaemons(now, rows, emptyIntent(),
		&fakeCoolReader{dueDefault: true},
		newFakeXMLValidatorPass(),
		newFakeRegistryAllManaged())
	expectNoDecision(t, decs, taskName)
}

func TestRecoverStoppedDaemons_Ready_Success_NoDecision(t *testing.T) {
	now := time.Date(2026, 5, 7, 12, 0, 0, 0, time.UTC)
	taskName := "\\mcp-local-hub-memory-default"
	rows := []DaemonStatus{
		{Server: "memory", Daemon: "default", TaskName: taskName,
			State: "Ready", LastResult: 0},
	}
	decs := RecoverStoppedDaemons(now, rows, emptyIntent(),
		&fakeCoolReader{dueDefault: true},
		newFakeXMLValidatorPass(),
		newFakeRegistryAllManaged())
	expectNoDecision(t, decs, taskName)
}

func TestRecoverStoppedDaemons_Ready_NeverRunSentinel_NoDecision(t *testing.T) {
	now := time.Date(2026, 5, 7, 12, 0, 0, 0, time.UTC)
	taskName := "\\mcp-local-hub-memory-default"
	rows := []DaemonStatus{
		{Server: "memory", Daemon: "default", TaskName: taskName,
			State: "Ready", LastResult: -1},
	}
	decs := RecoverStoppedDaemons(now, rows, emptyIntent(),
		&fakeCoolReader{dueDefault: true},
		newFakeXMLValidatorPass(),
		newFakeRegistryAllManaged())
	expectNoDecision(t, decs, taskName)
}

func TestRecoverStoppedDaemons_Ready_TaskSchedulerInfoCode_NoDecision(t *testing.T) {
	now := time.Date(2026, 5, 7, 12, 0, 0, 0, time.UTC)
	taskName := "\\mcp-local-hub-memory-default"
	rows := []DaemonStatus{
		{Server: "memory", Daemon: "default", TaskName: taskName,
			State: "Ready", LastResult: 0x41301},
	}
	decs := RecoverStoppedDaemons(now, rows, emptyIntent(),
		&fakeCoolReader{dueDefault: true},
		newFakeXMLValidatorPass(),
		newFakeRegistryAllManaged())
	expectNoDecision(t, decs, taskName)
}

func TestRecoverStoppedDaemons_Maintenance_Watchdog(t *testing.T) {
	now := time.Date(2026, 5, 7, 12, 0, 0, 0, time.UTC)
	taskName := "\\mcp-local-hub-watchdog"
	rows := []DaemonStatus{
		{Server: "", Daemon: "", TaskName: taskName,
			State: "Ready", LastResult: 1},
	}
	decs := RecoverStoppedDaemons(now, rows, emptyIntent(),
		&fakeCoolReader{dueDefault: true},
		newFakeXMLValidatorPass(),
		newFakeRegistryAllManaged())
	expectAction(t, decs, taskName, "maintenance", "")
}

func TestRecoverStoppedDaemons_Maintenance_WeeklyRefresh(t *testing.T) {
	now := time.Date(2026, 5, 7, 12, 0, 0, 0, time.UTC)
	taskName := "\\mcp-local-hub-memory-weekly-refresh"
	rows := []DaemonStatus{
		{Server: "", Daemon: "", TaskName: taskName,
			State: "Ready", LastResult: 1},
	}
	decs := RecoverStoppedDaemons(now, rows, emptyIntent(),
		&fakeCoolReader{dueDefault: true},
		newFakeXMLValidatorPass(),
		newFakeRegistryAllManaged())
	expectAction(t, decs, taskName, "maintenance", "")
}

func TestRecoverStoppedDaemons_Orphan_NotInRegistry(t *testing.T) {
	now := time.Date(2026, 5, 7, 12, 0, 0, 0, time.UTC)
	taskName := "\\mcp-local-hub-orphan-default"
	rows := []DaemonStatus{
		{Server: "orphan", Daemon: "default", TaskName: taskName,
			State: "Ready", LastResult: 1},
	}
	reg := &fakeRegistry{managedDefault: false}
	decs := RecoverStoppedDaemons(now, rows, emptyIntent(),
		&fakeCoolReader{dueDefault: true},
		newFakeXMLValidatorPass(),
		reg)
	expectAction(t, decs, taskName, "orphan", "")
}

func TestRecoverStoppedDaemons_UserStop_WithinTTL(t *testing.T) {
	now := time.Date(2026, 5, 7, 12, 0, 0, 0, time.UTC)
	taskName := "\\mcp-local-hub-memory-default"
	rows := []DaemonStatus{
		{Server: "memory", Daemon: "default", TaskName: taskName,
			State: "Ready", LastResult: 1},
	}
	intent := DaemonIntentFile{Tasks: map[string]DaemonIntent{
		taskName: {
			Desired:   IntentDesiredStopped,
			Reason:    IntentReasonUserStop,
			UpdatedAt: now.Add(-1 * time.Hour),
		},
	}}
	decs := RecoverStoppedDaemons(now, rows, intent,
		&fakeCoolReader{dueDefault: true},
		newFakeXMLValidatorPass(),
		newFakeRegistryAllManaged())
	expectAction(t, decs, taskName, "user-stop", "user-stop")
}

func TestRecoverStoppedDaemons_UserStop_WithinTTL_BackslashMismatchStillStops(t *testing.T) {
	now := time.Date(2026, 5, 8, 12, 0, 0, 0, time.UTC)
	row := DaemonStatus{
		TaskName: "\\mcp-local-hub-time-default",
		Server:   "time",
		Daemon:   "default",
		State:    "Stopped",
		// real failure
		LastResult: 1,
	}
	rows := []DaemonStatus{row}
	reg := newFakeRegistryAllManaged()
	cool := &fakeCoolReader{
		dueDefault: true,
	}
	v := newFakeXMLValidatorPass()

	intent := DaemonIntentFile{
		Tasks: map[string]DaemonIntent{
			"mcp-local-hub-time-default": {
				Desired:   IntentDesiredStopped,
				Reason:    IntentReasonUserStop,
				UpdatedAt: now.Add(-1 * time.Hour),
			},
		},
	}
	decs := RecoverStoppedDaemons(now, rows, intent, cool, v, reg)
	expectAction(t, decs, row.TaskName, "user-stop", "user-stop")
}

func TestRecoverStoppedDaemons_UserStop_AfterTTL_Restart(t *testing.T) {
	now := time.Date(2026, 5, 7, 12, 0, 0, 0, time.UTC)
	taskName := "\\mcp-local-hub-memory-default"
	rows := []DaemonStatus{
		{Server: "memory", Daemon: "default", TaskName: taskName,
			State: "Ready", LastResult: 1},
	}
	// 25 hours ago > StopIntentTTL (24h) → user-stop expired.
	intent := DaemonIntentFile{Tasks: map[string]DaemonIntent{
		taskName: {
			Desired:   IntentDesiredStopped,
			Reason:    IntentReasonUserStop,
			UpdatedAt: now.Add(-25 * time.Hour),
		},
	}}
	decs := RecoverStoppedDaemons(now, rows, intent,
		&fakeCoolReader{dueDefault: true},
		newFakeXMLValidatorPass(),
		newFakeRegistryAllManaged())
	expectAction(t, decs, taskName, "restart", "")
}

func TestRecoverStoppedDaemons_UserDisabled_Indefinite(t *testing.T) {
	now := time.Date(2026, 5, 7, 12, 0, 0, 0, time.UTC)
	taskName := "\\mcp-local-hub-memory-default"
	rows := []DaemonStatus{
		{Server: "memory", Daemon: "default", TaskName: taskName,
			State: "Ready", LastResult: 1},
	}
	intent := DaemonIntentFile{Tasks: map[string]DaemonIntent{
		taskName: {
			Desired:   IntentDesiredStopped,
			Reason:    IntentReasonUserDisabled,
			UpdatedAt: now.Add(-100 * 24 * time.Hour), // 100 days ago, still active
		},
	}}
	decs := RecoverStoppedDaemons(now, rows, intent,
		&fakeCoolReader{dueDefault: true},
		newFakeXMLValidatorPass(),
		newFakeRegistryAllManaged())
	expectAction(t, decs, taskName, "user-disabled", "user-disabled")
}

func TestRecoverStoppedDaemons_ChronicFailure_Intent(t *testing.T) {
	now := time.Date(2026, 5, 7, 12, 0, 0, 0, time.UTC)
	taskName := "\\mcp-local-hub-memory-default"
	rows := []DaemonStatus{
		{Server: "memory", Daemon: "default", TaskName: taskName,
			State: "Ready", LastResult: 1},
	}
	intent := DaemonIntentFile{Tasks: map[string]DaemonIntent{
		taskName: {
			Desired:   IntentDesiredStopped,
			Reason:    IntentReasonChronicFailure,
			UpdatedAt: now.Add(-30 * time.Minute),
		},
	}}
	decs := RecoverStoppedDaemons(now, rows, intent,
		&fakeCoolReader{dueDefault: true},
		newFakeXMLValidatorPass(),
		newFakeRegistryAllManaged())
	expectAction(t, decs, taskName, "chronic-failure", "chronic-failure")
}

func TestRecoverStoppedDaemons_Uninstalled_Intent(t *testing.T) {
	now := time.Date(2026, 5, 7, 12, 0, 0, 0, time.UTC)
	taskName := "\\mcp-local-hub-memory-default"
	rows := []DaemonStatus{
		{Server: "memory", Daemon: "default", TaskName: taskName,
			State: "Ready", LastResult: 1},
	}
	intent := DaemonIntentFile{Tasks: map[string]DaemonIntent{
		taskName: {
			Desired:   IntentDesiredStopped,
			Reason:    IntentReasonUninstalled,
			UpdatedAt: now.Add(-30 * time.Minute),
		},
	}}
	decs := RecoverStoppedDaemons(now, rows, intent,
		&fakeCoolReader{dueDefault: true},
		newFakeXMLValidatorPass(),
		newFakeRegistryAllManaged())
	expectAction(t, decs, taskName, "uninstalled", "uninstalled")
}

func TestRecoverStoppedDaemons_ClockSkewFutureSuspect(t *testing.T) {
	now := time.Date(2026, 5, 7, 12, 0, 0, 0, time.UTC)
	taskName := "\\mcp-local-hub-memory-default"
	rows := []DaemonStatus{
		{Server: "memory", Daemon: "default", TaskName: taskName,
			State: "Ready", LastResult: 1},
	}
	// 10 minutes in the future > 5min ClockSkewFutureTolerance → suspect.
	intent := DaemonIntentFile{Tasks: map[string]DaemonIntent{
		taskName: {
			Desired:   IntentDesiredStopped,
			Reason:    IntentReasonUserStop,
			UpdatedAt: now.Add(10 * time.Minute),
		},
	}}
	decs := RecoverStoppedDaemons(now, rows, intent,
		&fakeCoolReader{dueDefault: true},
		newFakeXMLValidatorPass(),
		newFakeRegistryAllManaged())
	expectAction(t, decs, taskName, "clock-skew-future-suspect", "clock-skew-future-suspect")
}

func TestRecoverStoppedDaemons_RestartPending_Skipped(t *testing.T) {
	now := time.Date(2026, 5, 7, 12, 0, 0, 0, time.UTC)
	taskName := "\\mcp-local-hub-memory-default"
	rows := []DaemonStatus{
		{Server: "memory", Daemon: "default", TaskName: taskName,
			State: "Ready", LastResult: 1},
	}
	cool := &fakeCoolReader{
		dueDefault:     true,
		restartPending: map[string]bool{taskName: true},
	}
	decs := RecoverStoppedDaemons(now, rows, emptyIntent(), cool,
		newFakeXMLValidatorPass(),
		newFakeRegistryAllManaged())
	expectAction(t, decs, taskName, "restart-pending-skipped", "")
}

func TestRecoverStoppedDaemons_Cooldown_NotDue(t *testing.T) {
	now := time.Date(2026, 5, 7, 12, 0, 0, 0, time.UTC)
	taskName := "\\mcp-local-hub-memory-default"
	rows := []DaemonStatus{
		{Server: "memory", Daemon: "default", TaskName: taskName,
			State: "Ready", LastResult: 1},
	}
	cool := &fakeCoolReader{
		dueDefault: false,
		due:        map[string]bool{taskName: false},
	}
	decs := RecoverStoppedDaemons(now, rows, emptyIntent(), cool,
		newFakeXMLValidatorPass(),
		newFakeRegistryAllManaged())
	expectAction(t, decs, taskName, "cooldown", "")
}

func TestRecoverStoppedDaemons_Cooldown_ChronicLimit(t *testing.T) {
	now := time.Date(2026, 5, 7, 12, 0, 0, 0, time.UTC)
	taskName := "\\mcp-local-hub-memory-default"
	rows := []DaemonStatus{
		{Server: "memory", Daemon: "default", TaskName: taskName,
			State: "Ready", LastResult: 1},
	}
	cool := &fakeCoolReader{
		dueDefault:                 true,
		chronicLimitReachedDefault: true,
	}
	decs := RecoverStoppedDaemons(now, rows, emptyIntent(), cool,
		newFakeXMLValidatorPass(),
		newFakeRegistryAllManaged())
	expectAction(t, decs, taskName, "chronic-failure", "")
}

func TestRecoverStoppedDaemons_SuspiciousXML(t *testing.T) {
	now := time.Date(2026, 5, 7, 12, 0, 0, 0, time.UTC)
	taskName := "\\mcp-local-hub-memory-default"
	rows := []DaemonStatus{
		{Server: "memory", Daemon: "default", TaskName: taskName,
			State: "Ready", LastResult: 1},
	}
	cool := &fakeCoolReader{dueDefault: true}
	v := &fakeXMLValidator{ownedAndValidDefault: false}
	decs := RecoverStoppedDaemons(now, rows, emptyIntent(), cool, v,
		newFakeRegistryAllManaged())
	expectAction(t, decs, taskName, "suspicious-xml", "")
}

// Bootstrap-mixed: one managed daemon has a missing intent entry (the
// canonical "missing intent → default running") + another is an orphan.
func TestRecoverStoppedDaemons_BootstrapMixed(t *testing.T) {
	now := time.Date(2026, 5, 7, 12, 0, 0, 0, time.UTC)
	memoryTask := "\\mcp-local-hub-memory-default"
	orphanTask := "\\mcp-local-hub-orphan-default"

	rows := []DaemonStatus{
		{Server: "memory", Daemon: "default", TaskName: memoryTask,
			State: "Ready", LastResult: 1},
		{Server: "orphan", Daemon: "default", TaskName: orphanTask,
			State: "Ready", LastResult: 1},
	}
	// intent file has an entry for memory (running) but not for orphan.
	// And the registry only manages memory.
	intent := DaemonIntentFile{Tasks: map[string]DaemonIntent{
		memoryTask: {
			Desired:   IntentDesiredRunning,
			Reason:    IntentReasonInstall,
			UpdatedAt: now.Add(-1 * time.Hour),
		},
	}}
	cool := &fakeCoolReader{dueDefault: true}
	reg := &fakeRegistry{
		managed:        map[string]bool{memoryTask: true},
		managedDefault: false,
	}
	decs := RecoverStoppedDaemons(now, rows, intent, cool,
		newFakeXMLValidatorPass(),
		reg)

	expectAction(t, decs, memoryTask, "restart", "")
	expectAction(t, decs, orphanTask, "orphan", "")
}

func TestRecoverStoppedDaemons_Bootstrap_MissingIntent_Managed_Restart(t *testing.T) {
	now := time.Date(2026, 5, 7, 12, 0, 0, 0, time.UTC)
	taskName := "\\mcp-local-hub-memory-default"
	rows := []DaemonStatus{
		{Server: "memory", Daemon: "default", TaskName: taskName,
			State: "Ready", LastResult: 1},
	}
	// Empty intent (bootstrap = missing) + managed → restart.
	cool := &fakeCoolReader{dueDefault: true}
	decs := RecoverStoppedDaemons(now, rows, emptyIntent(), cool,
		newFakeXMLValidatorPass(),
		newFakeRegistryAllManaged())
	expectAction(t, decs, taskName, "restart", "")
}

func TestRecoverStoppedDaemons_Bootstrap_MissingIntent_Orphan(t *testing.T) {
	now := time.Date(2026, 5, 7, 12, 0, 0, 0, time.UTC)
	taskName := "\\mcp-local-hub-orphan-default"
	rows := []DaemonStatus{
		{Server: "orphan", Daemon: "default", TaskName: taskName,
			State: "Ready", LastResult: 1},
	}
	reg := &fakeRegistry{managedDefault: false}
	decs := RecoverStoppedDaemons(now, rows, emptyIntent(),
		&fakeCoolReader{dueDefault: true},
		newFakeXMLValidatorPass(),
		reg)
	expectAction(t, decs, taskName, "orphan", "")
}

// Decision precedence: maintenance > orphan > lazy-proxy-failed >
// running-skip > non-recovery-state > IsRealFailure-skip > intent active >
// restart-pending > cooldown > chronic-limit > suspicious-xml > restart.
//
// Specifically: a maintenance row stays "maintenance" even when the
// registry would not recognize it as managed. Maintenance check fires
// FIRST.
func TestRecoverStoppedDaemons_Maintenance_WinsOverOrphan(t *testing.T) {
	now := time.Date(2026, 5, 7, 12, 0, 0, 0, time.UTC)
	taskName := "\\mcp-local-hub-watchdog"
	rows := []DaemonStatus{
		{Server: "", Daemon: "", TaskName: taskName,
			State: "Ready", LastResult: 1},
	}
	reg := &fakeRegistry{managedDefault: false}
	decs := RecoverStoppedDaemons(now, rows, emptyIntent(),
		&fakeCoolReader{dueDefault: true},
		newFakeXMLValidatorPass(),
		reg)
	expectAction(t, decs, taskName, "maintenance", "")
}

// Strict purity: passing a CooldownReader (not a Cooldown) compiles
// AND produces decisions. The compile-time check is the type signature
// of RecoverStoppedDaemons; this runtime case asserts no panic on the
// minimum interface surface.
func TestRecoverStoppedDaemons_StrictPurity_CooldownReaderAccepted(t *testing.T) {
	now := time.Date(2026, 5, 7, 12, 0, 0, 0, time.UTC)
	taskName := "\\mcp-local-hub-memory-default"
	rows := []DaemonStatus{
		{Server: "memory", Daemon: "default", TaskName: taskName,
			State: "Ready", LastResult: 1},
	}
	// Compile-time-narrowed to the read-only contract.
	var cool CooldownReader = &fakeCoolReader{dueDefault: true}
	decs := RecoverStoppedDaemons(now, rows, emptyIntent(), cool,
		newFakeXMLValidatorPass(),
		newFakeRegistryAllManaged())
	if len(decs) != 1 {
		t.Fatalf("expected 1 decision, got %d", len(decs))
	}
}

// Empty status slice → empty decision slice (no panic on edge case).
func TestRecoverStoppedDaemons_EmptyStatus(t *testing.T) {
	now := time.Date(2026, 5, 7, 12, 0, 0, 0, time.UTC)
	decs := RecoverStoppedDaemons(now, nil, emptyIntent(),
		&fakeCoolReader{dueDefault: true},
		newFakeXMLValidatorPass(),
		newFakeRegistryAllManaged())
	if len(decs) != 0 {
		t.Errorf("expected 0 decisions on empty status; got %d: %+v",
			len(decs), decs)
	}
}

// Non-recovery state (e.g., "Queued") with a real failure → no decision.
// Per plan §1: only Ready / Stopped / Failed qualify.
func TestRecoverStoppedDaemons_NonRecoveryState_NoDecision(t *testing.T) {
	now := time.Date(2026, 5, 7, 12, 0, 0, 0, time.UTC)
	taskName := "\\mcp-local-hub-memory-default"
	rows := []DaemonStatus{
		{Server: "memory", Daemon: "default", TaskName: taskName,
			State: "Queued", LastResult: 1},
	}
	decs := RecoverStoppedDaemons(now, rows, emptyIntent(),
		&fakeCoolReader{dueDefault: true},
		newFakeXMLValidatorPass(),
		newFakeRegistryAllManaged())
	expectNoDecision(t, decs, taskName)
}

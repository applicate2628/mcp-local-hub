package api

import (
	"context"
	"errors"
	"sort"
	"strings"
	"testing"

	"mcp-local-hub/internal/config"
)

// pruneSetKeys returns the sorted key set of m for deterministic test
// diagnostics. Local alias instead of reusing the existing keys() helper
// at codex_followup_test.go:219 because that one is typed for
// map[string]DaemonIntent, not the struct{}-valued prune set.
func pruneSetKeys(m map[string]struct{}) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// TestSupervisorIntent_ReconcilePruneBareKey verifies that the prune set
// returned by buildPruneSetForReconcile uses BARE-form keys (no leading
// backslash), matching install.go:1639-1642 planned map shape and the
// install.go:1773 TrimPrefix-on-lookup invariant. Storage in
// supervisor-intent.json uses canonical leading-backslash form; the
// reconcile path strips the prefix at compare time.
//
// Plan §2624-2641, Spec §"Q12 CLI/GUI status seam".
func TestSupervisorIntent_ReconcilePruneBareKey(t *testing.T) {
	intent := &SupervisorIntentFile{
		Daemons: []SupervisorDaemon{
			{TaskName: `\mcp-local-hub-memory-default`},
			{TaskName: `\mcp-local-hub-time-default`},
		},
	}
	prunePlanned := buildPruneSetForReconcile(intent)
	if _, ok := prunePlanned["mcp-local-hub-memory-default"]; !ok {
		t.Fatalf("planned set should use BARE form, got keys: %v", pruneSetKeys(prunePlanned))
	}
	if _, ok := prunePlanned["mcp-local-hub-time-default"]; !ok {
		t.Fatalf("planned set should use BARE form for second entry, got keys: %v", pruneSetKeys(prunePlanned))
	}
	if _, ok := prunePlanned[`\mcp-local-hub-memory-default`]; ok {
		t.Fatalf("planned set must NOT use canonical leading-backslash form: keys %v", pruneSetKeys(prunePlanned))
	}
	if len(prunePlanned) != 2 {
		t.Fatalf("expected 2 entries, got %d (keys: %v)", len(prunePlanned), pruneSetKeys(prunePlanned))
	}
}

// TestSupervisorIntent_ReconcilePruneNilIntent guards against nil panics.
// A nil intent (e.g. supervisor-intent.json missing on first install)
// must produce an empty prune set, not a nil-deref.
func TestSupervisorIntent_ReconcilePruneNilIntent(t *testing.T) {
	prunePlanned := buildPruneSetForReconcile(nil)
	if prunePlanned == nil {
		t.Fatal("buildPruneSetForReconcile(nil) returned nil; want empty map")
	}
	if len(prunePlanned) != 0 {
		t.Fatalf("nil intent should produce empty prune set; got %d entries", len(prunePlanned))
	}
}

// TestSupervisorIntent_ReconcilePruneAlreadyBareKey verifies idempotency:
// if a task_name happens to lack the leading backslash (defensive — the
// production write always includes it), TrimPrefix is a no-op so the
// key still lands in BARE form.
func TestSupervisorIntent_ReconcilePruneAlreadyBareKey(t *testing.T) {
	intent := &SupervisorIntentFile{
		Daemons: []SupervisorDaemon{
			{TaskName: "mcp-local-hub-serena-default"}, // bare
		},
	}
	prunePlanned := buildPruneSetForReconcile(intent)
	if _, ok := prunePlanned["mcp-local-hub-serena-default"]; !ok {
		t.Fatalf("bare task_name should stay bare in prune set, got keys: %v", pruneSetKeys(prunePlanned))
	}
}

// TestPlan_SupervisorIntentFieldPopulated verifies that buildPlan
// populates the new Plan.SupervisorIntent field in parallel with
// the legacy Plan.SchedulerTasks field. One entry per daemon, same
// canonical task name shape.
//
// Backward-compat contract: while SchedulerTasks stays on the struct
// during the transition, downstream consumers (migrate, install
// reconciler, ApplyHubReconcile) gain SupervisorIntent as the
// authoritative seam.
func TestPlan_SupervisorIntentFieldPopulated(t *testing.T) {
	// canonicalMcphubPath() depends on the user-home / canonical-bin
	// fixture. Use the existing test override seam to keep the test
	// hermetic.
	prev := testCanonicalMcphubPathOverride
	testCanonicalMcphubPathOverride = `C:\fake\mcphub.exe`
	defer func() { testCanonicalMcphubPathOverride = prev }()

	m := &config.ServerManifest{
		Name:      "demo",
		Kind:      "stdio",
		Transport: "http_listen",
		Daemons: []config.DaemonSpec{
			{Name: "default", Port: 9300},
			{Name: "secondary", Port: 9301},
		},
		ClientBindings: []config.ClientBinding{},
	}

	plan, err := BuildPlanWithOpts(m, BuildPlanOpts{IncludeAllClients: true})
	if err != nil {
		t.Fatalf("BuildPlanWithOpts: %v", err)
	}

	if len(plan.SupervisorIntent) != 2 {
		t.Fatalf("SupervisorIntent len = %d, want 2 (one per daemon)", len(plan.SupervisorIntent))
	}
	wantNames := map[string]bool{
		"mcp-local-hub-demo-default":   true,
		"mcp-local-hub-demo-secondary": true,
	}
	for _, entry := range plan.SupervisorIntent {
		if !wantNames[entry.Name] {
			t.Errorf("unexpected SupervisorIntent entry name %q", entry.Name)
		}
		if entry.Command != `C:\fake\mcphub.exe` {
			t.Errorf("entry %q Command = %q, want canonical override", entry.Name, entry.Command)
		}
	}
}

// TestPlan_SchedulerTasksFieldStillPresent is the backward-compat guard:
// the legacy Plan.SchedulerTasks field MUST still be present and
// populated so existing call sites (executeInstallTo Step 1, prune-set
// builder at install.go:1639-1642, dry-run printer printPlanTo) keep
// working unchanged during the transition.
//
// Removing the field is a separate later-release breaking-change task;
// this test fails if the transition gets rushed.
func TestPlan_SchedulerTasksFieldStillPresent(t *testing.T) {
	prev := testCanonicalMcphubPathOverride
	testCanonicalMcphubPathOverride = `C:\fake\mcphub.exe`
	defer func() { testCanonicalMcphubPathOverride = prev }()

	m := &config.ServerManifest{
		Name:      "compat",
		Kind:      "stdio",
		Transport: "http_listen",
		Daemons: []config.DaemonSpec{
			{Name: "default", Port: 9400},
		},
		ClientBindings: []config.ClientBinding{},
	}

	plan, err := BuildPlanWithOpts(m, BuildPlanOpts{IncludeAllClients: true})
	if err != nil {
		t.Fatalf("BuildPlanWithOpts: %v", err)
	}
	if len(plan.SchedulerTasks) == 0 {
		t.Fatal("Plan.SchedulerTasks is empty — legacy field must stay populated for backward compat")
	}
	if plan.SchedulerTasks[0].Name != "mcp-local-hub-compat-default" {
		t.Errorf("SchedulerTasks[0].Name = %q, want mcp-local-hub-compat-default",
			plan.SchedulerTasks[0].Name)
	}
}

// TestHealthSnapshot_IPCTimeoutReturnsFailLoud verifies the new IPC
// backing: when supervisorIPCStatusFn is configured and returns a
// timeout (or any error), the daemons-section fetch surfaces the
// error so the HTTP handler maps it to 500 +
// HEALTH_BACKEND_FAILED / STATUS_FAILED envelope. Silently falling
// back to an empty result would violate the fail-loud contract
// codified in PR #132 (Cloud bot P1).
func TestHealthSnapshot_IPCTimeoutReturnsFailLoud(t *testing.T) {
	a := NewAPI()

	// Install a fake IPC client that always times out. The seam
	// (supervisorIPCStatusFn package var) is swapped via a t.Cleanup
	// so a parallel test cannot leak fake state.
	prev := supervisorIPCStatusFn
	supervisorIPCStatusFn = func(_ context.Context) ([]DaemonStatus, error) {
		return nil, errors.New("supervisor IPC dial: context deadline exceeded")
	}
	t.Cleanup(func() { supervisorIPCStatusFn = prev })

	_, err := a.HealthSnapshot(context.Background(), HealthOpts{})
	if err == nil {
		t.Fatal("HealthSnapshot returned nil err on IPC timeout; want fail-loud")
	}
	if !strings.Contains(err.Error(), "supervisor IPC") {
		t.Errorf("err = %v, want fail-loud error mentioning supervisor IPC", err)
	}
}

// TestDaemonStatusSnapshot_IPCErrorReturnsFailLoud is the /api/status
// symmetric variant: when the IPC backing fails, DaemonStatusSnapshot
// must propagate the error so cmd/mcphub maps it to 500 +
// STATUS_FAILED. The pre-IPC contract was scheduler-side; the new
// contract is IPC-side, but the operator-visible envelope is identical.
func TestDaemonStatusSnapshot_IPCErrorReturnsFailLoud(t *testing.T) {
	a := NewAPI()

	prev := supervisorIPCStatusFn
	supervisorIPCStatusFn = func(_ context.Context) ([]DaemonStatus, error) {
		return nil, errors.New("supervisor IPC: pipe unavailable")
	}
	t.Cleanup(func() { supervisorIPCStatusFn = prev })

	rows, err := a.DaemonStatusSnapshot(context.Background())
	if err == nil {
		t.Fatalf("DaemonStatusSnapshot returned nil err on IPC failure; rows=%+v", rows)
	}
	if !strings.Contains(err.Error(), "pipe unavailable") {
		t.Errorf("err = %v, want propagated IPC error", err)
	}
}

// TestHealthSnapshot_IPCBackingDeliversDaemons verifies the happy
// path: the IPC backing returns rows, those rows propagate through
// to HealthSnapshot.Daemons.Items projection. Confirms the seam is
// actually being read (not bypassed by the fallback).
func TestHealthSnapshot_IPCBackingDeliversDaemons(t *testing.T) {
	a := NewAPI()

	prev := supervisorIPCStatusFn
	supervisorIPCStatusFn = func(_ context.Context) ([]DaemonStatus, error) {
		return []DaemonStatus{
			{Server: "memory", Daemon: "default", State: "Running", Port: 9301, PID: 1234},
		}, nil
	}
	t.Cleanup(func() { supervisorIPCStatusFn = prev })

	snap, err := a.HealthSnapshot(context.Background(), HealthOpts{})
	if err != nil {
		t.Fatalf("HealthSnapshot: %v", err)
	}
	if len(snap.Daemons.Items) != 1 {
		t.Fatalf("Daemons.Items len = %d, want 1 (IPC seam not engaged)", len(snap.Daemons.Items))
	}
	if snap.Daemons.Items[0].Server != "memory" {
		t.Errorf("Daemons.Items[0].Server = %q, want memory", snap.Daemons.Items[0].Server)
	}
}

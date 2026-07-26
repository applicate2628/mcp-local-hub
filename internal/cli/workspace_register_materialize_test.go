// Package cli — composition tests for the CLIENT side of the P1 fix
// (mcphub-register-intent): `mcphub workspace register <path>` used to
// commit a workspaces.yaml row and print an unqualified success message
// WITHOUT EVER touching supervisor-intent.json — no daemon row, no
// reconcile, no spawn, and no observable signal that anything was wrong.
//
// This file tests runWorkspaceRegister's new step 8 (workspace_cmd.go): the
// reconcile-apply nudge + spec-bearing-intent-row gate that must hold BOTH
// before an unqualified success is printed. The SERVER side of the
// materialization (handleReconcile's apply-mode self-heal via
// api.RepairSerenaIntentFromRegistry) is tested separately in
// supervise_reconcile_serena_repair_test.go.
//
// These tests stub serenaRegisterReconcileFn (the seam workspace_cmd.go
// calls in place of a live supervisor/IPC transport) with closures that
// invoke the REAL api.RepairSerenaIntentFromRegistry themselves — modeling
// "the supervisor received the reconcile-apply request and self-healed"
// without a live process boundary, while still exercising the real
// registry/intent materialization logic the fix wires together.
// serenaRegisterIntentCheckFn is left UNSTUBBED (real) throughout, so the
// CLI's own post-materialize verification runs against whatever the stub
// actually left on disk.
package cli

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"mcp-local-hub/internal/api"
)

// withSerenaDynamicPoolCatalog seeds a valid dynamic-pool serena manifest
// (daemon_template present) via MCPHUB_MANIFEST_DIR_OVERRIDE. This is the
// SAME env var api.ManifestGet honors regardless of package boundary, so it
// backs BOTH loadSerenaManifestForCLI (register's own port-pool resolution)
// and api.RepairSerenaIntentFromRegistry's internal loadSerenaCatalogManifest
// (used to build the appended daemon descriptor) with one consistent,
// hermetic manifest — no embed leakage, no live-registry dependency.
func withSerenaDynamicPoolCatalog(t *testing.T) {
	t.Helper()
	manifestDir := t.TempDir()
	seedSerenaManifest(t, manifestDir, alreadyMigratedManifestYAML)
	t.Setenv("MCPHUB_MANIFEST_DIR_OVERRIDE", manifestDir)
}

// seedHealthySerenaIntentRow writes supervisor-intent.json with ONE
// pre-existing spec-bearing serena daemon for workspacePath, satisfying the
// §7.1 "dynamic pool already introduced" precondition
// RepairSerenaIntentFromRegistry requires before it will APPEND a new row
// (rather than defer to `mcphub migrate serena legacy-to-dynamic-pool`).
func seedHealthySerenaIntentRow(t *testing.T, workspacePath string, port int) {
	t.Helper()
	key := api.WorkspaceKey(workspacePath)
	taskName := `\mcp-local-hub-serena-` + key
	intentPath, err := api.DefaultSupervisorIntentPath()
	if err != nil {
		t.Fatalf("resolve supervisor-intent path: %v", err)
	}
	f := &api.SupervisorIntentFile{
		Version: 1,
		Daemons: []api.SupervisorDaemon{
			{
				TaskName:  taskName,
				Server:    api.SerenaServerName,
				Daemon:    "serena-" + key,
				Command:   "mcphub",
				Workspace: workspacePath,
				Port:      port,
				RuntimeSpec: &api.DaemonRuntimeSpec{
					SpecVersion:   1,
					ChildCommand:  "uvx",
					UpstreamPort:  19000 + port,
					ExternalPort:  port,
					WorkspacePath: workspacePath,
				},
			},
		},
	}
	if err := api.WriteSupervisorIntent(intentPath, f); err != nil {
		t.Fatalf("seed supervisor-intent.json: %v", err)
	}
}

// useRealSerenaRegisterIntentCheck restores serenaRegisterIntentCheckFn to
// its REAL production behavior (realSerenaRegisterIntentCheck,
// workspace_cmd.go) for the duration of the test. withStateDir installs a
// default "assume materialized" stub for this seam so the MANY existing
// register tests that don't care about the new materialization gate keep
// passing without also having to fake supervisor IPC by hand — but every
// composition test in THIS file needs the ACTUAL post-materialize check to
// run against whatever the reconcile stub really left on disk, so each one
// calls this right after withStateDir.
func useRealSerenaRegisterIntentCheck(t *testing.T) {
	t.Helper()
	orig := serenaRegisterIntentCheckFn
	serenaRegisterIntentCheckFn = realSerenaRegisterIntentCheck
	t.Cleanup(func() { serenaRegisterIntentCheckFn = orig })
}

// stubSerenaRegisterReconcileWithRealRepair installs a serenaRegisterReconcileFn
// stub that invokes the REAL api.RepairSerenaIntentFromRegistry (against the
// test-redirected state dir) and then returns respErr. Passing a nil respErr
// models "the supervisor received and acknowledged the request"; a non-nil
// respErr models a lost/failed acknowledgment (which can still have run the
// repair to completion, e.g. the "lost reply after commit" scenario). Returns
// a pointer to a call counter so the test can assert the request actually
// happened ("an observable reconcile/start request").
func stubSerenaRegisterReconcileWithRealRepair(t *testing.T, respErr error) *int {
	t.Helper()
	calls := 0
	orig := serenaRegisterReconcileFn
	serenaRegisterReconcileFn = func(ctx context.Context, apply bool) (api.ReconcileResponse, error) {
		calls++
		if !apply {
			t.Fatal("register must always request apply=true")
		}
		stateDir, err := api.DaemonStateDir()
		if err != nil {
			t.Fatalf("resolve state dir inside stub: %v", err)
		}
		_, _, _ = api.NewAPI().RepairSerenaIntentFromRegistry(stateDir)
		if respErr != nil {
			return api.ReconcileResponse{}, respErr
		}
		return api.ReconcileResponse{DriftCount: 1, AppliedCount: 1}, nil
	}
	t.Cleanup(func() { serenaRegisterReconcileFn = orig })
	return &calls
}

// stubSerenaRegisterReconcileNoRepair installs a serenaRegisterReconcileFn
// stub that returns respErr WITHOUT ever invoking the real repair — modeling
// either an IPC-transport-level failure (respErr wraps
// api.ErrSupervisorIPCUnavailable) or a "the supervisor answered fine but its
// internal self-heal silently skipped" scenario (respErr == nil): either way
// the row is never materialized by this call.
func stubSerenaRegisterReconcileNoRepair(t *testing.T, respErr error) *int {
	t.Helper()
	calls := 0
	orig := serenaRegisterReconcileFn
	serenaRegisterReconcileFn = func(ctx context.Context, apply bool) (api.ReconcileResponse, error) {
		calls++
		if respErr != nil {
			return api.ReconcileResponse{}, respErr
		}
		return api.ReconcileResponse{DriftCount: 0, AppliedCount: 0}, nil
	}
	t.Cleanup(func() { serenaRegisterReconcileFn = orig })
	return &calls
}

// TestWorkspaceRegisterSerena_LiveSupervisorMaterializesBeforeSuccess is the
// composition test this fix was missing: registry allocation, the full
// auto-register transaction, auto-register idempotency, append-only intent
// repair, and router-miss-only auto-register are each covered SEPARATELY
// elsewhere in this package's tests — none of them compose "explicit
// register" with "live-supervisor convergence". This asserts all three hold
// together: registry row present, spec-bearing intent row present, an
// observable reconcile request, and the unqualified success line printed
// ONLY once all three are true.
func TestWorkspaceRegisterSerena_LiveSupervisorMaterializesBeforeSuccess(t *testing.T) {
	withSerenaDynamicPoolCatalog(t)
	withStateDir(t)
	useRealSerenaRegisterIntentCheck(t)

	// A pre-existing healthy workspace satisfies the §7.1 precondition so the
	// NEW workspace's row can be appended rather than deferred.
	healthyWS := t.TempDir()
	seedHealthySerenaIntentRow(t, healthyWS, 9150)

	calls := stubSerenaRegisterReconcileWithRealRepair(t, nil)

	tmp := t.TempDir()
	ws := makeWorkspaceDir(t, tmp, []string{"python"})
	wsKey := api.WorkspaceKey(ws)

	out, err := runWorkspaceCmd(t, "register", ws)
	if err != nil {
		t.Fatalf("register: %v\noutput: %s", err, out)
	}
	if *calls != 1 {
		t.Errorf("reconcile-apply requested %d times, want exactly 1", *calls)
	}
	if !strings.Contains(out, "Registered serena workspace") {
		t.Errorf("expected the unqualified success line; got %q", out)
	}

	// 1. Registry row present.
	regPath, _ := api.DefaultRegistryPath()
	reg := api.NewRegistry(regPath)
	if err := reg.Load(); err != nil {
		t.Fatalf("load registry: %v", err)
	}
	entries := reg.SerenaEntries()
	found := false
	for _, e := range entries {
		if e.WorkspaceKey == wsKey {
			found = true
		}
	}
	if !found {
		t.Fatalf("no registry row for %s; entries=%+v", wsKey, entries)
	}

	// 2. Spec-bearing intent row present.
	intentPath, _ := api.DefaultSupervisorIntentPath()
	intent, ierr := api.ReadSupervisorIntent(intentPath)
	if ierr != nil {
		t.Fatalf("read supervisor-intent.json: %v", ierr)
	}
	if !intent.HasSpecBearingSerenaDaemonForWorkspaceKey(wsKey) {
		t.Fatalf("supervisor-intent.json has no spec-bearing serena daemon for %s", wsKey)
	}
}

// TestWorkspaceRegisterSerena_IPCUnavailable_KeepsRegistryReportsPartial
// models "no supervisor is running" — the reconcile-apply dial itself fails
// with api.ErrSupervisorIPCUnavailable. The registry row must be KEPT (no
// rollback) and the command must report a non-success, actionable partial
// state — never the unqualified success line.
func TestWorkspaceRegisterSerena_IPCUnavailable_KeepsRegistryReportsPartial(t *testing.T) {
	withSerenaDynamicPoolCatalog(t)
	withStateDir(t)
	useRealSerenaRegisterIntentCheck(t)

	calls := stubSerenaRegisterReconcileNoRepair(t,
		fmt.Errorf("supervisor IPC reconcile: dial: %w", api.ErrSupervisorIPCUnavailable))

	tmp := t.TempDir()
	ws := makeWorkspaceDir(t, tmp, []string{"python"})
	wsKey := api.WorkspaceKey(ws)

	out, err := runWorkspaceCmd(t, "register", ws)
	if err == nil {
		t.Fatalf("register must NOT report success when the supervisor is unreachable; output: %s", out)
	}
	if *calls != 1 {
		t.Errorf("reconcile-apply requested %d times, want exactly 1", *calls)
	}
	if strings.Contains(out, "Registered serena workspace") {
		t.Errorf("must never print the unqualified success line on IPC-unavailable; got %q", out)
	}
	if !strings.Contains(err.Error(), "no supervisor is running") {
		t.Errorf("error should name the cause (no supervisor running); got %q", err.Error())
	}
	if !strings.Contains(err.Error(), "kept") && !strings.Contains(err.Error(), "was kept") {
		t.Errorf("error should state the registry row was kept, not rolled back; got %q", err.Error())
	}

	// Registry row is still there.
	regPath, _ := api.DefaultRegistryPath()
	reg := api.NewRegistry(regPath)
	if err := reg.Load(); err != nil {
		t.Fatalf("load registry: %v", err)
	}
	found := false
	for _, e := range reg.SerenaEntries() {
		if e.WorkspaceKey == wsKey {
			found = true
		}
	}
	if !found {
		t.Fatalf("registry row for %s must be KEPT despite the partial-state report", wsKey)
	}

	// No intent row materialized.
	intentPath, _ := api.DefaultSupervisorIntentPath()
	if intent, ierr := api.ReadSupervisorIntent(intentPath); ierr == nil && intent.HasSpecBearingSerenaDaemonForWorkspaceKey(wsKey) {
		t.Fatalf("no spec-bearing intent row should exist when the supervisor was never reached")
	}
}

// TestWorkspaceRegisterSerena_ReconcileAckedButRepairSkipped_KeepsRegistryReportsPartial
// models a live, healthy supervisor whose acknowledgment came back clean
// (no error) but whose internal self-heal did not actually materialize the
// row — e.g. a momentarily contended registry/intent lock
// (RepairSerenaIntentFromRegistry's own documented "skip silently, self-heals
// next call" contention outcome). The CLI cannot distinguish WHY the row is
// missing from a clean acknowledgment alone; it must still keep the registry
// row and report partial state rather than an unqualified success.
func TestWorkspaceRegisterSerena_ReconcileAckedButRepairSkipped_KeepsRegistryReportsPartial(t *testing.T) {
	withSerenaDynamicPoolCatalog(t)
	withStateDir(t)
	useRealSerenaRegisterIntentCheck(t)

	healthyWS := t.TempDir()
	seedHealthySerenaIntentRow(t, healthyWS, 9150)

	// Reconcile "succeeds" (no error) but never actually runs the repair —
	// modeling a live supervisor whose self-heal silently no-op'd.
	calls := stubSerenaRegisterReconcileNoRepair(t, nil)

	tmp := t.TempDir()
	ws := makeWorkspaceDir(t, tmp, []string{"python"})
	wsKey := api.WorkspaceKey(ws)

	out, err := runWorkspaceCmd(t, "register", ws)
	if err == nil {
		t.Fatalf("register must NOT report success when no spec-bearing row materialized; output: %s", out)
	}
	if *calls != 1 {
		t.Errorf("reconcile-apply requested %d times, want exactly 1", *calls)
	}
	if strings.Contains(out, "Registered serena workspace") {
		t.Errorf("must never print the unqualified success line; got %q", out)
	}
	if !strings.Contains(err.Error(), "reconcile --apply") {
		t.Errorf("error should suggest retrying via `mcphub reconcile --apply`; got %q", err.Error())
	}

	regPath, _ := api.DefaultRegistryPath()
	reg := api.NewRegistry(regPath)
	if err := reg.Load(); err != nil {
		t.Fatalf("load registry: %v", err)
	}
	found := false
	for _, e := range reg.SerenaEntries() {
		if e.WorkspaceKey == wsKey {
			found = true
		}
	}
	if !found {
		t.Fatalf("registry row for %s must be KEPT despite the partial-state report", wsKey)
	}
}

// TestWorkspaceRegisterSerena_FirstIntroductionDeferred_KeepsRegistryReportsPartial
// exercises the REAL §7.1 introduce-crash deferral: no pre-existing
// spec-bearing serena row exists anywhere, so RepairSerenaIntentFromRegistry
// correctly DEFERS (never appends while the dynamic pool has not been
// introduced) rather than risk the split-brain hazard. The register command
// must surface this honestly and point at the migrate command, not print
// success.
func TestWorkspaceRegisterSerena_FirstIntroductionDeferred_KeepsRegistryReportsPartial(t *testing.T) {
	withSerenaDynamicPoolCatalog(t)
	withStateDir(t)
	useRealSerenaRegisterIntentCheck(t)

	// No seedHealthySerenaIntentRow call — intent starts with no runtime_spec
	// row at all, so HasRuntimeSpecRow() is false and the real repair defers.
	calls := stubSerenaRegisterReconcileWithRealRepair(t, nil)

	tmp := t.TempDir()
	ws := makeWorkspaceDir(t, tmp, []string{"python"})
	wsKey := api.WorkspaceKey(ws)

	out, err := runWorkspaceCmd(t, "register", ws)
	if err == nil {
		t.Fatalf("register must NOT report success while the dynamic pool is un-introduced; output: %s", out)
	}
	if *calls != 1 {
		t.Errorf("reconcile-apply requested %d times, want exactly 1", *calls)
	}
	if strings.Contains(out, "Registered serena workspace") {
		t.Errorf("must never print the unqualified success line; got %q", out)
	}
	if !strings.Contains(err.Error(), "migrate serena legacy-to-dynamic-pool") {
		t.Errorf("error should point at the migrate command for first-introduction; got %q", err.Error())
	}

	regPath, _ := api.DefaultRegistryPath()
	reg := api.NewRegistry(regPath)
	if err := reg.Load(); err != nil {
		t.Fatalf("load registry: %v", err)
	}
	found := false
	for _, e := range reg.SerenaEntries() {
		if e.WorkspaceKey == wsKey {
			found = true
		}
	}
	if !found {
		t.Fatalf("registry row for %s must be KEPT despite the deferred introduction", wsKey)
	}
}

// TestWorkspaceRegisterSerena_LostReplyAfterIntentCommit_KeepsRegistryReportsPartial
// models the repair ACTUALLY committing the spec-bearing intent row (the
// stub invokes the real repair, which appends+writes it), but the
// acknowledgment itself being lost (the reconcile call returns an error
// anyway). Per the design's gate — BOTH the ack AND the on-disk row must
// hold — this must still report partial state and must NOT print the
// unqualified success line, even though the workspace is, in fact, usable.
func TestWorkspaceRegisterSerena_LostReplyAfterIntentCommit_KeepsRegistryReportsPartial(t *testing.T) {
	withSerenaDynamicPoolCatalog(t)
	withStateDir(t)
	useRealSerenaRegisterIntentCheck(t)

	healthyWS := t.TempDir()
	seedHealthySerenaIntentRow(t, healthyWS, 9150)

	calls := stubSerenaRegisterReconcileWithRealRepair(t, errors.New("simulated lost reply after the supervisor committed the intent write"))

	tmp := t.TempDir()
	ws := makeWorkspaceDir(t, tmp, []string{"python"})
	wsKey := api.WorkspaceKey(ws)

	out, err := runWorkspaceCmd(t, "register", ws)
	if err == nil {
		t.Fatalf("register must NOT report success when the reconcile acknowledgment itself failed, even if the row materialized; output: %s", out)
	}
	if *calls != 1 {
		t.Errorf("reconcile-apply requested %d times, want exactly 1", *calls)
	}
	if strings.Contains(out, "Registered serena workspace") {
		t.Errorf("must never print the unqualified success line when the ack failed; got %q", out)
	}
	if !strings.Contains(err.Error(), "simulated lost reply") {
		t.Errorf("error should surface the underlying reconcile failure; got %q", err.Error())
	}

	// The row DID materialize (the repair really ran) — proving the gate is
	// on the ACK, not merely on file state.
	intentPath, _ := api.DefaultSupervisorIntentPath()
	intent, ierr := api.ReadSupervisorIntent(intentPath)
	if ierr != nil {
		t.Fatalf("read supervisor-intent.json: %v", ierr)
	}
	if !intent.HasSpecBearingSerenaDaemonForWorkspaceKey(wsKey) {
		t.Fatalf("expected the intent row to have materialized despite the lost reply")
	}

	regPath, _ := api.DefaultRegistryPath()
	reg := api.NewRegistry(regPath)
	if err := reg.Load(); err != nil {
		t.Fatalf("load registry: %v", err)
	}
	found := false
	for _, e := range reg.SerenaEntries() {
		if e.WorkspaceKey == wsKey {
			found = true
		}
	}
	if !found {
		t.Fatalf("registry row for %s must be KEPT despite the lost reply", wsKey)
	}
}

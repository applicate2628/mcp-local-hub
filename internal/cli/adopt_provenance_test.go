package cli

import (
	"bytes"
	"errors"
	"strings"
	"testing"

	"mcp-local-hub/internal/api"
)

type fakeForgetCLIAPI struct {
	plan         *api.ForgetAdoptProvenancePlan
	planErr      error
	forgot       *api.ForgetAdoptProvenancePlan
	forgetErr    error
	forgetCalled bool
	forgetArg    string
}

func (f *fakeForgetCLIAPI) BuildForgetAdoptProvenancePlan(string) (*api.ForgetAdoptProvenancePlan, error) {
	return f.plan, f.planErr
}

func (f *fakeForgetCLIAPI) ForgetAdoptProvenance(m string, _ api.ForgetAdoptProvenanceOpts) (*api.ForgetAdoptProvenancePlan, error) {
	f.forgetCalled = true
	f.forgetArg = m
	return f.forgot, f.forgetErr
}

func runForgetCommandWithFake(t *testing.T, fake *fakeForgetCLIAPI, args ...string) (string, error) {
	t.Helper()
	prior := newForgetProvenanceCLIAPI
	newForgetProvenanceCLIAPI = func() forgetProvenanceCLIAPI { return fake }
	t.Cleanup(func() { newForgetProvenanceCLIAPI = prior })

	root := NewRootCmd()
	var out bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&out)
	root.SetArgs(args)
	err := root.Execute()
	return out.String(), err
}

func TestAdoptProvenanceForget_DryRunByDefaultRemovesNothing(t *testing.T) {
	fake := &fakeForgetCLIAPI{
		plan: &api.ForgetAdoptProvenancePlan{
			ManifestName:   "m1",
			HasRow:         true,
			RowState:       api.AdoptOperationStateAdopted,
			Clients:        []string{"codex-cli"},
			HasSnapshotDir: true,
			SnapshotDir:    "/x/adopt-provenance/m1",
			Warnings:       []string{"row is 'adopted' (a COMMITTED adopt): forgetting it discards the ability to run 'mcphub de-adopt m1'"},
		},
	}
	out, err := runForgetCommandWithFake(t, fake, "adopt-provenance", "forget", "m1")
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if fake.forgetCalled {
		t.Errorf("dry run (no --yes) must NOT call ForgetAdoptProvenance")
	}
	if !strings.Contains(out, "Dry run") {
		t.Errorf("expected a dry-run notice; out=%s", out)
	}
	if !strings.Contains(out, "de-adopt") {
		t.Errorf("expected the adopted-row warning printed; out=%s", out)
	}
	if !strings.Contains(out, "--yes") {
		t.Errorf("expected the re-run-with---yes hint; out=%s", out)
	}
}

func TestAdoptProvenanceForget_YesCallsForget(t *testing.T) {
	fake := &fakeForgetCLIAPI{
		plan:   &api.ForgetAdoptProvenancePlan{ManifestName: "m2", HasRow: true, RowState: api.AdoptOperationStateAdopting},
		forgot: &api.ForgetAdoptProvenancePlan{ManifestName: "m2", HasRow: true, HasSnapshotDir: true},
	}
	out, err := runForgetCommandWithFake(t, fake, "adopt-provenance", "forget", "m2", "--yes")
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if !fake.forgetCalled {
		t.Errorf("--yes must call ForgetAdoptProvenance")
	}
	if fake.forgetArg != "m2" {
		t.Errorf("forgetArg = %q, want m2", fake.forgetArg)
	}
	if !strings.Contains(out, "Forgotten") {
		t.Errorf("expected a confirmation line; out=%s", out)
	}
}

func TestAdoptProvenanceForget_BuildErrorSurfaces(t *testing.T) {
	fake := &fakeForgetCLIAPI{planErr: errors.New("no provenance row and no snapshot dir — nothing to forget")}
	_, err := runForgetCommandWithFake(t, fake, "adopt-provenance", "forget", "nope")
	if err == nil {
		t.Errorf("expected the BuildForgetAdoptProvenancePlan error to surface")
	}
	if fake.forgetCalled {
		t.Errorf("must not call ForgetAdoptProvenance when BuildPlan errored")
	}
}

func TestAdoptLeaseNamespaceInspectPrintsOnlyStableProjection(t *testing.T) {
	prior := inspectAdoptLeaseNamespaceCLI
	inspectAdoptLeaseNamespaceCLI = func() (api.AdoptLeaseNamespaceReport, error) {
		report := api.AdoptLeaseNamespaceReport{
			State: api.AdoptLeaseNamespaceRefused, ReasonID: api.AdoptLeaseReasonNamespaceLegacyDACL,
			Action: api.AdoptLeaseActionMigrateLegacy, MigrationEligible: true, LeaseLeafCount: 2,
		}
		return report, &api.LeaseNamespaceFailure{
			FailureID: "E_ADOPT_LEASE_NAMESPACE_REFUSED", ReasonID: report.ReasonID, Action: report.Action,
		}
	}
	t.Cleanup(func() { inspectAdoptLeaseNamespaceCLI = prior })

	root := NewRootCmd()
	var out bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&out)
	root.SetArgs([]string{"adopt-provenance", "lease-namespace", "inspect"})
	err := root.Execute()
	if err == nil {
		t.Fatal("refused inspection must return non-zero")
	}
	got := out.String() + err.Error()
	for _, want := range []string{"reason_id=namespace-legacy-dacl", "action=migrate-legacy-lease-namespace", "lease_leaves=2"} {
		if !strings.Contains(got, want) {
			t.Fatalf("missing %q in %q", want, got)
		}
	}
	for _, forbidden := range []string{"private-path-canary", "foreign-sid-canary", "secret-canary"} {
		if strings.Contains(got, forbidden) {
			t.Fatalf("inspection leaked %q in %q", forbidden, got)
		}
	}
}

func TestAdoptLeaseNamespaceMigrateLegacyDryRunDoesNotApply(t *testing.T) {
	prior := migrateLegacyAdoptLeaseNamespaceCLI
	var opts []api.AdoptLeaseNamespaceMigrationOpts
	migrateLegacyAdoptLeaseNamespaceCLI = func(got api.AdoptLeaseNamespaceMigrationOpts) (api.AdoptLeaseNamespaceReport, error) {
		opts = append(opts, got)
		return api.AdoptLeaseNamespaceReport{
			State: api.AdoptLeaseNamespaceLegacy, ReasonID: api.AdoptLeaseReasonNamespaceLegacyDACL,
			Action: api.AdoptLeaseActionMigrateLegacy, MigrationEligible: true,
		}, nil
	}
	t.Cleanup(func() { migrateLegacyAdoptLeaseNamespaceCLI = prior })

	root := NewRootCmd()
	var out bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&out)
	root.SetArgs([]string{"adopt-provenance", "lease-namespace", "migrate-legacy"})
	if err := root.Execute(); err != nil {
		t.Fatal(err)
	}
	if len(opts) != 1 || opts[0].Yes {
		t.Fatalf("dry-run opts=%+v", opts)
	}
	if !strings.Contains(out.String(), "dry_run=true mutation=false") || !strings.Contains(out.String(), "--yes") {
		t.Fatalf("dry-run output=%q", out.String())
	}
}

func TestAdoptLeaseNamespaceMigrateLegacyYesApplies(t *testing.T) {
	prior := migrateLegacyAdoptLeaseNamespaceCLI
	called := false
	migrateLegacyAdoptLeaseNamespaceCLI = func(opts api.AdoptLeaseNamespaceMigrationOpts) (api.AdoptLeaseNamespaceReport, error) {
		called = opts.Yes
		return api.AdoptLeaseNamespaceReport{
			State: api.AdoptLeaseNamespaceReady, ReasonID: api.AdoptLeaseReasonNamespaceReady,
			Action: api.AdoptLeaseActionRetryAdopt, ChangedLeafCount: 2, NamespaceChanged: true,
		}, nil
	}
	t.Cleanup(func() { migrateLegacyAdoptLeaseNamespaceCLI = prior })

	root := NewRootCmd()
	var out bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&out)
	root.SetArgs([]string{"adopt-provenance", "lease-namespace", "migrate-legacy", "--yes"})
	if err := root.Execute(); err != nil {
		t.Fatal(err)
	}
	if !called || !strings.Contains(out.String(), "changed_leaves=2 namespace_changed=true") {
		t.Fatalf("apply called=%t output=%q", called, out.String())
	}
}

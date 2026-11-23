package api

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"mcp-local-hub/internal/config"
)

func TestPrepareSerenaParsedInstallRefusesAlreadyAcquiredScope(t *testing.T) {
	root := daemonIntentTestHelper(t)
	lock, err := AcquireSupervisorLockQuiet(filepath.Join(root, "supervisor.lock"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(lock.Release)
	for name, change := range map[string]func(*InstallParsedManifestOpts){
		"quiet lock": func(opts *InstallParsedManifestOpts) { opts.SupervisorLockBypass = lock.AllowSpecBearingWriteBypass() },
		"binding":    func(opts *InstallParsedManifestOpts) { opts.SerenaPreparedBinding = &PreparedSerenaRuntimeBinding{} },
		"invocation": func(opts *InstallParsedManifestOpts) {
			opts.SerenaPreparedInvocation = &PreparedSerenaConsumerInvocation{}
		},
		"dry run": func(opts *InstallParsedManifestOpts) { opts.DryRun = true },
	} {
		t.Run(name, func(t *testing.T) {
			opts := InstallParsedManifestOpts{ManifestHash: testExactManifestHash, Workspaces: []WorkspaceEntry{{WorkspacePath: root, Port: 9401}}}
			change(&opts)
			prepared, err := NewAPI().PrepareSerenaParsedInstall(context.Background(), serenaTemplateManifest(), opts)
			if prepared != nil || err == nil || !strings.Contains(err.Error(), "SERENA_PARSED_PREPARATION_REQUIRES_UNLOCKED_CALLER") {
				t.Fatalf("preacquired scope reached preparation: prepared=%v err=%v", prepared, err)
			}
		})
	}
}

func TestPreparedSerenaParsedInstallRejectsUnissuedOwner(t *testing.T) {
	prepared := &PreparedSerenaParsedInstall{
		api: NewAPI(), owner: &PreparedSerenaRuntimeOwner{}, manifest: serenaTemplateManifest(),
		request: PreparedSerenaRequest{WorkDeadline: time.Now().Add(time.Minute)},
	}
	runErr, cleanupErr := prepared.WithConsumerInvocation(context.Background(), []WorkspaceEntry{{WorkspacePath: t.TempDir(), Port: 9401}}, InstallParsedManifestBypass{}, func(context.Context, *PreparedSerenaConsumerInvocation) error {
		t.Fatal("data-only owner reached the consumer callback")
		return nil
	})
	if runErr == nil || cleanupErr != nil || !strings.Contains(runErr.Error(), "SERENA_PARSED_PREPARATION_OWNER_DRIFT") {
		t.Fatalf("unissued owner was accepted: run=%v cleanup=%v", runErr, cleanupErr)
	}
}

func TestPreparedSerenaParsedInstallObserveConsumerLaunchRejectsInvalidScope(t *testing.T) {
	prepared, _ := parsedConsumerLaunchRefusalFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	cancelled, stop := context.WithCancel(ctx)
	stop()
	for _, tc := range []struct {
		name     string
		prepared *PreparedSerenaParsedInstall
		ctx      context.Context
		task     string
		want     string
	}{
		{"nil preparation", nil, ctx, "serena-test", "SERENA_PARSED_PREPARATION_MISSING"},
		{"zero preparation", &PreparedSerenaParsedInstall{}, ctx, "serena-test", "SERENA_PARSED_PREPARATION_MISSING"},
		{"nil context", prepared, nil, "serena-test", "SERENA_PREPARED_LAUNCH_INPUT_MISSING"},
		{"cancelled context", prepared, cancelled, "serena-test", "SERENA_PREPARED_LAUNCH_INPUT_MISSING"},
		{"unbounded context", prepared, context.Background(), "serena-test", "SERENA_PREPARED_LAUNCH_DEADLINE_MISSING"},
		{"empty task", prepared, ctx, "", "SERENA_PREPARED_LAUNCH_INPUT_MISSING"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			launch, err := tc.prepared.ObserveConsumerLaunch(tc.ctx, tc.task)
			if launch != nil || err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("invalid scope reached observation: launch=%v err=%v, want %s", launch, err, tc.want)
			}
		})
	}
	if err := prepared.validateConsumerLaunchSource(ctx, NewAPI()); err == nil || !strings.Contains(err.Error(), "SERENA_PARSED_PREPARATION_MISSING") {
		t.Fatalf("foreign API reached original preparation: %v", err)
	}
	if err := prepared.validateConsumerLaunchSource(cancelled, prepared.api); !errors.Is(err, context.Canceled) {
		t.Fatalf("original preparation lost cancellation: %v", err)
	}
}

func TestPreparedSerenaParsedInstallObserveConsumerLaunchRejectsSourceDriftBeforeIO(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	for _, tc := range []struct {
		name   string
		change func(*PreparedSerenaParsedInstall)
		want   string
	}{
		{"unbound owner", func(p *PreparedSerenaParsedInstall) { p.api.preparedSerenaOwner = nil }, "SERENA_PARSED_PREPARATION_OWNER_DRIFT"},
		{"replaced owner", func(p *PreparedSerenaParsedInstall) { p.api.preparedSerenaOwner = &PreparedSerenaRuntimeOwner{} }, "SERENA_PARSED_PREPARATION_OWNER_DRIFT"},
		{"manifest hash", func(p *PreparedSerenaParsedInstall) { p.manifestSHA = strings.Repeat("b", 64) }, "SERENA_PARSED_PREPARATION_SOURCE_DRIFT"},
		{"policy identity", func(p *PreparedSerenaParsedInstall) { p.policy.WorkLimit += time.Second }, "SERENA_PARSED_PREPARATION_SOURCE_DRIFT"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			prepared, _ := parsedConsumerLaunchRefusalFixture(t)
			tc.change(prepared)
			launch, err := prepared.ObserveConsumerLaunch(ctx, "serena-test")
			if launch != nil || err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("source drift reached environment or namespace observation: launch=%v err=%v, want %s", launch, err, tc.want)
			}
		})
	}
}

func TestPreparedSerenaLaunchObservationRejectsOriginalSelectionDrift(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	for _, tc := range []struct {
		name   string
		change func(*PreparedSerenaLaunchObservation)
	}{
		{"current descriptor binding", func(o *PreparedSerenaLaunchObservation) { o.binding.RecordSHA256 = strings.Repeat("b", 64) }},
		{"manifest source", func(o *PreparedSerenaLaunchObservation) { o.rawManifestSHA = strings.Repeat("b", 64) }},
		{"installed descriptor", func(o *PreparedSerenaLaunchObservation) { o.descriptor.ManifestHash = strings.Repeat("b", 64) }},
		{"publisher epoch", func(o *PreparedSerenaLaunchObservation) { o.epoch = strings.Repeat("b", 64) }},
		{"launch policy", func(o *PreparedSerenaLaunchObservation) { o.policy.CleanupLimit += time.Second }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, observation := parsedConsumerLaunchRefusalFixture(t)
			tc.change(observation)
			if err := observation.validatePreparedSource(ctx); err == nil || !strings.Contains(err.Error(), "SERENA_PARSED_PREPARATION_LAUNCH_DRIFT") {
				t.Fatalf("current selection substituted for original preparation: %v", err)
			}
		})
	}
}

func TestPreparedSerenaParsedInstallConsumerLaunchManifestCopyIsDetached(t *testing.T) {
	manifest := &config.ServerManifest{
		Name: SerenaServerName, BaseArgs: []string{"original-argument"},
		Env:            map[string]string{"SENTINEL": "original-value"},
		VendoredSource: &config.VendoredSource{Repo: "original-repository"},
	}
	prepared := &PreparedSerenaParsedInstall{manifest: manifest}
	before, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	copy, err := prepared.consumerLaunchManifest()
	if err != nil {
		t.Fatal(err)
	}
	if copy == manifest || copy.VendoredSource == manifest.VendoredSource {
		t.Fatal("manifest copy retained an original object")
	}
	copy.Name = "changed-name"
	copy.BaseArgs[0] = "changed-argument"
	copy.Env["SENTINEL"] = "changed-value"
	copy.VendoredSource.Repo = "changed-repository"
	after, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != string(before) {
		t.Fatalf("returned manifest changed the original: before=%s after=%s", before, after)
	}
	again, err := prepared.consumerLaunchManifest()
	if err != nil {
		t.Fatal(err)
	}
	againRaw, err := json.Marshal(again)
	if err != nil {
		t.Fatal(err)
	}
	if string(againRaw) != string(before) {
		t.Fatalf("later copy inherited caller mutations: got=%s want=%s", againRaw, before)
	}
}

// These data-only sentinels exercise refusal predicates, not issuance. No Ready
// record, namespace/read/close observation, publication, or consumer is created.
// The API has no assembly: a public-entry test reaching it gets a different
// error and fails rather than silently accepting an earlier unrelated refusal.
func parsedConsumerLaunchRefusalFixture(t *testing.T) (*PreparedSerenaParsedInstall, *PreparedSerenaLaunchObservation) {
	t.Helper()
	policy := DefaultSerenaPreparationPolicy()
	policySHA, err := policy.IdentitySHA256()
	if err != nil {
		t.Fatal(err)
	}
	rawSHA, epoch := strings.Repeat("a", 64), strings.Repeat("c", 64)
	owner := &PreparedSerenaRuntimeOwner{}
	a := NewAPI()
	a.preparedSerenaOwner = owner
	prepared := &PreparedSerenaParsedInstall{
		api: a, owner: owner, manifest: &config.ServerManifest{Name: SerenaServerName},
		manifestSHA: rawSHA, policy: policy,
		request: PreparedSerenaRequest{ManifestSHA256: rawSHA, PolicySHA256: policySHA},
		binding: PreparedSerenaRuntimeBinding{
			Schema: PreparedSerenaBindingSchema, RequestIdentitySHA256: strings.Repeat("d", 64),
			GenerationID: strings.Repeat("1", 32), RecordSHA256: strings.Repeat("e", 64),
			SourceSHA256: epoch, InterpreterSHA256: strings.Repeat("f", 64),
			EntrypointSHA256: strings.Repeat("2", 64), ResolutionSHA256: strings.Repeat("3", 64),
		},
		record: PreparedSerenaRuntimeRecord{SourceSHA256: epoch},
	}
	observation := &PreparedSerenaLaunchObservation{
		api: a, prepared: prepared, binding: prepared.binding,
		descriptor:     SupervisorDaemon{ManifestHash: rawSHA},
		rawManifestSHA: rawSHA, epoch: epoch, policy: policy,
	}
	return prepared, observation
}

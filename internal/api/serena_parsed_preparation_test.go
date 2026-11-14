package api

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"
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

package cli

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/gofrs/flock"
	"mcp-local-hub/internal/api"
	"mcp-local-hub/internal/config"
)

type refusingMigrateSerenaPreparation struct {
	t                  *testing.T
	root, task, rawSHA string
	cause              error
	calls              int
}

func (assembly *refusingMigrateSerenaPreparation) QualificationPort() api.PreparedSerenaQualificationPort {
	assembly.t.Fatal("refused preparation inputs reached package qualification")
	return nil
}

func (assembly *refusingMigrateSerenaPreparation) PrepareInputs(_ context.Context, manifest *config.ServerManifest, rawSHA string, tasks []string, _ *api.SerenaPreparationPolicyV1) (api.SerenaPreparationInputs, error) {
	assembly.calls++
	if manifest == nil || manifest.DaemonTemplate == nil || rawSHA != assembly.rawSHA || len(tasks) != 1 || tasks[0] != assembly.task {
		assembly.t.Fatalf("preparation lost exact source/task: manifest=%+v rawSHA=%q tasks=%v", manifest, rawSHA, tasks)
	}
	registryPath, err := api.DefaultRegistryPath()
	if err != nil {
		assembly.t.Fatal(err)
	}
	unlock, held, err := api.NewRegistry(registryPath).TryLock()
	if err != nil || !held {
		assembly.t.Fatalf("preparation ran under registry exclusion: held=%v err=%v", held, err)
	}
	if err = unlock(); err != nil {
		assembly.t.Fatal(err)
	}
	for _, leaf := range []string{"supervisor.lock.lock", "supervisor-intent.json.lock"} {
		lock := flock.New(filepath.Join(assembly.root, leaf))
		held, err := lock.TryLock()
		if err != nil || !held {
			assembly.t.Fatalf("preparation ran under %s: held=%v err=%v", leaf, held, err)
		}
		if err = lock.Unlock(); err != nil {
			assembly.t.Fatal(err)
		}
	}
	return api.SerenaPreparationInputs{}, assembly.cause
}

func TestMigrateSerenaPreparationFailurePrecedesReapAndRetainsRegistry(t *testing.T) {
	for _, late := range []bool{false, true} {
		name := "initial workspace"
		if late {
			name = "first workspace registered during reconcile"
		}
		t.Run(name, func(t *testing.T) {
			root, manifests := migrateSerenaTestEnv(t, migrateSerenaTestEnvOptions{canonicalRuntimeFixture: true})
			manifestPath := seedSerenaManifest(t, manifests, legacy2DaemonManifestYAML)
			before, err := os.ReadFile(manifestPath)
			if err != nil {
				t.Fatal(err)
			}
			workspace := t.TempDir()
			if !late {
				seedSerenaWorkspaceNoPort(t, workspace)
			}
			defer stubStartSupported(t, func() bool { return true })()
			defer stubSupervisorRunning(t, func() (bool, error) { return true, nil })()
			defer stubReap(t, func(context.Context, io.Writer) error {
				t.Fatal("preparation refusal reached reap")
				return nil
			})()
			defer stubStart(t, func(context.Context, io.Writer) error {
				t.Fatal("preparation refusal reached start")
				return nil
			})()
			defer stubAcquireInterlock(t, func() (*api.SupervisorLock, func(), error) {
				t.Fatal("preparation refusal acquired supervisor exclusion")
				return nil, nil, nil
			})()
			defer stubInstall(t, func(context.Context, *api.API, *config.ServerManifest, api.InstallParsedManifestOpts) (string, error) {
				t.Fatal("preparation refusal reached intent install")
				return "", nil
			})()
			reconciled, restored := 0, 0
			defer stubReconcile(t, func(context.Context, io.Writer) (*api.MigrateReport, error) {
				reconciled++
				if !late {
					t.Fatal("initial preparation failure changed clients")
				}
				seedSerenaWorkspaceNoPort(t, workspace)
				return &api.MigrateReport{}, nil
			})()
			originalRestore := restoreReconcileFn
			restoreReconcileFn = func(*api.MigrateReport) error { restored++; return nil }
			t.Cleanup(func() { restoreReconcileFn = originalRestore })
			cause := errors.New("controlled original Serena preparation input refusal")
			assembly := &refusingMigrateSerenaPreparation{t: t, root: root, task: api.SerenaTaskNameForWorkspace(workspace), rawSHA: api.ManifestHashContent(before), cause: cause}
			factories := 0
			err = runMigrateSerenaDynamicPoolWithDeps(context.Background(), io.Discard, migrateSerenaRunDeps{newInstallAPI: func() *api.API {
				factories++
				return api.NewAPIWithSerenaPreparation(assembly)
			}})
			if !errors.Is(err, cause) || assembly.calls != 1 || factories != 1 {
				t.Fatalf("original preparation cause/owner was lost: err=%v inputs=%d factories=%d", err, assembly.calls, factories)
			}
			wantReconcile := 0
			if late {
				wantReconcile = 1
			}
			if reconciled != wantReconcile || restored != wantReconcile {
				t.Fatalf("client compensation changed: reconcile=%d restore=%d want=%d", reconciled, restored, wantReconcile)
			}
			registryPath, err := api.DefaultRegistryPath()
			if err != nil {
				t.Fatal(err)
			}
			ports := loadRegistrySerenaPorts(t, registryPath)
			port, exists := ports[api.WorkspaceKey(workspace)]
			if !exists || port != 0 {
				t.Fatalf("pre-commit refusal lost workspace or retained allocated port: exists=%v port=%d", exists, port)
			}
			after, err := os.ReadFile(manifestPath)
			if err != nil || !bytes.Equal(before, after) {
				t.Fatalf("preparation refusal changed catalog manifest: %v", err)
			}
			if _, err := os.Stat(filepath.Join(root, "supervisor-intent.json")); !os.IsNotExist(err) {
				t.Fatalf("preparation refusal published intent: %v", err)
			}
		})
	}
}

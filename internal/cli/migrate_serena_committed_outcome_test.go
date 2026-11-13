package cli

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"mcp-local-hub/internal/api"
	"mcp-local-hub/internal/config"
)

// The stale workspace reaches the real parsed-install commit without issuing a
// prepared runtime. The returned path and bytes come from InstallParsedManifest;
// a real observed lease supplies the late-cancellation result. Package-release
// failure is injected at the caller seam. These are commit-disposition oracles,
// not proof of positive Serena package preparation or native consumer closure.
func TestMigrateSerenaInstallPathPreservesCommittedOutcome(t *testing.T) {
	for _, phase := range []string{"late cancellation", "package release failure", "precommit cancellation"} {
		t.Run(phase, func(t *testing.T) {
			root, manifests := migrateSerenaTestEnv(t, migrateSerenaTestEnvOptions{canonicalRuntimeFixture: true})
			seedSerenaManifest(t, manifests, legacy2DaemonManifestYAML)
			workspace := filepath.Join(t.TempDir(), "no-longer-present")
			seedSerenaWorkspaceNoPort(t, workspace)
			defer stubStartSupported(t, func() bool { return true })()
			defer stubSupervisorRunning(t, func() (bool, error) { return true, nil })()
			defer stubAcquireInterlock(t, realInterlockAcquire)()
			defer stubReconcile(t, func(context.Context, io.Writer) (*api.MigrateReport, error) {
				return &api.MigrateReport{}, nil
			})()
			reaps, starts, restores, installs := 0, 0, 0, 0
			defer stubReap(t, func(context.Context, io.Writer) error { reaps++; return nil })()
			defer stubRestoreReconcile(t, func(*api.MigrateReport) error { restores++; return nil })()
			defer stubStart(t, func(ctx context.Context, _ io.Writer) error {
				starts++
				if ctx.Err() != nil {
					t.Fatalf("operation cancellation escaped into supervisor continuation: %v", ctx.Err())
				}
				_, release, err := realInterlockAcquire()
				if err != nil {
					t.Fatalf("supervisor continuation still holds the quiet lock: %v", err)
				}
				release()
				return nil
			})()

			packageCause := errors.New("injected package lease release failure after intent commit")
			wantCommitted := phase != "precommit cancellation"
			wantCause := error(context.Canceled)
			if phase == "package release failure" {
				wantCause = packageCause
			}
			var committedPath string
			var committedBytes []byte
			defer stubInstall(t, func(ctx context.Context, installer *api.API, manifest *config.ServerManifest, opts api.InstallParsedManifestOpts) (string, error) {
				installs++
				if opts.SerenaPreparedInvocation != nil {
					t.Fatal("stale workspace unexpectedly supplied a prepared consumer")
				}
				work, cancel := context.WithTimeout(ctx, time.Minute)
				defer cancel()
				borrowPath := filepath.Join(root, "migration-outcome-borrow")
				lease, err := api.AcquireStateFileObservedLease(work, borrowPath)
				if err != nil {
					t.Fatal(err)
				}
				defer func() {
					proof, closeErr := lease.CloseObserved()
					if err := errors.Join(closeErr, proof.Validate(lease)); err != nil {
						t.Fatalf("close original fixture lease: %v", err)
					}
				}()
				path := ""
				borrowErr := lease.Borrow(work, borrowPath, func() error {
					if !wantCommitted {
						cancel()
					}
					var installErr error
					path, installErr = installer.InstallParsedManifest(work, manifest, opts)
					if !wantCommitted {
						if path != "" || !errors.Is(installErr, context.Canceled) {
							t.Fatalf("precommit cancellation returned path=%q err=%v", path, installErr)
						}
						return installErr
					}
					if installErr != nil || path == "" {
						t.Fatalf("real parsed install did not commit: path=%q err=%v", path, installErr)
					}
					committedPath = path
					committedBytes, err = os.ReadFile(path)
					if err != nil {
						t.Fatal(err)
					}
					if phase == "late cancellation" {
						cancel()
					}
					return nil
				})
				if phase == "package release failure" {
					borrowErr = errors.Join(borrowErr, api.ErrLockReleaseUnconfirmed, packageCause)
				}
				if !errors.Is(borrowErr, wantCause) || api.IsAppliedLockReleaseUnconfirmed(borrowErr) {
					t.Fatalf("fixture did not supply an unclassified settlement cause: %v", borrowErr)
				}
				return path, borrowErr
			})()

			var output bytes.Buffer
			err := runMigrateSerenaDynamicPool(context.Background(), &output)
			if !errors.Is(err, wantCause) {
				t.Fatalf("migration lost original settlement cause: %v", err)
			}
			if reaps != 1 || starts != 1 || installs != 1 {
				t.Fatalf("reap/start/install calls=%d/%d/%d, want 1/1/1", reaps, starts, installs)
			}
			regPath, err := api.DefaultRegistryPath()
			if err != nil {
				t.Fatal(err)
			}
			port := loadRegistrySerenaPorts(t, regPath)[api.WorkspaceKey(workspace)]
			if wantCommitted {
				after, err := os.ReadFile(committedPath)
				if err != nil || !bytes.Equal(after, committedBytes) {
					t.Fatalf("durable forward intent changed: %v", err)
				}
				if restores != 0 || port == 0 {
					t.Fatalf("committed intent was treated as precommit failure: client restores=%d registry port=%d", restores, port)
				}
			} else {
				if restores != 1 || port != 0 {
					t.Fatalf("precommit failure did not roll back: client restores=%d registry port=%d", restores, port)
				}
				if _, err := os.Stat(filepath.Join(root, "supervisor-intent.json")); !os.IsNotExist(err) {
					t.Fatalf("precommit cancellation wrote intent: %v", err)
				}
			}
			if strings.Contains(output.String(), "migration complete") {
				t.Fatalf("settlement failure reported clean completion: %s", output.String())
			}
		})
	}
}

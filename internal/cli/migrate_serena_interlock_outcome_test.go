package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"mcp-local-hub/internal/api"
	"mcp-local-hub/internal/config"
)

// These are driver-policy regressions. The adapter returns controlled outcomes;
// the original native acquisition/borrow/close contract is exercised separately
// by the Windows API owner tests and the qualified private migration cases.
func TestMigrateSerenaInterlockCloseControlsContinuation(t *testing.T) {
	for _, tc := range []struct {
		name               string
		running            bool
		lateWorkspace      bool
		acquireFails       bool
		poisonedLeaf       bool
		borrowFails        bool
		closeFails         bool
		committed          bool
		verifyFails        bool
		registryReadFails  bool
		secondSupportFails bool
		recoveryFails      bool
		restoreFails       bool
		wantReap           int
		wantInstall        int
		wantStart          int
		wantRestore        int
	}{
		{name: "prior-poison-refuses-without-new-owner", running: true, poisonedLeaf: true, wantReap: 1},
		{name: "initial-reap-partial-acquire-and-close-fail", running: true, acquireFails: true, closeFails: true, wantReap: 1},
		{name: "initial-reap-partial-acquire-close-settled", running: true, acquireFails: true, wantReap: 1, wantStart: 1, wantRestore: 1},
		{name: "no-reap-partial-acquire-and-close-fail", acquireFails: true, closeFails: true},
		{name: "late-reap-partial-acquire-and-close-fail", running: true, lateWorkspace: true, acquireFails: true, closeFails: true, wantReap: 1},
		{name: "registry-reread-and-close-fail", running: true, registryReadFails: true, closeFails: true, wantReap: 1},
		{name: "precommit-install-and-close-fail", running: true, closeFails: true, wantReap: 1, wantInstall: 1},
		{name: "no-reap-precommit-install-and-close-fail", closeFails: true, wantInstall: 1},
		{name: "precommit-install-close-settled", running: true, wantReap: 1, wantInstall: 1, wantStart: 1, wantRestore: 1},
		{name: "settled-close-keeps-recovery-and-restore-causes", running: true, recoveryFails: true, restoreFails: true, wantReap: 1, wantInstall: 1, wantStart: 1, wantRestore: 1},
		{name: "committed-install-and-close-fail", running: true, committed: true, closeFails: true, wantReap: 1, wantInstall: 1},
		{name: "committed-verify-and-close-fail", running: true, committed: true, verifyFails: true, closeFails: true, wantReap: 1, wantInstall: 1},
		{name: "deferred-close-vetoes-early-exit-rollback", running: true, secondSupportFails: true, closeFails: true, wantReap: 1},
		{name: "borrow-refuses-before-install", running: true, borrowFails: true, wantReap: 1, wantStart: 1, wantRestore: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			stateDir, manifests := migrateSerenaTestEnv(t, migrateSerenaTestEnvOptions{canonicalRuntimeFixture: true})
			seedSerenaManifest(t, manifests, legacy2DaemonManifestYAML)
			workspace := filepath.Join(t.TempDir(), "no-longer-present")
			if !tc.lateWorkspace {
				seedSerenaWorkspaceNoPort(t, workspace)
			}
			registryPath, err := api.DefaultRegistryPath()
			if err != nil {
				t.Fatal(err)
			}
			acquireCause := errors.New("controlled original partial acquisition failure")
			borrowCause := errors.New("controlled original borrow refusal")
			installCause := errors.New("controlled install outcome")
			closeCause := errors.New("controlled original interlock close failure")
			startCause := errors.New("controlled recovery start failure")
			restoreCause := errors.New("controlled client restore failure")
			var order []string
			var acquires, borrows, closes, reaps, installs, starts, restores, supportCalls int
			var registryBeforeContinuation, committedBytes []byte
			original := &migrateSerenaInterlock{
				borrow: func(_ context.Context, body func(api.InstallParsedManifestBypass) error) error {
					borrows++
					if tc.borrowFails {
						return borrowCause
					}
					return body(api.InstallParsedManifestBypass{})
				},
				close: func() error {
					closes++
					order = append(order, "close")
					var readErr error
					registryBeforeContinuation, readErr = os.ReadFile(registryPath)
					if readErr != nil {
						t.Fatal(readErr)
					}
					if tc.closeFails {
						return closeCause
					}
					return nil
				},
			}
			defer stubAcquireInterlock(t, func(context.Context) (*migrateSerenaInterlock, error) {
				acquires++
				if tc.poisonedLeaf {
					registryBeforeContinuation, err = os.ReadFile(registryPath)
					if err != nil {
						t.Fatal(err)
					}
					return nil, errors.Join(api.ErrLockReleaseUnconfirmed, acquireCause)
				}
				if tc.acquireFails {
					return original, acquireCause
				}
				return original, nil
			})()
			defer stubStartSupported(t, func() bool {
				supportCalls++
				return !tc.secondSupportFails || supportCalls == 1
			})()
			defer stubSupervisorRunning(t, func() (bool, error) { return tc.running, nil })()
			defer stubReconcile(t, func(context.Context, io.Writer) (*api.MigrateReport, error) {
				if tc.lateWorkspace {
					seedSerenaWorkspaceNoPort(t, workspace)
				}
				return &api.MigrateReport{}, nil
			})()
			defer stubReap(t, func(context.Context, io.Writer) error {
				reaps++
				if tc.registryReadFails {
					if err := os.WriteFile(registryPath, []byte("workspaces: ["), 0o600); err != nil {
						t.Fatal(err)
					}
				}
				return nil
			})()
			defer stubStart(t, func(context.Context, io.Writer) error {
				starts++
				order = append(order, "start")
				if closes != 1 || tc.closeFails {
					t.Error("supervisor continuation ran before a settled original close")
				}
				if tc.recoveryFails {
					return startCause
				}
				return nil
			})()
			defer stubRestoreReconcile(t, func(*api.MigrateReport) error {
				restores++
				order = append(order, "restore")
				if closes != 1 || tc.closeFails {
					t.Error("client rollback ran before a settled original close")
				}
				if tc.restoreFails {
					return restoreCause
				}
				return nil
			})()
			defer stubInstall(t, func(ctx context.Context, installer *api.API, manifest *config.ServerManifest, opts api.InstallParsedManifestOpts) (string, error) {
				installs++
				if !tc.committed {
					return "", installCause
				}
				path, installErr := installMigrateSerenaNoLiveWorkspacesForTest(t, ctx, installer, manifest, opts)
				if installErr != nil {
					t.Fatalf("real zero-live commit: %v", installErr)
				}
				if tc.verifyFails {
					if err := os.WriteFile(path, []byte("{not-valid-json"), 0o600); err != nil {
						t.Fatal(err)
					}
				}
				committedBytes, err = os.ReadFile(path)
				if err != nil {
					t.Fatal(err)
				}
				return path, installCause
			})()

			deps := defaultMigrateSerenaRunDeps()
			deps.newInstallOperation = callerMigrateSerenaInstallOperationForTest
			var output bytes.Buffer
			err = runMigrateSerenaDynamicPoolWithDeps(context.Background(), &output, deps)
			if err == nil {
				t.Fatal("controlled failure returned clean success")
			}
			for _, expected := range []struct {
				present bool
				cause   error
			}{
				{tc.acquireFails || tc.poisonedLeaf, acquireCause}, {tc.borrowFails, borrowCause},
				{tc.poisonedLeaf, api.ErrLockReleaseUnconfirmed},
				{tc.wantInstall != 0, installCause}, {tc.closeFails, closeCause},
				{tc.recoveryFails, startCause}, {tc.restoreFails, restoreCause},
			} {
				if errors.Is(err, expected.cause) != expected.present {
					t.Errorf("cause %v present=%t, want %t; outcome=%v", expected.cause, errors.Is(err, expected.cause), expected.present, err)
				}
			}
			var closeOutcome *migrateSerenaInterlockCloseError
			if errors.As(err, &closeOutcome) != tc.closeFails || (closeOutcome != nil && closeOutcome.original != original) {
				t.Fatalf("close outcome did not retain the exact original adapter: %v", err)
			}
			wantClose := 1
			if tc.poisonedLeaf {
				wantClose = 0
			}
			if acquires != 1 || closes != wantClose || reaps != tc.wantReap || installs != tc.wantInstall || starts != tc.wantStart || restores != tc.wantRestore {
				t.Fatalf("acquire/close/reap/install/start/restore=%d/%d/%d/%d/%d/%d, want 1/%d/%d/%d/%d/%d", acquires, closes, reaps, installs, starts, restores, wantClose, tc.wantReap, tc.wantInstall, tc.wantStart, tc.wantRestore)
			}
			wantBorrow := tc.wantInstall
			if tc.borrowFails {
				wantBorrow = 1
			}
			if borrows != wantBorrow {
				t.Fatalf("borrow calls=%d, want %d", borrows, wantBorrow)
			}
			if tc.poisonedLeaf {
				if len(order) != 0 {
					t.Fatalf("prior poison entered close/start/restore without a new owner: %v", order)
				}
			} else if len(order) == 0 || order[0] != "close" {
				t.Fatalf("continuation order=%v, want original close first", order)
			}
			if tc.secondSupportFails && supportCalls != 2 {
				t.Fatalf("implicit return did not reach the post-acquisition preflight: calls=%d", supportCalls)
			}
			if tc.registryReadFails && !strings.Contains(err.Error(), "re-read registry") {
				t.Fatalf("original registry read failure was lost: %v", err)
			}
			if tc.verifyFails {
				var syntaxErr *json.SyntaxError
				if !errors.As(err, &syntaxErr) {
					t.Fatalf("original committed-intent parse cause lost: %v", err)
				}
			}
			if tc.closeFails || tc.poisonedLeaf {
				after, readErr := os.ReadFile(registryPath)
				if readErr != nil || !bytes.Equal(after, registryBeforeContinuation) {
					t.Fatalf("unknown close allowed registry compensation: err=%v", readErr)
				}
			} else if port := loadRegistrySerenaPorts(t, registryPath)[api.WorkspaceKey(workspace)]; port != 0 {
				t.Fatalf("settled precommit close did not allow original registry rollback: port=%d", port)
			}
			intentPath := filepath.Join(stateDir, "supervisor-intent.json")
			if tc.committed {
				after, readErr := os.ReadFile(intentPath)
				if readErr != nil || !bytes.Equal(after, committedBytes) {
					t.Fatalf("unknown close changed the committed intent: err=%v", readErr)
				}
			} else if _, statErr := os.Stat(intentPath); !os.IsNotExist(statErr) {
				t.Fatalf("precommit failure wrote an intent: %v", statErr)
			}
			if strings.Contains(output.String(), "migration complete") {
				t.Fatalf("failed interlock continuation reported clean success: %s", output.String())
			}
		})
	}
}

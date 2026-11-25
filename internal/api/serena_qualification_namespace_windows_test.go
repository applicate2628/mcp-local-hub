//go:build windows && serena_qualification && test_state_path_env

package api

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"mcp-local-hub/internal/buildinfo"
)

func serenaQualificationDirectoryFixture(t *testing.T) (context.Context, string, string) {
	t.Helper()
	runRoot := hardenedTempDir(t)
	root := filepath.Join(runRoot, "state")
	if err := os.Mkdir(root, 0o700); err != nil {
		t.Fatal(err)
	}
	entries, err := allowlistExplicitAccess()
	if err != nil {
		t.Fatal(err)
	}
	applyProtectedDACLFromEntries(t, root, entries)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	t.Cleanup(cancel)
	return ctx, runRoot, root
}

func TestSerenaQualificationDirectoriesRetainIdentityUntilClose(t *testing.T) {
	ctx, runRoot, root := serenaQualificationDirectoryFixture(t)
	chain, err := openSerenaQualificationDirectories(ctx, runRoot, root)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := chain.close(); err != nil {
			t.Error(err)
		}
	})
	if err := chain.validate(ctx); err != nil {
		t.Fatal(err)
	}
	renamed := root + "-rebound"
	if err := os.Rename(root, renamed); err == nil {
		t.Fatal("retained state directory allowed its pathname to be rebound")
	}
	if err := os.WriteFile(filepath.Join(root, "allowed-state-write"), []byte("state"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := chain.validate(ctx); err != nil {
		t.Fatalf("legitimate contents changed the retained directory identity: %v", err)
	}
	if err := chain.close(); err != nil {
		t.Fatal(err)
	}
	if err := chain.validate(ctx); err == nil {
		t.Fatal("closed native directory owner remained valid")
	}
	if err := chain.close(); err != nil {
		t.Fatalf("second Close did not return the original result: %v", err)
	}
	for _, entry := range chain.entries {
		if !entry.handle.close.Attempted || !entry.handle.close.Closed {
			t.Fatal("original native directory handle has no successful close")
		}
	}
	if err := os.Rename(root, renamed); err != nil {
		t.Fatalf("native directory handle remained held after Close: %v", err)
	}
}

func TestSerenaQualificationDirectoriesRecheckACL(t *testing.T) {
	for _, target := range []string{"run-root", "state-root"} {
		t.Run(target, func(t *testing.T) {
			ctx, runRoot, root := serenaQualificationDirectoryFixture(t)
			chain, err := openSerenaQualificationDirectories(ctx, runRoot, root)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				if err := chain.close(); err != nil {
					t.Error(err)
				}
			})
			path := root
			if target == "run-root" {
				path = runRoot
			}
			broadenParentForStateFileTest(t, path)
			if err := chain.validate(ctx); err == nil {
				t.Fatal("broadened private directory ACL retained qualification authority")
			}
		})
	}
}

func TestSerenaQualificationDirectoriesRejectJunctionEscape(t *testing.T) {
	ctx, runRoot, _ := serenaQualificationDirectoryFixture(t)
	outside := hardenedTempDir(t)
	target := filepath.Join(outside, "outside-state")
	if err := os.Mkdir(target, 0o700); err != nil {
		t.Fatal(err)
	}
	junction := filepath.Join(runRoot, "junction")
	if err := createJunctionForTest(junction, outside); err != nil {
		t.Fatalf("required native junction fixture: %v", err)
	}
	chain, err := openSerenaQualificationDirectories(ctx, runRoot, filepath.Join(junction, "outside-state"))
	if err == nil {
		_ = chain.close()
		t.Fatal("no-follow chain traversed a junction outside the private run")
	}
	if chain == nil || !chain.closed || chain.closeErr != nil {
		t.Fatalf("refused chain did not settle acquired directory handles: %#v", chain)
	}
}

func TestSerenaQualificationDirectoriesClosePartialAcquisition(t *testing.T) {
	ctx, runRoot, root := serenaQualificationDirectoryFixture(t)
	leaf := filepath.Join(root, "regular-file")
	if err := os.WriteFile(leaf, []byte("not a directory"), 0o600); err != nil {
		t.Fatal(err)
	}
	chain, err := openSerenaQualificationDirectories(ctx, runRoot, leaf)
	if err == nil || chain == nil || !chain.closed || chain.closeErr != nil {
		t.Fatalf("partial native acquisition was not refused and closed: chain=%#v err=%v", chain, err)
	}
	for _, entry := range chain.entries {
		if !entry.handle.close.Attempted || !entry.handle.close.Closed {
			t.Fatal("partial acquisition retained an original native handle")
		}
	}
	if err := os.Rename(root, root+"-after-refusal"); err != nil {
		t.Fatalf("partial acquisition still holds the directory: %v", err)
	}
}

func TestSerenaQualificationDirectoriesCancellation(t *testing.T) {
	ctx, runRoot, root := serenaQualificationDirectoryFixture(t)
	bound, cancel := context.WithCancel(ctx)
	chain, err := openSerenaQualificationDirectories(bound, runRoot, root)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := chain.close(); err != nil {
			t.Error(err)
		}
	})
	cancel()
	if err := chain.validate(bound); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled namespace remained current: %v", err)
	}
}

func TestSerenaQualificationNamespaceRejectsInvalidInputs(t *testing.T) {
	ctx, runRoot, root := serenaQualificationDirectoryFixture(t)
	t.Setenv(serenaQualificationRunRootEnv, runRoot)
	t.Setenv("MCPHUB_STATE_DIR_OVERRIDE", root)
	t.Cleanup(SetDaemonStateRootForTest(root))
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	for _, test := range []struct {
		name    string
		ctx     context.Context
		runRoot string
		root    string
	}{
		{"nil-context", nil, runRoot, root},
		{"unbounded-context", context.Background(), runRoot, root},
		{"canceled-context", canceled, runRoot, root},
		{"missing-run-root", ctx, "", root},
		{"relative-root", ctx, runRoot, "relative"},
		{"run-root-is-state", ctx, runRoot, runRoot},
		{"sibling-root", ctx, runRoot, runRoot + "-sibling"},
		{"unclean-root", ctx, runRoot, root + string(filepath.Separator) + ".."},
		{"volume-root", ctx, filepath.VolumeName(runRoot) + string(filepath.Separator), root},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Setenv(serenaQualificationRunRootEnv, test.runRoot)
			scope, err := OpenSerenaQualificationNamespace(test.ctx, test.root)
			if scope != nil {
				_ = scope.Close()
				t.Fatal("invalid input issued a namespace owner")
			}
			if err == nil {
				t.Fatal("invalid input was accepted")
			}
		})
	}
}

func TestSerenaQualificationNamespaceRechecksStateBindings(t *testing.T) {
	_, runRoot, root := serenaQualificationDirectoryFixture(t)
	t.Setenv(serenaQualificationRunRootEnv, runRoot)
	t.Setenv("MCPHUB_STATE_DIR_OVERRIDE", root)
	t.Cleanup(SetDaemonStateRootForTest(root))
	if err := validateSerenaQualificationStateBinding(runRoot, root); err != nil {
		t.Fatal(err)
	}
	t.Setenv("MCPHUB_STATE_DIR_OVERRIDE", root+"-different")
	if err := validateSerenaQualificationStateBinding(runRoot, root); err == nil {
		t.Fatal("different cross-process state root was accepted")
	}
	t.Setenv("MCPHUB_STATE_DIR_OVERRIDE", root)
	restore := SetDaemonStateRootForTest(root + "-different")
	if err := validateSerenaQualificationStateBinding(runRoot, root); err == nil {
		t.Fatal("different ordinary state resolver root was accepted")
	}
	restore()
	t.Setenv(serenaQualificationRunRootEnv, runRoot+"-different")
	if err := validateSerenaQualificationStateBinding(runRoot, root); err == nil {
		t.Fatal("changed run-root confinement was accepted")
	}
	t.Setenv(serenaQualificationRunRootEnv, runRoot)
	if err := validateSerenaQualificationStateBinding(runRoot, root); err != nil {
		t.Fatal(err)
	}
}

func TestSerenaQualificationNamespaceRequiresIssuedPublisherSource(t *testing.T) {
	ctx, runRoot, root := serenaQualificationDirectoryFixture(t)
	t.Setenv(serenaQualificationRunRootEnv, runRoot)
	t.Setenv("MCPHUB_STATE_DIR_OVERRIDE", root)
	t.Cleanup(SetDaemonStateRootForTest(root))
	// This API unit-test invocation is deliberately an ordinary unmarked Go
	// test executable. The marked CLI integration binary exercises success.
	if _, err := buildinfo.CurrentPublisherSource(); !errors.Is(err, buildinfo.ErrPublisherSourceUnmarked) {
		t.Fatalf("ordinary unmarked API test executable required: %v", err)
	}
	scope, err := OpenSerenaQualificationNamespace(ctx, root)
	if scope != nil {
		_ = scope.Close()
		t.Fatal("unissued source acquired a private qualification namespace")
	}
	if !errors.Is(err, buildinfo.ErrPublisherSourceUnmarked) {
		t.Fatalf("unissued source was not the refusal cause: %v", err)
	}
	if path, observation, err := ResolveDaemonStateDirObserved(ctx); err == nil || path != "" || observation != nil {
		t.Fatalf("missing scope entered a namespace: path=%q observation=%v err=%v", path, observation, err)
	}
}

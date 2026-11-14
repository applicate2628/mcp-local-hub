//go:build windows

package cli

import (
	"bytes"
	"errors"
	"io"
	"testing"

	"github.com/spf13/cobra"

	"mcp-local-hub/internal/api"
	"mcp-local-hub/internal/binaryadmission"
)

func TestCanonicalEntryConstructorErrorPrecedesRelevantCommandEffects(t *testing.T) {
	oldBootstrap, oldDispatcher := setupBootstrapFn, upgradeDispatcher
	t.Cleanup(func() { setupBootstrapFn, upgradeDispatcher = oldBootstrap, oldDispatcher })
	effects := 0
	setupBootstrapFn = func(io.Writer) error { effects++; return nil }
	upgradeDispatcher = func(*cobra.Command) error { effects++; return nil }
	first, second := errors.New("first immutable constructor error"), errors.New("second immutable constructor error")
	for _, failure := range []error{first, second, first} {
		for _, args := range [][]string{{"setup"}, {"canonicalize"}, {"upgrade"}, {"install", "--upgrade"}} {
			root := NewRootCmdWithWindowsCanonicalEntryBinding(binaryadmission.WindowsCanonicalEntryBuildBindingV1{}, failure)
			root.SetOut(io.Discard)
			root.SetErr(io.Discard)
			root.SetArgs(args)
			if err := root.Execute(); !errors.Is(err, failure) {
				t.Fatalf("constructor error lost for %v: %v", args, err)
			}
		}
	}
	if effects != 0 {
		t.Fatal("constructor error reached a mutating command adapter")
	}
}

func TestCanonicalEntryConstructorErrorDoesNotChangeHelpOrVersion(t *testing.T) {
	for _, args := range [][]string{{"--help"}, {"--version"}, {"setup", "--help"}, {"upgrade", "--help"}} {
		root := NewRootCmdWithWindowsCanonicalEntryBinding(binaryadmission.WindowsCanonicalEntryBuildBindingV1{}, errors.New("unusable build binding"))
		var output bytes.Buffer
		root.SetOut(&output)
		root.SetErr(&output)
		root.SetArgs(args)
		if err := root.Execute(); err != nil || output.Len() == 0 {
			t.Fatalf("read-only discovery changed for %v: %v", args, err)
		}
	}
}

func TestCanonicalEntryBindingErrorDoesNotVetoIndependentRouterRollback(t *testing.T) {
	oldRouter, oldBootstrap := setupLSPClientRouterFn, setupBootstrapFn
	t.Cleanup(func() { setupLSPClientRouterFn, setupBootstrapFn = oldRouter, oldBootstrap })
	rollbacks, installs := 0, 0
	setupLSPClientRouterFn = func(rollback bool) (*api.LSPClientRouterReport, error) {
		if !rollback {
			t.Error("ordinary router install reached")
		}
		rollbacks++
		return &api.LSPClientRouterReport{}, nil
	}
	setupBootstrapFn = func(io.Writer) error { installs++; return nil }
	failure := errors.New("unusable canonical entry binding")
	root := NewRootCmdWithWindowsCanonicalEntryBinding(binaryadmission.WindowsCanonicalEntryBuildBindingV1{}, failure)
	root.SetOut(io.Discard)
	root.SetErr(io.Discard)
	root.SetArgs([]string{"setup", "--rollback-lsp-router"})
	if err := root.Execute(); err != nil || rollbacks != 1 || installs != 0 {
		t.Fatalf("independent rollback incorrectly gated: rollback=%d install=%d err=%v", rollbacks, installs, err)
	}
	root = NewRootCmdWithWindowsCanonicalEntryBinding(binaryadmission.WindowsCanonicalEntryBuildBindingV1{}, failure)
	root.SetOut(io.Discard)
	root.SetErr(io.Discard)
	root.SetArgs([]string{"setup"})
	if err := root.Execute(); !errors.Is(err, failure) || rollbacks != 1 || installs != 0 {
		t.Fatal("ordinary setup did not retain its pre-effect binding failure")
	}
}

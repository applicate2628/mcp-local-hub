// Package cli — area-5 gap-c bless co-design tests for `mcphub workspace
// register --backend serena`. The explicit serena register must seed trust by
// blessing the workspace's CANONICAL root (so the area-5 serena trust gate
// authorizes sibling auto-introduce under that tree), and a bless failure must
// only WARN — never fail the register.
package cli

import (
	"errors"
	"strings"
	"testing"

	"mcp-local-hub/internal/api"
)

// stubSerenaRegisterBless overrides the explicit-register bless seam for the
// test scope and returns a pointer to the captured canonical roots so a test
// can assert the bless fired exactly once with the canonical root.
func stubSerenaRegisterBless(t *testing.T, fn func(canonical string) error) *[]string {
	t.Helper()
	captured := &[]string{}
	orig := serenaRegisterBlessTrustedRootFn
	serenaRegisterBlessTrustedRootFn = func(canonical string) error {
		*captured = append(*captured, canonical)
		if fn != nil {
			return fn(canonical)
		}
		return nil
	}
	t.Cleanup(func() { serenaRegisterBlessTrustedRootFn = orig })
	return captured
}

// TestWorkspaceRegisterSerena_BlessesCanonicalRootOnce: an explicit serena
// register must call the bless seam exactly once, with the CANONICAL workspace
// root the registry row stores.
func TestWorkspaceRegisterSerena_BlessesCanonicalRootOnce(t *testing.T) {
	withSerenaManifest(t, 9121, 9123)
	withStateDir(t)
	blessed := stubSerenaRegisterBless(t, nil)

	tmp := t.TempDir()
	ws := makeWorkspaceDir(t, tmp, []string{"python"})
	wantCanonical, err := api.CanonicalWorkspacePath(ws)
	if err != nil {
		t.Fatalf("canonicalize: %v", err)
	}

	if out, err := runWorkspaceCmd(t, "register", ws); err != nil {
		t.Fatalf("register: %v\noutput: %s", err, out)
	}

	if len(*blessed) != 1 {
		t.Fatalf("bless seam called %d times, want exactly 1: %v", len(*blessed), *blessed)
	}
	if (*blessed)[0] != wantCanonical {
		t.Errorf("bless saw root %q, want canonical %q (must bless the canonical root the row stores)", (*blessed)[0], wantCanonical)
	}
}

// TestWorkspaceRegisterSerena_BlessFailureWarnsButRegisterSucceeds: a bless
// failure is best-effort/warn-only — the register still succeeds and the row is
// persisted, with a warning emitted.
func TestWorkspaceRegisterSerena_BlessFailureWarnsButRegisterSucceeds(t *testing.T) {
	withSerenaManifest(t, 9121, 9123)
	withStateDir(t)
	stubSerenaRegisterBless(t, func(string) error {
		return errors.New("simulated trusted-roots store write failure")
	})

	tmp := t.TempDir()
	ws := makeWorkspaceDir(t, tmp, []string{"python"})

	out, err := runWorkspaceCmd(t, "register", ws)
	if err != nil {
		t.Fatalf("register must SUCCEED despite a bless failure (best-effort); got %v\noutput: %s", err, out)
	}
	if !strings.Contains(out, "warning:") || !strings.Contains(out, "trusted root") {
		t.Errorf("a bless failure should emit a 'warning: ... trusted root' line; got %q", out)
	}

	// The row IS persisted.
	regPath, _ := api.DefaultRegistryPath()
	reg := api.NewRegistry(regPath)
	if err := reg.Load(); err != nil {
		t.Fatalf("load registry: %v", err)
	}
	if got := reg.SerenaEntries(); len(got) != 1 {
		t.Fatalf("want 1 serena row after a bless-failure register, got %d", len(got))
	}
}

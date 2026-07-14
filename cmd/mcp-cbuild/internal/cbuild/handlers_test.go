package cbuild

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"mcp-local-hub/cmd/mcp-cbuild/internal/mcp"
)

func toolNamed(t *testing.T, name string) mcp.Tool {
	t.Helper()
	for _, tl := range Tools("") {
		if tl.Name() == name {
			return tl
		}
	}
	t.Fatalf("tool %q not registered", name)
	return nil
}

func callWithArgs(t *testing.T, tl mcp.Tool, args map[string]any) (any, error) {
	t.Helper()
	b, err := json.Marshal(args)
	if err != nil {
		t.Fatalf("marshal args: %v", err)
	}
	return tl.Call(context.Background(), b)
}

// TestCmakeBuildRejectsFlagTarget proves a flag-shaped build target is rejected
// as a param error BEFORE any cmake resolution (so it holds on cmake-less
// hosts), preventing argument injection into the cmake command line.
func TestCmakeBuildRejectsFlagTarget(t *testing.T) {
	bld := toolNamed(t, "cmake_build")
	dir := t.TempDir()
	for _, bad := range []string{"--clean-first", "--", "-j"} {
		_, err := callWithArgs(t, bld, map[string]any{
			"preset":      "default",
			"working_dir": dir,
			"targets":     []string{bad},
		})
		if err == nil {
			t.Errorf("target %q: expected a param error, got nil", bad)
			continue
		}
		var pe *mcp.ParamError
		if !errors.As(err, &pe) {
			t.Errorf("target %q: error = %v (%T), want *mcp.ParamError", bad, err, err)
		}
	}

	// A normal target passes validation (it fails later only if cmake is absent,
	// which is a different, non-param error — not asserted here).
	_, err := callWithArgs(t, bld, map[string]any{
		"preset":      "default",
		"working_dir": dir,
		"targets":     []string{"mylib"},
	})
	var pe *mcp.ParamError
	if errors.As(err, &pe) {
		t.Errorf("normal target wrongly rejected as a param error: %v", err)
	}
}

// TestVcpkgSearchRejectsFlagQuery proves a flag-shaped search query is rejected
// as a param error before any vcpkg resolution.
func TestVcpkgSearchRejectsFlagQuery(t *testing.T) {
	search := toolNamed(t, "vcpkg_search")
	_, err := callWithArgs(t, search, map[string]any{"query": "--x-json"})
	if err == nil {
		t.Fatal("expected a param error for a flag-shaped query")
	}
	var pe *mcp.ParamError
	if !errors.As(err, &pe) {
		t.Errorf("error = %v (%T), want *mcp.ParamError", err, err)
	}
}

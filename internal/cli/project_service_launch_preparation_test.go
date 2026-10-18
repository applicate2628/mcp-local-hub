package cli

import (
	"bytes"
	"context"
	"errors"
	"path/filepath"
	"testing"

	"mcp-local-hub/internal/api"
	"mcp-local-hub/internal/daemon"
)

// These are acquisition-DATA fixtures, not original native/provider receipts.
// The API still has to qualify every actual selected file and module meaning.
func TestProjectServiceGraphifyProjectionUsesSelectedShim(t *testing.T) {
	root := t.TempDir()
	interpreter := filepath.Join(root, "venv", "Scripts", "python.exe")
	profile := daemon.StdioLaunchExecutionProfile{CommandPath: filepath.Join(root, "graphify-mcp.exe")}
	selected := []byte("synthetic PE prefix#!\"" + interpreter + "\"\nPK\x03\x04synthetic archive")
	files, err := projectServiceProviderInputs(api.ProjectServiceGraphify, profile, selected)
	if err != nil || len(files) != 7 {
		t.Fatalf("declared shim edge = %v", err)
	}
	if files[0].Path != profile.CommandPath || files[1].Path != interpreter || files[2].Path != filepath.Join(root, "venv", "pyvenv.cfg") {
		t.Fatal("projection replaced the actual selected shim/interpreter")
	}
	if files[5].Role != api.ProjectServiceProviderEntry || files[5].Path != filepath.Join(root, "venv", "Lib", "site-packages", "graphify", "serve.py") {
		t.Fatal("projection did not retain the declared package edge")
	}
	if _, err := projectServiceProviderInputs(api.ProjectServiceGraphify, profile, []byte("unjoined executable")); err == nil {
		t.Fatal("missing original selected shim became a provider")
	}
}

func TestProjectServiceGraftProjectionUsesDirectOriginalModule(t *testing.T) {
	root := t.TempDir()
	profile := daemon.StdioLaunchExecutionProfile{CommandPath: filepath.Join(root, "node.exe"), WorkingDirectory: root, Arguments: []string{filepath.Join("dist", "cli.js"), "mcp", "intentional-other-dataset"}}
	files, err := projectServiceProviderInputs(api.ProjectServiceGraft, profile, nil)
	if err != nil || len(files) != 3 {
		t.Fatalf("direct module = %v", err)
	}
	if files[1].Path != filepath.Join(root, "dist", "cli.js") || files[2].Path != filepath.Join(root, "package.json") || profile.Arguments[2] != "intentional-other-dataset" {
		t.Fatal("original module/context/dataset was rewritten")
	}
	profile.CommandPath = filepath.Join(root, "npx.cmd")
	if _, err := projectServiceProviderInputs(api.ProjectServiceGraft, profile, nil); err == nil {
		t.Fatal("npx was treated as a direct selected Node module")
	}
	profile.CommandPath = filepath.Join(root, "node.exe")
	profile.WorkingDirectory = ""
	if _, err := projectServiceProviderInputs(api.ProjectServiceGraft, profile, nil); err == nil {
		t.Fatal("missing actual relative-module context qualified")
	}
}

func TestProjectServiceCLISelectionCannotFallThroughToLegacy(t *testing.T) {
	for _, selectors := range [][]string{{"--project-service="}, {"--project-service", "unknown", "--project-service-intent", "define-stopped"}, {"--project-service-intent", "define-stopped"}} {
		t.Run(selectors[0], func(t *testing.T) {
			legacyCalled := false
			command := newAdoptCmdWithDepsAndPlanBuilder(api.NewAPI, nil, nil, func(*api.API, api.AdoptOpts) (*api.AdoptPlan, error) { legacyCalled = true; return nil, nil })
			command.SetOut(&bytes.Buffer{})
			command.SetErr(&bytes.Buffer{})
			command.SetContext(context.Background())
			command.SetArgs(append([]string{"entry", "--client", "codex-cli"}, selectors...))
			if err := command.Execute(); err == nil || legacyCalled {
				t.Fatal("selected common mode silently used the legacy builder")
			}
		})
	}
	if port := (&projectServiceLaunchReadSource{}).originalSourcePort(); port != nil {
		t.Fatal("managed source became a nonnil typed-nil port")
	}
}

func TestProjectServiceCLIOmissionKeepsLegacyBuilder(t *testing.T) {
	want := errors.New("synthetic legacy builder stop")
	legacyCalled := false
	command := newAdoptCmdWithDepsAndPlanBuilder(api.NewAPI, nil, nil, func(_ *api.API, opts api.AdoptOpts) (*api.AdoptPlan, error) {
		legacyCalled = true
		if opts.EntryName != "entry" || opts.Client != "codex-cli" {
			t.Fatal("legacy selection changed")
		}
		return nil, want
	})
	command.SetOut(&bytes.Buffer{})
	command.SetErr(&bytes.Buffer{})
	command.SetContext(context.Background())
	command.SetArgs([]string{"entry", "--client", "codex-cli"})
	if err := command.Execute(); !errors.Is(err, want) || !legacyCalled {
		t.Fatal("omitted common selectors did not retain the original legacy builder")
	}
}

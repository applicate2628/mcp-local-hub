package cli

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"mcp-local-hub/internal/api/apitest"
)

func TestInodeAnchorStrictModeRecoverRejectsBreadcrumbSymlink(t *testing.T) {
	tmp := setupSupervisorFixture(t)
	target := filepath.Join(tmp.stateDir, "target-strict-mode-mutation-incomplete.json")
	raw, err := json.Marshal(strictModeBreadcrumb{
		Intended:          true,
		ActualIntentState: false,
		ActualShimState:   false,
		Step2Error:        "simulated shim failure",
		RevertError:       "simulated revert failure",
		TS:                "2026-06-20T00:00:00Z",
	})
	if err != nil {
		t.Fatalf("marshal breadcrumb: %v", err)
	}
	if err := os.WriteFile(target, raw, 0o600); err != nil {
		t.Fatalf("write target breadcrumb: %v", err)
	}
	if err := os.Symlink(target, tmp.BreadcrumbPath()); err != nil {
		t.Skipf("symlink unsupported in this environment: %v", err)
	}

	deps := tmp.Deps()
	deps.PromptOperator = func() (string, error) {
		return "", errors.New("prompt should not run")
	}

	err = RunStrictModeRecover(deps)
	if err == nil {
		t.Fatalf("RunStrictModeRecover followed a symlinked breadcrumb; want inode-anchor refusal")
	}
	if strings.Contains(err.Error(), "prompt should not run") {
		t.Fatalf("RunStrictModeRecover read symlinked breadcrumb and reached prompt: %v", err)
	}
}

func TestInodeAnchorDefaultWorkspaceRejectsSymlink(t *testing.T) {
	stateDir := apitest.HardenedTempDir(t)
	target := filepath.Join(stateDir, "target-"+defaultWorkspaceFilename)
	if err := os.WriteFile(target, []byte(filepath.Join(stateDir, "workspace")), 0o600); err != nil {
		t.Fatalf("write target default workspace: %v", err)
	}
	link := filepath.Join(stateDir, defaultWorkspaceFilename)
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("symlink unsupported in this environment: %v", err)
	}

	if _, err := readDefaultWorkspace(stateDir); err == nil {
		t.Fatalf("readDefaultWorkspace followed a symlink; want inode-anchor refusal")
	}
}

func TestInodeAnchorDefaultWorkspaceMissingSemanticsPreserved(t *testing.T) {
	stateDir := apitest.HardenedTempDir(t)

	got, err := readDefaultWorkspace(stateDir)
	if err != nil {
		t.Fatalf("readDefaultWorkspace missing err = %v", err)
	}
	if got != "" {
		t.Fatalf("readDefaultWorkspace missing = %q, want empty string", got)
	}
}

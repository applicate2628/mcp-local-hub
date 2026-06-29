package cli

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"mcp-local-hub/internal/api"
)

func TestRepairStateDACL_PathRequired(t *testing.T) {
	stateDir := t.TempDir()
	restore := api.SetDaemonStateRootForTest(stateDir)
	t.Cleanup(restore)

	stdout, stderr, err := runRepairStateDACLCmd(t, "", "--yes")
	if err == nil {
		t.Fatalf("repair-state-dacl without --path succeeded; stdout=%q stderr=%q", stdout, stderr)
	}
	if !strings.Contains(err.Error(), "--path is required") {
		t.Fatalf("error = %q, want --path is required", err.Error())
	}
}

func TestRepairStateDACL_PathOutsideStateDirRejected(t *testing.T) {
	stateDir := t.TempDir()
	restore := api.SetDaemonStateRootForTest(stateDir)
	t.Cleanup(restore)

	outside := filepath.Join(t.TempDir(), "workspaces.yaml")
	if err := os.WriteFile(outside, []byte("version: 1\nworkspaces: []\n"), 0o600); err != nil {
		t.Fatalf("write outside target: %v", err)
	}

	stdout, stderr, err := runRepairStateDACLCmd(t, "", "--path", outside, "--yes")
	if err == nil {
		t.Fatalf("repair-state-dacl accepted outside path; stdout=%q stderr=%q", stdout, stderr)
	}
	if !strings.Contains(err.Error(), "outside state dir") {
		t.Fatalf("error = %q, want outside state dir", err.Error())
	}
}

func TestRepairStateDACL_RelativePathResolvesUnderStateDir(t *testing.T) {
	stateDir := t.TempDir()
	restore := api.SetDaemonStateRootForTest(stateDir)
	t.Cleanup(restore)

	want, err := filepath.Abs(filepath.Join(stateDir, "workspaces.yaml"))
	if err != nil {
		t.Fatalf("abs target: %v", err)
	}
	var repaired []string
	restoreRepair := setRepairStateDACLRepairForTest(func(path string) (api.StateFileDACLRepairReport, error) {
		repaired = append(repaired, path)
		return api.StateFileDACLRepairReport{Path: path, Status: api.StateFileDACLRepairStatusRepaired}, nil
	})
	t.Cleanup(restoreRepair)

	stdout, stderr, err := runRepairStateDACLCmd(t, "", "--path", "workspaces.yaml", "--yes")
	if err != nil {
		t.Fatalf("repair-state-dacl --path workspaces.yaml --yes: %v\nstdout=%s\nstderr=%s", err, stdout, stderr)
	}
	if len(repaired) != 1 || repaired[0] != want {
		t.Fatalf("repaired = %v, want [%s]", repaired, want)
	}
}

func TestRepairStateDACL_HelpDocumentsPOSIXOpenWriterLimitation(t *testing.T) {
	cmd := newRepairStateDACLCmd()
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetArgs([]string{"--help"})

	if err := cmd.Execute(); err != nil {
		t.Fatalf("repair-state-dacl --help: %v", err)
	}
	help := out.String()
	for _, want := range []string{
		"repair-state-dacl --path <state-file> [--yes]",
		"Windows",
		"writer-exclusion guarantee",
		"best-effort",
		"POSIX",
		"must ensure no other process already holds the file open",
	} {
		if !strings.Contains(help, want) {
			t.Fatalf("help = %q, want %q", help, want)
		}
	}
	if strings.Contains(help, "--all") {
		t.Fatalf("help still documents removed --all scan surface: %q", help)
	}
}

func TestRepairStateDACL_NonInteractiveWithoutYesRefusesWhenRepairNeeded(t *testing.T) {
	stateDir := t.TempDir()
	restore := api.SetDaemonStateRootForTest(stateDir)
	t.Cleanup(restore)

	target := filepath.Join(stateDir, "workspaces.yaml")
	restoreRepair := setRepairStateDACLRepairForTest(func(path string) (api.StateFileDACLRepairReport, error) {
		return api.StateFileDACLRepairReport{Path: path, Status: api.StateFileDACLRepairStatusRepaired}, nil
	})
	t.Cleanup(restoreRepair)

	stdout, stderr, err := runRepairStateDACLCmd(t, "", "--path", target)
	if err == nil {
		t.Fatalf("repair-state-dacl without --yes in non-interactive mode succeeded; stdout=%q stderr=%q", stdout, stderr)
	}
	var fe *forceExitError
	if !errors.As(err, &fe) || fe.ExitCode() != 6 {
		t.Fatalf("err = %T %v, want forceExitError code 6", err, err)
	}
	if !strings.Contains(stderr, "--yes") {
		t.Fatalf("stderr = %q, want --yes guidance", stderr)
	}
}

func TestRepairStateDACL_PathWithYesRepairsTargetAndPrintsSummary(t *testing.T) {
	stateDir := t.TempDir()
	restore := api.SetDaemonStateRootForTest(stateDir)
	t.Cleanup(restore)

	target := filepath.Join(stateDir, "workspaces.yaml")
	var repaired []string
	restoreRepair := setRepairStateDACLRepairForTest(func(path string) (api.StateFileDACLRepairReport, error) {
		repaired = append(repaired, path)
		return api.StateFileDACLRepairReport{
			Path:                     path,
			Status:                   api.StateFileDACLRepairStatusRepaired,
			WriterExclusionGuarantee: api.StateFileDACLWriterExclusionBestEffort,
			RepairOpenTier:           api.StateFileDACLRepairOpenTierMetadataOnlyFallback,
			FallbackPath:             "tier1-access-denied-metadata-only",
		}, nil
	})
	t.Cleanup(restoreRepair)

	stdout, stderr, err := runRepairStateDACLCmd(t, "", "--path", target, "--yes")
	if err != nil {
		t.Fatalf("repair-state-dacl --path --yes: %v\nstdout=%s\nstderr=%s", err, stdout, stderr)
	}
	if len(repaired) != 1 || repaired[0] != target {
		t.Fatalf("repaired = %v, want [%s]", repaired, target)
	}
	if !strings.Contains(stdout, "repaired: "+target) {
		t.Fatalf("stdout = %q, want repaired target", stdout)
	}
	for _, want := range []string{"guarantee=best-effort", "fallback=tier1-access-denied-metadata-only"} {
		if !strings.Contains(stdout, want) {
			t.Fatalf("stdout = %q, want %q", stdout, want)
		}
	}
}

func runRepairStateDACLCmd(t *testing.T, stdin string, args ...string) (stdout, stderr string, err error) {
	t.Helper()
	cmd := newRepairStateDACLCmd()
	var out bytes.Buffer
	var errBuf bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&errBuf)
	cmd.SetIn(strings.NewReader(stdin))
	cmd.SetArgs(args)
	err = cmd.Execute()
	return out.String(), errBuf.String(), err
}

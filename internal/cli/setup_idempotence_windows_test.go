//go:build windows

package cli

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"mcp-local-hub/internal/binaryadmission"
)

func TestBootstrapProductToTargetDelegatesPairBeforeSuccess(t *testing.T) {
	withTempStateDir(t)
	sourceDir := t.TempDir()
	targetDir := t.TempDir()
	sourceCLI := filepath.Join(sourceDir, "mcphub.exe")
	sourceWindowless := filepath.Join(sourceDir, "mcphub-windowless.exe")
	targetCLI := filepath.Join(targetDir, "mcphub.exe")
	for path, body := range map[string][]byte{sourceCLI: []byte("source-cli"), sourceWindowless: []byte("source-windowless")} {
		if err := os.WriteFile(path, body, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	originalFactory := newWindowsProductPairTxnFn
	t.Cleanup(func() { newWindowsProductPairTxnFn = originalFactory })
	called := false
	var order []string
	newWindowsProductPairTxnFn = func(req windowsProductPairTxnRequest) (*WindowsProductPairTxn, error) {
		called = true
		if req.Mode != WindowsProductPairModeSetup || req.CLIPath != targetCLI || req.WindowlessPath != filepath.Join(targetDir, "mcphub-windowless.exe") {
			t.Fatalf("setup pair request=%+v", req)
		}
		if samePath(req.StagedCLI, sourceCLI) || samePath(req.StagedWindowless, sourceWindowless) {
			t.Fatal("setup passed caller source directly to promotion owner")
		}
		return &WindowsProductPairTxn{Opts: WindowsProductPairTxnOpts{
			CLIPath: req.CLIPath, WindowlessPath: req.WindowlessPath,
			StagedCLI: req.StagedCLI, StagedWindowless: req.StagedWindowless,
			ReceiptPath: filepath.Join(req.StateDir, UpgradeReceiptSchemaV2+".json"), Mode: req.Mode,
			Tasks: &productPairTaskFake{order: &order}, Deps: productPairTestDeps(&order),
		}}, nil
	}
	var out bytes.Buffer
	if err := bootstrapProductToTarget(&out, sourceCLI, targetCLI); err != nil {
		t.Fatalf("bootstrap pair: %v", err)
	}
	if !called || !strings.Contains(out.String(), "Windows product pair installed") {
		t.Fatalf("factory called=%v output=%q", called, out.String())
	}
	if got := mustReadPairFile(t, sourceCLI); !bytes.Equal(got, []byte("source-cli")) {
		t.Fatalf("caller CLI source was moved or changed: %q", got)
	}
	if got := mustReadPairFile(t, sourceWindowless); !bytes.Equal(got, []byte("source-windowless")) {
		t.Fatalf("caller windowless source was moved or changed: %q", got)
	}
	if len(order) == 0 || !slices.Contains(order, "publish-committed") {
		t.Fatalf("pair transaction not executed: %v", order)
	}
}

func TestBootstrapProductToTargetPairFailurePrintsNoSuccess(t *testing.T) {
	withTempStateDir(t)
	sourceDir := t.TempDir()
	targetDir := t.TempDir()
	for _, leaf := range []string{"mcphub.exe", "mcphub-windowless.exe"} {
		if err := os.WriteFile(filepath.Join(sourceDir, leaf), []byte(leaf), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	originalFactory := newWindowsProductPairTxnFn
	t.Cleanup(func() { newWindowsProductPairTxnFn = originalFactory })
	var order []string
	newWindowsProductPairTxnFn = func(req windowsProductPairTxnRequest) (*WindowsProductPairTxn, error) {
		return &WindowsProductPairTxn{Opts: WindowsProductPairTxnOpts{
			CLIPath: req.CLIPath, WindowlessPath: req.WindowlessPath, StagedCLI: req.StagedCLI, StagedWindowless: req.StagedWindowless,
			ReceiptPath: filepath.Join(req.StateDir, UpgradeReceiptSchemaV2+".json"), Mode: req.Mode,
			Tasks: &productPairTaskFake{order: &order}, Deps: WindowsProductPairTxnDeps{
				AdmitPair: func(_, _ binaryadmission.WindowsArtifact) (binaryadmission.WindowsProductPair, error) {
					return binaryadmission.WindowsProductPair{}, context.Canceled
				},
				PublishCommitted: func(WindowsProductPairCommitEvent) (string, error) { return "", nil },
			},
		}}, nil
	}
	var out bytes.Buffer
	err := bootstrapProductToTarget(&out, filepath.Join(sourceDir, "mcphub.exe"), filepath.Join(targetDir, "mcphub.exe"))
	if err == nil || strings.Contains(out.String(), "installed") {
		t.Fatalf("error=%v output=%q", err, out.String())
	}
}

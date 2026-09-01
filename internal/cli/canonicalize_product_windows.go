//go:build windows

package cli

import (
	"context"
	"fmt"
	"io"
	"path/filepath"

	"mcp-local-hub/internal/api"
	"mcp-local-hub/internal/binaryadmission"
	"mcp-local-hub/internal/scheduler"
)

func canonicalizeProductToTarget(w io.Writer, cliSource, cliTarget string) error {
	windowlessSource := scheduler.WindowsOwnedEntrypointPath(cliSource)
	windowlessTarget := scheduler.WindowsOwnedEntrypointPath(cliTarget)
	if _, err := binaryadmission.AdmitWindowsRole(binaryadmission.WindowsArtifact{Path: cliSource, Role: binaryadmission.WindowsArtifactRoleCLI}); err != nil {
		return fmt.Errorf("admit canonical CLI candidate: %w", err)
	}
	if _, err := binaryadmission.AdmitWindowsRole(binaryadmission.WindowsArtifact{Path: windowlessSource, Role: binaryadmission.WindowsArtifactRoleWindowless}); err != nil {
		return fmt.Errorf("admit canonical windowless candidate: %w", err)
	}
	if samePath(cliSource, cliTarget) && samePath(windowlessSource, windowlessTarget) {
		fmt.Fprintf(w, "✓ mcphub product pair already canonical at %s (running binaries are the targets)\n", filepath.Dir(cliTarget))
		return nil
	}
	cliSame, cliErr := sameFileContents(cliSource, cliTarget)
	windowlessSame, windowlessErr := sameFileContents(windowlessSource, windowlessTarget)
	if cliErr == nil && windowlessErr == nil && cliSame && windowlessSame {
		fmt.Fprintf(w, "✓ mcphub product pair already up to date at %s\n", filepath.Dir(cliTarget))
		return nil
	}
	stateDir, err := api.DaemonStateDir()
	if err != nil {
		return fmt.Errorf("resolve state-dir for Windows product pair canonicalize: %w", err)
	}
	txn, err := newWindowsProductPairTxnFn(windowsProductPairTxnRequest{
		Context:          context.Background(),
		StateDir:         stateDir,
		CLIPath:          cliTarget,
		WindowlessPath:   windowlessTarget,
		StagedCLI:        cliSource,
		StagedWindowless: windowlessSource,
	})
	if err != nil {
		return fmt.Errorf("construct Windows product pair canonicalize transaction: %w", err)
	}
	if _, err := txn.Run(context.Background()); err != nil {
		return fmt.Errorf("canonicalize Windows product pair: %w", err)
	}
	fmt.Fprintf(w, "✓ mcphub product pair canonicalized at %s\n", filepath.Dir(cliTarget))
	return nil
}

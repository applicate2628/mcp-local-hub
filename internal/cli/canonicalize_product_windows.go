//go:build windows

package cli

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"mcp-local-hub/internal/api"
	"mcp-local-hub/internal/scheduler"
)

func canonicalizeProductToTarget(w io.Writer, cliSource, cliTarget string) (retErr error) {
	windowlessSource := scheduler.WindowsOwnedEntrypointPath(cliSource)
	windowlessTarget := scheduler.WindowsOwnedEntrypointPath(cliTarget)
	stateDir, err := api.DaemonStateDir()
	if err != nil {
		return fmt.Errorf("resolve state-dir for Windows product pair canonicalize: %w", err)
	}
	fence, acquired, err := api.TryAcquireUpgradeFence(context.Background(), stateDir)
	if err != nil {
		return fmt.Errorf("acquire Windows product-pair canonicalize fence: %w", err)
	}
	if !acquired {
		return fmt.Errorf("acquire Windows product-pair canonicalize fence: another product-pair transaction is active")
	}
	defer api.ReleaseAndJoin(&retErr, fence.Release, "release Windows product-pair canonicalize fence")
	settled, err := reconcileWiredWindowsProductPairRecovery(windowsProductPairRecoveryRuntimeRequest(context.Background(), stateDir, cliTarget, windowlessTarget))
	if err != nil {
		return fmt.Errorf("recover Windows product-pair canonicalize: %w", err)
	}
	if settled != nil && settled.Outcome == WindowsProductPairRecoverySettledCommit {
		fmt.Fprintf(w, "\u2713 mcphub product-pair crash recovery kept committed pair at %s\n", filepath.Dir(cliTarget))
		return nil
	}
	if reconciled, err := reconcileWindowsProductPairReceipt(stateDir, cliSource, windowlessSource, cliTarget, windowlessTarget, WindowsProductPairModeCanonicalize); err != nil {
		return err
	} else if reconciled {
		fmt.Fprintf(w, "\u2713 mcphub product pair already committed at %s; committed event reconciled\n", filepath.Dir(cliTarget))
		return nil
	}
	stagedCLI, err := stageWindowsProductCandidate(cliSource, cliTarget)
	if err != nil {
		return fmt.Errorf("stage canonical CLI candidate: %w", err)
	}
	defer os.Remove(stagedCLI)
	stagedWindowless, err := stageWindowsProductCandidate(windowlessSource, windowlessTarget)
	if err != nil {
		return fmt.Errorf("stage windowless candidate: %w", err)
	}
	defer os.Remove(stagedWindowless)
	txn, err := newWindowsProductPairTxnFn(windowsProductPairTxnRequest{
		Context:          context.Background(),
		StateDir:         stateDir,
		CLIPath:          cliTarget,
		WindowlessPath:   windowlessTarget,
		StagedCLI:        stagedCLI,
		StagedWindowless: stagedWindowless,
		Mode:             WindowsProductPairModeCanonicalize,
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

func stageWindowsProductCandidate(source, target string) (string, error) {
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		return "", err
	}
	in, err := os.Open(source)
	if err != nil {
		return "", err
	}
	defer in.Close()
	out, err := os.CreateTemp(filepath.Dir(target), filepath.Base(target)+".*.stage")
	if err != nil {
		return "", err
	}
	path := out.Name()
	ok := false
	defer func() {
		_ = out.Close()
		if !ok {
			_ = os.Remove(path)
		}
	}()
	if _, err := io.Copy(out, in); err != nil {
		return "", err
	}
	if err := out.Sync(); err != nil {
		return "", err
	}
	if err := out.Chmod(0o755); err != nil {
		return "", err
	}
	if err := out.Close(); err != nil {
		return "", err
	}
	ok = true
	return path, nil
}

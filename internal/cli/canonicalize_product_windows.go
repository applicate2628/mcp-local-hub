//go:build windows

package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"mcp-local-hub/internal/api"
	"mcp-local-hub/internal/binaryadmission"
	"mcp-local-hub/internal/scheduler"
)

func canonicalizeProductToTarget(w io.Writer, cliSource, cliTarget string) error {
	return canonicalizeProductToTargetWithWindowsCanonicalEntryInput(context.Background(), w, cliSource, cliTarget, windowsCanonicalEntryCommandInput{})
}

func canonicalizeProductToTargetWithWindowsCanonicalEntryInput(ctx context.Context, w io.Writer, _ string, cliTarget string, input windowsCanonicalEntryCommandInput) error {
	return installWindowsCanonicalEntrySet(ctx, w, cliTarget, WindowsProductPairModeCanonicalize, input)
}

// installWindowsCanonicalEntrySet is the shared setup/canonicalize composition.
// The existing fence and transaction own publication, tasks and recovery; the
// executing-Product owner alone selects and stages the complete original set.
func installWindowsCanonicalEntrySet(ctx context.Context, w io.Writer, canonicalEntry string, mode WindowsProductPairMode, input windowsCanonicalEntryCommandInput) (retErr error) {
	if ctx == nil {
		return errors.New("canonical entry installation context required")
	}
	if input.bindingErr != nil {
		return input.bindingErr
	}
	if _, err := binaryadmission.NewWindowsCanonicalEntryBuildBindingV1(input.binding.BuildID(), input.binding.ExpectedCanonicalEntrySHA256(), input.binding.ReverseProjection()); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	stateDir, err := api.DaemonStateDir()
	if err != nil {
		return fmt.Errorf("resolve Windows entry-set state directory: %w", err)
	}
	fence, acquired, err := api.TryAcquireUpgradeFence(ctx, stateDir)
	if fence != nil {
		defer api.ReleaseAndJoin(&retErr, fence.Release, "release Windows entry-set fence")
	}
	if err != nil {
		return fmt.Errorf("acquire Windows entry-set fence: %w", err)
	}
	if !acquired || fence == nil {
		return errors.New("another Windows product transaction is active")
	}
	legacy := scheduler.WindowsLegacyEntrypointPath(canonicalEntry)
	// Receive legacy or complete-set recovery before any fresh acquisition or
	// staging. A historical Product-only commit is not complete-set success.
	recoveryRequest, err := windowsCanonicalEntryRecoveryRuntimeRequest(ctx, stateDir, canonicalEntry, legacy)
	if err != nil {
		return err
	}
	if _, err := reconcileWiredWindowsProductRecovery(recoveryRequest); err != nil {
		return err
	}
	original, err := binaryadmission.AcquireExecutingWindowsCanonicalEntrySetV1(ctx, input.binding)
	if original != nil {
		defer api.ReleaseAndJoin(&retErr, original.Close, "close original Windows entry-set acquisition")
	}
	if err != nil {
		return err
	}
	if original == nil {
		return errors.New("executing Windows entry-set owner unavailable")
	}
	staged, err := original.Stage(ctx, canonicalEntry)
	if staged != nil {
		defer api.ReleaseAndJoin(&retErr, staged.Close, "close original Windows entry-set stage")
	}
	if err != nil {
		return err
	}
	if staged == nil {
		return errors.New("Windows entry-set stage owner unavailable")
	}
	stagedTargets, err := staged.Targets()
	if err != nil {
		return err
	}
	targets, err := staged.DestinationTargets()
	if err != nil {
		return err
	}
	// Source read/name guards would conflict with the same installation's
	// prior DELETE acquisition. Receive their close before entering that cut;
	// the actual staged owner and the same upgrade fence remain retained.
	if err := original.Close(); err != nil {
		return err
	}
	txn, err := newWindowsProductTxnFn(windowsProductTxnRequest{
		Context: ctx, StateDir: stateDir, CanonicalEntryPath: targets.CanonicalEntryPath,
		ProductPath: targets.ProductPath, IdentityPath: targets.IdentityPath,
		LegacyWindowlessPath: legacy, StagedProduct: stagedTargets.ProductPath,
		EntrySet: &stagedWindowsCanonicalEntrySetV1{original: staged}, Mode: mode,
	})
	if txn != nil {
		defer api.ReleaseAndJoin(&retErr, txn.Close, "close Windows entry-set transaction")
	}
	if err != nil {
		return err
	}
	if txn == nil {
		return errors.New("Windows entry-set transaction unavailable")
	}
	if same, err := reconcileWindowsCanonicalEntryNoop(ctx, stateDir, mode, txn.Opts.EntrySet, txn.Opts.Tasks); err != nil {
		return err
	} else if same {
		fmt.Fprintf(w, "✓ mcphub canonical entry set already committed at %s\n", filepath.Dir(canonicalEntry))
		return nil
	}
	result, err := txn.Run(ctx)
	if err != nil {
		return err
	}
	if result.Outcome != WindowsProductPairCommittedOutcome {
		return fmt.Errorf("Windows entry-set transaction returned %q", result.Outcome)
	}
	fmt.Fprintf(w, "✓ mcphub canonical entry set installed at %s\n", filepath.Dir(canonicalEntry))
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

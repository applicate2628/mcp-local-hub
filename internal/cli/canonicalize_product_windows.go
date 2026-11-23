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
	current, err := windowsProductUsesCurrentRegistry()
	if err != nil {
		return err
	}
	allowance, err := resolveWindowsProductJournalAllowance()
	if err != nil {
		return err
	}
	evidence := &windowsProductJournalReadEvidence{}
	defer func() { retErr = withWindowsProductJournalEvidence(retErr, evidence) }()
	if err := validateWindowsProductJournalOperation(ctx, allowance); err != nil {
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
	if err := requireWindowsProductWithoutBackendHold(ctx, stateDir); err != nil {
		return err
	}
	legacy := scheduler.WindowsLegacyEntrypointPath(canonicalEntry)
	var original *binaryadmission.WindowsCanonicalEntrySetV2
	if current {
		original, err = binaryadmission.AcquireExecutingWindowsCanonicalEntrySetV2(ctx, input.binding)
		if original != nil {
			defer api.ReleaseAndJoin(&retErr, original.Close, "close original Windows entry-set acquisition")
		}
		if err != nil || original == nil {
			return errors.Join(err, errors.New("executing Windows entry-set owner unavailable"))
		}
	} else if err := verifyWindowsLegacyCallerSource(ctx, input.binding); err != nil {
		return err
	}
	// Receive pending recovery before staging. Only the current branch can lend
	// its actual executing original to current successor quiescence.
	recoveryRequest, err := windowsCanonicalEntryRecoveryRequestForDomain(ctx, stateDir, canonicalEntry, legacy, current, allowance, evidence)
	if err != nil {
		return err
	}
	if current {
		bindWindowsProductSuccessorQuiescence(&recoveryRequest, original)
	}
	if _, err := reconcileWiredWindowsProductRecovery(recoveryRequest); err != nil {
		return err
	}
	request := windowsProductTxnRequest{
		Context: ctx, StateDir: stateDir, LegacyWindowlessPath: legacy, Mode: mode,
		JournalAllowance: allowance, journalReadEvidence: evidence,
	}
	if current {
		staged, err := original.Stage(ctx, canonicalEntry)
		if staged != nil {
			request.EntryBundle = &stagedWindowsCanonicalEntrySetV2{original: staged}
			defer api.ReleaseAndJoin(&retErr, request.EntryBundle.Close, "close original Windows entry-set carrier")
		}
		if err != nil || staged == nil {
			return errors.Join(err, errors.New("Windows entry-set stage owner unavailable"))
		}
		request.StagedProduct, err = currentStageProductPath(staged)
		if err != nil {
			return err
		}
		targets, err := staged.DestinationTargets()
		if err != nil {
			return err
		}
		request.CanonicalEntryPath, request.ProductPath, request.IdentityPath = targets.CanonicalEntryPath, targets.ProductPath, targets.IdentityPath
		if err := original.Close(); err != nil {
			return err
		}
	} else {
		legacySource, err := acquireWindowsLegacyCallerSource(ctx, input.binding)
		if legacySource != nil {
			defer api.ReleaseAndJoin(&retErr, legacySource.Close, "close original legacy Windows entry-set acquisition")
		}
		if err != nil {
			return err
		}
		staged, err := legacySource.Stage(ctx, canonicalEntry)
		if staged != nil {
			defer api.ReleaseAndJoin(&retErr, staged.Close, "close original Windows entry-set stage")
		}
		if err != nil || staged == nil {
			return errors.Join(err, errors.New("Windows entry-set stage owner unavailable"))
		}
		stagedTargets, err := staged.Targets()
		if err != nil {
			return err
		}
		targets, err := staged.DestinationTargets()
		if err != nil {
			return err
		}
		request.CanonicalEntryPath, request.ProductPath, request.IdentityPath = targets.CanonicalEntryPath, targets.ProductPath, targets.IdentityPath
		request.StagedProduct = stagedTargets.ProductPath
		request.EntrySet = &stagedWindowsCanonicalEntrySetV1{original: staged}
		if err := legacySource.Close(); err != nil {
			return err
		}
	}
	txn, err := newWindowsProductTxnFn(request)
	if txn != nil {
		defer api.ReleaseAndJoin(&retErr, txn.Close, "close Windows entry-set transaction")
	}
	if err != nil {
		return err
	}
	if txn == nil {
		return errors.New("Windows entry-set transaction unavailable")
	}
	var same bool
	if current {
		same, err = reconcileWindowsCurrentEntryNoop(ctx, stateDir, mode, txn.Opts.EntryBundle, txn.Opts.Tasks)
	} else {
		same, err = reconcileWindowsCanonicalEntryNoop(ctx, stateDir, mode, txn.Opts.EntrySet, txn.Opts.Tasks)
	}
	if err != nil {
		return err
	}
	if same {
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
// Only an explicitly absent compiler input is eligible for held legacy proof.
func windowsProductUsesCurrentRegistry() (bool, error) {
	_, err := binaryadmission.CompiledWindowsProductRegistry()
	if errors.Is(err, binaryadmission.ErrCompiledWindowsProductRegistryAbsent) {
		return false, nil
	}
	return err == nil, err
}

func acquireWindowsLegacyCallerSource(ctx context.Context, binding binaryadmission.WindowsCanonicalEntryBuildBindingV1) (*binaryadmission.WindowsCanonicalEntrySetV1, error) {
	original, err := binaryadmission.AcquireExecutingWindowsCanonicalEntrySetV1(ctx, binding)
	if err != nil {
		return original, err
	}
	if original == nil {
		return nil, errors.New("executing Windows legacy entry-set owner unavailable")
	}
	return original, original.VerifyLegacyProductBuild(ctx)
}

func verifyWindowsLegacyCallerSource(ctx context.Context, binding binaryadmission.WindowsCanonicalEntryBuildBindingV1) (err error) {
	original, err := acquireWindowsLegacyCallerSource(ctx, binding)
	if original != nil {
		defer api.ReleaseAndJoin(&err, original.Close, "close Windows legacy admission observation")
	}
	return err
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

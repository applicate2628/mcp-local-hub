package cli

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func TestWindowsProductPairTxnPromotionErrorWithoutMutationRestartsPrior(t *testing.T) {
	for _, tc := range []struct {
		name        string
		failedPath  func(productPairPaths) string
		result      func(productPairPaths) WindowsProductPairPromotion
		wantRestore int
	}{
		{
			name:       "windowless absent target rename failure",
			failedPath: func(paths productPairPaths) string { return paths.windowless },
			result:     func(productPairPaths) WindowsProductPairPromotion { return WindowsProductPairPromotion{} },
		},
		{
			name:       "cli prior canonical without retained file",
			failedPath: func(paths productPairPaths) string { return paths.priorCLI },
			result: func(paths productPairPaths) WindowsProductPairPromotion {
				return WindowsProductPairPromotion{Target: paths.priorCLI, PriorPresent: true, NewSHA256: productPairTestCLISHA}
			},
			wantRestore: 1,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			paths := productPairFixturePaths(dir)
			writePairFixture(t, paths.priorCLI, []byte("old-cli"))
			writePairFixture(t, paths.windowless, []byte("old-windowless"))
			writePairFixture(t, paths.stagedCLI, []byte("new-cli"))
			writePairFixture(t, paths.stagedWindowless, []byte("new-windowless"))

			var order []string
			tasks := &productPairTaskFake{order: &order}
			deps := productPairTestDeps(&order)
			basePromote := deps.Promote
			deps.Promote = func(src, dst, sha string) (WindowsProductPairPromotion, error) {
				if dst == tc.failedPath(paths) {
					return tc.result(paths), errors.New("injected no-mutation promotion failure")
				}
				return basePromote(src, dst, sha)
			}
			restores := 0
			baseRestore := deps.RestorePromotion
			deps.RestorePromotion = func(p WindowsProductPairPromotion) (string, error) {
				restores++
				return baseRestore(p)
			}
			restarts := 0
			deps.RestartPrior = func(string) error { restarts++; return nil }

			_, err := (WindowsProductPairTxn{Opts: WindowsProductPairTxnOpts{
				CLIPath: paths.priorCLI, WindowlessPath: paths.windowless,
				StagedCLI: paths.stagedCLI, StagedWindowless: paths.stagedWindowless,
				ReceiptPath: paths.receipt, Mode: WindowsProductPairModeUpgrade,
				Tasks: tasks, Deps: deps,
			}}).Run(context.Background())
			if err == nil || !errors.Is(err, ErrWindowsProductPairRecoveryRollback) && !strings.Contains(err.Error(), "injected no-mutation promotion failure") {
				t.Fatalf("Run() error = %v, want promotion failure", err)
			}
			if restores != tc.wantRestore {
				t.Fatalf("restore calls = %d, want %d", restores, tc.wantRestore)
			}
			if restarts != 1 {
				t.Fatalf("prior restarts = %d, want 1", restarts)
			}
		})
	}
}

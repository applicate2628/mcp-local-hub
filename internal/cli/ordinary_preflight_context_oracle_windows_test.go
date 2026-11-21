//go:build windows

package cli

import (
	"bytes"
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"mcp-local-hub/internal/api"
)

func TestOrdinaryUpgradePreflightHonorsCallerCancellation(t *testing.T) {
	resetUpgradeSeams(t)
	dir := t.TempDir()
	target := filepath.Join(dir, "mcphub.exe")
	upgradeExecutableFn = func() (string, error) { return filepath.Join(dir, "candidate.exe"), nil }
	upgradeTargetPathFn = func() (string, error) { return target, nil }

	priorDispatch := v5UpgradeFn
	t.Cleanup(func() { v5UpgradeFn = priorDispatch })

	advisory := errors.New("synthetic advisory GUI inventory error")
	for _, tc := range []struct {
		name           string
		cancelInFinder bool
		probeError     error
		wantError      error
		wantDispatches int
	}{
		{name: "caller canceled during finder", cancelInFinder: true, wantError: context.Canceled},
		{name: "live caller with probe error", probeError: advisory, wantDispatches: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()

			var stderr bytes.Buffer
			cmd := newUpgradeCmdReal()
			cmd.SetContext(ctx)
			cmd.SetErr(&stderr)

			finderCalls := 0
			findRunningGUIsOnTargetFn = func(gotTarget string) ([]api.ProcessInfo, error) {
				finderCalls++
				if gotTarget != target || ctx.Err() != nil {
					t.Fatalf("finder entered with target=%q context_error=%v", gotTarget, ctx.Err())
				}
				if tc.cancelInFinder {
					cancel()
				}
				return nil, tc.probeError
			}
			dispatches := 0
			v5UpgradeFn = func(*cobra.Command) error {
				dispatches++
				return nil
			}

			err := runV5UpgradeReal(cmd)
			if finderCalls != 1 {
				t.Errorf("finder calls=%d, want one", finderCalls)
			}
			if !errors.Is(err, tc.wantError) {
				t.Errorf("ordinary upgrade error=%v, want %v", err, tc.wantError)
			}
			if dispatches != tc.wantDispatches {
				t.Errorf("ordinary upgrade dispatched %d times, want %d", dispatches, tc.wantDispatches)
			}
			if tc.probeError != nil && !strings.Contains(stderr.String(), "GUI detection failed (best-effort): "+tc.probeError.Error()) {
				t.Errorf("advisory GUI inventory warning missing: %q", stderr.String())
			}
		})
	}

	t.Run("explicit GUI opt-in keeps fenced admission", func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())
		cancel()

		cmd := newUpgradeCmdReal()
		cmd.SetContext(ctx)
		if err := cmd.Flags().Set("terminate-gui-for-upgrade", "true"); err != nil {
			t.Fatal(err)
		}

		finderCalls := 0
		findRunningGUIsOnTargetFn = func(string) ([]api.ProcessInfo, error) {
			finderCalls++
			return nil, nil
		}
		admissionErr := errors.New("canonical entry build binding is missing or invalid")
		dispatches := 0
		v5UpgradeFn = func(dispatched *cobra.Command) error {
			dispatches++
			if dispatched != cmd || dispatched.Context() != ctx || !errors.Is(dispatched.Context().Err(), context.Canceled) {
				t.Error("fenced admission did not receive the original canceled command context")
			}
			return admissionErr
		}

		err := runV5UpgradeReal(cmd)
		if err != admissionErr || dispatches != 1 || finderCalls != 0 {
			t.Errorf("explicit GUI opt-in: error=%v, fenced dispatches=%d, finder calls=%d; want exact admission error, one fenced dispatch, zero finder calls", err, dispatches, finderCalls)
		}
	})
}

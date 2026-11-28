package cli

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"

	"mcp-local-hub/internal/api"
)

// Product command composition holds the upgrade fence before entering the
// fresh or recovery owner. Take the existing intent lease only for the
// protected read. A known hold is unavailable until the stopped receiver
// can receive exact Product, terminal Stop, and writer cohort authority.
func requireWindowsProductWithoutBackendHold(ctx context.Context, stateDir string) (retErr error) {
	if !cleanAbsolutePath(stateDir) {
		return errors.New("absolute Product state directory required")
	}
	path := filepath.Join(stateDir, "supervisor-intent.json")
	lease, err := api.AcquireStateFileObservedLease(ctx, path)
	if lease != nil {
		defer func() {
			closed, closeErr := lease.CloseObserved()
			if closeErr == nil && lease.AcquisitionObservation().Held {
				closeErr = closed.Validate(lease)
			}
			retErr = errors.Join(retErr, closeErr)
		}()
	}
	if err != nil {
		return fmt.Errorf("inspect Product backend hold: %w", err)
	}
	return lease.Borrow(ctx, path, func() error {
		intent, err := api.ReadSupervisorIntentIfPresentObserved(ctx, path)
		if err != nil {
			return fmt.Errorf("inspect supervisor-intent.json: %w", err)
		}
		if err := intent.BackendStartError(); err != nil {
			return errors.Join(ErrWindowsProductRecoveryPending, err)
		}
		return nil
	})
}

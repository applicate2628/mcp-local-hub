package api

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// UpgradeTransactionLockFileLeaf is the one cross-process exclusion leaf
// shared by the Windows upgrade transaction and the scheduled liveness tick.
const UpgradeTransactionLockFileLeaf = "upgrade-transaction.lock"

const upgradeFenceRetryInterval = 10 * time.Millisecond

// UpgradeFenceLease owns the kernel-held upgrade transaction flock. Release is
// one-shot: repeated calls return the first release outcome, including an
// unconfirmed-release poison from the shared lock ledger.
type UpgradeFenceLease struct {
	release func() error
}

// Release releases the fence once and returns the same outcome on every call.
func (l *UpgradeFenceLease) Release() error {
	if l == nil || l.release == nil {
		return errors.New("upgrade fence lease is nil")
	}
	return l.release()
}

// AcquireUpgradeFence waits for the upgrade fence or for ctx cancellation.
func AcquireUpgradeFence(ctx context.Context, stateDir string) (*UpgradeFenceLease, error) {
	if ctx == nil {
		return nil, errors.New("upgrade fence context is nil")
	}
	for {
		lease, acquired, err := TryAcquireUpgradeFence(ctx, stateDir)
		if err != nil {
			return nil, err
		}
		if acquired {
			if err := ctx.Err(); err != nil {
				releaseErr := lease.Release()
				return nil, errors.Join(err, releaseErr)
			}
			return lease, nil
		}
		timer := time.NewTimer(upgradeFenceRetryInterval)
		select {
		case <-ctx.Done():
			if !timer.Stop() {
				<-timer.C
			}
			return nil, ctx.Err()
		case <-timer.C:
		}
	}
}

// TryAcquireUpgradeFence attempts the fence once without waiting. A busy fence
// returns (nil, false, nil); an unverifiable or poisoned leaf returns an error.
func TryAcquireUpgradeFence(ctx context.Context, stateDir string) (*UpgradeFenceLease, bool, error) {
	if ctx == nil {
		return nil, false, errors.New("upgrade fence context is nil")
	}
	if err := ctx.Err(); err != nil {
		return nil, false, err
	}
	if stateDir == "" {
		return nil, false, errors.New("upgrade fence state directory is empty")
	}
	lockPath := filepath.Join(stateDir, UpgradeTransactionLockFileLeaf)
	if info, err := os.Lstat(lockPath); err == nil {
		if !info.Mode().IsRegular() {
			return nil, false, fmt.Errorf("upgrade fence leaf is not a regular file: %s", lockPath)
		}
	} else if !os.IsNotExist(err) {
		return nil, false, fmt.Errorf("inspect upgrade fence leaf: %w", err)
	}
	release, acquired, err := tryLockLeafLedgered(lockPath)
	if err != nil {
		return nil, false, fmt.Errorf("acquire upgrade fence: %w", err)
	}
	if !acquired {
		return nil, false, nil
	}
	return &UpgradeFenceLease{release: release}, true, nil
}

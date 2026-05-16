// Package migration drives the v0.4.x → v0.5.0 supervisor migration. The
// journal driver lives in journal.go; this file owns the universal
// migration-lock → --once-lock acquisition order from spec §"Lock
// ordering" (line 229–234).
//
// The lock-acquire helper exists as its own file so callers in
// internal/cli can take the same ordered pair without re-implementing
// the LIFO release contract.
package migration

import (
	"errors"
	"fmt"
	"path/filepath"

	"mcp-local-hub/internal/api"
)

// LockSet holds the two locks the migration driver acquires in the
// universal v0.5.0 ordering: migration.lock first, --once.lock second.
// Callers MUST invoke Release() on every path (defer immediately after
// successful Acquire). LIFO release is honored regardless of which
// lock failed last.
type LockSet struct {
	migration *api.SupervisorLock
	once      *api.SupervisorLock
}

// ErrMigrationLockHeld is the sentinel returned by AcquireMigrationLocks
// when the outer migration.lock is held by another live holder. The CLI
// layer maps this to exit code 8 (INSTALL_BUSY) or 9 (STRICT_MODE_BUSY)
// depending on which command surfaced the error.
var ErrMigrationLockHeld = errors.New("migration: migration.lock held")

// ErrOnceLockHeld is the sentinel returned when --once.lock is held
// after migration.lock acquisition succeeded. The migration driver
// rolls back the migration.lock acquisition before surfacing this.
var ErrOnceLockHeld = errors.New("migration: --once.lock held")

// AcquireMigrationLocks takes <stateDir>/migration.lock then
// <stateDir>/--once.lock in that order. On --once.lock failure the
// already-acquired migration.lock is released so callers do not need
// to defer a partial-Release path.
//
// On success the returned LockSet has both locks held; the caller must
// invoke ls.Release() exactly once on every termination path. Release
// is LIFO (--once.lock first, then migration.lock) per spec §"Lock
// ordering".
func AcquireMigrationLocks(stateDir string) (*LockSet, error) {
	if stateDir == "" {
		return nil, fmt.Errorf("migration: empty state-dir")
	}
	migPath := filepath.Join(stateDir, "migration")
	mig, err := api.AcquireSupervisorLock(migPath)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrMigrationLockHeld, err)
	}
	oncePath := filepath.Join(stateDir, "--once")
	once, err := api.AcquireSupervisorLock(oncePath)
	if err != nil {
		// LIFO unwind: release the migration.lock we just acquired.
		mig.Release()
		return nil, fmt.Errorf("%w: %v", ErrOnceLockHeld, err)
	}
	return &LockSet{migration: mig, once: once}, nil
}

// Release unlocks both flocks in LIFO order: --once.lock first, then
// migration.lock. Idempotent — repeated calls after the first are a
// no-op.
func (ls *LockSet) Release() {
	if ls == nil {
		return
	}
	if ls.once != nil {
		ls.once.Release()
		ls.once = nil
	}
	if ls.migration != nil {
		ls.migration.Release()
		ls.migration = nil
	}
}

// AcquireMigrationLockOnly takes only the outer migration.lock, used
// by the rollback path before its IPC sequence (rollback acquires
// --once.lock at step 4, not step 2). The returned LockSet has only
// `migration` populated; Release() still works LIFO.
func AcquireMigrationLockOnly(stateDir string) (*LockSet, error) {
	if stateDir == "" {
		return nil, fmt.Errorf("migration: empty state-dir")
	}
	migPath := filepath.Join(stateDir, "migration")
	mig, err := api.AcquireSupervisorLock(migPath)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrMigrationLockHeld, err)
	}
	return &LockSet{migration: mig}, nil
}

// AcquireOnceLockOnto extends an existing LockSet (migration-only) with
// the --once.lock. Used by rollback after IPC quiesce + force-kill
// completes. On failure the existing migration.lock is left held —
// caller decides whether to abort + release or retry.
func (ls *LockSet) AcquireOnceLockOnto(stateDir string) error {
	if ls == nil {
		return fmt.Errorf("migration: nil lockset")
	}
	if ls.once != nil {
		return fmt.Errorf("migration: --once.lock already held")
	}
	oncePath := filepath.Join(stateDir, "--once")
	once, err := api.AcquireSupervisorLock(oncePath)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrOnceLockHeld, err)
	}
	ls.once = once
	return nil
}

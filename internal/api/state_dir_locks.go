// Package api — universal state-dir lock acquisition (migration.lock →
// --once.lock).
//
// This helper was migrated out of the deleted internal/migration package in
// v0.6 Phase F (the v0.4.x→v0.5.0 migration engine drop). It is NOT
// migration-specific: the two flocks it manages (<state-dir>/migration.lock
// and <state-dir>/--once.lock) are the canonical universal lock-order
// primitive that `mcphub strict-mode {enable,disable}` and the serena
// migrate/cutover interlock rely on (CLAUDE.md "universal lock order:
// migration.lock → --once.lock"). The on-disk lock-file basenames are
// preserved verbatim (`migration`, `--once`) so the cross-process contract is
// byte-symmetric with the pre-Phase-F layout.
package api

import (
	"errors"
	"fmt"
	"path/filepath"
)

// StateDirLockSet holds the two locks acquired in the universal ordering:
// migration.lock first, --once.lock second. Callers MUST invoke Release() on
// every path (defer immediately after a successful Acquire). LIFO release is
// honored regardless of which lock failed last.
type StateDirLockSet struct {
	migration *SupervisorLock
	once      *SupervisorLock
}

// ErrStateDirMigrationLockHeld is the sentinel returned by AcquireStateDirLocks
// when the outer migration.lock is held by another live holder. The CLI layer
// maps this to exit code 9 (STRICT_MODE_BUSY).
var ErrStateDirMigrationLockHeld = errors.New("state-dir: migration.lock held")

// ErrStateDirOnceLockHeld is the sentinel returned when --once.lock is held
// after migration.lock acquisition succeeded. The already-acquired
// migration.lock is released before surfacing this.
var ErrStateDirOnceLockHeld = errors.New("state-dir: --once.lock held")

// AcquireStateDirLocks takes <stateDir>/migration.lock then
// <stateDir>/--once.lock in that order. On --once.lock failure the
// already-acquired migration.lock is released so callers do not need to defer
// a partial-Release path.
//
// On success the returned StateDirLockSet has both locks held; the caller must
// invoke ls.Release() exactly once on every termination path. Release is LIFO
// (--once.lock first, then migration.lock) per the universal lock ordering.
func AcquireStateDirLocks(stateDir string) (*StateDirLockSet, error) {
	if stateDir == "" {
		return nil, fmt.Errorf("state-dir: empty state-dir")
	}
	migPath := filepath.Join(stateDir, "migration")
	mig, err := AcquireSupervisorLock(migPath)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrStateDirMigrationLockHeld, err)
	}
	oncePath := filepath.Join(stateDir, "--once")
	once, err := AcquireSupervisorLock(oncePath)
	if err != nil {
		// LIFO unwind: release the migration.lock we just acquired.
		mig.Release()
		return nil, fmt.Errorf("%w: %v", ErrStateDirOnceLockHeld, err)
	}
	return &StateDirLockSet{migration: mig, once: once}, nil
}

// Release unlocks both flocks in LIFO order: --once.lock first, then
// migration.lock. Idempotent — repeated calls after the first are a no-op.
func (ls *StateDirLockSet) Release() {
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

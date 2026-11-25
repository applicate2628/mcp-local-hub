package api

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"sync/atomic"
)

// A cutover holding the exact supervisor lock is the launch owner while no
// supervisor can acquire that lock. A quiet holder intentionally has no owner
// sidecar and does not serve supervisor IPC. Admit its actual Product image
// instead of interpreting the held mutex as an authenticated supervisor.
func runtimeMcphubExecutableUnderSupervisorLock(bypass InstallParsedManifestBypass) (path string, err error) {
	defer func() {
		if err != nil {
			err = &RuntimeProductImageError{Cause: err}
		}
	}()
	root, err := daemonStateDirReadOnly()
	if err != nil {
		return "", fmt.Errorf("runtime product state: %w", err)
	}
	if !bypass.holdsSupervisorLock(root) {
		return "", fmt.Errorf("runtime product supervisor exclusion is absent, released, or belongs to another state directory")
	}
	if testCanonicalMcphubPathOverride != "" {
		return testCanonicalMcphubPathOverride, nil
	}
	candidate, err := SelfMcphubExecutable()
	if err != nil {
		return "", err
	}
	return selectRuntimeMcphubImage(candidate, nil, admitRuntimeMcphubImage)
}

// supervisorInstallBorrow is a scoped capability, not a lock owner. Each entered
// borrow gets a distinct permit that can never become active again.
type supervisorInstallBorrow struct {
	lease      *StateFileObservedLease
	generation uint64
	ctx        context.Context
	active     atomic.Bool
}

// WithSupervisorLockBypass uses this original lease's existing borrow lifetime.
// CloseObserved cannot unlock beneath the entered callback. A retained token
// expires before that borrow exits, even if the same lease is borrowed again.
func (lease *StateFileObservedLease) WithSupervisorLockBypass(ctx context.Context, stateDir string, body func(InstallParsedManifestBypass) error) error {
	if lease == nil || body == nil {
		return errors.New("original supervisor interlock lease and callback required")
	}
	path := filepath.Join(stateDir, "supervisor.lock")
	return lease.Borrow(ctx, path, func() error {
		permit := &supervisorInstallBorrow{lease: lease, generation: lease.AcquisitionObservation().Generation, ctx: ctx}
		permit.active.Store(true)
		defer permit.active.Store(false)
		return body(InstallParsedManifestBypass{observed: permit})
	})
}

func (bypass InstallParsedManifestBypass) supplied() bool {
	return bypass.lk != nil || bypass.observed != nil
}

func (bypass InstallParsedManifestBypass) holdsSupervisorLock(root string) bool {
	if permit := bypass.observed; permit != nil {
		if bypass.lk != nil || permit.lease == nil || permit.generation == 0 || !permit.active.Load() {
			return false
		}
		if err := permit.lease.ValidateHeld(permit.ctx, filepath.Join(root, "supervisor.lock")); err != nil {
			return false
		}
		return permit.lease.AcquisitionObservation().Generation == permit.generation && permit.active.Load()
	}
	return bypass.lk != nil && bypass.lk.fl != nil && bypass.lk.path == filepath.Join(root, "supervisor.lock")
}

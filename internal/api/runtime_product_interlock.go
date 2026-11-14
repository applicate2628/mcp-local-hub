package api

import (
	"fmt"
	"path/filepath"
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

func (bypass InstallParsedManifestBypass) holdsSupervisorLock(root string) bool {
	return bypass.lk != nil && bypass.lk.fl != nil && bypass.lk.path == filepath.Join(root, "supervisor.lock")
}

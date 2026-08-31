//go:build !windows

package cli

import (
	"fmt"

	"mcp-local-hub/internal/autostart"
)

func enableAutostartForPlatform(_ autostart.OwnerMode, ownerExplicit, strict bool, _ bool) error {
	if ownerExplicit {
		return fmt.Errorf("--owner-mode is only supported on Windows")
	}
	backend, err := autostartBackendFactoryFn()
	if err != nil {
		return fmt.Errorf("autostart backend: %w", err)
	}
	return backend.Enable(autostart.Options{StrictMode: strict})
}

func autostartStatusOptionsForPlatform(strict bool, _ bool) (autostart.Options, bool, error) {
	return autostart.Options{StrictMode: strict}, false, nil
}

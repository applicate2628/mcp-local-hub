//go:build !windows

package cli

import (
	"fmt"
	"io"

	"mcp-local-hub/internal/api"
	"mcp-local-hub/internal/autostart"
)

func withAutostartGUIControl(_ StrictModeDeps, enable bool, fn func(*api.GUIOwnerControl) error) error {
	if enable {
		return api.ErrBootPolicyProjectionUnsupported
	}
	// POSIX autostart owns a headless supervisor and retains its existing
	// start/stop effects; it does not consume a Windows GUI boot choice.
	return fn(nil)
}

func disableAutostartForPlatform(backend autostart.Backend) error {
	return backend.Disable()
}

func writeAutostartPlatformDetails(_ io.Writer) error { return nil }

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

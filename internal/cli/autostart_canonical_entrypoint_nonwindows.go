//go:build !windows

package cli

import "mcp-local-hub/internal/autostart"

func newAutostartBackendForCLI() (autostart.Backend, error) { return autostart.New() }

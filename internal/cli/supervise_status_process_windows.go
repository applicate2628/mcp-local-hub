//go:build windows

package cli

import "mcp-local-hub/internal/process"

var defaultStatusPIDObserver = process.ObservePIDStatus

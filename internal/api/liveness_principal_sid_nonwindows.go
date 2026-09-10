//go:build !windows

package api

import "mcp-local-hub/internal/scheduler"

func livenessPrincipalSID() (string, error) {
	return "", scheduler.ErrNotImplemented
}

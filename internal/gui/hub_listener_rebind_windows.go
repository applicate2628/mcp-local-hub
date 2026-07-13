//go:build windows

package gui

import "mcp-local-hub/internal/api"

// isHubListenerSamePortRebindPendingErr delegates to the single-owner predicate
// (api.IsPortBindRefusedErr). The WSAEADDRINUSE/WSAEACCES check was RELOCATED to
// internal/api so the daemon-proxy bind path shares one owner with the hub
// listener (arch C1). This thin wrapper is retained so the hub-listener call
// sites keep their intent-named local helper.
func isHubListenerSamePortRebindPendingErr(err error) bool {
	return api.IsPortBindRefusedErr(err)
}

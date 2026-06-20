//go:build !windows

package gui

func isHubListenerSamePortRebindPendingErr(error) bool {
	return false
}

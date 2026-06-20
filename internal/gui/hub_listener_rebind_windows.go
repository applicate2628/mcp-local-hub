//go:build windows

package gui

import (
	"errors"

	"golang.org/x/sys/windows"
)

func isHubListenerSamePortRebindPendingErr(err error) bool {
	return errors.Is(err, windows.WSAEADDRINUSE) || errors.Is(err, windows.WSAEACCES)
}

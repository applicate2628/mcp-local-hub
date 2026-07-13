//go:build windows

package api

import (
	"errors"

	"golang.org/x/sys/windows"
)

// IsPortBindRefusedErr is the SINGLE OWNER of the "a 127.0.0.1 bind was refused
// because another process already holds the port" predicate. It reports whether
// err (or anything it wraps) is Winsock WSAEADDRINUSE (10048, the port has a
// listener) OR WSAEACCES (10013, the port is held by an *established* socket a
// SO_EXCLUSIVEADDRUSE bind cannot share). Both mean "our loopback listener could
// not take this port" — the exact class the ephemeral-collision self-heal moves
// a daemon off of.
//
// It was RELOCATED here from internal/gui/hub_listener_rebind_windows.go
// (isHubListenerSamePortRebindPendingErr) so the GUI hub-listener rebind path AND
// the supervised daemon-proxy bind path (internal/cli/daemon_workspace.go,
// internal/cli/daemon_serena.go) share ONE predicate instead of two copies that
// could drift (arch C1 single-owner). internal/api is the lowest package both the
// gui and cli packages already import, so it is the correct home; the GUI helper
// now delegates here.
func IsPortBindRefusedErr(err error) bool {
	return errors.Is(err, windows.WSAEADDRINUSE) || errors.Is(err, windows.WSAEACCES)
}

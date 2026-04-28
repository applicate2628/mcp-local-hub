// Package tray wraps getlantern/systray so the CLI wire-up stays the
// same across platforms; only Windows ships a live tray icon in MVP.
package tray

import "context"

// Config is what Run needs to produce the menu and route actions.
type Config struct {
	// ActivateWindow is called when the user left-clicks the tray icon
	// or picks "Open dashboard" from the right-click menu.
	ActivateWindow func()
	// Quit is called when the user picks "Quit (keep daemons)". The CLI
	// uses this to cancel the GUI context and exit cleanly.
	Quit func()
	// StateCh delivers TrayState transitions. The tray loop selects on
	// it and calls SetIcon + SetTooltip when a new state arrives.
	// nil channel disables state-driven icon swapping (the tray runs
	// with a single static icon — useful for tests / non-Windows).
	// The producer should NOT close StateCh during normal operation;
	// closing is treated as ctx-cancelled in the select loop.
	StateCh <-chan TrayState
}

// Run blocks running the tray event loop until ctx is canceled. On
// non-Windows platforms it returns immediately — the GUI server runs
// normally without an accompanying tray icon.
//
// Implementation selected at build time via tray_windows.go /
// tray_other.go build tags.
func Run(ctx context.Context, cfg Config) error {
	return runImpl(ctx, cfg)
}

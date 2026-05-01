package tray

import "testing"

// TestDispatchEvent_RoutesEachEventToTheRightCallback exhaustively
// covers the event-name → callback mapping. Audit gap (Round 1 #6):
// 3 new tray menu items were added (rescan-clients, open-logs-folder,
// open-data-folder); without coverage a future event-name typo on
// either side would silently fall through to the unknown-event log
// instead of triggering the user's click.
func TestDispatchEvent_RoutesEachEventToTheRightCallback(t *testing.T) {
	type counters struct {
		activate, quit, quitStop          int
		runAll, stopAll                   int
		rescan, openLogs, openData, other int
	}
	c := &counters{}
	cfg := Config{
		ActivateWindow: func() { c.activate++ },
		Quit:           func() { c.quit++ },
		QuitAndStopAll: func() { c.quitStop++ },
		RunAllDaemons:  func() { c.runAll++ },
		StopAllDaemons: func() { c.stopAll++ },
		RescanClients:  func() { c.rescan++ },
		OpenLogsFolder: func() { c.openLogs++ },
		OpenDataFolder: func() { c.openData++ },
	}

	// One representative invocation per known event name.
	events := []string{
		"open-dashboard", "quit", "quit-and-stop-all",
		"run-all", "stop-all",
		"rescan-clients", "open-logs-folder", "open-data-folder",
	}
	for _, ev := range events {
		dispatchEvent(ev, cfg)
	}

	if c.activate != 1 || c.quit != 1 || c.quitStop != 1 ||
		c.runAll != 1 || c.stopAll != 1 ||
		c.rescan != 1 || c.openLogs != 1 || c.openData != 1 {
		t.Errorf("counters mismatch: %+v", c)
	}
}

// TestDispatchEvent_QuitAndStopAllFallsBackToQuit verifies the
// fallback policy: if the GUI did not wire QuitAndStopAll, a
// "quit-and-stop-all" event still closes the GUI by routing to
// plain Quit. Silent no-op would be a UX bug (user clicked Quit-
// and-stop, nothing happens, daemons stay live AND GUI stays open).
func TestDispatchEvent_QuitAndStopAllFallsBackToQuit(t *testing.T) {
	quit := 0
	cfg := Config{
		Quit:           func() { quit++ },
		QuitAndStopAll: nil, // not wired
	}
	dispatchEvent("quit-and-stop-all", cfg)
	if quit != 1 {
		t.Errorf("Quit fallback was not invoked; quit=%d", quit)
	}
}

// TestDispatchEvent_NilCallbacksAreSafe confirms every event with
// nil callbacks is a quiet no-op rather than a panic. A future
// caller that constructs a partial Config (e.g. an integration
// test stubbing only one event) must not crash on unrelated
// events the test doesn't care about.
func TestDispatchEvent_NilCallbacksAreSafe(t *testing.T) {
	defer func() {
		if r := recover(); r != nil {
			t.Errorf("panic on nil callbacks: %v", r)
		}
	}()
	cfg := Config{} // every callback nil
	for _, ev := range []string{
		"open-dashboard", "quit", "quit-and-stop-all",
		"run-all", "stop-all",
		"rescan-clients", "open-logs-folder", "open-data-folder",
		"unknown-event",
	} {
		dispatchEvent(ev, cfg)
	}
}

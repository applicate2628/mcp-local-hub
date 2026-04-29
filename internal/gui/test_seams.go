// internal/gui/test_seams.go
//
// Exported test seams for the gui package. These are plain exported
// functions and package-level vars — no build tags — so tests in any
// build mode can reach them without link-time switches.
//
// Consumers (e.g. internal/cli tests) call the getter to snapshot the
// current value, call the setter to install their override, and restore
// the original in a defer.
package gui

// --- openFolderSpawn seam ---

// OpenFolderSpawnForTest returns the current openFolderSpawn function
// so callers can snapshot it before installing an override.
func OpenFolderSpawnForTest() func(string, ...string) error { return openFolderSpawn }

// SetOpenFolderSpawn replaces the package-level openFolderSpawn used
// by OpenFolderAt. Tests must restore the original via
// RestoreOpenFolderSpawn(prev) in a defer.
func SetOpenFolderSpawn(fn func(string, ...string) error) { openFolderSpawn = fn }

// RestoreOpenFolderSpawn reinstates a previously-snapshotted
// openFolderSpawn. Passing nil restores the production default
// (openFolderDefault); passing the value returned by
// OpenFolderSpawnForTest restores whatever was in place at snapshot time.
func RestoreOpenFolderSpawn(fn func(string, ...string) error) {
	if fn == nil {
		openFolderSpawn = openFolderDefault
	} else {
		openFolderSpawn = fn
	}
}

// --- identityGateOverride seam ---

// identityGateOverride, when non-nil, replaces the three-part
// (image/argv/start-time) identity gate inside KillRecordedHolder.
// The function receives the current Verdict (populated by probe) and
// returns (refused=true, reason) to block the kill, or
// (refused=false, "") to let KillRecordedHolder proceed as normal.
var identityGateOverride func(Verdict) (refused bool, reason string)

// IdentityGateForTest returns the current identityGateOverride so
// callers can snapshot it before installing an override.
func IdentityGateForTest() func(Verdict) (refused bool, reason string) {
	return identityGateOverride
}

// SetIdentityGate replaces the identity gate used by
// KillRecordedHolder. Set to nil to re-enable the production gate.
func SetIdentityGate(fn func(Verdict) (refused bool, reason string)) {
	identityGateOverride = fn
}

// RestoreIdentityGate reinstates a previously-snapshotted gate value.
func RestoreIdentityGate(fn func(Verdict) (refused bool, reason string)) {
	identityGateOverride = fn
}

// --- postKillHook seam ---

// postKillHook, when non-nil, is called by KillRecordedHolder
// immediately after killProcess succeeds and BEFORE the acquire-poll
// loop begins. Tests use it to re-acquire the flock from a "competing
// process" to exercise the RaceLost path.
var postKillHook func()

// PostKillHookForTest returns the current postKillHook.
func PostKillHookForTest() func() { return postKillHook }

// SetPostKillHook installs a hook that fires between the kill signal
// and the acquire-poll loop inside KillRecordedHolder.
func SetPostKillHook(fn func()) { postKillHook = fn }

// RestorePostKillHook reinstates a previously-snapshotted hook value.
func RestorePostKillHook(fn func()) { postKillHook = fn }

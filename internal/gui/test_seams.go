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

// --- processID seam ---

// processIDOverride, when non-nil, replaces processID() inside
// probeOnce. Tests use it to inject specific ProcessIdentity
// payloads (e.g. an oversize argv[0] for the truncation regression
// in Codex iter-3 P2 #1, or errMacOSProbeUnsupported on
// linux/windows runners for the macOS regression in Codex iter-3
// P2 #2). Production code path is unchanged when this is nil.
var processIDOverride func(pid int) (ProcessIdentity, error)

// ProcessIDForTest returns the current processIDOverride so callers
// can snapshot it before installing an override.
func ProcessIDForTest() func(pid int) (ProcessIdentity, error) { return processIDOverride }

// SetProcessIDOverride installs a function that replaces processID()
// inside probeOnce. Tests must restore the original via
// RestoreProcessID(prev) in a defer.
func SetProcessIDOverride(fn func(pid int) (ProcessIdentity, error)) { processIDOverride = fn }

// RestoreProcessID reinstates a previously-snapshotted override.
// Passing nil disables the override and returns to production behavior.
func RestoreProcessID(fn func(pid int) (ProcessIdentity, error)) { processIDOverride = fn }

// ErrMacOSProbeUnsupportedForTest exposes the package-private
// errMacOSProbeUnsupported sentinel so tests in any package can
// build override results that simulate the darwin processIDImpl
// stub on linux/windows runners.
func ErrMacOSProbeUnsupportedForTest() error { return errMacOSProbeUnsupported }

// --- killProcessOverride seam ---

// killProcessOverride, when non-nil, replaces the killProcess helper
// inside KillRecordedHolder. Used only by the wait-for-exit unit
// test (Codex iter-9 P2 #2) so the test can drive the wait loop
// without actually SIGKILLing/TerminateProcessing any real process.
// Kept package-private — same-package tests can assign to it
// directly, and there is no production caller that should ever
// install it.
var killProcessOverride func(pid int) error

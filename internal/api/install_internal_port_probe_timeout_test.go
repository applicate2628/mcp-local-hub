package api

import (
	"context"
	"path/filepath"
	"testing"

	"mcp-local-hub/internal/config"
)

// TestInternalPortOwnership_TimedOutIdentityProbeIsNotOwnership is the FIX-1
// regression guard.
//
// Scenario is exactly the condition the probe-bounding change was built for: a
// native-http internal upstream port, a live supervisor wrapper PID known via
// IPC, a FOREIGN process squatting the internal port, and a congested WMI
// service so the identity probe is cut by its deadline.
//
// Before this fix the timeout collapsed into the same `!ok` as "no such
// process", which took the "process lookup unavailable" downgrade at
// install.go and returned "held by our daemon" — so /api/server/readiness
// answered Ready=true about a port it had NOT established was ours. That
// converted the original visible hang into a silent lie, which is strictly
// worse: a hang is at least honest.
//
// RED against 7e4c4955 (bounded probes, boolean seam): portHeldBySupervisorIntentDaemon
// returns TRUE here. GREEN once the timeout is threaded as ErrProbeTimeout and
// the downgrade is restricted to a structurally absent probe surface.
func TestInternalPortOwnership_TimedOutIdentityProbeIsNotOwnership(t *testing.T) {
	stateDir := daemonIntentTestHelper(t)
	const (
		externalPort = 33121
		internalPort = externalPort + config.NativeHTTPInternalPortOffset
		listenerPID  = 88121 // the FOREIGN squatter actually holding the port
		wrapperPID   = 77121 // our live supervisor wrapper
	)
	const taskName = `\mcp-local-hub-demo-alpha`

	intentPath := filepath.Join(stateDir, supervisorIntentFileLeaf)
	if err := WriteSupervisorIntent(intentPath, &SupervisorIntentFile{
		Version: 1,
		Daemons: []SupervisorDaemon{{
			TaskName: taskName,
			Server:   "demo",
			Daemon:   "alpha",
			Command:  "go",
			Port:     externalPort,
		}},
	}); err != nil {
		t.Fatalf("seed supervisor intent: %v", err)
	}

	origLookup, origStatus, origNameParent := lookupProcess, supervisorIPCStatusFn, processNameAndParentByPID
	t.Cleanup(func() {
		lookupProcess, supervisorIPCStatusFn, processNameAndParentByPID = origLookup, origStatus, origNameParent
	})

	supervisorIPCStatusFn = func(context.Context) ([]DaemonStatus, error) {
		return []DaemonStatus{{TaskName: taskName, PID: wrapperPID, State: "Running"}}, nil
	}
	// netstat resolves a listener, and it is NOT our wrapper.
	lookupProcess = func(p int) (int, uint64, int64, bool) {
		if p == internalPort {
			return listenerPID, 0, 0, true
		}
		return 0, 0, 0, false
	}
	// The identity probe RAN and was cut by its deadline — ownership unknown.
	processNameAndParentByPID = fakeProcessNameAndParentTimingOut()

	if portHeldBySupervisorIntentDaemon(internalPort, "demo", "alpha") {
		t.Fatal("a TIMED-OUT identity probe was treated as proof of ownership: " +
			"the internal port is reported as held by our daemon while a foreign " +
			"process squats it. A probe that did not answer must never license the " +
			"ownership downgrade — readiness would report Ready=true on evidence " +
			"it never obtained.")
	}
}

// TestInternalPortOwnership_UnresolvablePortOwnerIsNotOwnership covers the
// SECOND funnel into the same downgrade: the port-owner (netstat) probe rather
// than the identity (wmic/PowerShell) probe.
//
// The ownership gate is only consulted for a port that is already BOUND
// (fixedPortStatus gates on !portAvailable). "Bound, yet netstat resolves no
// owner" is therefore an unresolved probe, not evidence the port is ours.
func TestInternalPortOwnership_UnresolvablePortOwnerIsNotOwnership(t *testing.T) {
	stateDir := daemonIntentTestHelper(t)
	const (
		externalPort = 33122
		internalPort = externalPort + config.NativeHTTPInternalPortOffset
		wrapperPID   = 77122
	)
	const taskName = `\mcp-local-hub-demo-alpha`

	intentPath := filepath.Join(stateDir, supervisorIntentFileLeaf)
	if err := WriteSupervisorIntent(intentPath, &SupervisorIntentFile{
		Version: 1,
		Daemons: []SupervisorDaemon{{
			TaskName: taskName,
			Server:   "demo",
			Daemon:   "alpha",
			Command:  "go",
			Port:     externalPort,
		}},
	}); err != nil {
		t.Fatalf("seed supervisor intent: %v", err)
	}

	origLookup, origStatus, origNameParent := lookupProcess, supervisorIPCStatusFn, processNameAndParentByPID
	t.Cleanup(func() {
		lookupProcess, supervisorIPCStatusFn, processNameAndParentByPID = origLookup, origStatus, origNameParent
	})

	supervisorIPCStatusFn = func(context.Context) ([]DaemonStatus, error) {
		return []DaemonStatus{{TaskName: taskName, PID: wrapperPID, State: "Running"}}, nil
	}
	// A WIRED port-owner probe that cannot resolve the owner — distinct from
	// having no probe surface at all (lookupProcess == nil), which legitimately
	// keeps the downgrade and is asserted below.
	lookupProcess = func(int) (int, uint64, int64, bool) { return 0, 0, 0, false }
	processNameAndParentByPID = fakeProcessNameAndParent(nil)

	if portHeldBySupervisorIntentDaemon(internalPort, "demo", "alpha") {
		t.Fatal("a wired-but-unresolvable port-owner probe was treated as proof of ownership")
	}

	// Control: with NO probe surface at all the documented downgrade still
	// applies. This is the case the downgrade was written for, and the fix must
	// not break it (non-Windows hosts rely on it).
	lookupProcess = nil
	processNameAndParentByPID = nil
	if !portHeldBySupervisorIntentDaemon(internalPort, "demo", "alpha") {
		t.Fatal("with no probe surface wired at all, the documented live-wrapper-PID " +
			"downgrade must still apply; got not-held")
	}
}

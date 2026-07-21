package api

import (
	"strings"
	"testing"
	"time"
)

// TestAllServerReadiness_BoundedByFanOutBudget pins the fan-out half of the fix.
//
// Per-probe deadlines stop any SINGLE probe from blocking forever, but they do
// not stop N servers × M ports × several WMI-backed probes from STACKING: the
// measured pre-fix cost of this serial fan-out was 338s on a loaded reference
// host, overrunning even a 5-minute budget.
//
// Budget exhaustion is driven by an INJECTED clock, not by hoping the real
// probes are slow — a test that depends on natural timing is a design defect.
func TestAllServerReadiness_BoundedByFanOutBudget(t *testing.T) {
	origClock, origPortAvailable := readinessClock, portAvailable
	t.Cleanup(func() { readinessClock, portAvailable = origClock, origPortAvailable })

	// Hermetic and fast: every port reads as free, so the OS ownership probe
	// (netstat/wmic/PowerShell/schtasks) never runs and this test does not touch
	// the host's live daemons.
	portAvailable = func(int) bool { return true }

	base := time.Now()
	calls := 0
	// Call 1 sets the deadline at base+budget. Every later call reports a clock
	// already PAST that deadline, so truncation begins at the second server and
	// is fully deterministic.
	readinessClock = func() time.Time {
		calls++
		if calls == 1 {
			return base
		}
		return base.Add(allServerReadinessBudget + time.Second)
	}

	reps := AllServerReadiness()
	if len(reps) < 5 {
		t.Fatalf("got %d reports, want >= 5 embedded servers", len(reps))
	}

	// The first server is never truncated: a report where nothing was probed is
	// useless, and one server's cost is already per-probe capped.
	if isBudgetExhausted(reps[0]) {
		t.Errorf("first server %q was truncated; it must always be probed", reps[0].Server)
	}

	for _, r := range reps[1:] {
		// The load-bearing assertion: an unprobed server is UNKNOWN. A readiness
		// endpoint that reports unknown as ready is worse than one that hangs.
		if r.Ready {
			t.Errorf("server %q reported Ready=true after the probe budget was exhausted", r.Server)
		}
		if !isBudgetExhausted(r) {
			t.Errorf("server %q was truncated but carries no honest budget-exhausted reason; requirements=%+v",
				r.Server, r.Requirements)
		}
	}
}

// TestAllServerReadiness_BudgetReasonIsHonestlyUnknown pins the WORDING, because
// the failure mode this whole fix exists to avoid is a readiness surface that
// states something it did not establish.
func TestAllServerReadiness_BudgetReasonIsHonestlyUnknown(t *testing.T) {
	rep := budgetExhaustedReport("some-server")
	if rep.Ready {
		t.Fatal("budget-exhausted report must never be Ready")
	}
	if len(rep.Requirements) != 1 {
		t.Fatalf("want exactly 1 requirement, got %d", len(rep.Requirements))
	}
	req := rep.Requirements[0]
	if req.OK {
		t.Error("budget-exhausted requirement must not be OK")
	}
	// It must read as "we did not find out", NOT as "we found a problem".
	if !strings.Contains(req.Reason, "UNKNOWN") {
		t.Errorf("reason must state the result is UNKNOWN, got %q", req.Reason)
	}
	if !strings.Contains(req.Reason, "NOT determined") {
		t.Errorf("reason must say readiness was not determined, got %q", req.Reason)
	}
	if req.Fix == "" {
		t.Error("budget-exhausted requirement must carry an actionable fix")
	}
}

// TestFixedPortStatus_UnverifiedOwnershipIsNotAssertedAsForeign guards the other
// honesty edge: the ownership gate returns false both when the holder is
// provably foreign AND when the probe could not answer. The reason must not
// assert the former when it might be the latter.
func TestFixedPortStatus_UnverifiedOwnershipIsNotAssertedAsForeign(t *testing.T) {
	origPortAvailable := portAvailable
	t.Cleanup(func() { portAvailable = origPortAvailable })
	portAvailable = func(int) bool { return false } // port in use

	ok, reason := fixedPortStatus(readinessBudget{}, 65000, "srv", "dmn", false)
	if ok {
		t.Fatal("a port in use and not owned by us must not report ok")
	}
	if !strings.Contains(reason, "could not be verified") {
		t.Errorf("reason must admit ownership may be unverified rather than "+
			"asserting a foreign owner it did not establish; got %q", reason)
	}
}

// TestFixedPortStatus_SpentBudgetReportsUnknownNotConflict pins FIX-5's
// operator-visible distinction: a port left unprobed because the time budget
// was spent must read as UNKNOWN, not as a detected conflict. The two carry
// different runbooks — "WMI is slow, retry" vs "reclaim a squatted port".
func TestFixedPortStatus_SpentBudgetReportsUnknownNotConflict(t *testing.T) {
	origPortAvailable, origClock := portAvailable, readinessClock
	t.Cleanup(func() { portAvailable, readinessClock = origPortAvailable, origClock })
	portAvailable = func(int) bool { return false } // port in use

	base := time.Now()
	readinessClock = func() time.Time { return base }
	spent := readinessBudget{deadline: base.Add(-time.Second)} // already past

	ok, reason := fixedPortStatus(spent, 65001, "srv", "dmn", false)
	if ok {
		t.Fatal("an unprobed in-use port must not report ok")
	}
	if !strings.Contains(reason, "UNKNOWN") {
		t.Errorf("a budget-skipped ownership probe must read as UNKNOWN, got %q", reason)
	}
	if strings.Contains(reason, "not this server's daemon") {
		t.Errorf("must not assert a foreign owner it never probed for; got %q", reason)
	}
}

func isBudgetExhausted(r *ReadinessReport) bool {
	for _, req := range r.Requirements {
		if req.Name == "readiness probe" && strings.Contains(req.Reason, "probe budget") {
			return true
		}
	}
	return false
}

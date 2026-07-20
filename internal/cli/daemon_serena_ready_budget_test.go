package cli

import (
	"testing"
	"time"

	"mcp-local-hub/internal/api"
	"mcp-local-hub/internal/daemon"
)

// serenaColdIndexFloor is the MEASURED serena cold-start cost the readiness
// budget has to cover: ~46 s for a cold Go LSP index, the same measurement that
// justified the 120 s supervisory first-bind deadline
// (internal/api/supervisor_intent.go:77-91). It is the floor a serena readiness
// budget must clear, NOT a target — a budget at or below it fails a perfectly
// healthy backend on an idle machine, before any contention.
const serenaColdIndexFloor = 46 * time.Second

// serenaReadyBudgetDescriptor builds a serena-proxy descriptor of the shape the
// supervisor actually writes (a dynamic-pool row keyed by workspace hash).
func serenaReadyBudgetDescriptor() *api.SupervisorDaemon {
	return &api.SupervisorDaemon{
		TaskName: `\mcp-local-hub-serena-b133f336`,
		Server:   "serena",
		Daemon:   "b133f336",
		Command:  "mcphub",
		Args:     []string{"daemon", "serena-proxy", "--port", "9151"},
		Port:     9151,
		RuntimeSpec: &api.DaemonRuntimeSpec{
			SpecVersion:  api.DaemonRuntimeSpecVersion,
			ChildCommand: "uvx",
			ChildArgs:    []string{"--from", "serena", "serena"},
			UpstreamPort: 9151 + 1000,
			ExternalPort: 9151,
		},
	}
}

// TestSerenaProxy_ReadyBudgetIsBoundedByItsOwnBindDeadline is the cross-layer
// regression test for the timeout inversion. It asserts the ordering between
// the two layers against their SOURCE owners — the outer value comes from
// api.EffectiveStartupBindDeadlineSeconds and the inner from the host's own
// resolution — rather than against copied literals, so it keeps its teeth if
// either constant is retuned later.
//
// NON-VACUITY: against the pre-fix tree (serena's HTTPHostConfig built without
// any readiness budget, falling to the generic 30 s default) the inner budget
// is 30 s, which is BELOW serenaColdIndexFloor, and this test fails. See the
// RED evidence in the delivery report.
func TestSerenaProxy_ReadyBudgetIsBoundedByItsOwnBindDeadline(t *testing.T) {
	desc := serenaReadyBudgetDescriptor()

	// OUTER: owned by internal/api, not re-typed here.
	outer := time.Duration(api.EffectiveStartupBindDeadlineSeconds(*desc)) * time.Second
	if outer <= 0 {
		t.Fatalf("descriptor resolved a non-positive first-bind deadline (%v); the fixture no longer classifies as serena", outer)
	}

	// INNER: whatever the REAL serena wiring hands the host, after the host's
	// own default/derive/clamp resolution.
	h, err := daemon.NewHTTPHost(serenaProxyHTTPHostConfig(desc, []string{"serena"}, nil, nil, ""))
	if err != nil {
		t.Fatalf("NewHTTPHost: %v", err)
	}
	inner := h.HealthTimeout()

	// (1) The defect: the inner budget must actually cover a healthy cold start.
	if inner <= serenaColdIndexFloor {
		t.Fatalf("serena readiness budget = %v, which does not clear the measured %v cold index — a healthy but slow serena is declared 'upstream not ready' and exits 1 while still indexing (outer deadline was %v and is unreachable for this failure mode)", inner, serenaColdIndexFloor, outer)
	}

	// (2) The invariant: inner must still expire beneath outer so the supervisor
	// stays the authority on a truly wedged backend.
	if inner >= outer {
		t.Fatalf("serena readiness budget = %v >= first-bind deadline %v; the child would outlive the supervisor's own deadline, inverting which layer decides a wedged backend", inner, outer)
	}
}

// TestSerenaProxy_ReadyBudgetTracksARetunedBindDeadline proves the inner budget
// is DERIVED from the outer one rather than being a second hardcoded literal
// that merely happens to be ordered today: retuning the descriptor's deadline
// must move the readiness budget with it.
func TestSerenaProxy_ReadyBudgetTracksARetunedBindDeadline(t *testing.T) {
	base := serenaReadyBudgetDescriptor()
	baseHost, err := daemon.NewHTTPHost(serenaProxyHTTPHostConfig(base, []string{"serena"}, nil, nil, ""))
	if err != nil {
		t.Fatalf("NewHTTPHost(base): %v", err)
	}

	// An operator-set explicit deadline (the highest-precedence input to
	// api.EffectiveStartupBindDeadlineSeconds), well above serena's default.
	retuned := serenaReadyBudgetDescriptor()
	retuned.StartupBindDeadlineSeconds = 420
	retunedHost, err := daemon.NewHTTPHost(serenaProxyHTTPHostConfig(retuned, []string{"serena"}, nil, nil, ""))
	if err != nil {
		t.Fatalf("NewHTTPHost(retuned): %v", err)
	}

	if retunedHost.HealthTimeout() <= baseHost.HealthTimeout() {
		t.Fatalf("raising the descriptor's first-bind deadline to %ds left the readiness budget at %v (base %v) — the inner budget is not derived from the outer one, so the two can drift apart again",
			retuned.StartupBindDeadlineSeconds, retunedHost.HealthTimeout(), baseHost.HealthTimeout())
	}
	// Ordering must hold at the retuned value too.
	outer := time.Duration(api.EffectiveStartupBindDeadlineSeconds(*retuned)) * time.Second
	if retunedHost.HealthTimeout() >= outer {
		t.Fatalf("retuned readiness budget %v >= retuned deadline %v", retunedHost.HealthTimeout(), outer)
	}
}

package daemon

import (
	"bytes"
	"strings"
	"testing"
	"time"
)

// The readiness-budget tests below cover the inner-vs-outer timeout inversion:
// HTTPHost's readiness probe (INNER) used to be a flat 30 s default with no
// relation to the supervisory first-bind deadline (OUTER) the daemon runs
// under, so a caller granting 120 s got a child that killed itself at 30 s and
// an outer deadline that was unreachable dead code for that failure mode.
//
// NON-VACUITY: each test below was run against a mutated NewHTTPHost whose
// OuterBindDeadline handling was removed (i.e. the pre-fix
// `if cfg.HealthTimeout <= 0 { cfg.HealthTimeout = 30*time.Second }`); all four
// fail there. See the RED evidence in the delivery report.

// TestNewHTTPHost_DerivesReadyBudgetFromOuterBindDeadline is the core fix: a
// caller that declares an outer deadline and leaves HealthTimeout unset gets a
// budget DERIVED from that outer value, not the generic 30 s default.
func TestNewHTTPHost_DerivesReadyBudgetFromOuterBindDeadline(t *testing.T) {
	const outer = 120 * time.Second

	h, err := NewHTTPHost(HTTPHostConfig{
		Command:           "true",
		UpstreamPort:      9999,
		OuterBindDeadline: outer,
	})
	if err != nil {
		t.Fatalf("NewHTTPHost: %v", err)
	}

	got := h.HealthTimeout()
	if got == defaultHTTPHostHealthTimeout {
		t.Fatalf("HealthTimeout = %v — still the generic default; the outer deadline (%v) was ignored, so the inner budget is unrelated to the outer one (the inversion this fix removes)", got, outer)
	}
	if want := maxHealthTimeoutUnder(outer); got != want {
		t.Fatalf("HealthTimeout = %v, want %v (%d/%d of the %v outer deadline)", got, want, httpHostReadyBudgetNum, httpHostReadyBudgetDen, outer)
	}
}

// TestNewHTTPHost_ReadyBudgetOrderingInvariant asserts the property that
// actually matters, across the whole legal range of outer deadlines rather than
// at one hand-picked point: the resolved inner budget must be STRICTLY LESS
// than the outer deadline, with real margin left for the child's teardown and
// the outer owner observing the exit.
//
// 600 s is the manifest validation cap (internal/config/manifest.go:1329).
func TestNewHTTPHost_ReadyBudgetOrderingInvariant(t *testing.T) {
	for _, outer := range []time.Duration{
		1 * time.Second,
		30 * time.Second,
		60 * time.Second,
		120 * time.Second,
		240 * time.Second,
		600 * time.Second,
	} {
		h, err := NewHTTPHost(HTTPHostConfig{
			Command:           "true",
			UpstreamPort:      9999,
			OuterBindDeadline: outer,
		})
		if err != nil {
			t.Fatalf("outer=%v NewHTTPHost: %v", outer, err)
		}
		inner := h.HealthTimeout()
		if inner <= 0 {
			t.Fatalf("outer=%v: inner budget %v is non-positive; the probe would fail before its first attempt", outer, inner)
		}
		if inner >= outer {
			t.Fatalf("outer=%v: inner budget %v >= outer — the child's own probe would not expire before the outer first-bind deadline, so the outer owner stops being the authority on a wedged backend", outer, inner)
		}
		// The margin must scale with the outer deadline, not shrink to a sliver
		// at large values.
		if margin := outer - inner; margin < outer/8 {
			t.Fatalf("outer=%v inner=%v: margin %v is under 1/8 of the outer deadline — too thin for child teardown + exit observation", outer, inner, margin)
		}
	}
}

// TestNewHTTPHost_ClampsAndWarnsOnMisorderedReadyBudget is the STRUCTURAL half
// of the fix. Setting two constants that happen to be ordered is not
// enforcement: a caller that hand-sets an over-large HealthTimeout beneath a
// declared outer deadline must be clamped and warned, not silently allowed to
// re-invert the two layers. Mirrors the NewLazyProxy timeout-ordering clamp.
func TestNewHTTPHost_ClampsAndWarnsOnMisorderedReadyBudget(t *testing.T) {
	var diag bytes.Buffer
	restore := SetDaemonDiagWriterForTest(&diag)
	defer restore()

	const outer = 120 * time.Second

	h, err := NewHTTPHost(HTTPHostConfig{
		Command:      "true",
		UpstreamPort: 9999,
		// Deliberately misordered: an inner budget LONGER than the outer
		// deadline it is supposed to expire beneath.
		HealthTimeout:     150 * time.Second,
		OuterBindDeadline: outer,
	})
	if err != nil {
		t.Fatalf("NewHTTPHost: %v", err)
	}

	if got := h.HealthTimeout(); got >= outer {
		t.Fatalf("HealthTimeout = %v, want it clamped below the %v outer deadline — a misordered config was accepted verbatim", got, outer)
	}
	if got, want := h.HealthTimeout(), maxHealthTimeoutUnder(outer); got != want {
		t.Fatalf("clamped HealthTimeout = %v, want %v", got, want)
	}
	if s := diag.String(); !strings.Contains(s, "clamping") || !strings.Contains(s, "OuterBindDeadline") {
		t.Fatalf("clamp produced no operator-visible warning; diag output = %q", s)
	}
}

// TestNewHTTPHost_GenericDefaultUnchangedWithoutOuterDeadline pins the SCOPE of
// the change: a host whose caller declares NO outer deadline (every non-serena
// native-http daemon, stdio children, tests) keeps the historical 30 s budget
// byte-for-byte, so failure-detection latency there is untouched.
func TestNewHTTPHost_GenericDefaultUnchangedWithoutOuterDeadline(t *testing.T) {
	h, err := NewHTTPHost(HTTPHostConfig{
		Command:      "true",
		UpstreamPort: 9999,
	})
	if err != nil {
		t.Fatalf("NewHTTPHost: %v", err)
	}
	if got := h.HealthTimeout(); got != 30*time.Second {
		t.Fatalf("HealthTimeout = %v, want 30s — the generic no-outer-deadline default must not move (it would change failure-detection latency for every native-http daemon)", got)
	}
}

// TestNewHTTPHost_ExplicitBudgetUnderOuterDeadlineIsRespected confirms the
// clamp is a CEILING, not an override: an explicit budget that already honors
// the ordering passes through untouched.
func TestNewHTTPHost_ExplicitBudgetUnderOuterDeadlineIsRespected(t *testing.T) {
	h, err := NewHTTPHost(HTTPHostConfig{
		Command:           "true",
		UpstreamPort:      9999,
		HealthTimeout:     10 * time.Second,
		OuterBindDeadline: 120 * time.Second,
	})
	if err != nil {
		t.Fatalf("NewHTTPHost: %v", err)
	}
	if got := h.HealthTimeout(); got != 10*time.Second {
		t.Fatalf("HealthTimeout = %v, want the explicit 10s preserved", got)
	}
}

package api

import (
	"runtime"
	"strings"
	"testing"

	"mcp-local-hub/internal/config"
)

// hostPlatform is this build/run host's canonical "GOOS/GOARCH" token — the same
// string the install gate + browse classifier compare a probe's platforms[]
// against via config.PlatformMatches(runtime.GOOS, runtime.GOARCH, …).
func hostPlatform() string { return runtime.GOOS + "/" + runtime.GOARCH }

// otherPlatform returns a "GOOS/GOARCH" token guaranteed NOT to equal the host's,
// so a platforms[] list of just this value is a deterministic MISMATCH on any
// runner. It flips the arch half to whichever of amd64/arm64 the host is NOT (and
// flips the OS too as belt-and-suspenders), so it never accidentally matches.
func otherPlatform() string {
	arch := "arm64"
	if runtime.GOARCH == "arm64" {
		arch = "amd64"
	}
	goos := "plan9"
	if runtime.GOOS == "plan9" {
		goos = "solaris"
	}
	return goos + "/" + arch
}

// TestAvailabilityProbePasses_PlatformGate is TEST — the install-time arch gate:
// a probe whose platforms[] CONTAINS the host passes (subject to the binary/file
// terms); a probe whose platforms[] EXCLUDES the host fails with a host-platform
// reason, regardless of the binary term matching. This is the install gate the
// marketplace one-click + readiness paths run via availabilityProbeFinding.
func TestAvailabilityProbePasses_PlatformGate(t *testing.T) {
	// MATCH: host in the list + a present binary ("go") → pass.
	if ok, why := availabilityProbePasses(&config.AvailabilityProbe{
		Binaries:  []string{"go"},
		Platforms: []string{hostPlatform(), otherPlatform()},
	}); !ok {
		t.Fatalf("probe with host platform in list + present binary should pass: %q", why)
	}

	// MISMATCH: host NOT in the list → fail, EVEN THOUGH the binary ("go") is
	// present (the arch gate is checked first and short-circuits).
	ok, why := availabilityProbePasses(&config.AvailabilityProbe{
		Binaries:  []string{"go"},
		Platforms: []string{otherPlatform()},
	})
	if ok {
		t.Fatalf("probe with host platform EXCLUDED should fail even with a present binary; got pass")
	}
	if !strings.Contains(why, "host platform") || !strings.Contains(why, runtime.GOOS) {
		t.Fatalf("mismatch reason %q should name the host platform", why)
	}

	// EMPTY platforms → no arch gate → behaves exactly as before (present binary
	// passes). This is the additive-rollout invariant for every existing probe.
	if ok, why := availabilityProbePasses(&config.AvailabilityProbe{
		Binaries: []string{"go"},
	}); !ok {
		t.Fatalf("probe with empty platforms[] should not be arch-gated: %q", why)
	}
}

// TestMarketplaceEntryBrowseProbeState_PlatformMismatchBlocked is TEST — the
// browse mirror gate: an inert row whose platforms[] excludes the host classifies
// inert-blocked (greyed "probe to enable"), NOT inert-unknown — the arch gate is a
// pure runtime.GOOS/GOARCH compare (no os.Stat / LookPath), so the browse path can
// decide it definitively, matching the install gate. A row whose platforms[]
// INCLUDES the host falls through to the normal binary classification.
func TestMarketplaceEntryBrowseProbeState_PlatformMismatchBlocked(t *testing.T) {
	mk := func(p *CatalogAvailabilityProbe) *MarketplaceEntry {
		return &MarketplaceEntry{ID: "platrow", Availability: "disabled-until-probe", InstallProbe: p}
	}

	// Host EXCLUDED → inert-blocked, even though "go" is on PATH (the arch gate is
	// definitive and pure, so the browse path greys the row instead of offering an
	// install that would 412 at the install-time gate).
	got := MarketplaceEntryBrowseProbeState(mk(&CatalogAvailabilityProbe{
		Binaries:  []string{"go"},
		Platforms: []string{otherPlatform()},
	}))
	if got != ProbeBrowseInertBlocked {
		t.Fatalf("platform-mismatch browse state = %q, want %q", got, ProbeBrowseInertBlocked)
	}

	// Host INCLUDED + present binary → ready (falls through to bare-binary class).
	got = MarketplaceEntryBrowseProbeState(mk(&CatalogAvailabilityProbe{
		Binaries:  []string{"go"},
		Platforms: []string{hostPlatform(), otherPlatform()},
	}))
	if got != ProbeBrowseReady {
		t.Fatalf("platform-match + present binary browse state = %q, want %q", got, ProbeBrowseReady)
	}

	// The full install-gate mirror agrees: excluded → not installable; included →
	// installable (the binary is present).
	if MarketplaceEntryProbePasses(mk(&CatalogAvailabilityProbe{Binaries: []string{"go"}, Platforms: []string{otherPlatform()}})) {
		t.Fatalf("full gate should refuse a platform-excluded row")
	}
	if !MarketplaceEntryProbePasses(mk(&CatalogAvailabilityProbe{Binaries: []string{"go"}, Platforms: []string{hostPlatform()}})) {
		t.Fatalf("full gate should pass a platform-matched row with a present binary")
	}
}

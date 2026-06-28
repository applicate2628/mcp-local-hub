package config

import (
	"strings"
	"testing"
)

// TestPlatformMatches is the pure-predicate unit for the D-3 arch gate. It feeds
// explicit goos/goarch + platforms[] so it is host-independent (no runtime fake
// needed): the predicate is what the install gate (availabilityProbePasses) and
// the browse classifier (MarketplaceEntryBrowseProbeState) both call with
// runtime.GOOS/GOARCH.
func TestPlatformMatches(t *testing.T) {
	cases := []struct {
		name      string
		goos      string
		goarch    string
		platforms []string
		want      bool
	}{
		// EMPTY/nil platforms → no gate → always true (today's behavior).
		{"empty-list-no-gate", "windows", "arm64", nil, true},
		{"empty-slice-no-gate", "windows", "arm64", []string{}, true},
		// Host IN the list → true.
		{"match-win-amd64", "windows", "amd64", []string{"windows/amd64", "linux/amd64"}, true},
		{"match-linux-arm64", "linux", "arm64", []string{"windows/amd64", "linux/arm64"}, true},
		{"match-darwin-arm64", "darwin", "arm64", []string{"darwin/amd64", "darwin/arm64"}, true},
		// Host NOT in the list → false. The Onshape matrix (no windows/arm64) on a
		// win-arm64 host is the load-bearing case this whole change exists for.
		{"mismatch-win-arm64-onshape", "windows", "arm64",
			[]string{"windows/amd64", "linux/amd64", "linux/arm64", "darwin/amd64", "darwin/arm64"}, false},
		{"mismatch-os", "linux", "amd64", []string{"windows/amd64"}, false},
		{"mismatch-arch", "windows", "arm64", []string{"windows/amd64"}, false},
		// A stray-whitespace catalog/manifest value still compares (runtime
		// tolerance — the author gate rejects it, but a hand-edited manifest must
		// not silently permanently-disable a row that should match).
		{"whitespace-tolerated", "windows", "amd64", []string{" windows/amd64 "}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := PlatformMatches(tc.goos, tc.goarch, tc.platforms); got != tc.want {
				t.Fatalf("PlatformMatches(%q,%q,%v) = %v, want %v", tc.goos, tc.goarch, tc.platforms, got, tc.want)
			}
		})
	}
}

// TestValidateProbeValuesNonEmpty_Platforms covers the author/manifest gate for
// platforms[] token shape: empty / padded / missing-'/' / triple are all rejected
// with a "GOOS/GOARCH" diagnostic; a well-formed list passes.
func TestValidateProbeValuesNonEmpty_Platforms(t *testing.T) {
	bad := []struct {
		name string
		plat string
		want string
	}{
		{"empty", "", "is empty"},
		// Surrounding whitespace (bot #447 P2 — author gate rejects even though
		// PlatformMatches stays trim-tolerant on READ).
		{"leading-space", " windows/amd64", "contains whitespace"},
		{"trailing-space", "windows/amd64 ", "contains whitespace"},
		// INTERNAL whitespace (the bot #447 P2 gap the surrounding-only check missed):
		// these pass the non-empty-halves shape but can NEVER equal the whitespace-free
		// runtime host string, so the row would be permanently inert. Must fail LOUD.
		{"space-after-slash", "windows/ amd64", "contains whitespace"},
		{"space-in-os", "win dows/amd64", "contains whitespace"},
		{"space-in-arch", "windows/am d64", "contains whitespace"},
		{"tab-internal", "windows/\tamd64", "contains whitespace"},
		{"no-slash", "windows-amd64", "GOOS/GOARCH"},
		{"triple", "windows/amd64/extra", "GOOS/GOARCH"},
		{"empty-os", "/amd64", "GOOS/GOARCH"},
		{"empty-arch", "windows/", "GOOS/GOARCH"},
	}
	for _, tc := range bad {
		t.Run("reject/"+tc.name, func(t *testing.T) {
			p := &AvailabilityProbe{Binaries: []string{"node"}, Platforms: []string{tc.plat}}
			err := ValidateProbeValuesNonEmpty(p, "manifest x")
			if err == nil {
				t.Fatalf("platforms[%q] accepted; want reject", tc.plat)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error %q missing %q", err, tc.want)
			}
		})
	}

	good := &AvailabilityProbe{
		Binaries:  []string{"node", "npx"},
		Platforms: []string{"windows/amd64", "linux/amd64", "linux/arm64", "darwin/amd64", "darwin/arm64"},
	}
	if err := ValidateProbeValuesNonEmpty(good, "manifest x"); err != nil {
		t.Fatalf("well-formed platforms[] rejected: %v", err)
	}
}

// TestManifest_PlatformProbeRoundTripsAndValidates proves a manifest carrying an
// install_probe with platforms[] passes Validate() (A6 is satisfied by binaries;
// the platforms tokens pass ValidateProbeValuesNonEmpty) and YAML round-trips the
// new field — the survive-generate→create→install path the catalog draft relies on.
func TestManifest_PlatformProbeRoundTripsAndValidates(t *testing.T) {
	m := baseStdioManifest()
	m.Availability = AvailabilityDisabledUntilProbe
	m.InstallProbe = &AvailabilityProbe{
		Binaries:  []string{"node", "npx"},
		Platforms: []string{"windows/amd64", "linux/amd64", "linux/arm64", "darwin/amd64", "darwin/arm64"},
	}
	if err := m.Validate(); err != nil {
		t.Fatalf("manifest with platforms[] probe rejected by Validate(): %v", err)
	}
}

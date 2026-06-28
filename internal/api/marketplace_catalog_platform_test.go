package api

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestParseCatalog_PlatformsAcceptedUnderV2 proves the new install_probe.platforms[]
// sub-field parses cleanly on the fetch path AND survives the AUTHOR-STRICT decode
// (DisallowUnknownFields), so the repo's own catalog can carry it. The row is inert
// (disabled-until-probe) with a binaries+platforms probe — Onshape's exact shape.
func TestParseCatalog_PlatformsAcceptedUnderV2(t *testing.T) {
	raw := `{"schema_version":"2","entries":[{
      "id":"onshape","name":"Onshape","transport":"stdio","command":"npx","args":["x"],
      "availability":"disabled-until-probe",
      "install_probe":{"binaries":["node","npx"],"platforms":["windows/amd64","linux/amd64","linux/arm64","darwin/amd64","darwin/arm64"]}
    }]}`
	// Fetch (tolerant) path.
	cat, err := ParseMarketplaceCatalog([]byte(raw))
	if err != nil {
		t.Fatalf("fetch decode rejected install_probe.platforms: %v", err)
	}
	p := cat.Entries[0].InstallProbe
	if p == nil || len(p.Platforms) != 5 {
		t.Fatalf("platforms[] not decoded into the probe: %#v", p)
	}
	// Author-strict path (DisallowUnknownFields) — proves platforms is a KNOWN key,
	// so the in-repo catalog carrying it does not fail the strict author gate.
	if _, err := ParseMarketplaceCatalogStrict([]byte(raw)); err != nil {
		t.Fatalf("author-strict decode rejected install_probe.platforms (it must be a known key): %v", err)
	}
}

// TestParseCatalog_PlatformsRequireSchemaV2 proves the platforms[] sub-field is
// v2-gated TRANSITIVELY: it lives inside install_probe, which is itself a v2-gated
// top-level entry key (newCatalogFieldKeys). So a schema_version 1 catalog carrying
// install_probe (with or without platforms) is rejected naming install_probe — an
// older v1-only client (DisallowUnknownFields) never has to face the unknown
// install_probe key on a v1 doc, and therefore never the platforms sub-key either.
// No NEW top-level key is introduced by platforms[], so the existing gate already
// covers it; this asserts that invariant explicitly.
func TestParseCatalog_PlatformsRequireSchemaV2(t *testing.T) {
	v1 := `{"schema_version":"1","entries":[{"id":"x","name":"X","transport":"stdio","command":"npx",` +
		`"install_probe":{"binaries":["node"],"platforms":["windows/amd64"]}}]}`
	_, err := ParseMarketplaceCatalog([]byte(v1))
	if err == nil {
		t.Fatalf("v1 catalog accepted install_probe carrying platforms[]; want reject")
	}
	if !strings.Contains(err.Error(), `install_probe requires catalog schema_version "2"`) {
		t.Fatalf("v1 reject error %q must name install_probe as the v2-gated key", err)
	}

	// The SAME body under schema_version 2 is accepted (must also be inert for the
	// pre-existing A5 install_probe shape rule).
	v2 := `{"schema_version":"2","entries":[{"id":"x","name":"X","transport":"stdio","command":"npx",` +
		`"availability":"watch","install_probe":{"binaries":["node"],"platforms":["windows/amd64"]}}]}`
	if _, err := ParseMarketplaceCatalog([]byte(v2)); err != nil {
		t.Fatalf("v2 catalog rejected install_probe.platforms; want accept: %v", err)
	}
}

// oldCatalogProbe is a SNAPSHOT of the pre-this-change CatalogAvailabilityProbe —
// the struct an ALREADY-DEPLOYED older client has, WITHOUT the Platforms field. It
// is the forward-compat oracle: an old client must keep parsing a v2 catalog that
// carries the new platforms[] sub-field instead of choking on it.
type oldCatalogProbe struct {
	Binaries  []string `json:"binaries,omitempty"`
	Files     []string `json:"files,omitempty"`
	FileGlobs []string `json:"file_globs,omitempty"`
}

// TestParseCatalog_OldParserIgnoresPlatforms is the FORWARD-COMPAT regression: the
// deployed fetch parser leaves DisallowUnknownFields OFF, so an OLDER client whose
// install_probe struct predates the platforms[] field must DECODE a v2 install_probe
// carrying platforms[] WITHOUT error — the unknown platforms key is silently ignored
// (the binaries it DOES know still decode). This is what makes adding platforms[] a
// non-breaking additive rollout for clients already in the field.
func TestParseCatalog_OldParserIgnoresPlatforms(t *testing.T) {
	// The exact install_probe JSON a v2 catalog ships with the platforms[] field.
	probeJSON := []byte(`{"binaries":["node","npx"],"platforms":["windows/amd64","linux/amd64"]}`)

	var old oldCatalogProbe
	// No DisallowUnknownFields — mirrors the deployed-client fetch decode contract.
	if err := json.Unmarshal(probeJSON, &old); err != nil {
		t.Fatalf("old (no-Platforms) probe struct failed to decode a v2 probe carrying platforms[]; forward-compat broken: %v", err)
	}
	if len(old.Binaries) != 2 || old.Binaries[0] != "node" {
		t.Fatalf("old parser dropped the binaries it DOES know: %#v", old)
	}
}

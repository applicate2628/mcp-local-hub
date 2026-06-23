package api

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestParseCatalog_VendoredAvailabilityFieldsAccepted proves the new catalog
// struct fields EXIST: ParseMarketplaceCatalog uses dec.DisallowUnknownFields(),
// so a catalog carrying vendored_source/availability/install_probe would reject
// wholesale if the fields were missing. It parses cleanly here. The new fields
// are gated to schema_version 2 (codex r6 finding 2), so the catalog declares "2".
func TestParseCatalog_VendoredAvailabilityFieldsAccepted(t *testing.T) {
	raw := `{
  "schema_version": "2",
  "entries": [
    {"id": "mathcad", "name": "Mathcad MCP", "transport": "stdio", "command": "python",
     "vendored_source": {"repo": "https://github.com/puran-water/mathcad-mcp",
       "pinned_ref": "a1b2c3d4e5f6a1b2c3d4e5f6a1b2c3d4e5f6a1b2",
       "install_cmd": "uv pip install .", "run_cmd": "python -m mathcad_mcp",
       "license_status": "confirmed"},
     "availability": "watch",
     "install_probe": {"binaries": ["matlab"], "files": []}}
  ]
}`
	cat, err := ParseMarketplaceCatalog([]byte(raw))
	if err != nil {
		t.Fatalf("ParseMarketplaceCatalog rejected vendored+availability entry: %v", err)
	}
	e := cat.Entries[0]
	if e.VendoredSource == nil || e.VendoredSource.PinnedRef == "" {
		t.Fatalf("vendored_source not parsed: %#v", e.VendoredSource)
	}
	if e.Availability != "watch" {
		t.Fatalf("availability = %q, want watch", e.Availability)
	}
	if e.InstallProbe == nil || len(e.InstallProbe.Binaries) != 1 {
		t.Fatalf("install_probe not parsed: %#v", e.InstallProbe)
	}
}

// TestParseCatalog_VendoredDefenseInDepth pins the catalog-side mirror of the
// manifest Gate A: an unpinned vendored entry, a moving-branch pin, a bad
// license_status, a bad availability, and a watch row with no probe all reject
// at catalog parse — one layer before the manifest gate.
func TestParseCatalog_VendoredDefenseInDepth(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		want string
	}{
		{
			"unpinned-vendored",
			`{"schema_version":"2","entries":[{"id":"x","name":"X","transport":"stdio","command":"npx","vendored_source":{"repo":"r"}}]}`,
			"requires a non-empty pinned_ref",
		},
		{
			"moving-branch-pin",
			`{"schema_version":"2","entries":[{"id":"x","name":"X","transport":"stdio","command":"npx","vendored_source":{"pinned_ref":"main"}}]}`,
			"requires a non-empty pinned_ref",
		},
		{
			"bad-license-status",
			`{"schema_version":"2","entries":[{"id":"x","name":"X","transport":"stdio","command":"npx","vendored_source":{"pinned_ref":"v1","license_status":"weird"}}]}`,
			"license_status",
		},
		{
			"bad-availability",
			`{"schema_version":"2","entries":[{"id":"x","name":"X","transport":"stdio","command":"npx","availability":"bogus"}]}`,
			"availability",
		},
		{
			"watch-no-probe",
			`{"schema_version":"2","entries":[{"id":"x","name":"X","transport":"stdio","command":"npx","availability":"watch"}]}`,
			"requires a non-empty install_probe",
		},
		{
			"probe-on-ready",
			`{"schema_version":"2","entries":[{"id":"x","name":"X","transport":"stdio","command":"npx","availability":"ready","install_probe":{"binaries":["matlab"]}}]}`,
			"only meaningful with availability=watch",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := ParseMarketplaceCatalog([]byte(tc.raw))
			if err == nil {
				t.Fatalf("catalog accepted %s; want reject", tc.name)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error %q missing %q", err, tc.want)
			}
		})
	}
}

// TestParseCatalog_CurrentCatalogStillParses is the byte-identical regression
// guard for the existing repo catalog: the 14 shipped entries (none carrying the
// new fields) parse cleanly with the new struct fields present, and none of them
// decodes a non-nil vendored_source / availability / install_probe.
func TestParseCatalog_CurrentCatalogStillParses(t *testing.T) {
	// internal/api/ test cwd → repo root is two levels up.
	path := filepath.Join("..", "..", "marketplace", "v1", "catalog.json")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Skipf("current catalog not readable at %s: %v", path, err)
	}
	cat, err := ParseMarketplaceCatalog(raw)
	if err != nil {
		t.Fatalf("current shipped catalog failed to parse with new struct fields: %v", err)
	}
	if len(cat.Entries) == 0 {
		t.Fatalf("current catalog parsed to zero entries")
	}
	for _, e := range cat.Entries {
		if e.VendoredSource != nil || e.Availability != "" || e.InstallProbe != nil {
			t.Fatalf("existing entry %q decoded a Tier-0 field though catalog omits it: vs=%#v av=%q probe=%#v",
				e.ID, e.VendoredSource, e.Availability, e.InstallProbe)
		}
	}
}

// TestParseCatalog_BranchQualifiedMovingRefAndEmptyProbe is the catalog-side
// mirror of FINDING-4 (D-2 branch-qualified moving-ref rejection — ANY
// refs/heads/* or refs/remotes/* ref is a moving branch regardless of the bare
// name) and the D-3 A7 empty-probe-value rejection. The catalog gate must catch
// both shapes one layer before the manifest gate, reusing the SAME config owners
// (config.IsMovingGitRef / config.ValidateProbeValuesNonEmpty) so it cannot
// drift. A refs/tags/<tag> pin (immutable) must still be ACCEPTED.
func TestParseCatalog_BranchQualifiedMovingRefAndEmptyProbe(t *testing.T) {
	reject := []struct {
		name string
		raw  string
		want string
	}{
		{
			"branch-qualified-moving-pin",
			`{"schema_version":"2","entries":[{"id":"x","name":"X","transport":"stdio","command":"npx","vendored_source":{"pinned_ref":"refs/heads/main"}}]}`,
			"requires a non-empty pinned_ref",
		},
		{
			"branch-qualified-nonlisted-name-pin",
			`{"schema_version":"2","entries":[{"id":"x","name":"X","transport":"stdio","command":"npx","vendored_source":{"pinned_ref":"refs/heads/release-2026"}}]}`,
			"requires a non-empty pinned_ref",
		},
		{
			"remote-tracking-moving-pin",
			`{"schema_version":"2","entries":[{"id":"x","name":"X","transport":"stdio","command":"npx","vendored_source":{"pinned_ref":"refs/remotes/origin/master"}}]}`,
			"requires a non-empty pinned_ref",
		},
		{
			"remote-tracking-nonlisted-name-pin",
			`{"schema_version":"2","entries":[{"id":"x","name":"X","transport":"stdio","command":"npx","vendored_source":{"pinned_ref":"refs/remotes/origin/feature/foo"}}]}`,
			"requires a non-empty pinned_ref",
		},
		{
			// bot catalog-r3 P2: bare "<remote>/<branch>" shorthand (no refs/
			// prefix) is a moving branch — any non-tag slash form is rejected.
			"remote-branch-shorthand-pin",
			`{"schema_version":"2","entries":[{"id":"x","name":"X","transport":"stdio","command":"npx","vendored_source":{"pinned_ref":"origin/main"}}]}`,
			"requires a non-empty pinned_ref",
		},
		{
			"upstream-branch-shorthand-pin",
			`{"schema_version":"2","entries":[{"id":"x","name":"X","transport":"stdio","command":"npx","vendored_source":{"pinned_ref":"upstream/develop"}}]}`,
			"requires a non-empty pinned_ref",
		},
		{
			"empty-probe-binary",
			`{"schema_version":"2","entries":[{"id":"x","name":"X","transport":"stdio","command":"npx","availability":"watch","install_probe":{"binaries":[""]}}]}`,
			"binaries[0] is empty",
		},
		{
			"empty-probe-file",
			`{"schema_version":"2","entries":[{"id":"x","name":"X","transport":"stdio","command":"npx","availability":"disabled-until-probe","install_probe":{"files":["   "]}}]}`,
			"files[0] is empty",
		},
		{
			// FINDING-3 (catalog r4 P2) mirror: a whitespace-padded binary token
			// passes the trimmed-non-empty check but the runtime probe looks up the
			// verbatim padded value → permanent-disable. Rejected one layer earlier.
			"whitespace-padded-binary",
			`{"schema_version":"2","entries":[{"id":"x","name":"X","transport":"stdio","command":"npx","availability":"watch","install_probe":{"binaries":["go "]}}]}`,
			"has leading/trailing whitespace",
		},
		{
			// FINDING-3 (catalog r4 P2) mirror: a padded file path is stat'd verbatim.
			"whitespace-padded-file",
			`{"schema_version":"2","entries":[{"id":"x","name":"X","transport":"stdio","command":"npx","availability":"watch","install_probe":{"files":["/opt/x/marker "]}}]}`,
			"has leading/trailing whitespace",
		},
		{
			// FINDING-2 (catalog r4 P2) mirror: a relative file probe is stat'd
			// against the process CWD, so the gate would depend on which directory
			// mcphub runs from. Rejected at catalog parse.
			"relative-probe-file",
			`{"schema_version":"2","entries":[{"id":"x","name":"X","transport":"stdio","command":"npx","availability":"watch","install_probe":{"files":["marker"]}}]}`,
			"must be an absolute path",
		},
	}
	for _, tc := range reject {
		t.Run("reject/"+tc.name, func(t *testing.T) {
			_, err := ParseMarketplaceCatalog([]byte(tc.raw))
			if err == nil {
				t.Fatalf("catalog accepted %s; want reject", tc.name)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error %q missing %q", err, tc.want)
			}
		})
	}
	// Immutable pins ACCEPTED: a fully-qualified tag, a slash tag under
	// refs/tags/, a bare tag, a 40-hex SHA, and a 7-hex short SHA (bot
	// catalog-r3 P2 — the complete invertible immutability rule).
	accepts := []string{
		`{"schema_version":"2","entries":[{"id":"x","name":"X","transport":"stdio","command":"npx","vendored_source":{"pinned_ref":"refs/tags/v1.2.3"}}]}`,
		`{"schema_version":"2","entries":[{"id":"x","name":"X","transport":"stdio","command":"npx","vendored_source":{"pinned_ref":"refs/tags/release/2026"}}]}`,
		`{"schema_version":"2","entries":[{"id":"x","name":"X","transport":"stdio","command":"npx","vendored_source":{"pinned_ref":"v1.2.3"}}]}`,
		`{"schema_version":"2","entries":[{"id":"x","name":"X","transport":"stdio","command":"npx","vendored_source":{"pinned_ref":"a1b2c3d4e5f6a1b2c3d4e5f6a1b2c3d4e5f6a1b2"}}]}`,
		`{"schema_version":"2","entries":[{"id":"x","name":"X","transport":"stdio","command":"npx","vendored_source":{"pinned_ref":"a1b2c3d"}}]}`,
	}
	// FINDING-2/3 (catalog r4 P2): a clean, un-padded, HOST-ABSOLUTE file probe on
	// an inert row is the valid shape and must still be ACCEPTED. The path is built
	// from t.TempDir (absolute on every OS — a hardcoded POSIX "/opt/..." is NOT
	// absolute under filepath.IsAbs on Windows) and JSON-encoded so a Windows
	// backslash path is escaped correctly in the literal.
	absMarker, err := json.Marshal(filepath.Join(t.TempDir(), "marker"))
	if err != nil {
		t.Fatalf("marshal abs marker: %v", err)
	}
	accepts = append(accepts, `{"schema_version":"2","entries":[{"id":"x","name":"X","transport":"stdio","command":"npx","availability":"watch","install_probe":{"binaries":["go"],"files":[`+string(absMarker)+`]}}]}`)
	for _, raw := range accepts {
		if _, err := ParseMarketplaceCatalog([]byte(raw)); err != nil {
			t.Fatalf("catalog rejected an immutable pin; want accept: %v\nraw=%s", err, raw)
		}
	}
}

// TestParseCatalog_NewFieldsRequireSchemaV2 is the codex r6 finding 2 regression:
// the additive D-2/D-3 entry fields (vendored_source / availability / install_probe)
// are gated to schema_version 2. A schema_version 1 catalog that carries ANY of them
// is REJECTED (naming the field + "requires catalog schema_version \"2\""), so a v1
// catalog can NEVER carry the new keys — which is what stops an OLDER v1-only client
// (DisallowUnknownFields rejects the WHOLE catalog on an unknown key) from breaking
// when Tier-1 rolls out the metadata. The SAME entry under schema_version 2 is
// accepted. The current shipped v1 catalog (which carries none of the fields) still
// parses (asserted by TestParseCatalog_CurrentCatalogStillParses).
func TestParseCatalog_NewFieldsRequireSchemaV2(t *testing.T) {
	// v1Body is rejected under schema_version 1 by the new-fields-require-v2 gate
	// (asserting wantReject names the offending field FIRST, before any downstream
	// vendored/availability shape check). v2Body is the valid v2 shape that parses.
	// They differ only where the new-fields gate and the existing A5 shape gate
	// would otherwise collide (an install_probe is only valid alongside an inert
	// availability — a pre-existing rule independent of versioning).
	cases := []struct {
		name       string
		v1Body     string
		v2Body     string
		wantReject string
	}{
		{
			"vendored_source",
			`"vendored_source":{"pinned_ref":"a1b2c3d4e5f6a1b2c3d4e5f6a1b2c3d4e5f6a1b2"}`,
			`"vendored_source":{"pinned_ref":"a1b2c3d4e5f6a1b2c3d4e5f6a1b2c3d4e5f6a1b2"}`,
			`vendored_source requires catalog schema_version "2"`,
		},
		{
			"availability-and-probe",
			`"availability":"watch","install_probe":{"binaries":["matlab"]}`,
			`"availability":"watch","install_probe":{"binaries":["matlab"]}`,
			`availability requires catalog schema_version "2"`,
		},
		{
			// install_probe ALONE (no availability) is rejected under v1 by the
			// new-fields gate (install_probe message), and under v2 it must be paired
			// with an inert availability to satisfy the pre-existing A5 shape rule.
			"install_probe-only",
			`"install_probe":{"binaries":["matlab"]}`,
			`"availability":"watch","install_probe":{"binaries":["matlab"]}`,
			`install_probe requires catalog schema_version "2"`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			v1 := `{"schema_version":"1","entries":[{"id":"x","name":"X","transport":"stdio","command":"npx",` + tc.v1Body + `}]}`
			_, err := ParseMarketplaceCatalog([]byte(v1))
			if err == nil {
				t.Fatalf("v1 catalog accepted new field %s; want reject", tc.name)
			}
			if !strings.Contains(err.Error(), tc.wantReject) {
				t.Fatalf("v1 reject error %q missing %q", err, tc.wantReject)
			}
			// The valid v2 shape parses (the additive metadata is allowed at v2).
			v2 := `{"schema_version":"2","entries":[{"id":"x","name":"X","transport":"stdio","command":"npx",` + tc.v2Body + `}]}`
			if _, err := ParseMarketplaceCatalog([]byte(v2)); err != nil {
				t.Fatalf("v2 catalog rejected new field %s; want accept: %v", tc.name, err)
			}
		})
	}

	// A v1 catalog with NONE of the new fields still parses (the additive-rollout
	// invariant: existing v1 clients are unaffected).
	cleanV1 := `{"schema_version":"1","entries":[{"id":"x","name":"X","transport":"stdio","command":"npx"}]}`
	if _, err := ParseMarketplaceCatalog([]byte(cleanV1)); err != nil {
		t.Fatalf("clean v1 catalog (no new fields) rejected; want accept: %v", err)
	}
}

// TestParseCatalog_NewFieldsRequireSchemaV2_KeyPresence is the codex r7 P2
// regression: the forward-compat gate must reject on raw KEY PRESENCE, not the
// decoded value. A v1 catalog that includes `availability:""` / `availability:null`
// / `vendored_source:null` / `install_probe:null` has the KEY present in the JSON,
// but a typed decode leaves the field empty/nil — so the old value-only check
// treated those as ABSENT and ACCEPTED the v1 doc, even though an older v1-only
// client (DisallowUnknownFields) rejects that same catalog SOLELY because the key
// is present. Each present-empty / present-null form under schema_version 1 must
// now REJECT naming the offending KEY; the same body under schema_version 2 is
// ACCEPTED (a present-empty/null new field is benign at v2).
func TestParseCatalog_NewFieldsRequireSchemaV2_KeyPresence(t *testing.T) {
	cases := []struct {
		name       string
		body       string // the new-key fragment spliced into one entry
		wantReject string // the KEY the v1 rejection must name
	}{
		{"availability-empty-string", `"availability":""`, `availability requires catalog schema_version "2"`},
		{"availability-null", `"availability":null`, `availability requires catalog schema_version "2"`},
		{"vendored_source-null", `"vendored_source":null`, `vendored_source requires catalog schema_version "2"`},
		{"install_probe-null", `"install_probe":null`, `install_probe requires catalog schema_version "2"`},
		// A populated field is the r6-covered case; re-assert it goes through the
		// SAME key-presence gate (the key is present and populated).
		{"availability-populated", `"availability":"watch","install_probe":{"binaries":["matlab"]}`, `availability requires catalog schema_version "2"`},
	}
	for _, tc := range cases {
		t.Run("v1-reject/"+tc.name, func(t *testing.T) {
			v1 := `{"schema_version":"1","entries":[{"id":"x","name":"X","transport":"stdio","command":"npx",` + tc.body + `}]}`
			_, err := ParseMarketplaceCatalog([]byte(v1))
			if err == nil {
				t.Fatalf("v1 catalog accepted present-but-empty/null new key %s; want reject", tc.name)
			}
			if !strings.Contains(err.Error(), tc.wantReject) {
				t.Fatalf("v1 reject error %q missing %q", err, tc.wantReject)
			}
		})
	}

	// Under schema_version 2, a present-empty/null new key is benign and ACCEPTED
	// (the value-based shape gates short-circuit on empty/nil). Populated v2 shapes
	// are covered by TestParseCatalog_NewFieldsRequireSchemaV2.
	acceptV2 := []string{
		`"availability":""`,
		`"availability":null`,
		`"vendored_source":null`,
		`"install_probe":null`,
	}
	for _, body := range acceptV2 {
		t.Run("v2-accept/"+body, func(t *testing.T) {
			v2 := `{"schema_version":"2","entries":[{"id":"x","name":"X","transport":"stdio","command":"npx",` + body + `}]}`
			if _, err := ParseMarketplaceCatalog([]byte(v2)); err != nil {
				t.Fatalf("v2 catalog rejected present-empty/null new key %q; want accept: %v", body, err)
			}
		})
	}

	// A v1 catalog with NONE of the keys present still parses (the additive-rollout
	// invariant on the key-presence path).
	cleanV1 := `{"schema_version":"1","entries":[{"id":"x","name":"X","transport":"stdio","command":"npx"}]}`
	if _, err := ParseMarketplaceCatalog([]byte(cleanV1)); err != nil {
		t.Fatalf("clean v1 catalog (no new keys present) rejected; want accept: %v", err)
	}
}

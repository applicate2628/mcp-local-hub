package api

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

const trailProviderTestRow = `{"id":"trail","name":"Trail MCP","kind":"project-provider","service":"trail","package":{"manager":"npm","name":"@trailhq/trail-code","bin":"trail","channel":"latest"}}`
const reaProviderTestRow = `{"id":"rea","name":"REA MCP","kind":"project-provider","service":"rea","package":{"manager":"npm","name":"rea-agents","bin":"rea","channel":"latest"}}`

func TestMarketplaceTrailV3CatalogParity(t *testing.T) {
	readCatalog := func(version string) []byte {
		t.Helper()
		raw, err := os.ReadFile(filepath.Join("..", "..", "marketplace", version, "catalog.json"))
		if err != nil {
			t.Fatal(err)
		}
		return raw
	}
	var old, current struct {
		Entries []json.RawMessage `json:"entries"`
		Docs    []json.RawMessage `json:"docs_only"`
	}
	v3 := readCatalog("v3")
	if err := json.Unmarshal(readCatalog("v2"), &old); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(v3, &current); err != nil {
		t.Fatal(err)
	}
	wantProviders := []struct {
		id          string
		packageName string
	}{
		{"trail", "@trailhq/trail-code"},
		{"rea", "rea-agents"},
	}
	if len(current.Entries) != len(old.Entries)+len(wantProviders) ||
		!reflect.DeepEqual(old.Entries, current.Entries[:len(old.Entries)]) ||
		!reflect.DeepEqual(old.Docs, current.Docs) {
		t.Fatal("V3 must preserve V2 rows and append Trail and REA")
	}
	if !strings.HasSuffix(DefaultMarketplaceRegistryURL, "/marketplace/v2/catalog.json") {
		t.Fatal("V2 must remain the default registry")
	}
	cat, err := ParseMarketplaceCatalogStrict(v3)
	if err != nil {
		t.Fatal(err)
	}
	if cat.SchemaVersion != MarketplaceCatalogSchemaVersionV3 {
		t.Fatalf("expected V3 catalog, got %q", cat.SchemaVersion)
	}
	for i, want := range wantProviders {
		entry := &cat.Entries[len(old.Entries)+i]
		wantPackage := MarketplaceProjectPackage{Manager: "npm", Name: want.packageName, Bin: want.id, Channel: "latest"}
		if entry.ID != want.id || entry.Kind != MarketplaceProjectProviderKind || entry.Service != want.id ||
			entry.Transport != "" || entry.Package == nil || *entry.Package != wantPackage {
			t.Fatalf("unexpected %s row: %+v", want.id, entry)
		}
		draft, warnings, err := GenerateDraftManifest(entry, GenerateOpts{})
		if err == nil || draft != "" || len(warnings) != 0 {
			t.Fatalf("%s draft=%q warnings=%v err=%v", want.id, draft, warnings, err)
		}
	}
}

func TestMarketplaceTrailV3RawAdmission(t *testing.T) {
	const v3 = `{"schema_version":"3","entries":[`
	for _, tc := range []struct {
		name    string
		raw     string
		wantErr bool
	}{
		{"official Trail", v3 + trailProviderTestRow + `]}`, false},
		{"official REA", v3 + reaProviderTestRow + `]}`, false},
		{"V2 REA provider", `{"schema_version":"2","entries":[` + reaProviderTestRow + `]}`, true},
		{"REA using Trail package", v3 + strings.Replace(reaProviderTestRow, "rea-agents", "@trailhq/trail-code", 1) + `]}`, true},
		{"V2 provider", `{"schema_version":"2","entries":[` + trailProviderTestRow + `]}`, true},
		{"mixed managed recipe", v3 + trailProviderTestRow[:len(trailProviderTestRow)-1] + `,"managed_recipe":null}]}`, true},
		{"present null transport", v3 + trailProviderTestRow[:len(trailProviderTestRow)-1] + `,"transport":null}]}`, true},
		{"folded empty executable field", v3 + trailProviderTestRow[:len(trailProviderTestRow)-1] + `,"Args":[]}]}`, true},
		{"duplicate record key", v3 + `{"id":"trail",` + trailProviderTestRow[1:] + `]}`, true},
		{"duplicate package key", v3 + strings.Replace(trailProviderTestRow, `"bin":"trail"`, `"bin":"trail","bin":"trail"`, 1) + `]}`, true},
		{"unofficial package", v3 + strings.Replace(trailProviderTestRow, "@trailhq/trail-code", "@example/trail", 1) + `]}`, true},
		{"earlier explicit marker overwritten", v3 + `{"id":"other","name":"Other","transport":"stdio","command":"fixture","kind":"project\u002dprovider","KIND":"server","package":"distribution note"}]}`, true},
		{"duplicate entries hides raw record", v3 + trailProviderTestRow + `],"ENTRIES":[{}]}`, true},
		{"duplicate entries hides provider presence", v3 + trailProviderTestRow + `],"entries":[]}`, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cat, err := ParseMarketplaceCatalog([]byte(tc.raw))
			if (err != nil) != tc.wantErr || (cat == nil) != tc.wantErr {
				t.Fatalf("catalog=%+v err=%v wantErr=%t", cat, err, tc.wantErr)
			}
		})
	}
}

func TestMarketplaceTrailV3OrdinaryMetadataCompatibility(t *testing.T) {
	const ordinary = `{"schema_version":"2","entries":[
		{"id":"one","name":"One","transport":"stdio","command":"fixture","kind":"server","service":{"note":true},"package":"distribution note"},
		{"id":"two","name":"Two","transport":"stdio","command":"fixture","kind":42,"service":null,"package":null}
	]}`
	assertOrdinary := func(cat *MarketplaceCatalog, err error) {
		t.Helper()
		if err != nil || cat == nil || len(cat.Entries) != 2 {
			t.Fatalf("ordinary catalog=%+v err=%v", cat, err)
		}
		for _, entry := range cat.Entries {
			if entry.Transport != "stdio" || entry.Command != "fixture" ||
				entry.Kind != "" || entry.Service != "" || entry.Package != nil {
				t.Fatalf("ordinary metadata affected typed entry: %+v", entry)
			}
		}
	}
	assertOrdinary(ParseMarketplaceCatalog([]byte(ordinary)))
	if cat, err := ParseMarketplaceCatalogStrict([]byte(ordinary)); cat != nil || err == nil || !strings.Contains(err.Error(), "unknown field") {
		t.Fatalf("strict authoring accepted ordinary unknown metadata: catalog=%+v err=%v", cat, err)
	}

	_ = hubMcpStateTestHelper(t)
	payload := []byte(`{"schema_version":"2","catalog":` + ordinary + `}`)
	if err := writeHubMcpStateFile(marketplaceCacheFileLeaf, payload); err != nil {
		t.Fatal(err)
	}
	cached, err := readMarketplaceCache()
	if err != nil || cached == nil {
		t.Fatalf("ordinary cache=%+v err=%v", cached, err)
	}
	assertOrdinary(&cached.Catalog, nil)
}

func TestMarketplaceTrailV3CacheRoundTrip(t *testing.T) {
	_ = hubMcpStateTestHelper(t)
	raw := []byte(`{"schema_version":"3","entries":[` + trailProviderTestRow + "," + reaProviderTestRow + `]}`)
	cat, err := ParseMarketplaceCatalog(raw)
	if err != nil {
		t.Fatal(err)
	}
	const url = "https://registry.example/explicit/v3/catalog.json"
	if err := writeMarketplaceCache(&marketplaceCacheFile{SchemaVersion: cat.SchemaVersion, SourceURL: url, Catalog: *cat}); err != nil {
		t.Fatal(err)
	}
	payload, err := readHubMcpStateFile(marketplaceCacheFileLeaf)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(payload, []byte(`"transport"`)) {
		t.Fatal("cache serialized transport for a provider")
	}
	cached, err := readMarketplaceCache()
	if err != nil || cached == nil || cached.SourceURL != url || !reflect.DeepEqual(&cached.Catalog, cat) {
		t.Fatalf("cache=%+v err=%v", cached, err)
	}
	merged := []byte(`{"schema_version":"3","catalog":` + string(raw) + `,"CATALOG":{"entries":[{}]}}`)
	if err := writeHubMcpStateFile(marketplaceCacheFileLeaf, merged); err != nil {
		t.Fatal(err)
	}
	if cached, err := readMarketplaceCache(); cached != nil || err == nil {
		t.Fatalf("ambiguous catalog cache=%+v err=%v", cached, err)
	}
}

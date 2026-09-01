package api

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestPaperSearchCatalogsConstrainMCPToV1(t *testing.T) {
	want := []string{"--from", "paper-search-mcp==0.1.4", "--with", "fastmcp==3.4.7", "--with", "mcp==1.29.0", "python", "-m", "paper_search_mcp.server"}
	embedded, err := loadManifestYAMLEmbedFirst("paper-search-mcp")
	if err != nil {
		t.Fatal(err)
	}
	manifest, err := parseManifestForName("paper-search-mcp", embedded)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(manifest.BaseArgs, want) {
		t.Fatalf("embedded base_args = %#v, want %#v", manifest.BaseArgs, want)
	}
	for _, catalogPath := range []string{
		filepath.Join("..", "..", "marketplace", "v1", "catalog.json"),
		filepath.Join("..", "..", "marketplace", "v2", "catalog.json"),
	} {
		raw, err := os.ReadFile(catalogPath)
		if err != nil {
			t.Fatal(err)
		}
		catalog, err := ParseMarketplaceCatalog(raw)
		if err != nil {
			t.Fatal(err)
		}
		for _, entry := range catalog.Entries {
			if entry.ID != "paper-search-mcp" {
				continue
			}
			if !reflect.DeepEqual(entry.Args, want) {
				t.Fatalf("%s args = %#v, want %#v", catalogPath, entry.Args, want)
			}
			break
		}
	}
}

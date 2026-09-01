package config

import (
	"bytes"
	"os"
	"reflect"
	"testing"
)

func TestPaperSearchManifestConstrainsMCPToV1(t *testing.T) {
	raw, err := os.ReadFile("../../servers/paper-search-mcp/manifest.yaml")
	if err != nil {
		t.Fatal(err)
	}
	manifest, err := ParseManifest(bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"--from", "paper-search-mcp==0.1.4", "--with", "fastmcp==3.4.7", "--with", "mcp==1.29.0", "python", "-m", "paper_search_mcp.server"}
	if !reflect.DeepEqual(manifest.BaseArgs, want) {
		t.Fatalf("base_args = %#v, want %#v", manifest.BaseArgs, want)
	}
}

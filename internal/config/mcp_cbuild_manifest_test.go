package config

import (
	"bytes"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestCBuildManifestUsesMcphubSubcommandAndCMakeOnly(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "servers", "mcp-cbuild", "manifest.yaml"))
	if err != nil {
		t.Fatalf("read mcp-cbuild manifest: %v", err)
	}
	manifest, err := ParseManifest(bytes.NewReader(raw))
	if err != nil {
		t.Fatalf("parse mcp-cbuild manifest: %v", err)
	}
	if manifest.Command != "mcphub" || !reflect.DeepEqual(manifest.BaseArgs, []string{"cbuild"}) {
		t.Fatalf("launcher = command:%q args:%v, want mcphub cbuild", manifest.Command, manifest.BaseArgs)
	}
	if !reflect.DeepEqual(manifest.RequiredBinaries, []string{"cmake"}) {
		t.Fatalf("required_binaries = %v, want [cmake]", manifest.RequiredBinaries)
	}
}

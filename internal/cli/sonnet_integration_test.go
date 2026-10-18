package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"mcp-local-hub/internal/config"
)

func TestRootRegistersHiddenSonnetStdioCommand(t *testing.T) {
	root := NewRootCmd()
	command, _, err := root.Find([]string{"sonnet"})
	if err != nil || command == nil || command.Name() != "sonnet" {
		t.Fatalf("find sonnet command=(%v, %v)", command, err)
	}
	if !command.Hidden || command.RunE == nil {
		t.Fatalf("sonnet command hidden=%v runE=%v", command.Hidden, command.RunE != nil)
	}
	if err := command.ParseFlags([]string{"--probe"}); err == nil {
		t.Fatal("sonnet accepted retired --probe flag")
	}
}

func TestSonnetManifestValidatesAsInertNativeStdioBridge(t *testing.T) {
	body, err := os.ReadFile(filepath.Join("..", "..", "servers", "sonnet", "manifest.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	manifest, err := config.ParseManifest(bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	if manifest.Name != "sonnet" || manifest.Transport != config.TransportStdioBridge || manifest.Command != "mcphub" {
		t.Fatalf("manifest identity=%+v", manifest)
	}
	if len(manifest.BaseArgs) != 1 || manifest.BaseArgs[0] != "sonnet" || manifest.Availability != config.AvailabilityDisabledUntilProbe {
		t.Fatalf("manifest command/availability=%+v", manifest)
	}
	if manifest.InstallProbe == nil || len(manifest.InstallProbe.Binaries) != 1 || manifest.InstallProbe.Binaries[0] != "mcphub" {
		t.Fatalf("manifest install probe=%+v", manifest.InstallProbe)
	}
	if config.PlatformMatches("linux", "amd64", manifest.InstallProbe.Platforms) {
		t.Fatalf("non-Windows host matched Sonnet manifest platforms=%v", manifest.InstallProbe.Platforms)
	}
}

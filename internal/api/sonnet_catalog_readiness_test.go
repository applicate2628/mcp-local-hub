package api

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"mcp-local-hub/internal/config"
)

func TestSonnetCatalogBridgeProbeDoesNotRequireProductExecutable(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("Sonnet catalog row is Windows-only")
	}
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "mcphub.exe"), []byte("bridge"), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)
	probe := &config.AvailabilityProbe{Binaries: []string{"mcphub"}, Platforms: []string{"windows/" + runtime.GOARCH}}
	if ok, reason := availabilityProbePasses(probe); !ok {
		t.Fatalf("native mcphub bridge probe rejected: %s", reason)
	}
	if binaryAvailable("em.exe") {
		t.Fatal("test PATH unexpectedly supplied em.exe; catalog probe must not require it")
	}
}

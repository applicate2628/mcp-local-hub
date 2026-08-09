package discovery

import (
	"errors"
	"os"
	"testing"

	"mcp-local-hub/internal/vcpkgmcp/evidence"
)

func TestR21RelativeEnvironmentRootFailsClosedBeforeFilesystemProbe(t *testing.T) {
	statCalls := 0
	deps := DefaultDeps()
	deps.Getenv = func(name string) string {
		if name == "VCPKG_ROOT" {
			return "relative-vcpkg"
		}
		return ""
	}
	deps.Stat = func(string) (os.FileInfo, error) {
		statCalls++
		return nil, errors.New("must not probe relative environment root")
	}
	result := DiscoverRoot("", deps)
	if result.Status != evidence.StatusUnknown || result.Reason != ReasonEnvRootRelative {
		t.Fatalf("result=%+v, want unknown/%s", result, ReasonEnvRootRelative)
	}
	if statCalls != 0 {
		t.Fatalf("relative environment root caused %d filesystem probes", statCalls)
	}
}

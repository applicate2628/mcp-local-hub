package discovery

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"mcp-local-hub/internal/vcpkgmcp/evidence"
)

func TestR35ExplicitRegularFileIsInvalidRoot(t *testing.T) {
	rootFile := testRoot("root-file")
	deps := baseDeps("windows", newFakeFS())
	statCalls := 0
	deps.Stat = func(path string) (os.FileInfo, error) {
		statCalls++
		if filepath.Clean(path) != filepath.Clean(rootFile) {
			t.Fatalf("explicit root file must be rejected before probing a child path; stat(%q)", path)
		}
		return fakeFileInfo{name: filepath.Base(rootFile), isDir: false}, nil
	}

	result := DiscoverRoot(rootFile, deps)
	if result.Status != evidence.StatusUnknown || result.Reason != ReasonExplicitRootInvalid {
		t.Fatalf("status/reason=%v/%v, want unknown/%v; result=%+v", result.Status, result.Reason, ReasonExplicitRootInvalid, result)
	}
	if statCalls != 1 {
		t.Fatalf("stat calls=%d, want only the explicit root type probe", statCalls)
	}
	if len(result.Candidates) != 1 || !strings.Contains(result.Candidates[0].Detail, "not a directory") {
		t.Fatalf("candidates=%+v, want explicit not-a-directory detail", result.Candidates)
	}
}

package archguard

import (
	"context"
	"fmt"
	"testing"
)

func TestExplicitAutomaticTagsMayBeSuppliedThroughTags(t *testing.T) {
	cases := []struct {
		name       string
		explicit   string
		otherPath  string
		otherBuild string
	}{
		{name: "goos", explicit: "windows", otherPath: "internal/dep/b_linux.go"},
		{name: "goarch", explicit: "arm64", otherPath: "internal/dep/b_amd64.go"},
		{name: "unix", explicit: "unix", otherPath: "internal/dep/b_windows.go"},
		{name: "compiler", explicit: "gccgo", otherPath: "internal/dep/b.go", otherBuild: "//go:build gc\n"},
		{name: "cgo", explicit: "cgo", otherPath: "internal/dep/b_js_wasm.go"},
		{name: "release", explicit: "go1.27", otherPath: "internal/dep/b.go", otherBuild: "//go:build !go1.26\n"},
		{name: "architecture feature", explicit: "amd64.v4", otherPath: "internal/dep/b_arm64.go"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			root := newFixtureRepo(t, map[string]string{
				"internal/dep/a.go": tcBuildSource(tc.explicit, "alpha"),
				tc.otherPath:         tc.otherBuild + "package beta\n",
			})
			policy := mustLoadPolicyForTest(t)
			policy.SourceRoots = []string{"internal"}
			if _, err := Scan(context.Background(), ScanOptions{Root: root, Policy: policy}); err == nil {
				t.Fatalf("explicit %s tag did not overlap a conflicting build selected through -tags", tc.explicit)
			}
		})
	}
}

func TestFilenamePlatformConstraintsRemainTargetOnly(t *testing.T) {
	root := newFixtureRepo(t, map[string]string{
		"internal/dep/a_windows.go": "package alpha\n",
		"internal/dep/b_linux.go":   "package beta\n",
	})
	policy := mustLoadPolicyForTest(t)
	policy.SourceRoots = []string{"internal"}
	if _, err := Scan(context.Background(), ScanOptions{Root: root, Policy: policy}); err != nil {
		t.Fatalf("filename-derived Windows and Linux constraints must remain disjoint: %v", err)
	}
}

func TestNegatedExplicitAutomaticTagHonorsTagsOverride(t *testing.T) {
	root := newFixtureRepo(t, map[string]string{
		"internal/dep/a.go":         "//go:build !windows\npackage alpha\n",
		"internal/dep/b_windows.go": "package beta\n",
	})
	policy := mustLoadPolicyForTest(t)
	policy.SourceRoots = []string{"internal"}
	if _, err := Scan(context.Background(), ScanOptions{Root: root, Policy: policy}); err != nil {
		t.Fatalf("!windows and a Windows filename must remain disjoint after the override model: %v", err)
	}
}

func tcBuildSource(tag, packageName string) string {
	return fmt.Sprintf("//go:build %s\npackage %s\n", tag, packageName)
}

package archguard

import (
	"context"
	"reflect"
	"strings"
	"testing"
)

func TestScanDoesNotMutateCallerPolicySlices(t *testing.T) {
	root := newFixtureRepo(t, map[string]string{"internal/x/x.go": "package x\n"})
	policy := mustLoadPolicyForTest(t)
	policy.SourceRoots = []string{" internal "}
	policy.ExcludeGlobs = []string{" ./vendor/** "}
	policy.APIConstructors[0].AllowedGlobs = []string{" ./internal/app/** "}

	before := clonePolicy(policy)
	if _, err := Scan(context.Background(), ScanOptions{Root: root, Policy: policy}); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(policy.SourceRoots, before.SourceRoots) ||
		!reflect.DeepEqual(policy.ExcludeGlobs, before.ExcludeGlobs) ||
		!reflect.DeepEqual(policy.APIConstructors, before.APIConstructors) {
		t.Fatalf("Scan mutated caller policy\nbefore=%#v\nafter=%#v", before, policy)
	}
}

func TestScanRejectsEmptyProgrammaticRoot(t *testing.T) {
	policy := mustLoadPolicyForTest(t)
	_, err := Scan(context.Background(), ScanOptions{Root: "  ", Policy: policy})
	if err == nil || !strings.Contains(err.Error(), "root") {
		t.Fatalf("error=%v, want empty root rejection", err)
	}
}

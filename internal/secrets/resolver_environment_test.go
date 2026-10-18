package secrets

import (
	"strings"
	"testing"
)

func TestResolverWithEnvironmentLookupPreservesReferenceAndOmission(t *testing.T) {
	values := map[string]string{"MCPHUB_CAPTURED_REFERENCE": "captured", "MCPHUB_CAPTURED_EMPTY": ""}
	lookup := func(name string) (string, bool) { value, present := values[name]; return value, present }
	r, err := NewResolver(nil, nil).WithEnvironmentLookup(lookup)
	if err != nil {
		t.Fatal(err)
	}
	actual, sources, err := r.ResolveMapWithProvenance(map[string]string{
		"direct":    "$MCPHUB_CAPTURED_REFERENCE",
		"composite": "prefix-${env:MCPHUB_CAPTURED_REFERENCE}",
		"empty":     "${env:MCPHUB_CAPTURED_EMPTY}",
	})
	if err != nil {
		t.Fatal(err)
	}
	if actual["direct"] != "captured" || actual["composite"] != "prefix-captured" || actual["empty"] != "" || !sources["empty"].Resolved() {
		t.Fatal("snapshot reference semantics were not retained")
	}
	resolved, omitted, err := r.ResolveMapBestEffort(map[string]string{"optional": "secret:SYNTHETIC_ABSENT", "required": "$MCPHUB_CAPTURED_REFERENCE"})
	if err != nil || resolved["required"] != "captured" || omitted["optional"] != "secret:SYNTHETIC_ABSENT" {
		t.Fatal("ordinary optional secret omission changed")
	}
	if _, _, err := r.ResolveMapBestEffort(map[string]string{"required": "$MCPHUB_CAPTURED_MISSING"}); err == nil {
		t.Fatal("missing required reference was accepted")
	}
	if _, err := r.Resolve("file:synthetic"); err == nil || !strings.Contains(err.Error(), "local config not available") {
		t.Fatal("nil local source became an empty map")
	}
	if _, err := r.WithEnvironmentLookup(nil); err == nil {
		t.Fatal("missing snapshot lookup was accepted")
	}
}

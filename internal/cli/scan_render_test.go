package cli

import (
	"bytes"
	"strings"
	"testing"

	"mcp-local-hub/internal/api"
)

// TestRenderScanGroups_PrintsViaHubInherited is the regression guard for bot
// finding #3 (PR #422): the pretty `mcphub scan` output groups by status and
// prints a FIXED bucket list. Before the fix the list omitted
// "via-hub-inherited", so an inherited (import / below-write-target) hub-routed
// row — which the JSON output still carried — was silently dropped from the
// human-readable scan. This asserts the read-only inherited bucket IS printed.
func TestRenderScanGroups_PrintsViaHubInherited(t *testing.T) {
	result := &api.ScanResult{
		Entries: []api.ScanEntry{
			{Name: "memory", Status: "via-hub"},
			{Name: "time", Status: "via-hub-inherited"},
			{Name: "sequential-thinking", Status: "can-migrate"},
		},
	}
	var buf bytes.Buffer
	renderScanGroups(&buf, result, false)
	out := buf.String()

	if !strings.Contains(out, "via-hub-inherited (1):") {
		t.Errorf("pretty scan output must print the via-hub-inherited section header; got:\n%s", out)
	}
	if !strings.Contains(out, "time") {
		t.Errorf("the inherited `time` row must appear in the pretty scan output; got:\n%s", out)
	}
	// The owned via-hub bucket must remain present and DISTINCT (not collapsed
	// into the inherited bucket) — both headers print.
	if !strings.Contains(out, "via-hub (1):") {
		t.Errorf("pretty scan output must still print the owned via-hub section; got:\n%s", out)
	}
}

// TestScanStatusBuckets_CoverViaHubInherited pins the bucket list itself so a
// future edit that drops "via-hub-inherited" (re-introducing the finding #3
// drop) fails loudly at the list level, independent of the render path.
func TestScanStatusBuckets_CoverViaHubInherited(t *testing.T) {
	found := false
	for _, s := range scanStatusBuckets {
		if s == "via-hub-inherited" {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("scanStatusBuckets must contain via-hub-inherited; got %v", scanStatusBuckets)
	}
}

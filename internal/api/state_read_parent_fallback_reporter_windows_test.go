//go:build windows

package api

import (
	"os"
	"path/filepath"
	"testing"
)

// TestReadStateFileInodeAnchoredWithAuditSink_CustomSinkRetainsPerCallWarnings
// protects the route-owned diagnostic boundary: only the default hub-mcp.log
// sink is aggregated; caller-owned sinks still observe every fallback.
func TestReadStateFileInodeAnchoredWithAuditSink_CustomSinkRetainsPerCallWarnings(t *testing.T) {
	isolateStateDir(t)
	resetStrictModeIntentCacheForTest()
	t.Cleanup(resetStrictModeIntentCacheForTest)
	t.Setenv(RequireSingleUserHomeEnv, "")

	parent := filepath.Join(t.TempDir(), "leaky-parent")
	if err := os.Mkdir(parent, 0o700); err != nil {
		t.Fatalf("mkdir parent: %v", err)
	}
	synthesizeDirWithAuthUsersReadACE(t, parent)
	target := filepath.Join(parent, "supervisor-intent.json")
	if err := os.WriteFile(target, []byte(`{"strict_mode":false}`), 0o600); err != nil {
		t.Fatalf("write target: %v", err)
	}
	applyAllowlistOnlyDACL(t, target)

	var parentFallbacks int
	sink := func(_ string, event string, _ map[string]any) error {
		if event == stateReadUnhardenedParentFallbackEvent {
			parentFallbacks++
		}
		return nil
	}
	for range 2 {
		if _, err := ReadStateFileInodeAnchoredWithAuditSink(target, sink); err != nil {
			t.Fatalf("custom-sink read: %v", err)
		}
	}
	if parentFallbacks != 2 {
		t.Fatalf("custom sink parent fallback calls = %d, want 2 (no aggregation outside default hub-mcp.log sink)", parentFallbacks)
	}
}

// TestReadStateFileInodeAnchored_DefaultSinkAggregatesAndSettles exercises the
// actual default reader seam: repeated relaxed parent reads write one warning,
// and a subsequently tight parent emits the final settled summary.
func TestReadStateFileInodeAnchored_DefaultSinkAggregatesAndSettles(t *testing.T) {
	isolateStateDir(t)
	resetStrictModeIntentCacheForTest()
	t.Cleanup(resetStrictModeIntentCacheForTest)
	t.Setenv(RequireSingleUserHomeEnv, "")

	parent := filepath.Join(t.TempDir(), "leaky-parent")
	if err := os.Mkdir(parent, 0o700); err != nil {
		t.Fatalf("mkdir parent: %v", err)
	}
	synthesizeDirWithAuthUsersReadACE(t, parent)
	target := filepath.Join(parent, "supervisor-intent.json")
	if err := os.WriteFile(target, []byte(`{"strict_mode":false}`), 0o600); err != nil {
		t.Fatalf("write target: %v", err)
	}
	applyAllowlistOnlyDACL(t, target)

	for range 2 {
		if _, err := ReadStateFileInodeAnchored(target); err != nil {
			t.Fatalf("default-sink relaxed read: %v", err)
		}
	}
	if rows := stateReadParentFallbackRows(t, target); len(rows) != 1 {
		t.Fatalf("default-sink rows after repeat = %d, want 1", len(rows))
	}

	applyAllowlistOnlyDACL(t, parent)
	if _, err := ReadStateFileInodeAnchored(target); err != nil {
		t.Fatalf("default-sink clean read: %v", err)
	}
	rows := stateReadParentFallbackRows(t, target)
	if len(rows) != 2 {
		t.Fatalf("default-sink rows after settle = %d, want 2", len(rows))
	}
	if aggregation, _ := rows[1][stateReadFallbackAggregationField].(string); aggregation != stateReadFallbackAggregationSettled {
		t.Fatalf("final aggregation = %q, want settled", aggregation)
	}
}

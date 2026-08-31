//go:build !windows

package api

import (
	"os"
	"path/filepath"
	"testing"
)

// TestReadStateFileInodeAnchored_DefaultSinkDualBroadeningDoesNotChurn proves
// that POSIX 0777's simultaneous write and read/exec parent findings are one
// observation batch, not a sequence of reason transitions.
func TestReadStateFileInodeAnchored_DefaultSinkDualBroadeningDoesNotChurn(t *testing.T) {
	isolateStateDir(t)
	resetStrictModeIntentCacheForTest()
	t.Cleanup(resetStrictModeIntentCacheForTest)
	t.Setenv(RequireSingleUserHomeEnv, "")

	parent := filepath.Join(t.TempDir(), "leaky-parent")
	if err := os.Mkdir(parent, 0o700); err != nil {
		t.Fatalf("mkdir parent: %v", err)
	}
	if err := os.Chmod(parent, 0o777); err != nil {
		t.Fatalf("chmod parent 0777: %v", err)
	}
	target := filepath.Join(parent, "supervisor-intent.json")
	if err := os.WriteFile(target, []byte(`{"strict_mode":false}`), 0o600); err != nil {
		t.Fatalf("write target: %v", err)
	}
	if err := os.Chmod(target, 0o600); err != nil {
		t.Fatalf("chmod target 0600: %v", err)
	}

	if _, err := ReadStateFileInodeAnchored(target); err != nil {
		t.Fatalf("first dual-broadened read: %v", err)
	}
	if rows := stateReadParentFallbackRows(t, target); len(rows) != 2 {
		t.Fatalf("first read rows = %d, want write + read/exec reasons", len(rows))
	}
	if _, err := ReadStateFileInodeAnchored(target); err != nil {
		t.Fatalf("repeat dual-broadened read: %v", err)
	}
	if rows := stateReadParentFallbackRows(t, target); len(rows) != 2 {
		t.Fatalf("repeat read rows = %d, want no transition churn", len(rows))
	}

	if err := os.Chmod(parent, 0o700); err != nil {
		t.Fatalf("chmod parent clean: %v", err)
	}
	if _, err := ReadStateFileInodeAnchored(target); err != nil {
		t.Fatalf("clean read: %v", err)
	}
	rows := stateReadParentFallbackRows(t, target)
	if len(rows) != 4 {
		t.Fatalf("clean read rows = %d, want two settled finals", len(rows))
	}
	for _, row := range rows[2:] {
		if aggregation, _ := row[stateReadFallbackAggregationField].(string); aggregation != stateReadFallbackAggregationSettled {
			t.Fatalf("clean aggregation = %q, want settled", aggregation)
		}
	}
}

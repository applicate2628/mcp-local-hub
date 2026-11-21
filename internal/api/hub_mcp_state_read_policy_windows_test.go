//go:build windows

package api

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestReadStateFileInodeAnchored_PersistedStrictPolicyDiagnostics(t *testing.T) {
	statePathsHelper(t)
	t.Setenv(RequireSingleUserHomeEnv, "")
	stateDir := hardenedTempDir(t)
	daemonStateRootOverride = stateDir
	resetStrictModeIntentCacheForTest()
	t.Cleanup(resetStrictModeIntentCacheForTest)

	intentPath := filepath.Join(stateDir, supervisorIntentFileLeaf)
	intent := []byte(`{"strict_mode":true}`)
	if err := os.WriteFile(intentPath, intent, 0600); err != nil {
		t.Fatalf("write strict intent: %v", err)
	}
	applyAllowlistOnlyDACL(t, intentPath)
	raw, err := readStateFileInodeAnchoredEnvStrictOnlyNoAudit(intentPath)
	if err != nil || string(raw) != string(intent) {
		t.Fatalf("read persisted strict intent = %q, err = %v", raw, err)
	}
	if !operatorRequiresSingleUserHome() {
		t.Fatal("persisted strict intent must enable the strict policy with an empty environment setting")
	}

	for _, gate := range []string{"parent", "file"} {
		t.Run(gate, func(t *testing.T) {
			dir := hardenedTempDir(t)
			target := filepath.Join(dir, "strict-policy.json")
			if err := os.WriteFile(target, []byte("{}"), 0600); err != nil {
				t.Fatal(err)
			}
			applyAllowlistOnlyDACL(t, target)
			gatePath := target
			if gate == "parent" {
				synthesizeDirWithAuthUsersReadACE(t, dir)
				gatePath = dir
			} else {
				applyFileDACLWithAuthUsersReadACE(t, target)
			}

			raw, err := readStateFileInodeAnchored(target)
			if !errors.Is(err, ErrDaclOutsideAllowlist) {
				t.Fatalf("%s gate error = %v, want ErrDaclOutsideAllowlist", gate, err)
			}
			if raw != nil {
				t.Fatalf("%s gate returned bytes after rejecting the DACL", gate)
			}
			msg := err.Error()
			if !strings.Contains(msg, gate+" "+gatePath) {
				t.Errorf("diagnostic %q must identify the rejected %s %q", msg, gate, gatePath)
			}
			if !strings.Contains(msg, "effective strict policy is active") {
				t.Errorf("diagnostic %q must describe the effective policy", msg)
			}
			if strings.Contains(msg, RequireSingleUserHomeEnv+"=1 is set") || strings.Contains(msg, "unset that env var") {
				t.Errorf("diagnostic %q incorrectly attributes persisted strict policy to the empty environment setting", msg)
			}
		})
	}
}

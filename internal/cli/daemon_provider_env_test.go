package cli

import (
	"testing"

	"mcp-local-hub/internal/api/apitest"
	"mcp-local-hub/internal/secrets"
)

func TestDaemonEnvWithOverlayForwardsLocalNamesAtLaunchTime(t *testing.T) {
	stateDir := apitest.HardenedTempDir(t)
	t.Setenv("MCPHUB_STATE_DIR_OVERRIDE", stateDir)
	seedDaemonOverlay(t, stateDir, map[string]string{"FORWARD_OVERLAY": "overlay-wins"})
	t.Setenv("FORWARD_PRESENT", "first")
	t.Setenv("FORWARD_OVERLAY", "inherited-must-lose")

	forwarded := []string{"FORWARD_PRESENT", "FORWARD_ABSENT", "FORWARD_OVERLAY"}
	first, firstUnset, err := daemonEnvWithOverlay("memory", "default", map[string]string{"STATIC": "literal"}, secrets.NewResolver(nil, nil), forwarded)
	if err != nil {
		t.Fatal(err)
	}
	if first["FORWARD_PRESENT"] != "first" || first["FORWARD_OVERLAY"] != "overlay-wins" || first["STATIC"] != "literal" {
		t.Fatalf("first launch env=%v", first)
	}
	if !containsDaemonEnvName(firstUnset, "FORWARD_ABSENT") {
		t.Fatalf("first unset=%v, want FORWARD_ABSENT", firstUnset)
	}

	t.Setenv("FORWARD_PRESENT", "second")
	t.Setenv("FORWARD_ABSENT", "now-present")
	second, secondUnset, err := daemonEnvWithOverlay("memory", "default", map[string]string{"STATIC": "literal"}, secrets.NewResolver(nil, nil), forwarded)
	if err != nil {
		t.Fatal(err)
	}
	if second["FORWARD_PRESENT"] != "second" || second["FORWARD_ABSENT"] != "now-present" || second["FORWARD_OVERLAY"] != "overlay-wins" {
		t.Fatalf("second launch env=%v", second)
	}
	if containsDaemonEnvName(secondUnset, "FORWARD_ABSENT") {
		t.Fatalf("second unset=%v must not contain a now-present forwarded key", secondUnset)
	}
}

func containsDaemonEnvName(names []string, want string) bool {
	for _, name := range names {
		if name == want {
			return true
		}
	}
	return false
}

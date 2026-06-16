package unsafegate

import (
	"bytes"
	"strings"
	"testing"
)

func TestEnabled_ExactOneOnly(t *testing.T) {
	const env = "MCP_LOCAL_HUB_TEST_UNSAFE_GATE"
	for _, tc := range []struct {
		val  string
		want bool
	}{
		{"", false},
		{"0", false},
		{"true", false},
		{"yes", false},
		{" 1", false},
		{"1", true},
	} {
		t.Setenv(env, tc.val)
		if got := Enabled(env); got != tc.want {
			t.Errorf("Enabled(%q=%q) = %v, want %v", env, tc.val, got, tc.want)
		}
	}
}

func TestRegisterAllowed_LogsWhenDisabled(t *testing.T) {
	const env = "MCP_LOCAL_HUB_TEST_UNSAFE_GATE"

	t.Setenv(env, "1")
	var buf bytes.Buffer
	if !registerAllowed(&buf, env, "toolx") {
		t.Fatal("registerAllowed should return true when opted in")
	}
	if buf.Len() != 0 {
		t.Errorf("no diagnostic expected when enabled, got %q", buf.String())
	}

	t.Setenv(env, "0")
	buf.Reset()
	if registerAllowed(&buf, env, "toolx") {
		t.Fatal("registerAllowed should return false when disabled")
	}
	msg := buf.String()
	if !strings.Contains(msg, "toolx") || !strings.Contains(msg, env) || !strings.Contains(msg, "NOT registered") {
		t.Errorf("diagnostic must name the tool, the env var, and 'NOT registered': %q", msg)
	}
}

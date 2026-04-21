package perftools

import (
	"testing"
)

// TestHyperfineEnabled_RequiresExactOptIn guards the security contract
// of MCP_LOCAL_HUB_ENABLE_UNSAFE_HYPERFINE. Only the literal "1" unlocks
// the hyperfine tool — any typo, truthy-looking string, or unset state
// keeps the RCE-grade shell-exec surface closed. If this assertion ever
// loosens (e.g., `strconv.ParseBool`-style "true"/"yes" acceptance), a
// typo in a deployment .env silently exposes arbitrary command exec to
// every MCP client — this test is the backstop.
func TestHyperfineEnabled_RequiresExactOptIn(t *testing.T) {
	cases := []struct {
		name   string
		value  string
		unset  bool
		expect bool
	}{
		{"unset", "", true, false},
		{"empty-explicit", "", false, false},
		{"literal-one", "1", false, true},
		{"literal-zero", "0", false, false},
		{"truthy-true", "true", false, false},
		{"truthy-yes", "yes", false, false},
		{"typo-whitespace", "1 ", false, false},
		{"typo-newline", "1\n", false, false},
		{"quoted", "\"1\"", false, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if tc.unset {
				t.Setenv(hyperfineOptInEnv, "")
				_ = tc.value // keep the field declared for readability
			} else {
				t.Setenv(hyperfineOptInEnv, tc.value)
			}
			got := hyperfineEnabled()
			if got != tc.expect {
				t.Errorf("value=%q: hyperfineEnabled()=%v, want %v", tc.value, got, tc.expect)
			}
		})
	}
}

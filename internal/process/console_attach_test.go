package process

import (
	"os"
	"os/exec"
	"strings"
	"testing"
)

// TestConsoleAttachSuppressed covers the composition-root read.
func TestConsoleAttachSuppressed(t *testing.T) {
	tests := []struct {
		name string
		val  string
		set  bool
		want bool
	}{
		{"unset: attach normally", "", false, false},
		{"empty: attach normally", "", true, false},
		{"0: attach normally", "0", true, false},
		{"unrelated value: attach normally", "maybe", true, false},

		{"1 suppresses", "1", true, true},
		{"true suppresses", "true", true, true},
		{"TRUE suppresses (case-insensitive)", "TRUE", true, true},
		{"padded suppresses (trimmed)", " 1\t", true, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if tc.set {
				t.Setenv(SuppressConsoleAttachEnv, tc.val)
			} else {
				t.Setenv(SuppressConsoleAttachEnv, "")
				os.Unsetenv(SuppressConsoleAttachEnv)
			}
			if got := ConsoleAttachSuppressed(); got != tc.want {
				t.Errorf("ConsoleAttachSuppressed() with %s=%q (set=%v) = %v, want %v",
					SuppressConsoleAttachEnv, tc.val, tc.set, got, tc.want)
			}
		})
	}
}

// TestSuppressConsoleAttachMarksTheChildEnvironment asserts the marker
// actually reaches the child's environment, from BOTH starting states.
//
// The nil-Env case is the one that matters in production and the one an
// obvious implementation gets wrong: exec.Cmd treats a nil Env as
// "inherit the parent's", so a naive `cmd.Env = append(cmd.Env, marker)`
// would produce a single-entry environment and strip everything the
// supervisor needs (state-dir overrides, PATH, strict-mode posture).
func TestSuppressConsoleAttachMarksTheChildEnvironment(t *testing.T) {
	t.Setenv("MCPHUB_CONSOLE_ATTACH_TEST_CANARY", "present")

	t.Run("nil Env inherits the parent environment and adds the marker", func(t *testing.T) {
		cmd := exec.Command("does-not-need-to-exist")
		SuppressConsoleAttach(cmd)

		if !hasEnv(cmd.Env, SuppressConsoleAttachEnv+"=1") {
			t.Fatalf("marker missing from composed env: %v", tail(cmd.Env))
		}
		if !hasEnv(cmd.Env, "MCPHUB_CONSOLE_ATTACH_TEST_CANARY=present") {
			t.Fatal("composed env dropped the inherited parent environment; a supervisor " +
				"spawned this way would lose PATH, state-dir overrides and strict-mode posture")
		}
	})

	t.Run("explicit Env is preserved and extended", func(t *testing.T) {
		cmd := exec.Command("does-not-need-to-exist")
		cmd.Env = []string{"EXPLICIT=kept"}
		SuppressConsoleAttach(cmd)

		if !hasEnv(cmd.Env, "EXPLICIT=kept") {
			t.Fatalf("explicit env entry was dropped: %v", cmd.Env)
		}
		if !hasEnv(cmd.Env, SuppressConsoleAttachEnv+"=1") {
			t.Fatalf("marker missing from composed env: %v", cmd.Env)
		}
		if hasEnv(cmd.Env, "MCPHUB_CONSOLE_ATTACH_TEST_CANARY=present") {
			t.Fatal("an explicit Env must not be silently widened with the parent environment")
		}
	})
}

func hasEnv(env []string, want string) bool {
	for _, e := range env {
		if strings.EqualFold(e, want) {
			return true
		}
	}
	return false
}

func tail(env []string) []string {
	if len(env) > 4 {
		return env[len(env)-4:]
	}
	return env
}

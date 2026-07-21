package daemon

import (
	"strings"
	"testing"

	"mcp-local-hub/internal/process"
)

// TestComposeChildEnvPropagatesConsoleAttachSuppression pins an INTENDED
// inheritance that would otherwise look accidental.
//
// The supervisor is spawned with MCPHUB_NO_CONSOLE_ATTACH=1
// (process.SuppressConsoleAttach), and every daemon child's environment is
// composed from that supervisor's os.Environ(). So the marker reaches every
// managed daemon and, through them, every third-party MCP server in the
// fleet.
//
// That is CORRECT, not a leak: a managed daemon IS a detached background
// child, and there is no case where one should attach a console. For
// third-party children (uvx / npx / python / node) the variable is simply an
// unrecognized string and is inert.
//
// The pin exists because "correct" and "accidental" look identical in a
// diff. Someone tidying the child environment could add this key to a strip
// list for cleanliness and would silently re-open the class for every daemon
// the supervisor spawns — with nothing else in the tree objecting. This test
// objects.
func TestComposeChildEnvPropagatesConsoleAttachSuppression(t *testing.T) {
	t.Setenv(process.SuppressConsoleAttachEnv, "1")
	marker := process.SuppressConsoleAttachEnv + "="

	hasMarker := func(env []string) bool {
		for _, kv := range env {
			if strings.HasPrefix(strings.ToUpper(kv), strings.ToUpper(marker)) {
				return true
			}
		}
		return false
	}

	t.Run("inherited when the daemon adds its own env", func(t *testing.T) {
		got := composeChildEnv(map[string]string{"SOME_DAEMON_VAR": "x"}, nil)
		if !hasMarker(got) {
			t.Fatalf("composed daemon-child env dropped %s; every daemon the supervisor "+
				"spawns is a detached background child and must inherit the "+
				"console-attach suppression", process.SuppressConsoleAttachEnv)
		}
	})

	t.Run("inherited when the daemon unsets unrelated keys", func(t *testing.T) {
		got := composeChildEnv(nil, []string{"SOME_UNRELATED_VAR"})
		if !hasMarker(got) {
			t.Fatalf("composed daemon-child env dropped %s while unsetting an unrelated key",
				process.SuppressConsoleAttachEnv)
		}
	})

	// The remaining branch needs no assertion here and is recorded so the
	// coverage claim stays honest: when a daemon declares neither Env nor
	// UnsetEnv, the hosts leave cmd.Env nil and exec inherits the parent
	// environment wholesale, so the marker propagates by definition.
}

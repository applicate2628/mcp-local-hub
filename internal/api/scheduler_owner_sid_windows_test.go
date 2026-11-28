//go:build windows

package api

import (
	"os/user"
	"strings"
	"testing"

	"mcp-local-hub/internal/scheduler"
)

// This reaches the actual scheduler-backed port decision with private empty
// state and mock PID/image/task observations. No socket, process query, or native
// Scheduler operation is performed; current-user identity is read-only.
func TestSchedulerPortOwnerRequiresCurrentSID(t *testing.T) {
	current, err := user.Current()
	if err != nil || current == nil || current.Uid == "" || current.Username == "" {
		t.Fatal("current Windows user identity unavailable")
	}
	short := current.Username
	if i := strings.LastIndex(short, `\`); i >= 0 {
		short = short[i+1:]
	}
	if short == "" {
		t.Fatal("current short-name fixture unavailable")
	}
	previousRoot := daemonStateRootOverride
	previousLookup, previousIdentity, previousStatus := lookupProcess, processIdentityByPID, schedulerStatusForOwnPort
	t.Cleanup(func() {
		daemonStateRootOverride = previousRoot
		lookupProcess, processIdentityByPID, schedulerStatusForOwnPort = previousLookup, previousIdentity, previousStatus
	})
	daemonStateRootOverride = t.TempDir()
	lookupProcess = func(int) (int, uint64, int64, bool) { return 12345, 0, 0, true }
	processIdentityByPID = func(int) (string, string, bool) { return "mcphub.exe", "fixture-parent.exe", true }
	for _, tc := range []struct {
		name, owner string
		want        bool
	}{
		{"current-SID", current.Uid, true},
		{"unresolved-short-name", short, false},
		{"same-short-name-other-domain", `UNRELATED_FIXTURE_DOMAIN\` + short, false},
		{"unresolved-qualified-name", current.Username, false},
		{"missing-owner", "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			schedulerStatusForOwnPort = func(name string) (scheduler.TaskStatus, error) {
				calls++
				if name != `\mcp-local-hub-sid-fixture-default` {
					t.Fatalf("unexpected task query %q", name)
				}
				return scheduler.TaskStatus{Name: name, State: "Running", Owner: tc.owner}, nil
			}
			got := portHeldByOurDaemon(43123, "sid-fixture", "default")
			if calls != 1 || got != tc.want {
				t.Fatalf("scheduler owner decision=%t want=%t; task queries=%d", got, tc.want, calls)
			}
		})
	}
}

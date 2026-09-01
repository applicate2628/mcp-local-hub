package cli

import (
	"context"
	"errors"
	"testing"
	"time"

	"mcp-local-hub/internal/api"
)

// TestControlCompatibilityGateHoldsStopAndRestartBeforeTheirMutationPath uses
// a held channel at the command boundary. While compatibility is unresolved,
// neither command may reach its API control path (where StopAll records intent
// and RestartAll can issue a respawn). Releasing the hold returns a sentinel so
// the test never invokes a real scheduler or process operation.
func TestControlCompatibilityGateHoldsStopAndRestartBeforeTheirMutationPath(t *testing.T) {
	for _, tc := range []struct {
		name string
		new  func() commandForSupervisorCompatTest
	}{
		{name: "stop all", new: func() commandForSupervisorCompatTest { return newStopCmdReal() }},
		{name: "restart all", new: func() commandForSupervisorCompatTest { return newRestartCmdReal() }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			entered := make(chan struct{})
			release := make(chan struct{})
			gateErr := errors.New("compatibility probe deliberately held")
			restore := api.RegisterSupervisorControlAdmission(func(context.Context) error {
				close(entered)
				<-release
				return gateErr
			})
			defer restore()

			cmd := tc.new()
			cmd.SetArgs([]string{"--all"})
			done := make(chan error, 1)
			go func() { done <- cmd.Execute() }()
			select {
			case <-entered:
			case <-time.After(time.Second):
				t.Fatal("compatibility gate was not reached")
			}
			select {
			case err := <-done:
				t.Fatalf("command completed before compatibility gate release: %v", err)
			default:
			}
			close(release)
			if err := <-done; !errors.Is(err, gateErr) {
				t.Fatalf("command error=%v, want held compatibility error", err)
			}
		})
	}
}

type commandForSupervisorCompatTest interface {
	SetArgs([]string)
	Execute() error
}

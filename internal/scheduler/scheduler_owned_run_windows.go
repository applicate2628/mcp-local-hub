//go:build windows

package scheduler

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
)

// OwnedRunner is an optional native capability. It never falls back to Run.
// Recovery callers retain their existing cooperative transaction lease.
type OwnedRunner interface {
	RunOwned(context.Context, OwnedRunRequest) error
}

type OwnedRunRequest struct {
	Name            string
	ExpectedCommand string
	ExpectedRole    string
	ExpectedOwner   string
}

func (s *windowsScheduler) RunOwned(ctx context.Context, request OwnedRunRequest) error {
	if ctx == nil {
		return fmt.Errorf("%w: guarded run requires context", ErrUnavailable)
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if request.Name == "" || !strings.HasPrefix(request.Name, `\`) || strings.Contains(request.Name[1:], `\`) ||
		request.ExpectedOwner == "" || request.ExpectedRole != "gui" || !filepath.IsAbs(request.ExpectedCommand) || filepath.Clean(request.ExpectedCommand) != request.ExpectedCommand {
		return fmt.Errorf("%w: incomplete guarded run expectation", ErrTaskCorrupt)
	}
	response, err := schedulerCOM(ctx, schedulerCOMRequest{Operation: "run_owned", Name: request.Name, ExpectedCommand: request.ExpectedCommand, ExpectedRole: request.ExpectedRole, ExpectedOwner: request.ExpectedOwner})
	if err != nil {
		return err
	}
	if !response.Started {
		return fmt.Errorf("%w: guarded run did not confirm start", ErrUnavailable)
	}
	return nil
}

package cli

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"mcp-local-hub/internal/api"
	"mcp-local-hub/internal/gui"
)

// guiQuitOperation is the one lifetime operation shared by tray and HTTP/CLI.
// It owns no additional process/store. Its first accepted result remains bound
// to the original lifecycle; duplicate requests cannot escalate stop-all.
type guiQuitOperation struct {
	mu          sync.Mutex
	lifecycle   *gui.GUIOwnerLifecycle
	stop        context.CancelFunc
	stopDaemons func() error
	emit        func(gui.GUIExitReason, map[string]any) error
	persist     func(context.Context, string, string) (api.GUIOwnerControlResult, error)
	result      gui.GUIQuitResult
	err         error
}

func (operation *guiQuitOperation) requestGUIQuit(ctx context.Context, request gui.GUIQuitRequest, cause gui.GUIExitReason) (gui.GUIQuitResult, error) {
	operation.mu.Lock()
	defer operation.mu.Unlock()
	if operation.lifecycle == nil || request.ExpectedGeneration == "" || request.ExpectedGeneration != operation.lifecycle.Generation() {
		return gui.GUIQuitResult{}, gui.ErrGUIQuitGeneration
	}
	if operation.result.Accepted {
		return operation.result, operation.err
	}
	if ctx == nil {
		return gui.GUIQuitResult{}, errors.New("GUI_QUIT_CONTEXT_REQUIRED")
	}
	if err := ctx.Err(); err != nil {
		return gui.GUIQuitResult{}, err
	}
	if operation.stop == nil || operation.persist == nil || operation.emit == nil || operation.stopDaemons == nil {
		return gui.GUIQuitResult{}, errors.New("GUI_QUIT_OWNER_UNAVAILABLE")
	}
	var result gui.GUIQuitResult
	err := operation.lifecycle.WithActiveGeneration(request.ExpectedGeneration, func(record gui.GUIOwnerRecord) error {
		policy, policyErr := operation.persist(ctx, record.Generation, string(cause))
		if !policy.Applied {
			if policyErr == nil {
				policyErr = errors.New("GUI_QUIT_POLICY_NOT_RECEIVED")
			}
			return policyErr
		}
		result = gui.GUIQuitResult{Accepted: true, Generation: record.Generation, StopDaemons: policy.Control.GUIStopIncludesDaemons()}
		// A post-readback release/event/daemon failure retains accepted intent.
		// Required original shutdown still runs, with every acquired cause kept.
		eventErr := operation.emit(cause, map[string]any{"generation": record.Generation, "stop_daemons": result.StopDaemons})
		if eventErr != nil {
			eventErr = fmt.Errorf("GUI_QUIT_EVENT: %w", eventErr)
		}
		var daemonErr error
		if result.StopDaemons {
			daemonErr = operation.stopDaemons()
			if daemonErr != nil {
				daemonErr = fmt.Errorf("GUI_QUIT_DAEMONS_PARTIAL: %w", daemonErr)
			}
		}
		operation.stop()
		return errors.Join(policyErr, eventErr, daemonErr)
	})
	if result.Accepted {
		operation.result, operation.err = result, err
	}
	return result, err
}

func (operation *guiQuitOperation) request(ctx context.Context, request gui.GUIQuitRequest) (gui.GUIQuitResult, error) {
	cause := gui.GUIExitReason("cli-quit")
	if request.StopDaemons {
		cause = gui.GUIExitReason("cli-quit-and-stop-all")
	}
	return operation.requestGUIQuit(ctx, request, cause)
}

func stopGUIRequestedDaemons() error {
	results, err := api.NewAPI().StopAll()
	for _, result := range results {
		if result.Err != "" {
			err = errors.Join(err, fmt.Errorf("stop daemon %s: %s", result.TaskName, result.Err))
		}
	}
	return err
}

func (operation *guiQuitOperation) outcomeError() error {
	operation.mu.Lock()
	defer operation.mu.Unlock()
	return operation.err
}

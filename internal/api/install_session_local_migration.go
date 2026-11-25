package api

import (
	"context"
	"errors"
	"path/filepath"
	"reflect"
	"time"
)

// The WL-04 retained connection is the sole process-stop authority. Capture
// precedes the first intent mutation and its flock. A removed row is never
// rediscovered by PID, port, task name or a fresh connection.
func (a *API) retireSessionLocalLegacy(ctx context.Context, plan *Plan) (resultErr error) {
	if err := ctx.Err(); err != nil {
		return err
	}
	if plan == nil || plan.sessionLocal == nil {
		return ErrSessionLocalInstallUnavailable
	}
	target := plan.sessionLocal
	if len(target.Legacy) == 0 || target.legacyRetired {
		return nil
	}
	root, err := DaemonStateDirReadOnly()
	if err != nil {
		return err
	}
	if err := VerifySupervisorControlGenerationFromStateDir(ctx, root, target.Supervisor); err != nil {
		return err
	}
	captures := captureRemovedSupervisorTargets(ctx, target.Legacy)
	defer func() {
		resultErr = errors.Join(resultErr, captures.close())
		if resultErr != nil && target.legacyRemovalCommitted {
			resultErr = &InstallCommittedUnverifiedError{cause: resultErr}
		}
	}()
	if err := VerifySupervisorControlGenerationFromStateDir(ctx, root, target.Supervisor); err != nil {
		return err
	}
	return retireCapturedSessionLocalLegacy(ctx, filepath.Join(root, supervisorIntentFileLeaf), target, captures)
}

// This transaction consumes only previously captured original owners. The
// exact descriptor fingerprint is checked under the intent lock. Its existing
// uninstalled stop record is a durable refusal fence, not a native receipt.
// Ordinary clear preserves that record in LegacyStopWatermarks; both forms
// deny a fresh private apply. Only this original capture can clear its exact
// fence after full receiving. A lost Complete therefore survives a retry and
// process restart as refusal, without minting replacement cleanup authority.
func retireCapturedSessionLocalLegacy(ctx context.Context, path string, target *sessionLocalInstallTarget, captures supervisorRemovedCaptures) error {
	for _, descriptor := range target.Legacy {
		if err := captures.validate(descriptor); err != nil {
			return err
		}
	}
	stamp := DaemonIntent{Desired: IntentDesiredStopped, Reason: IntentReasonUninstalled, UpdatedAt: time.Now().UTC()}
	removed, err := MutateSupervisorIntentIfChangedReturning(path, func(current *SupervisorIntentFile) (bool, error) {
		if ctx.Err() != nil {
			return false, ctx.Err()
		}
		route := findBuiltinRouteDescriptor(current)
		if route == nil || !reflect.DeepEqual(*route, target.Route) {
			return false, ErrSessionLocalInstallUnavailable
		}
		selected, err := sessionLocalLegacySelection(current, target.Providers)
		if err != nil || !reflect.DeepEqual(selected, target.Legacy) {
			return false, errors.Join(ErrSessionLocalInstallUnavailable, err)
		}
		for _, descriptor := range selected {
			if err := captures.validate(descriptor); err != nil {
				return false, err
			}
		}
		selectedTasks := make(map[string]bool, len(selected))
		if current.Stops == nil {
			current.Stops = make(map[string]DaemonIntent)
		}
		for _, descriptor := range selected {
			task := canonicalIntentTaskKey(descriptor.TaskName)
			selectedTasks[task] = true
			current.Stops[task] = stamp
			delete(current.LegacyStopWatermarks, task)
		}
		kept := current.Daemons[:0]
		for _, descriptor := range current.Daemons {
			if !selectedTasks[canonicalIntentTaskKey(descriptor.TaskName)] {
				kept = append(kept, descriptor)
			}
		}
		current.Daemons = kept
		return true, nil
	})
	target.legacyRemovalCommitted = removed.Changed
	if err != nil {
		return err
	}
	// MutateSupervisorIntentIfChangedReturning reports success only after the
	// original ledgered flock release has been received.
	nudge := nudgeSupervisorReconcileAfterRemovedTargets(ctx)
	if nudge.status != supervisorReconcileNudgeSucceeded {
		return errors.Join(ErrSessionLocalInstallUnavailable, nudge.err)
	}
	for _, descriptor := range target.Legacy {
		if err := captures.complete(ctx, descriptor); err != nil {
			return err
		}
	}
	_, err = MutateSupervisorIntentIfChangedReturning(path, func(current *SupervisorIntentFile) (bool, error) {
		if ctx.Err() != nil {
			return false, ctx.Err()
		}
		route := findBuiltinRouteDescriptor(current)
		if route == nil || !reflect.DeepEqual(*route, target.Route) {
			return false, ErrSessionLocalInstallUnavailable
		}
		for _, descriptor := range target.Legacy {
			task := canonicalIntentTaskKey(descriptor.TaskName)
			if current.FindSupervisorDaemonByTaskName(task) != nil || current.Stops[task] != stamp {
				return false, ErrSessionLocalInstallUnavailable
			}
			if _, altered := current.LegacyStopWatermarks[task]; altered {
				return false, ErrSessionLocalInstallUnavailable
			}
		}
		for _, descriptor := range target.Legacy {
			delete(current.Stops, canonicalIntentTaskKey(descriptor.TaskName))
		}
		return true, nil
	})
	if err != nil {
		return err
	}
	target.legacyRetired = true
	return nil
}

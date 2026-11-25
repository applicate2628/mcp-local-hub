package api

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"time"
)

type supervisorRemovedStop interface {
	Complete(context.Context) (ForceStopResultV1, error)
	Close() error
}

var prepareSupervisorRemovedStopFn = prepareSupervisorRemovedStop

func prepareSupervisorRemovedStop(ctx context.Context, task string, port int) (supervisorRemovedStop, error) {
	return PrepareSupervisorIPCRemovedForceStop(ctx, task, port)
}

type supervisorRemovedCapture struct {
	descriptorFingerprint string
	stop                  supervisorRemovedStop
	err                   error
}

// A preliminary superset is captured before acquiring the intent flock. Only
// descriptors actually removed by the locked commit may consume their capture.
// A row introduced or replaced during that gap has no transferable authority.
type supervisorRemovedCaptures map[string]supervisorRemovedCapture

func captureRemovedSupervisorTargets(ctx context.Context, targets []SupervisorDaemon) supervisorRemovedCaptures {
	if len(targets) == 0 {
		return nil
	}
	ctx, cancel := context.WithTimeout(ctx, 12*time.Second)
	defer cancel()
	captures := make(supervisorRemovedCaptures, len(targets))
	for _, descriptor := range targets {
		task := canonicalIntentTaskKey(descriptor.TaskName)
		if _, duplicate := captures[task]; duplicate {
			continue
		}
		port, _ := EffectiveDaemonPort(descriptor)
		stop, err := prepareSupervisorRemovedStopFn(ctx, task, port)
		captures[task] = supervisorRemovedCapture{descriptorFingerprint: supervisorIntentTargetFingerprint(&descriptor), stop: stop, err: err}
	}
	return captures
}

func (captures supervisorRemovedCaptures) close() error {
	var result error
	for task, capture := range captures {
		if capture.stop != nil {
			result = errors.Join(result, capture.stop.Close())
		}
		delete(captures, task)
	}
	return result
}

func (captures supervisorRemovedCaptures) validate(descriptor SupervisorDaemon) error {
	capture, found := captures[canonicalIntentTaskKey(descriptor.TaskName)]
	if !found || capture.descriptorFingerprint != supervisorIntentTargetFingerprint(&descriptor) {
		return fmt.Errorf("%w: descriptor was not captured before removal or changed before commit", ErrOriginalProcessOwnerRequired)
	}
	if capture.err != nil {
		return capture.err
	}
	if capture.stop == nil {
		return ErrOriginalProcessOwnerRequired
	}
	return nil
}

func (captures supervisorRemovedCaptures) complete(ctx context.Context, descriptor SupervisorDaemon) error {
	if err := captures.validate(descriptor); err != nil {
		return err
	}
	capture := captures[canonicalIntentTaskKey(descriptor.TaskName)]
	result, err := capture.stop.Complete(ctx)
	if err != nil {
		return err
	}
	if result.State != ForceStopClosed || !result.RootExited || !result.TreeClosed || !result.HandlesClosed || result.Error != "" {
		return fmt.Errorf("original creator closure unconfirmed: state=%s reason=%s root_exited=%t tree_closed=%t handles_closed=%t: %s",
			result.State, result.Reason, result.RootExited, result.TreeClosed, result.HandlesClosed, result.Error)
	}
	return nil
}

// Uninstall writes stop tombstones for legacy tasks before deleting them. Its
// capture must precede that first intent mutation too, including mixed installs
// where a legacy task and a Supervisor descriptor name the same daemon.
func prepareUninstallSupervisorCaptures(server string, scope *supervisorIntentOwnershipScope, report *UninstallReport) supervisorRemovedCaptures {
	stateDir, err := DaemonStateDir()
	var targets []SupervisorDaemon
	if err == nil {
		targets, err = preliminarySupervisorTargetsForServerScope(filepath.Join(stateDir, supervisorIntentFileLeaf), server, scope)
	}
	if err != nil {
		if report != nil {
			report.TaskDeleteWarns = append(report.TaskDeleteWarns, fmt.Sprintf("capture original Supervisor before uninstall: %v", err))
		}
		return nil
	}
	return captureRemovedSupervisorTargets(context.Background(), targets)
}

func closeUninstallSupervisorCaptures(captures supervisorRemovedCaptures, report *UninstallReport) {
	if err := captures.close(); err != nil && report != nil {
		report.TaskDeleteWarns = append(report.TaskDeleteWarns, fmt.Sprintf("close removed-target captures: %v", err))
	}
}

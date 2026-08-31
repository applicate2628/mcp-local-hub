package cli

import (
	"errors"
	"fmt"
	"runtime"
	"time"

	"mcp-local-hub/internal/api"
	"mcp-local-hub/internal/process"
)

// terminationExpectedTuple is the immutable identity captured from the live
// runtime tracker before a termination attempt mutates any process state.  A
// startup snapshot may supply a fallback PID for a best-effort termination,
// but it never makes this tuple valid and therefore never authorizes a
// synthetic child-exit event.
type terminationExpectedTuple struct {
	CanonicalTaskName string
	PID               int
	StartedAt         string
	PIDGeneration     int
	Valid             bool
}

type terminationOutcomeKind string

const (
	terminationOutcomeTargetAbsent     terminationOutcomeKind = "target_absent"
	terminationOutcomeUncertain        terminationOutcomeKind = "uncertain"
	terminationOutcomeAlreadyExited    terminationOutcomeKind = "already_exited"
	terminationOutcomeIdentityMismatch terminationOutcomeKind = "identity_mismatch"
	terminationOutcomeFailed           terminationOutcomeKind = "failed"
	terminationOutcomeCommitted        terminationOutcomeKind = "termination_committed"
	terminationOutcomeTerminated       terminationOutcomeKind = "terminated"
)

// terminationOutcome distinguishes an accepted signal from an observed exit.
// CleanupError is diagnostic-only: a successful Windows process termination is
// never downgraded merely because releasing its held OS handle failed.
type terminationOutcome struct {
	Kind         terminationOutcomeKind
	Expected     terminationExpectedTuple
	Cause        error
	CleanupError error
}

// AllowsSynthetic is deliberately stricter than a legacy nil error.  Only an
// observed terminal outcome for one exact live tracker generation may replace
// the real cmd.Wait child-exit event for a foreign warm-start process.
func (o terminationOutcome) AllowsSynthetic() bool {
	return o.Expected.Valid && (o.Kind == terminationOutcomeAlreadyExited || o.Kind == terminationOutcomeTerminated)
}

func (o terminationOutcome) legacyError() error {
	switch o.Kind {
	case terminationOutcomeAlreadyExited, terminationOutcomeTerminated:
		return nil
	case terminationOutcomeCommitted:
		if o.Cause != nil {
			return fmt.Errorf("termination committed but exit was not observed: %w", o.Cause)
		}
		return errors.New("termination committed but exit was not observed")
	case terminationOutcomeIdentityMismatch:
		// Keep the old error-only caller contract (PID reuse is an
		// abort-without-kill, not an application failure), while retaining the
		// typed nonterminal outcome for controller synthesis and receipts.
		return nil
	case terminationOutcomeTargetAbsent:
		if o.Cause != nil {
			return o.Cause
		}
		return errors.New("termination target absent")
	case terminationOutcomeUncertain:
		// Preserve the legacy callback's observable nil return for callers
		// that still consume only error. Its typed representation remains
		// uncertain and invalid, so controller synthesis is still forbidden.
		return o.Cause
	default:
		if o.Cause != nil {
			return o.Cause
		}
		return errors.New("termination outcome uncertain")
	}
}

type terminationOutcomeFunc func(api.SupervisorDaemon, terminationExpectedTuple) terminationOutcome

// makeProductionTerminateFn is the legacy Error-only adapter retained for the
// startup reconciler and early IPC wiring. A nil legacy result is never used by
// the controller as terminal proof; the controller receives the typed closure
// directly from runSupervise.
func makeProductionTerminateFn(events *api.SupervisorEventLog, runningPIDs map[string]runningProcessIdentity, tracker *DaemonRuntimeTracker) TerminateFunc {
	return makeProductionTerminateFnWithStatePath(events, runningPIDs, tracker, "")
}

func makeProductionTerminateFnWithStatePath(events *api.SupervisorEventLog, runningPIDs map[string]runningProcessIdentity, tracker *DaemonRuntimeTracker, statePath string) TerminateFunc {
	terminate := makeProductionTerminationOutcomeFn(events, runningPIDs, tracker, statePath)
	return func(d api.SupervisorDaemon) error {
		expected := terminationExpectedTupleForTask(tracker, d.TaskName)
		return terminate(d, expected).legacyError()
	}
}

func terminationExpectedTupleForTask(tracker *DaemonRuntimeTracker, taskName string) terminationExpectedTuple {
	tuple := terminationExpectedTuple{CanonicalTaskName: canonicalSupervisorTaskName(taskName)}
	if tracker == nil {
		return tuple
	}
	entry, ok := tracker.Get(tuple.CanonicalTaskName)
	if !ok || entry.CurrentPID <= 0 || entry.PIDGeneration <= 0 || entry.StartedAt.IsZero() {
		return tuple
	}
	tuple.PID = entry.CurrentPID
	tuple.StartedAt = entry.StartedAt.UTC().Format(time.RFC3339Nano)
	tuple.PIDGeneration = entry.PIDGeneration
	tuple.Valid = true
	return tuple
}

func terminationOutcomeFromLegacy(expected terminationExpectedTuple, err error) terminationOutcome {
	// A legacy TerminateFunc cannot tell a nil return from a confirmed process
	// exit.  Never promote it: the controller must await a real cmd.Wait event
	// or use the typed production path.
	if err == nil {
		expected.Valid = false
		return terminationOutcome{Kind: terminationOutcomeUncertain, Expected: expected}
	}
	if errors.Is(err, process.ErrProcessIdentityMismatch) {
		return terminationOutcome{Kind: terminationOutcomeIdentityMismatch, Expected: expected, Cause: err}
	}
	if errors.Is(err, process.ErrProcessAlreadyExited) || errors.Is(err, errTerminateTargetGone) {
		return terminationOutcome{Kind: terminationOutcomeAlreadyExited, Expected: expected, Cause: err}
	}
	return terminationOutcome{Kind: terminationOutcomeFailed, Expected: expected, Cause: err}
}

func terminationTupleMatchesReceipt(tuple terminationExpectedTuple, receipt api.StopSettlementReceiptV1) bool {
	return tuple.Valid && tuple.CanonicalTaskName == receipt.TaskName && tuple.PID == receipt.PID && tuple.StartedAt == receipt.StartedAt && tuple.PIDGeneration == receipt.PIDGeneration
}

func makeProductionTerminationOutcomeFn(events *api.SupervisorEventLog, runningPIDs map[string]runningProcessIdentity, tracker *DaemonRuntimeTracker, statePath string) terminationOutcomeFunc {
	return func(d api.SupervisorDaemon, expected terminationExpectedTuple) (outcome terminationOutcome) {
		if expected.CanonicalTaskName == "" {
			expected.CanonicalTaskName = canonicalSupervisorTaskName(d.TaskName)
		}
		outcome.Expected = expected
		target := runningPIDs[expected.CanonicalTaskName]
		if target.PID <= 0 {
			target = runningPIDs[d.TaskName]
		}
		pid := target.PID
		if expected.Valid {
			pid = expected.PID
			target.PID = expected.PID
			target.StartedAt = expected.StartedAt
		}
		if pid <= 0 {
			outcome.Kind = terminationOutcomeTargetAbsent
			outcome.Cause = fmt.Errorf("no running PID recorded for task %q", d.TaskName)
			return outcome
		}

		state, stateErr := productionQueryPIDStateFn(pid)
		if stateErr != nil || state == process.PIDStateUnknown {
			outcome.Kind = terminationOutcomeUncertain
			if stateErr != nil {
				outcome.Cause = fmt.Errorf("query PID %d state: %w", pid, stateErr)
			} else {
				outcome.Cause = fmt.Errorf("query PID %d state returned unknown", pid)
			}
			emitDaemonTerminateFailed(events, d, pid, outcome.Cause)
			return outcome
		}
		if state == process.PIDStateDead {
			markTerminationOutcomeExited(events, tracker, statePath, d, expected)
			emitDaemonTerminateAlreadyExited(events, d, pid)
			outcome.Kind = terminationOutcomeAlreadyExited
			return outcome
		}

		proof := process.PIDIdentityProof{PID: pid, ExecutablePath: daemonExpectedIdentityExe(d.Command), StartedAt: target.StartedAt}
		if proof.StartedAt == "" {
			outcome.Kind = terminationOutcomeUncertain
			outcome.Cause = fmt.Errorf("missing started_at for PID %d", pid)
			emitDaemonTerminateFailed(events, d, pid, outcome.Cause)
			return outcome
		}

		if runtime.GOOS == "windows" {
			return terminateProductionWindows(events, tracker, statePath, d, expected, proof)
		}
		if err := productionVerifyPIDIdentityFn(proof); err != nil {
			return terminationOutcomeFromVerification(events, d, expected, pid, err)
		}
		_ = events.Emit(api.SupervisorEvent{Severity: api.SupervisorEventSeverityInfo, Source: "lifecycle", Event: "daemon-terminate-requested", TaskName: d.TaskName, Body: map[string]any{"pid": pid}})
		if err := productionTerminatePIDWithIdentityFn(proof); err != nil {
			return terminationOutcomeFromVerification(events, d, expected, pid, err)
		}
		if err := finishProductionTerminate(proof, d, events); err != nil {
			outcome.Kind = terminationOutcomeFailed
			outcome.Cause = err
			emitDaemonTerminateFailed(events, d, pid, err)
			return outcome
		}
		// SIGTERM/SIGKILL acknowledgement alone is not an observed exit. Re-probe
		// directly; if the kernel does not report Dead, preserve the committed
		// distinction so callers do not synthesize over a possibly-live process.
		state, stateErr = productionQueryPIDStateFn(pid)
		if stateErr == nil && state == process.PIDStateDead {
			markTerminationOutcomeExited(events, tracker, statePath, d, expected)
			outcome.Kind = terminationOutcomeTerminated
			return outcome
		}
		outcome.Kind = terminationOutcomeCommitted
		if stateErr != nil {
			outcome.Cause = fmt.Errorf("re-query PID %d after termination: %w", pid, stateErr)
		} else {
			outcome.Cause = fmt.Errorf("PID %d not observed dead after termination", pid)
		}
		return outcome
	}
}

func terminationOutcomeFromVerification(events *api.SupervisorEventLog, d api.SupervisorDaemon, expected terminationExpectedTuple, pid int, err error) terminationOutcome {
	if errors.Is(err, process.ErrProcessAlreadyExited) {
		emitDaemonTerminateAlreadyExited(events, d, pid)
		return terminationOutcome{Kind: terminationOutcomeAlreadyExited, Expected: expected, Cause: err}
	}
	if errors.Is(err, process.ErrProcessIdentityMismatch) {
		_ = events.Emit(api.SupervisorEvent{Severity: api.SupervisorEventSeverityWarn, Source: "lifecycle", Event: "daemon-terminate-aborted-pid-reuse", TaskName: d.TaskName, Body: map[string]any{"pid": pid, "reason": err.Error()}})
		return terminationOutcome{Kind: terminationOutcomeIdentityMismatch, Expected: expected, Cause: err}
	}
	emitDaemonTerminateFailed(events, d, pid, err)
	return terminationOutcome{Kind: terminationOutcomeFailed, Expected: expected, Cause: err}
}

func markTerminationOutcomeExited(events *api.SupervisorEventLog, tracker *DaemonRuntimeTracker, statePath string, d api.SupervisorDaemon, expected terminationExpectedTuple) {
	if tracker == nil || !expected.Valid {
		return
	}
	if tracker.MarkExitedIfCurrent(expected.CanonicalTaskName, expected.PIDGeneration) {
		_ = persistDaemonRuntimeTracker(events, tracker, statePath, d.TaskName)
	}
}

func terminateProductionWindows(events *api.SupervisorEventLog, tracker *DaemonRuntimeTracker, statePath string, d api.SupervisorDaemon, expected terminationExpectedTuple, proof process.PIDIdentityProof) (outcome terminationOutcome) {
	outcome.Expected = expected
	held, err := productionHoldPIDForTerminationFn(proof.PID)
	if err != nil {
		return terminationOutcomeFromVerification(events, d, expected, proof.PID, err)
	}
	defer func() {
		if closeErr := held.Close(); closeErr != nil {
			// Held handle release is observable cleanup evidence. It cannot
			// reverse a process exit already proven by WaitForSingleObject.
			outcome.CleanupError = closeErr
		}
	}()
	if err := held.VerifyIdentity(proof); err != nil {
		outcome = terminationOutcomeFromVerification(events, d, expected, proof.PID, err)
		if outcome.Kind == terminationOutcomeAlreadyExited {
			markTerminationOutcomeExited(events, tracker, statePath, d, expected)
		}
		return outcome
	}
	_ = events.Emit(api.SupervisorEvent{Severity: api.SupervisorEventSeverityInfo, Source: "lifecycle", Event: "daemon-terminate-requested", TaskName: d.TaskName, Body: map[string]any{"pid": proof.PID}})
	committed, err := held.Terminate()
	if !committed {
		outcome = terminationOutcomeFromVerification(events, d, expected, proof.PID, err)
		if outcome.Kind == terminationOutcomeAlreadyExited {
			markTerminationOutcomeExited(events, tracker, statePath, d, expected)
		}
		return outcome
	}
	if err != nil {
		return terminationOutcome{Kind: terminationOutcomeCommitted, Expected: expected, Cause: err}
	}
	markTerminationOutcomeExited(events, tracker, statePath, d, expected)
	_ = events.Emit(api.SupervisorEvent{Severity: api.SupervisorEventSeverityInfo, Source: "lifecycle", Event: "daemon-terminated", TaskName: d.TaskName, Body: map[string]any{"pid": proof.PID}})
	return terminationOutcome{Kind: terminationOutcomeTerminated, Expected: expected}
}

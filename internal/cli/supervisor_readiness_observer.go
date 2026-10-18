package cli

import (
	"context"
	"errors"
	"time"

	"mcp-local-hub/internal/api"
)

// observeSupervisorReadiness records bounded, current-generation evidence
// after a spawn. The tracker remains the only writer; API later reduces the
// observation. Bind and initial-protocol work stay deadline-bounded; a
// retryable post-budget failure remains in this same generation-scoped
// goroutine at the existing liveness cadence. It does not mutate tasks,
// intents, ports, DACLs, or client configuration.
func observeSupervisorReadiness(ctx context.Context, tracker *DaemonRuntimeTracker, events *api.SupervisorEventLog, d api.SupervisorDaemon, generation int, pid int, startedAt time.Time) {
	observeSupervisorReadinessWithDeps(ctx, tracker, events, d, generation, pid, startedAt, supervisorReadinessObserverDeps{
		now:  func() time.Time { return time.Now().UTC() },
		wait: waitSupervisorReadiness,
		inspect: func(d api.SupervisorDaemon, entry DaemonRuntimeEntry, now time.Time) (bool, bool) {
			live, _, bound, _ := supervisorDaemonEntryLiveWithProbe(d, entry, now, supervisorLivenessProbeFns, 0)
			return live, bound
		},
		probe:      api.ProbeMCPReadinessContext,
		routeProbe: api.AssertMCPFrontRoutesLive,
	})
}

type supervisorReadinessObserverDeps struct {
	now     func() time.Time
	wait    func(context.Context, time.Duration) bool
	inspect func(api.SupervisorDaemon, DaemonRuntimeEntry, time.Time) (live, bound bool)
	probe   func(context.Context, int) api.MCPReadinessResult
	// routeProbe is the API-owned, combined readiness predicate for the
	// supervisor's one reserved route-front descriptor. It owns the Serena and
	// LSP route checks; ordinary daemon descriptors retain probe's /mcp check.
	routeProbe func(context.Context, int) error
}

func observeSupervisorReadinessWithDeps(ctx context.Context, tracker *DaemonRuntimeTracker, events *api.SupervisorEventLog, d api.SupervisorDaemon, generation int, pid int, startedAt time.Time, deps supervisorReadinessObserverDeps) {
	if ctx == nil {
		ctx = context.Background()
	}
	if tracker == nil || generation <= 0 || deps.now == nil || deps.wait == nil || deps.inspect == nil || deps.probe == nil {
		return
	}
	policy := api.ReadinessPolicyMCPUpstream
	if api.IsMaintenanceTaskName(d.TaskName) {
		policy = api.ReadinessPolicyProcessOnly
	} else if api.IsLazyProxyTaskName(d.TaskName) {
		policy = api.ReadinessPolicySyntheticProxy
	}
	deadlineSeconds := d.StartupBindDeadlineSeconds
	if deadlineSeconds <= 0 {
		deadlineSeconds = 5
	}
	listenerDeadline := startedAt.Add(time.Duration(deadlineSeconds) * time.Second)
	initialProtocolDeadline := listenerDeadline.Add(api.MCPReadinessProbeBudget)
	newObservation := func() api.DaemonReadinessObservationV1 {
		return api.DaemonReadinessObservationV1{
			TaskName: d.TaskName, Server: d.Server, Daemon: d.Daemon, Port: d.Port, PID: pid,
			ProcessState: "Running", CurrentPIDGeneration: uint64(generation), ObservedPIDGeneration: uint64(generation),
			IntentPresent: true, IntentRunnable: true, WrapperStarted: true, Policy: policy,
		}
	}
	current := func() (DaemonRuntimeEntry, bool) {
		if ctx.Err() != nil {
			return DaemonRuntimeEntry{}, false
		}
		entry, ok := tracker.Get(d.TaskName)
		if !ok || entry.PIDGeneration != generation || entry.CurrentPID != pid || entry.State != daemonRuntimeStateRunning {
			return DaemonRuntimeEntry{}, false
		}
		return entry, true
	}
	for {
		entry, ok := current()
		if !ok {
			return
		}
		observation := newObservation()
		if policy == api.ReadinessPolicySyntheticProxy {
			if _, ok := current(); !ok {
				return
			}
			emitReadinessSettlement(events, tracker, observation)
			return
		}
		live, bound := deps.inspect(d, entry, deps.now())
		if _, ok := current(); !ok {
			return
		}
		observation.ListenerReady = live && bound
		if observation.ListenerReady {
			if policy == api.ReadinessPolicyProcessOnly {
				emitReadinessSettlement(events, tracker, observation)
				return
			}
			if d.TaskName == api.BuiltinRouteTaskName {
				if deps.routeProbe == nil {
					return
				}
				err := deps.routeProbe(ctx, d.Port)
				if _, ok := current(); !ok {
					return
				}
				if err == nil {
					observation.MCPInitializeReady, observation.MCPToolsListReady = true, true
					emitReadinessSettlement(events, tracker, observation)
					return
				}
				observation.Failures = []api.ReadinessFailureV1{routeFrontReadinessFailure(d, err)}
				if emitReadinessSettlement(events, tracker, observation) {
					emitRouteFrontReadinessDiagnostic(events, d, err)
				}
				return
			}
			break
		}
		if !deps.now().Before(listenerDeadline) {
			observation.DeadlineExceeded = true
			emitReadinessSettlement(events, tracker, observation)
			return
		}
		if _, ok := current(); !ok {
			return
		}
		if !deps.wait(ctx, 100*time.Millisecond) {
			return
		}
		if _, ok := current(); !ok {
			return
		}
	}

	deadlineExceeded := false
	for {
		if _, ok := current(); !ok {
			return
		}
		attemptCtx := ctx
		cancel := func() {}
		if deadlineExceeded {
			attemptCtx, cancel = context.WithTimeout(ctx, api.MCPReadinessProbeBudget)
		} else {
			attemptCtx, cancel = context.WithDeadline(ctx, initialProtocolDeadline)
		}
		result := deps.probe(attemptCtx, d.Port)
		cancel()
		if _, ok := current(); !ok {
			return
		}
		observation := newObservation()
		observation.ListenerReady = true
		if result.State == api.MCPReadinessReady {
			observation.MCPInitializeReady, observation.MCPToolsListReady = true, true
			emitReadinessSettlement(events, tracker, observation)
			return
		}
		stage := api.ReadinessStageMCPInitialize
		if result.Stage == api.MCPReadinessStageToolsList {
			stage = api.ReadinessStageMCPToolsList
		}
		observation.Failures = []api.ReadinessFailureV1{{
			Stage: stage, FailureID: result.FailureID, Retryable: result.Retryable,
			TaskName: d.TaskName, Port: d.Port,
		}}
		if !result.Retryable {
			emitReadinessSettlement(events, tracker, observation)
			return
		}
		if !deadlineExceeded && deps.now().Before(initialProtocolDeadline) {
			emitReadinessSettlement(events, tracker, observation)
			if _, ok := current(); !ok {
				return
			}
			if !deps.wait(ctx, 100*time.Millisecond) {
				return
			}
			continue
		}
		deadlineExceeded = true
		observation.DeadlineExceeded = true
		emitReadinessSettlement(events, tracker, observation)
		if _, ok := current(); !ok {
			return
		}
		if !deps.wait(ctx, supervisorLivenessInterval) {
			return
		}
	}
}

func waitSupervisorReadiness(ctx context.Context, delay time.Duration) bool {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

// routeFrontReadinessFailure maps the API's combined route proof to its
// existing stable public failure identifier. The full Serena-versus-LSP cause
// remains in the supervisor event; ReadinessFailureV1 intentionally exposes
// only safe stable coordinates.
func routeFrontReadinessFailure(d api.SupervisorDaemon, err error) api.ReadinessFailureV1 {
	failureID := "mcp_initialize_failed"
	var routeErr *api.MCPFrontRoutesLiveError
	if errors.As(err, &routeErr) && routeErr.Code == api.MCPFrontRouteNotReadyCode {
		failureID = api.MCPFrontRouteNotReadyCode
	}
	return api.ReadinessFailureV1{
		Stage: api.ReadinessStageMCPInitialize, FailureID: failureID, TaskName: d.TaskName, Port: d.Port,
	}
}

// emitRouteFrontReadinessDiagnostic retains the causal route projection in
// the event log without widening ReadinessFailureV1's deliberately safe wire
// schema. Its fields are the API's closed enums and manifest-derived names.
func emitRouteFrontReadinessDiagnostic(events *api.SupervisorEventLog, d api.SupervisorDaemon, err error) {
	if events == nil {
		return
	}
	var routeErr *api.MCPFrontRoutesLiveError
	if !errors.As(err, &routeErr) {
		return
	}
	if err := events.Emit(api.SupervisorEvent{
		Severity: api.SupervisorEventSeverityWarn, Source: "readiness", Event: "route-front-readiness-failed-v1", TaskName: d.TaskName,
		Body: map[string]any{
			"schema_version": "route-front-readiness-failed-v1", "task_name": d.TaskName,
			"route": string(routeErr.Stage), "probe_stage": string(routeErr.ProbeStage),
			"language": routeErr.Language, "backend": routeErr.Backend,
		},
	}); err != nil {
		_ = events.Emit(api.SupervisorEvent{
			Severity: api.SupervisorEventSeverityError, Source: "readiness", Event: "supervisor_event_write_failed", TaskName: d.TaskName,
			Body: map[string]any{"event": "route-front-readiness-failed-v1"},
		})
	}
}

func emitReadinessSettlement(events *api.SupervisorEventLog, tracker *DaemonRuntimeTracker, observation api.DaemonReadinessObservationV1) bool {
	accepted, event := tracker.MarkReadinessObservationWithSettlement(observation)
	if !accepted {
		return false
	}
	if event == nil || events == nil {
		return true
	}
	if err := events.Emit(*event); err != nil {
		_ = events.Emit(api.SupervisorEvent{
			Severity: api.SupervisorEventSeverityError, Source: "readiness", Event: "supervisor_event_write_failed", TaskName: event.TaskName,
			Body: map[string]any{"event": event.Event},
		})
	}
	return true
}

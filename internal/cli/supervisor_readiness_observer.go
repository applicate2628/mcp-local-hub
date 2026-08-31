package cli

import (
	"context"
	"errors"
	"time"

	"mcp-local-hub/internal/api"
)

// observeSupervisorReadiness records bounded, current-generation evidence
// after a spawn. The tracker remains the only writer; API later reduces the
// observation. This goroutine has a fixed deadline and does not mutate tasks,
// intents, ports, DACLs, or client configuration.
func observeSupervisorReadiness(tracker *DaemonRuntimeTracker, events *api.SupervisorEventLog, d api.SupervisorDaemon, generation int, pid int, startedAt time.Time) {
	observeSupervisorReadinessWithDeps(tracker, events, d, generation, pid, startedAt, supervisorReadinessObserverDeps{
		now:  func() time.Time { return time.Now().UTC() },
		wait: time.Sleep,
		inspect: func(d api.SupervisorDaemon, entry DaemonRuntimeEntry, now time.Time) (bool, bool) {
			live, _, bound, _ := supervisorDaemonEntryLiveWithProbe(d, entry, now, supervisorLivenessProbeFns, 0)
			return live, bound
		},
		probe:      api.ProbeMCPReadiness,
		routeProbe: api.AssertMCPFrontRoutesLive,
	})
}

type supervisorReadinessObserverDeps struct {
	now     func() time.Time
	wait    func(time.Duration)
	inspect func(api.SupervisorDaemon, DaemonRuntimeEntry, time.Time) (live, bound bool)
	probe   func(int) api.MCPReadinessResult
	// routeProbe is the API-owned, combined readiness predicate for the
	// supervisor's one reserved route-front descriptor. It owns the Serena and
	// LSP route checks; ordinary daemon descriptors retain probe's /mcp check.
	routeProbe func(context.Context, int) error
}

func observeSupervisorReadinessWithDeps(tracker *DaemonRuntimeTracker, events *api.SupervisorEventLog, d api.SupervisorDaemon, generation int, pid int, startedAt time.Time, deps supervisorReadinessObserverDeps) {
	if tracker == nil || generation <= 0 {
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
	deadline := startedAt.Add(time.Duration(deadlineSeconds) * time.Second)
	for {
		entry, ok := tracker.Get(d.TaskName)
		if !ok || entry.PIDGeneration != generation || entry.CurrentPID != pid {
			return
		}
		observation := api.DaemonReadinessObservationV1{
			TaskName: d.TaskName, Server: d.Server, Daemon: d.Daemon, Port: d.Port, PID: pid,
			ProcessState: "Running", CurrentPIDGeneration: uint64(generation), ObservedPIDGeneration: uint64(generation),
			IntentPresent: true, IntentRunnable: true, WrapperStarted: true, Policy: policy,
		}
		if policy == api.ReadinessPolicySyntheticProxy {
			emitReadinessSettlement(events, tracker, observation)
			return
		}
		live, bound := deps.inspect(d, entry, deps.now())
		observation.ListenerReady = live && bound
		if observation.ListenerReady {
			if policy == api.ReadinessPolicyProcessOnly {
				emitReadinessSettlement(events, tracker, observation)
				return
			}
			if d.TaskName == api.BuiltinRouteTaskName {
				if err := deps.routeProbe(context.Background(), d.Port); err == nil {
					observation.MCPInitializeReady, observation.MCPToolsListReady = true, true
					emitReadinessSettlement(events, tracker, observation)
					return
				} else {
					observation.Failures = []api.ReadinessFailureV1{routeFrontReadinessFailure(d, err)}
					emitRouteFrontReadinessDiagnostic(events, d, err)
					emitReadinessSettlement(events, tracker, observation)
					return
				}
			}
			result := deps.probe(d.Port)
			if result.State == api.MCPReadinessReady {
				observation.MCPInitializeReady, observation.MCPToolsListReady = true, true
				emitReadinessSettlement(events, tracker, observation)
				return
			}
			stage := api.ReadinessStageMCPInitialize
			if result.Stage == api.MCPReadinessStageToolsList {
				stage = api.ReadinessStageMCPToolsList
			}
			observation.Failures = []api.ReadinessFailureV1{{Stage: stage, FailureID: result.FailureID, TaskName: d.TaskName, Port: d.Port}}
			emitReadinessSettlement(events, tracker, observation)
			return
		}
		if !deps.now().Before(deadline) {
			observation.DeadlineExceeded = true
			emitReadinessSettlement(events, tracker, observation)
			return
		}
		deps.wait(100 * time.Millisecond)
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

func emitReadinessSettlement(events *api.SupervisorEventLog, tracker *DaemonRuntimeTracker, observation api.DaemonReadinessObservationV1) {
	_, event := tracker.MarkReadinessObservationWithSettlement(observation)
	if event == nil || events == nil {
		return
	}
	if err := events.Emit(*event); err != nil {
		_ = events.Emit(api.SupervisorEvent{
			Severity: api.SupervisorEventSeverityError, Source: "readiness", Event: "supervisor_event_write_failed", TaskName: event.TaskName,
			Body: map[string]any{"event": event.Event},
		})
	}
}

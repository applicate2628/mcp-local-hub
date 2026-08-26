package api

import (
	"context"
	"fmt"
	"sort"
	"time"
)

// ReadinessModeV1 selects whether a caller wants the latest supplied
// observation or a settled observation supplied by its owning runtime. The
// reducer never performs polling itself: callers inject the observation they
// obtained through the supervisor-owned path.
type ReadinessModeV1 string

const (
	ReadinessModeSnapshot     ReadinessModeV1 = "snapshot"
	ReadinessModeAwaitSettled ReadinessModeV1 = "await_settled"
)

// ReadinessStageV1 is the closed causal order for readiness failures.
type ReadinessStageV1 string

const (
	ReadinessStageIntentAccess        ReadinessStageV1 = "intent_access"
	ReadinessStageSupervisorBootstrap ReadinessStageV1 = "supervisor_bootstrap"
	ReadinessStageSupervisorIPC       ReadinessStageV1 = "supervisor_ipc"
	ReadinessStageTaskIntent          ReadinessStageV1 = "task_intent"
	ReadinessStageWrapperStart        ReadinessStageV1 = "wrapper_start"
	ReadinessStageUpstreamListener    ReadinessStageV1 = "upstream_listener"
	ReadinessStageMCPInitialize       ReadinessStageV1 = "mcp_initialize"
	ReadinessStageMCPToolsList        ReadinessStageV1 = "mcp_tools_list"
	ReadinessStageClientBinding       ReadinessStageV1 = "client_binding"
	ReadinessStageComplete            ReadinessStageV1 = "complete"
)

// ServiceStateV1 is distinct from the legacy scheduler/process state. Only
// Running represents a usable external MCP service.
type ServiceStateV1 string

const (
	ServiceStateAbsent   ServiceStateV1 = "Absent"
	ServiceStateDisabled ServiceStateV1 = "Disabled"
	ServiceStateStarting ServiceStateV1 = "Starting"
	ServiceStateRunning  ServiceStateV1 = "Running"
	ServiceStateDegraded ServiceStateV1 = "Degraded"
	ServiceStateFailed   ServiceStateV1 = "Failed"
	ServiceStateStopped  ServiceStateV1 = "Stopped"
)

type MaterializationStateV1 string

const (
	MaterializationAbsent     MaterializationStateV1 = "absent"
	MaterializationIntentOnly MaterializationStateV1 = "intent_only"
	MaterializationStarting   MaterializationStateV1 = "starting"
	MaterializationReady      MaterializationStateV1 = "ready"
	MaterializationDegraded   MaterializationStateV1 = "degraded"
	MaterializationFailed     MaterializationStateV1 = "failed"
	MaterializationStopped    MaterializationStateV1 = "stopped"
	MaterializationDisabled   MaterializationStateV1 = "disabled"
)

type BindingStateV1 string

const (
	BindingStateNone       BindingStateV1 = "none"
	BindingStateHub        BindingStateV1 = "hub"
	BindingStateDirect     BindingStateV1 = "direct"
	BindingStateDisabled   BindingStateV1 = "disabled"
	BindingStateMixed      BindingStateV1 = "mixed"
	BindingStateUnreadable BindingStateV1 = "unreadable"
)

type ReadinessClassificationV1 string

const (
	ReadinessClassificationUnhealthyDegraded          ReadinessClassificationV1 = "unhealthy/degraded"
	ReadinessClassificationInstalledAndBound          ReadinessClassificationV1 = "installed-and-bound"
	ReadinessClassificationInstalledUnbound           ReadinessClassificationV1 = "installed-unbound"
	ReadinessClassificationConfiguredDirectCanMigrate ReadinessClassificationV1 = "configured-direct/can-migrate"
	ReadinessClassificationDisabled                   ReadinessClassificationV1 = "disabled"
	ReadinessClassificationGenuinelyNotInstalled      ReadinessClassificationV1 = "genuinely-not-installed"
)

type ReadinessPolicyV1 string

const (
	ReadinessPolicyMCPUpstream    ReadinessPolicyV1 = "mcp-upstream"
	ReadinessPolicyProcessOnly    ReadinessPolicyV1 = "process-only"
	ReadinessPolicySyntheticProxy ReadinessPolicyV1 = "synthetic-proxy"
)

type BindingRouteV1 string

const (
	BindingRouteNone   BindingRouteV1 = "none"
	BindingRouteHub    BindingRouteV1 = "hub"
	BindingRouteDirect BindingRouteV1 = "direct"
)

// ReadinessFailureV1 intentionally carries only stable identifiers and safe
// coordinates. The reducer does not copy Detail from observations, because
// observations can originate at an untrusted filesystem/process/MCP boundary.
type ReadinessFailureV1 struct {
	Stage     ReadinessStageV1 `json:"stage"`
	FailureID string           `json:"failure_id"`
	Retryable bool             `json:"retryable,omitempty"`
	Detail    string           `json:"detail,omitempty"`
	TaskName  string           `json:"task_name,omitempty"`
	Client    string           `json:"client,omitempty"`
	Port      int              `json:"port,omitempty"`
}

// DaemonReadinessObservationV1 is a runtime observation supplied by the
// supervisor/IPC owner. It contains no callback that can read live state;
// AssessReadiness is therefore safe for status/scan consumers.
type DaemonReadinessObservationV1 struct {
	TaskName string
	Server   string
	Daemon   string
	Port     int
	PID      int

	ProcessState          string
	CurrentPIDGeneration  uint64
	ObservedPIDGeneration uint64
	IntentPresent         bool
	IntentRunnable        bool
	IntentDisabled        bool
	Stopped               bool
	WrapperStarted        bool
	ListenerReady         bool
	MCPInitializeReady    bool
	MCPToolsListReady     bool
	DeadlineExceeded      bool
	Policy                ReadinessPolicyV1
	Failures              []ReadinessFailureV1
}

type BindingObservationV1 struct {
	Client        string
	Present       bool
	Enabled       bool
	Disabled      bool
	Readable      bool
	Route         BindingRouteV1
	ExactHubRoute bool
	Requested     bool
}

// ReadinessRequest is intentionally all-data. Production composition supplies
// observations from existing owners; tests inject the same shape directly.
type ReadinessRequest struct {
	Selectors        []string
	Mode             ReadinessModeV1
	Bindings         []BindingObservationV1
	RequireBinding   bool
	ManifestPresence bool
	CanMigrate       bool
	Now              time.Time
	Observations     []DaemonReadinessObservationV1
}

// ReadinessRequestV1 keeps the design's versioned public spelling while the
// initially requested ReadinessRequest API remains the source-compatible name.
type ReadinessRequestV1 = ReadinessRequest

type DaemonReadinessV1 struct {
	TaskName      string              `json:"task_name"`
	Server        string              `json:"server,omitempty"`
	Daemon        string              `json:"daemon,omitempty"`
	Port          int                 `json:"port,omitempty"`
	PID           int                 `json:"pid,omitempty"`
	PIDGeneration uint64              `json:"pid_generation,omitempty"`
	ProcessState  string              `json:"process_state,omitempty"`
	ServiceState  ServiceStateV1      `json:"service_state"`
	Stage         ReadinessStageV1    `json:"readiness_stage"`
	Settled       bool                `json:"readiness_settled"`
	Failure       *ReadinessFailureV1 `json:"failure,omitempty"`
}

type ReadinessSnapshotV1 struct {
	SchemaVersion        string                    `json:"schema_version"`
	ObservedAt           time.Time                 `json:"observed_at"`
	Settled              bool                      `json:"settled"`
	Daemons              []DaemonReadinessV1       `json:"daemons"`
	Bindings             []BindingObservationV1    `json:"bindings,omitempty"`
	MaterializationState MaterializationStateV1    `json:"materialization_state"`
	BindingState         BindingStateV1            `json:"binding_state"`
	Classification       ReadinessClassificationV1 `json:"classification"`
	PrimaryFailure       *ReadinessFailureV1       `json:"primary_failure,omitempty"`
	Failures             []ReadinessFailureV1      `json:"failures,omitempty"`
}

type ReadinessErrorV1 struct {
	Stage       ReadinessStageV1
	FailureID   string
	Remediation string
}

func (e *ReadinessErrorV1) Error() string {
	if e == nil {
		return ""
	}
	base := fmt.Sprintf("readiness failed at %s (%s)", e.Stage, e.FailureID)
	if e.Remediation != "" {
		return base + "; repair: " + e.Remediation
	}
	return base
}

// AssessReadiness is the API-owned operation. BCD1 deliberately accepts only
// observations already read by their owning boundaries; wiring supervisor IPC,
// client adapters, and mutation commands belongs to later B/C/D slices.
func (a *API) AssessReadiness(ctx context.Context, request ReadinessRequest) (ReadinessSnapshotV1, error) {
	if err := ctx.Err(); err != nil {
		snapshot := ReduceReadinessV1(request)
		failure := ReadinessFailureV1{Stage: ReadinessStageWrapperStart, FailureID: "readiness_cancelled"}
		snapshot.PrimaryFailure = &failure
		snapshot.Failures = append([]ReadinessFailureV1{failure}, snapshot.Failures...)
		snapshot.Settled = false
		snapshot.MaterializationState = MaterializationDegraded
		snapshot.Classification = ReadinessClassificationUnhealthyDegraded
		return snapshot, &ReadinessErrorV1{Stage: failure.Stage, FailureID: failure.FailureID}
	}

	snapshot := ReduceReadinessV1(request)
	if snapshot.PrimaryFailure != nil {
		return snapshot, &ReadinessErrorV1{Stage: snapshot.PrimaryFailure.Stage, FailureID: snapshot.PrimaryFailure.FailureID}
	}
	return snapshot, nil
}

// ReduceReadinessV1 is the sole pure reducer for the version-one readiness
// contract. It never reads a task, process, socket, client configuration, or
// clock implicitly; all of those facts are explicit request inputs.
func ReduceReadinessV1(request ReadinessRequest) ReadinessSnapshotV1 {
	now := request.Now.UTC()
	if now.IsZero() {
		now = time.Time{}
	}
	snapshot := ReadinessSnapshotV1{
		SchemaVersion: "readiness-snapshot-v1",
		ObservedAt:    now,
		Bindings:      append([]BindingObservationV1(nil), request.Bindings...),
	}
	sort.Slice(snapshot.Bindings, func(i, j int) bool { return snapshot.Bindings[i].Client < snapshot.Bindings[j].Client })

	observations := selectedReadinessObservations(request)
	for _, observation := range observations {
		daemon, failures := reduceDaemonReadinessV1(observation)
		snapshot.Daemons = append(snapshot.Daemons, daemon)
		snapshot.Failures = append(snapshot.Failures, failures...)
	}
	sort.Slice(snapshot.Daemons, func(i, j int) bool { return snapshot.Daemons[i].TaskName < snapshot.Daemons[j].TaskName })
	sortReadinessFailures(snapshot.Failures)
	if len(snapshot.Failures) > 0 {
		failure := snapshot.Failures[0]
		snapshot.PrimaryFailure = &failure
	}

	snapshot.MaterializationState = deriveMaterializationState(snapshot.Daemons)
	snapshot.BindingState = deriveBindingState(snapshot.Bindings)
	snapshot.Classification = deriveReadinessClassification(snapshot.MaterializationState, snapshot.BindingState, request.CanMigrate)
	snapshot.Settled = readinessSnapshotSettled(snapshot, request.RequireBinding)
	return snapshot
}

func selectedReadinessObservations(request ReadinessRequest) []DaemonReadinessObservationV1 {
	if len(request.Selectors) == 0 {
		return append([]DaemonReadinessObservationV1(nil), request.Observations...)
	}
	wanted := make(map[string]struct{}, len(request.Selectors))
	for _, selector := range request.Selectors {
		wanted[selector] = struct{}{}
	}
	out := make([]DaemonReadinessObservationV1, 0, len(request.Observations))
	for _, observation := range request.Observations {
		if _, ok := wanted[observation.TaskName]; ok {
			out = append(out, observation)
		}
	}
	return out
}

func reduceDaemonReadinessV1(observation DaemonReadinessObservationV1) (DaemonReadinessV1, []ReadinessFailureV1) {
	daemon := DaemonReadinessV1{
		TaskName: observation.TaskName, Server: observation.Server, Daemon: observation.Daemon,
		Port: observation.Port, PID: observation.PID, PIDGeneration: observation.ObservedPIDGeneration,
		ProcessState: observation.ProcessState,
	}
	failures := safeReadinessFailures(observation.Failures, observation)
	if len(failures) > 0 {
		failure := failures[0]
		daemon.Failure = &failure
		daemon.Stage = failure.Stage
		if failure.Stage == ReadinessStageIntentAccess || failure.Stage == ReadinessStageSupervisorBootstrap || failure.Stage == ReadinessStageSupervisorIPC || failure.Stage == ReadinessStageTaskIntent || failure.Stage == ReadinessStageWrapperStart {
			daemon.ServiceState = ServiceStateFailed
		} else {
			daemon.ServiceState = ServiceStateDegraded
		}
		daemon.Settled = true
		return daemon, failures
	}
	if !observation.IntentPresent {
		daemon.ServiceState, daemon.Stage, daemon.Settled = ServiceStateAbsent, ReadinessStageIntentAccess, true
		return daemon, nil
	}
	if observation.IntentDisabled {
		daemon.ServiceState, daemon.Stage, daemon.Settled = ServiceStateDisabled, ReadinessStageTaskIntent, true
		return daemon, nil
	}
	if observation.Stopped {
		daemon.ServiceState, daemon.Stage, daemon.Settled = ServiceStateStopped, ReadinessStageWrapperStart, true
		return daemon, nil
	}
	if !observation.IntentRunnable {
		daemon.ServiceState, daemon.Stage = ServiceStateStarting, ReadinessStageTaskIntent
		return settleOrDegrade(daemon, observation, "task_intent_missing")
	}
	if observation.ObservedPIDGeneration == 0 || observation.ObservedPIDGeneration != observation.CurrentPIDGeneration || !observation.WrapperStarted {
		daemon.ServiceState, daemon.Stage = ServiceStateStarting, ReadinessStageWrapperStart
		return settleOrDegrade(daemon, observation, "wrapper_start_timeout")
	}
	if observation.Policy == ReadinessPolicySyntheticProxy {
		daemon.ServiceState, daemon.Stage = ServiceStateStarting, ReadinessStageUpstreamListener
		return settleOrDegrade(daemon, observation, "synthetic_proxy_not_materialized")
	}
	if !observation.ListenerReady {
		daemon.ServiceState, daemon.Stage = ServiceStateStarting, ReadinessStageUpstreamListener
		return settleOrDegrade(daemon, observation, "upstream_listener_unbound")
	}
	if observation.Policy != ReadinessPolicyProcessOnly && !observation.MCPInitializeReady {
		daemon.ServiceState, daemon.Stage = ServiceStateStarting, ReadinessStageMCPInitialize
		return settleOrDegrade(daemon, observation, "mcp_initialize_failed")
	}
	if observation.Policy != ReadinessPolicyProcessOnly && !observation.MCPToolsListReady {
		daemon.ServiceState, daemon.Stage = ServiceStateStarting, ReadinessStageMCPToolsList
		return settleOrDegrade(daemon, observation, "mcp_tools_list_failed")
	}
	daemon.ServiceState, daemon.Stage, daemon.Settled = ServiceStateRunning, ReadinessStageComplete, true
	return daemon, nil
}

func settleOrDegrade(daemon DaemonReadinessV1, observation DaemonReadinessObservationV1, failureID string) (DaemonReadinessV1, []ReadinessFailureV1) {
	if !observation.DeadlineExceeded {
		return daemon, nil
	}
	failure := ReadinessFailureV1{Stage: daemon.Stage, FailureID: failureID, TaskName: daemon.TaskName, Port: daemon.Port}
	daemon.ServiceState, daemon.Settled, daemon.Failure = ServiceStateDegraded, true, &failure
	return daemon, []ReadinessFailureV1{failure}
}

func safeReadinessFailures(in []ReadinessFailureV1, observation DaemonReadinessObservationV1) []ReadinessFailureV1 {
	out := make([]ReadinessFailureV1, 0, len(in))
	for _, failure := range in {
		if !validReadinessStage(failure.Stage) || failure.FailureID == "" {
			continue
		}
		failure.Detail = ""
		if failure.TaskName == "" {
			failure.TaskName = observation.TaskName
		}
		if failure.Port == 0 {
			failure.Port = observation.Port
		}
		out = append(out, failure)
	}
	sortReadinessFailures(out)
	return out
}

func validReadinessStage(stage ReadinessStageV1) bool {
	return readinessStageOrder(stage) > 0 && stage != ReadinessStageComplete
}

func sortReadinessFailures(failures []ReadinessFailureV1) {
	sort.SliceStable(failures, func(i, j int) bool {
		left, right := readinessStageOrder(failures[i].Stage), readinessStageOrder(failures[j].Stage)
		if left != right {
			return left < right
		}
		if failures[i].TaskName != failures[j].TaskName {
			return failures[i].TaskName < failures[j].TaskName
		}
		return failures[i].Client < failures[j].Client
	})
}

func readinessStageOrder(stage ReadinessStageV1) int {
	switch stage {
	case ReadinessStageIntentAccess:
		return 1
	case ReadinessStageSupervisorBootstrap:
		return 2
	case ReadinessStageSupervisorIPC:
		return 3
	case ReadinessStageTaskIntent:
		return 4
	case ReadinessStageWrapperStart:
		return 5
	case ReadinessStageUpstreamListener:
		return 6
	case ReadinessStageMCPInitialize:
		return 7
	case ReadinessStageMCPToolsList:
		return 8
	case ReadinessStageClientBinding:
		return 9
	case ReadinessStageComplete:
		return 10
	default:
		return 0
	}
}

func deriveMaterializationState(daemons []DaemonReadinessV1) MaterializationStateV1 {
	if len(daemons) == 0 {
		return MaterializationAbsent
	}
	state := MaterializationAbsent
	for _, daemon := range daemons {
		next := MaterializationAbsent
		switch daemon.ServiceState {
		case ServiceStateRunning:
			next = MaterializationReady
		case ServiceStateDegraded:
			next = MaterializationDegraded
		case ServiceStateFailed:
			next = MaterializationFailed
		case ServiceStateStarting:
			if daemon.Stage == ReadinessStageTaskIntent {
				next = MaterializationIntentOnly
			} else {
				next = MaterializationStarting
			}
		case ServiceStateStopped:
			next = MaterializationStopped
		case ServiceStateDisabled:
			next = MaterializationDisabled
		}
		if materializationPrecedence(next) > materializationPrecedence(state) {
			state = next
		}
	}
	return state
}

func materializationPrecedence(state MaterializationStateV1) int {
	switch state {
	case MaterializationFailed:
		return 8
	case MaterializationDegraded:
		return 7
	case MaterializationStarting:
		return 6
	case MaterializationReady:
		return 5
	case MaterializationIntentOnly:
		return 4
	case MaterializationStopped:
		return 3
	case MaterializationDisabled:
		return 2
	default:
		return 1
	}
}

func deriveBindingState(bindings []BindingObservationV1) BindingStateV1 {
	states := map[BindingStateV1]struct{}{}
	for _, binding := range bindings {
		if !binding.Present {
			continue
		}
		if !binding.Readable {
			states[BindingStateUnreadable] = struct{}{}
			continue
		}
		if binding.Disabled || !binding.Enabled {
			states[BindingStateDisabled] = struct{}{}
			continue
		}
		switch binding.Route {
		case BindingRouteHub:
			states[BindingStateHub] = struct{}{}
		case BindingRouteDirect:
			states[BindingStateDirect] = struct{}{}
		default:
			states[BindingStateNone] = struct{}{}
		}
	}
	delete(states, BindingStateNone)
	if len(states) == 0 {
		return BindingStateNone
	}
	if len(states) > 1 {
		return BindingStateMixed
	}
	for state := range states {
		return state
	}
	return BindingStateNone
}

func deriveReadinessClassification(materialization MaterializationStateV1, binding BindingStateV1, canMigrate bool) ReadinessClassificationV1 {
	if materialization == MaterializationStarting || materialization == MaterializationDegraded || materialization == MaterializationFailed {
		return ReadinessClassificationUnhealthyDegraded
	}
	if materialization == MaterializationReady {
		if binding == BindingStateHub || binding == BindingStateMixed {
			return ReadinessClassificationInstalledAndBound
		}
		return ReadinessClassificationInstalledUnbound
	}
	if (binding == BindingStateDirect || binding == BindingStateMixed) && canMigrate {
		return ReadinessClassificationConfiguredDirectCanMigrate
	}
	if binding == BindingStateDisabled {
		return ReadinessClassificationDisabled
	}
	return ReadinessClassificationGenuinelyNotInstalled
}

func readinessSnapshotSettled(snapshot ReadinessSnapshotV1, requireBinding bool) bool {
	if snapshot.PrimaryFailure != nil {
		return false
	}
	for _, daemon := range snapshot.Daemons {
		if daemon.ServiceState != ServiceStateRunning {
			return false
		}
	}
	if !requireBinding {
		return true
	}
	for _, binding := range snapshot.Bindings {
		if binding.Requested && (!binding.Present || !binding.Readable || !binding.Enabled || binding.Disabled || binding.Route != BindingRouteHub || !binding.ExactHubRoute) {
			return false
		}
	}
	return true
}

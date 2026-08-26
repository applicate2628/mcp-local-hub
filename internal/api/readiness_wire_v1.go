package api

// ReadinessObservationWireV1 is the additive supervisor IPC representation.
// It transports raw current-generation facts; it never transports a derived
// service state or scan classification.
type ReadinessObservationWireV1 struct {
	TaskName              string               `json:"task_name"`
	Server                string               `json:"server,omitempty"`
	Daemon                string               `json:"daemon,omitempty"`
	Port                  int                  `json:"port,omitempty"`
	PID                   int                  `json:"pid,omitempty"`
	ProcessState          string               `json:"process_state,omitempty"`
	CurrentPIDGeneration  uint64               `json:"current_pid_generation,omitempty"`
	ObservedPIDGeneration uint64               `json:"observed_pid_generation,omitempty"`
	IntentPresent         bool                 `json:"intent_present,omitempty"`
	IntentRunnable        bool                 `json:"intent_runnable,omitempty"`
	IntentDisabled        bool                 `json:"intent_disabled,omitempty"`
	Stopped               bool                 `json:"stopped,omitempty"`
	WrapperStarted        bool                 `json:"wrapper_started,omitempty"`
	ListenerReady         bool                 `json:"listener_ready,omitempty"`
	MCPInitializeReady    bool                 `json:"mcp_initialize_ready,omitempty"`
	MCPToolsListReady     bool                 `json:"mcp_tools_list_ready,omitempty"`
	DeadlineExceeded      bool                 `json:"deadline_exceeded,omitempty"`
	Policy                ReadinessPolicyV1    `json:"policy,omitempty"`
	Failures              []ReadinessFailureV1 `json:"failures,omitempty"`
}

func EncodeReadinessObservationV1(in *DaemonReadinessObservationV1) *ReadinessObservationWireV1 {
	if in == nil {
		return nil
	}
	return &ReadinessObservationWireV1{TaskName: in.TaskName, Server: in.Server, Daemon: in.Daemon, Port: in.Port, PID: in.PID, ProcessState: in.ProcessState, CurrentPIDGeneration: in.CurrentPIDGeneration, ObservedPIDGeneration: in.ObservedPIDGeneration, IntentPresent: in.IntentPresent, IntentRunnable: in.IntentRunnable, IntentDisabled: in.IntentDisabled, Stopped: in.Stopped, WrapperStarted: in.WrapperStarted, ListenerReady: in.ListenerReady, MCPInitializeReady: in.MCPInitializeReady, MCPToolsListReady: in.MCPToolsListReady, DeadlineExceeded: in.DeadlineExceeded, Policy: in.Policy, Failures: append([]ReadinessFailureV1(nil), in.Failures...)}
}

func (w *ReadinessObservationWireV1) Decode() *DaemonReadinessObservationV1 {
	if w == nil {
		return nil
	}
	return &DaemonReadinessObservationV1{TaskName: w.TaskName, Server: w.Server, Daemon: w.Daemon, Port: w.Port, PID: w.PID, ProcessState: w.ProcessState, CurrentPIDGeneration: w.CurrentPIDGeneration, ObservedPIDGeneration: w.ObservedPIDGeneration, IntentPresent: w.IntentPresent, IntentRunnable: w.IntentRunnable, IntentDisabled: w.IntentDisabled, Stopped: w.Stopped, WrapperStarted: w.WrapperStarted, ListenerReady: w.ListenerReady, MCPInitializeReady: w.MCPInitializeReady, MCPToolsListReady: w.MCPToolsListReady, DeadlineExceeded: w.DeadlineExceeded, Policy: w.Policy, Failures: append([]ReadinessFailureV1(nil), w.Failures...)}
}

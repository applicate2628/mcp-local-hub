package api

// IPCRequest is the JSON wire frame for one client → supervisor command.
// Spec §"Control IPC trust boundary (detail)".
type IPCRequest struct {
	ID   int64          `json:"id"`
	Cmd  string         `json:"cmd"` // status|reload|restart|exit|quiesce-timers
	Args map[string]any `json:"args,omitempty"`
}

// IPCResponse is the JSON wire frame for one supervisor → client response.
// `Final` is true on the final frame for multi-frame commands like
// quiesce-timers (immediate {accepted:true} then later {drained,
// still_running, final:true}).
type IPCResponse struct {
	ID     int64   `json:"id"`
	OK     bool    `json:"ok,omitempty"`
	Result any     `json:"result,omitempty"`
	Error  *IPCErr `json:"error,omitempty"`
	Final  bool    `json:"final,omitempty"`
}

type IPCErr struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// IPCHello is the connection-handshake frame supervisor sends first.
// Client compares against pre-read supervisor.lock owner; mismatch
// closes connection.
type IPCHello struct {
	Version   int    `json:"version"`
	PID       int    `json:"pid"`
	StartedAt string `json:"started_at"`
}

// ValidateHandshake returns true iff hello.PID == expected.PID AND
// hello.StartedAt == expected.StartedAt. Used by IPC clients after
// reading supervisor.lock for the expected owner.
func ValidateHandshake(hello IPCHello, expected SupervisorLockOwner) bool {
	return hello.PID == expected.PID && hello.StartedAt == expected.StartedAt
}

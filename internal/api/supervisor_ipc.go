package api

// IPCRequest is the JSON wire frame for one client → supervisor command.
// Spec §"Control IPC trust boundary (detail)".
//
// Version is the request-envelope schema version. Zero (the JSON-omitted
// default) is treated as v1 for backward compatibility with clients that
// predate the explicit-version requirement; an explicit Version != 1
// MUST be refused by the dispatcher per
// `ValidateRequestEnvelope` so a future v2 schema doesn't silently
// downgrade against a v1-only supervisor.
type IPCRequest struct {
	Version int            `json:"version,omitempty"`
	ID      int64          `json:"id"`
	Cmd     string         `json:"cmd"` // status|reload|restart|exit|quiesce-timers
	Args    map[string]any `json:"args,omitempty"`
}

// IPCCommandIsReadOnly is the single owner of "read-only supervisor IPC command".
// Fail-safe allowlist: true ONLY for enumerated pure-query commands (answered in the
// pre-reconcileReady-gate dispatch switch); every other command — including unknown/
// future verbs — is NOT read-only and keeps its audit row. See
// work-items/decisions/2026-07-19-ipc-audit-readonly-allowlist.md.
func IPCCommandIsReadOnly(cmd string) bool {
	switch cmd {
	case "status":
		return true
	default:
		return false
	}
}

// ValidateRequestEnvelope returns nil iff req.Version is 0 (treated as
// v1 default) or 1. Any other version returns an error so the IPC
// dispatcher can refuse the frame with a structured error response
// rather than honoring an unknown wire shape. Spec §"Wire format" line
// 498 + "Pin Version == 1 validation".
func ValidateRequestEnvelope(req IPCRequest) error {
	if req.Version == 0 || req.Version == 1 {
		return nil
	}
	return &IPCErr{
		Code:    "UNSUPPORTED_PROTOCOL_VERSION",
		Message: "request envelope version not supported; this supervisor accepts version=1 only",
	}
}

// Error returns Message so IPCErr satisfies the error interface, which
// lets ValidateRequestEnvelope return its sentinel as a typed error
// without an extra wrapper. Callers that need the structured code can
// type-assert via errors.As.
func (e *IPCErr) Error() string {
	if e == nil {
		return ""
	}
	return e.Code + ": " + e.Message
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
	Code      string `json:"code"`
	Message   string `json:"message"`
	Retryable bool   `json:"retryable,omitempty"`
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

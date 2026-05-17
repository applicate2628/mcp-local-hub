package process

import "errors"

// ErrProcessAlreadyExited marks a terminate request that lost a race
// because the target PID was already gone by the time the signal ran.
var ErrProcessAlreadyExited = errors.New("process already exited")

// ErrProcessIdentityMismatch marks a PID that exists but no longer matches the
// executable/start-time identity recorded by supervisor-state.json.
var ErrProcessIdentityMismatch = errors.New("process identity mismatch")

// PIDState is a tri-state liveness probe. Unknown means the OS refused or
// failed the probe, so callers must not collapse it to "already exited".
type PIDState string

const (
	PIDStateAlive   PIDState = "alive"
	PIDStateDead    PIDState = "dead"
	PIDStateUnknown PIDState = "unknown"
)

// PIDIdentityProof is the supervisor's stale-PID guard for destructive
// operations. ExecutablePath is the canonical mcphub binary path expected for
// the target PID, and StartedAt is the RFC3339Nano timestamp persisted in
// supervisor-state.json for that PID generation.
type PIDIdentityProof struct {
	PID            int
	ExecutablePath string
	StartedAt      string
}

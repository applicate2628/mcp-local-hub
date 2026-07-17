package process

import (
	"errors"
	"time"
)

// ErrProcessAlreadyExited marks a terminate request that lost a race
// because the target PID was already gone by the time the signal ran.
var ErrProcessAlreadyExited = errors.New("process already exited")

// ErrProcessIdentityMismatch marks a PID that exists but no longer matches the
// executable/start-time identity recorded by supervisor-state.json.
var ErrProcessIdentityMismatch = errors.New("process identity mismatch")

// ErrProcessIdentityUnsupported marks platforms where this package cannot yet
// verify executable/start-time identity for a PID generation.
var ErrProcessIdentityUnsupported = errors.New("process identity proof unsupported")

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
	// CommandLine carries the argv evidence used by callers that must prove a
	// specific process role. The generic identity primitive cannot query argv
	// from an OS handle, so callers remain responsible for validating it while
	// the matching process generation is held.
	CommandLine string
	StartedAt   string
	// StartTolerance optionally tightens or widens the start-time proof for a
	// specific destructive call. Zero keeps the package default used by existing
	// supervisor call sites.
	StartTolerance time.Duration
}

// HeldPIDGeneration is an open operating-system reference to one PID
// generation. Holding it across classification and termination prevents a PID
// reused by a later process from inheriting the destructive authority.
//
// Terminate reports committed=true once the OS accepted the termination, even
// if the subsequent bounded wait fails. Callers that must finish a recovery
// after the point of no return can distinguish that state from a refused kill.
type HeldPIDGeneration interface {
	PID() int
	VerifyIdentity(PIDIdentityProof) error
	Terminate() (committed bool, err error)
	Close() error
}

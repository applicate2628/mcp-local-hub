package api

import (
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

const (
	// SupervisorCstIdentityCommandV1 is the sole supervisor status opcode
	// available to the CST daemon service identity. It has no task selector:
	// the task is always the implicit SupervisorCstTaskV1 value.
	SupervisorCstIdentityCommandV1 = "GET_CURRENT_CST_TASK_IDENTITY_V1"
	SupervisorCstTaskV1            = "cst"

	// Windows mandatory integrity RIDs used by the pure authorization core.
	SupervisorIntegrityMedium uint32 = 0x2000
	SupervisorIntegrityHigh   uint32 = 0x3000
	SupervisorIntegritySystem uint32 = 0x4000
)

// SupervisorProcessIdentityV1 is a kernel-observed process proof. Callers must
// populate it from the connected named-pipe endpoint and process/token handles;
// no field is accepted from an IPC frame.
type SupervisorProcessIdentityV1 struct {
	PID           int
	CreationTime  string
	UserSID       string
	SessionID     uint32
	IntegrityRID  uint32
	ImagePath     string
	SCMServicePID int
}

// SupervisorCstTaskIdentityV1 is the complete, closed response body required
// by HubEnrollmentProtocolV1. It intentionally contains no command, args,
// workspace, port, policy, source path, or caller-selected task field.
type SupervisorCstTaskIdentityV1 struct {
	Task          string `json:"task"`
	PID           int    `json:"pid"`
	PIDGeneration int    `json:"pid_generation"`
	CreationTime  string `json:"creation_time"`
}

// SupervisorCstIdentityPolicyV1 is resolved by supervisor composition from the
// fixed SCM service descriptor. The numeric SID and image path are expected to
// come from LookupAccountName/QueryServiceConfig, not request data.
type SupervisorCstIdentityPolicyV1 struct {
	DaemonServiceSID string
	DaemonImagePath  string
	DaemonSessionID  uint32
	MinimumIntegrity uint32
}

// SupervisorCstIdentityAuthorizerV1 owns the status-only, pre-generic-dispatch
// authorization branch. Successful request IDs are consumed for this
// supervisor process lifetime; a replay cannot reach task-state dispatch.
type SupervisorCstIdentityAuthorizerV1 struct {
	policy SupervisorCstIdentityPolicyV1
	mu     sync.Mutex
	seen   map[int64]struct{}
}

func NewSupervisorCstIdentityAuthorizerV1(policy SupervisorCstIdentityPolicyV1) *SupervisorCstIdentityAuthorizerV1 {
	return &SupervisorCstIdentityAuthorizerV1{policy: policy, seen: make(map[int64]struct{})}
}

// Authorize validates the closed opcode, kernel peer proof and immutable copy
// of the current task rows before returning the one exact CST identity. It does
// not mutate supervisor task state and never calls generic command dispatch.
func (a *SupervisorCstIdentityAuthorizerV1) Authorize(req IPCRequest, peer SupervisorProcessIdentityV1, current []SupervisorCstTaskIdentityV1) (SupervisorCstTaskIdentityV1, error) {
	var zero SupervisorCstTaskIdentityV1
	if a == nil {
		return zero, fmt.Errorf("supervisor CST identity authorizer unavailable")
	}
	if req.Version != 1 || req.ID <= 0 || req.Cmd != SupervisorCstIdentityCommandV1 || len(req.Args) != 0 {
		return zero, fmt.Errorf("supervisor CST identity request denied")
	}
	if err := validateSupervisorCstDaemonIdentityV1(a.policy, peer); err != nil {
		return zero, err
	}

	var selected SupervisorCstTaskIdentityV1
	matches := 0
	for _, row := range current {
		if row.Task != SupervisorCstTaskV1 {
			continue
		}
		matches++
		selected = row
	}
	if matches != 1 || selected.PID <= 0 || selected.PIDGeneration <= 0 {
		return zero, fmt.Errorf("current CST task identity unavailable or ambiguous")
	}
	if _, err := parseSupervisorIdentityTime(selected.CreationTime); err != nil {
		return zero, fmt.Errorf("current CST task creation time invalid: %w", err)
	}

	a.mu.Lock()
	defer a.mu.Unlock()
	if _, exists := a.seen[req.ID]; exists {
		return zero, fmt.Errorf("supervisor CST identity request replayed")
	}
	a.seen[req.ID] = struct{}{}
	return selected, nil
}

func validateSupervisorCstDaemonIdentityV1(policy SupervisorCstIdentityPolicyV1, peer SupervisorProcessIdentityV1) error {
	if policy.DaemonServiceSID == "" || policy.DaemonImagePath == "" || policy.MinimumIntegrity < SupervisorIntegrityHigh {
		return fmt.Errorf("supervisor CST identity policy incomplete")
	}
	if peer.PID <= 0 || peer.SCMServicePID != peer.PID {
		return fmt.Errorf("CST daemon SCM process identity mismatch")
	}
	if _, err := parseSupervisorIdentityTime(peer.CreationTime); err != nil {
		return fmt.Errorf("CST daemon creation time invalid: %w", err)
	}
	if peer.UserSID == "" || !strings.EqualFold(peer.UserSID, policy.DaemonServiceSID) {
		return fmt.Errorf("CST daemon service SID mismatch")
	}
	if peer.SessionID != policy.DaemonSessionID {
		return fmt.Errorf("CST daemon session mismatch")
	}
	if peer.IntegrityRID < policy.MinimumIntegrity {
		return fmt.Errorf("CST daemon integrity level too low")
	}
	if !supervisorImagePathEqual(peer.ImagePath, policy.DaemonImagePath) {
		return fmt.Errorf("CST daemon image mismatch")
	}
	return nil
}

// ValidateSupervisorStatusServerIdentityV1 binds a status client to the exact
// server process named by SupervisorLockOwner and to independently observed
// token/session/canonical-image facts. The hello frame is not sufficient.
func ValidateSupervisorStatusServerIdentityV1(owner SupervisorLockOwner, observed SupervisorProcessIdentityV1, expectedUserSID string, expectedSessionID uint32, expectedImagePath string) error {
	if owner.PID <= 0 || observed.PID != owner.PID {
		return fmt.Errorf("supervisor PID mismatch")
	}
	wantCreated, err := parseSupervisorIdentityTime(owner.StartedAt)
	if err != nil {
		return fmt.Errorf("supervisor lock creation time invalid: %w", err)
	}
	gotCreated, err := parseSupervisorIdentityTime(observed.CreationTime)
	if err != nil || !gotCreated.Equal(wantCreated) {
		return fmt.Errorf("supervisor creation time mismatch")
	}
	if expectedUserSID == "" || observed.UserSID == "" || !strings.EqualFold(observed.UserSID, expectedUserSID) {
		return fmt.Errorf("supervisor token user mismatch")
	}
	if observed.SessionID != expectedSessionID {
		return fmt.Errorf("supervisor session mismatch")
	}
	if !supervisorImagePathEqual(observed.ImagePath, expectedImagePath) {
		return fmt.Errorf("supervisor image mismatch")
	}
	return nil
}

func parseSupervisorIdentityTime(raw string) (time.Time, error) {
	if raw == "" {
		return time.Time{}, fmt.Errorf("missing time")
	}
	t, err := time.Parse(time.RFC3339Nano, raw)
	if err != nil {
		return time.Time{}, err
	}
	return t.UTC(), nil
}

func supervisorImagePathEqual(got, want string) bool {
	if got == "" || want == "" {
		return false
	}
	return strings.EqualFold(filepath.Clean(got), filepath.Clean(want))
}

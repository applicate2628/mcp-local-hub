package api

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"time"
)

type StateReadPolicySnapshotV1 struct {
	Schema                string                   `json:"schema"`
	Version               uint32                   `json:"version"`
	EffectiveStrict       bool                     `json:"effective_strict"`
	EnvIdentitySHA256     string                   `json:"env_identity_sha256"`
	IntentState           string                   `json:"intent_state"`
	IntentIdentitySHA256  *string                  `json:"intent_identity_sha256"`
	IntentContentSHA256   *string                  `json:"intent_content_sha256"`
	BootstrapReadComplete bool                     `json:"bootstrap_read_complete"`
	ResourcesComplete     bool                     `json:"resources_complete"`
	FirstCause            *StateFileReadFailureV1  `json:"first_cause"`
	CleanupFailures       []StateFileReadFailureV1 `json:"cleanup_failures"`
}

type StateReadPolicyObservation struct {
	issuer          *stateFileReadIssuer
	view            StateReadPolicySnapshotV1
	bootstrap       *StateFileReadObservation
	resolution      *StateDirResolutionObservation
	stateDir        string
	deadline        time.Time
	deadlineDefined bool
}

func StateReadPolicyEnvironmentIdentity() string {
	return stateFileReadObservationDigest([]byte(os.Getenv(RequireSingleUserHomeEnv)))
}

func (p *StateReadPolicyObservation) Snapshot() StateReadPolicySnapshotV1 {
	if p == nil {
		return StateReadPolicySnapshotV1{}
	}
	view := p.view
	if view.IntentIdentitySHA256 != nil {
		value := *view.IntentIdentitySHA256
		view.IntentIdentitySHA256 = &value
	}
	if view.IntentContentSHA256 != nil {
		value := *view.IntentContentSHA256
		view.IntentContentSHA256 = &value
	}
	if view.FirstCause != nil {
		value := *view.FirstCause
		view.FirstCause = &value
	}
	view.CleanupFailures = append(make([]StateFileReadFailureV1, 0, len(view.CleanupFailures)), view.CleanupFailures...)
	return view
}

func (p *StateReadPolicyObservation) Validate(ctx context.Context, effectiveEnvIdentity string) error {
	if p == nil || p.issuer == nil || ctx == nil || !p.view.ResourcesComplete || !p.view.BootstrapReadComplete || p.view.FirstCause != nil || p.view.EnvIdentitySHA256 != effectiveEnvIdentity || effectiveEnvIdentity != StateReadPolicyEnvironmentIdentity() {
		return errors.New("original resource-complete state read policy required")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := p.resolution.Validate(ctx, p.stateDir); err != nil {
		return err
	}
	if p.deadlineDefined {
		deadline, ok := ctx.Deadline()
		if !ok || deadline.After(p.deadline) {
			return errors.New("policy receiving deadline renewed")
		}
	}
	return nil
}

// This is the same policy owner and same ENV-only/no-audit bootstrap core.
// It never promotes the legacy process scalar cache into a close observation.
func ResolveStateReadPolicyObserved(ctx context.Context) (policy *StateReadPolicyObservation, retErr error) {
	environment := os.Getenv(RequireSingleUserHomeEnv)
	value := strings.ToLower(strings.TrimSpace(environment))
	envStrict := value == "1" || value == "true"
	policy = &StateReadPolicyObservation{issuer: &stateFileReadIssuer{marker: 1}, view: StateReadPolicySnapshotV1{Schema: "state-read-policy-observation-v1", Version: 1, EffectiveStrict: true, EnvIdentitySHA256: stateFileReadObservationDigest([]byte(environment)), IntentState: "anomalous", CleanupFailures: make([]StateFileReadFailureV1, 0)}}
	if ctx == nil {
		policy.view.FirstCause = &StateFileReadFailureV1{Code: "POLICY_CONTEXT_REQUIRED", Stage: "input", Slot: ""}
		return policy, errors.New("original policy context required")
	}
	policy.deadline, policy.deadlineDefined = ctx.Deadline()
	if err := ctx.Err(); err != nil {
		policy.view.FirstCause = &StateFileReadFailureV1{Code: "POLICY_CONTEXT_FAILED", Stage: "input", Slot: ""}
		return policy, err
	}
	stateDir, resolution, err := ResolveDaemonStateDirObserved(ctx)
	policy.resolution = resolution
	policy.stateDir = stateDir
	if err != nil {
		policy.view.FirstCause = &StateFileReadFailureV1{Code: "POLICY_NAMESPACE_UNAVAILABLE", Stage: "resolve", Slot: ""}
		return policy, err
	}
	if err := resolution.Validate(ctx, stateDir); err != nil {
		policy.view.FirstCause = &StateFileReadFailureV1{Code: "POLICY_NAMESPACE_CLOSURE_UNKNOWN", Stage: "resolve", Slot: "known-folder"}
		return policy, err
	}
	path := joinStateFilePath(stateDir, supervisorIntentFileLeaf)
	b := newStateFileReadBuilder(ctx, path, stateFileReadCapBytes(path))
	b.view.ReadAttempted = true
	// The bootstrap receives this captured ENV-only policy directly. It never
	// enters the persisted policy resolver or a scalar legacy cache recursively.
	b.view.PolicyQualified = true
	raw, bodyErr := readStateFileInodeAnchoredWithObservation(b.path, func() bool { return envStrict }, b.maxBytes, false, false, LogHubMcpEvent, nil, ctx, b)
	raw, observation, readErr := b.finish(raw, bodyErr)
	policy.bootstrap = observation
	view := observation.Snapshot()
	policy.view.IntentIdentitySHA256 = view.LeafIdentitySHA256
	policy.view.IntentContentSHA256 = view.ContentSHA256
	policy.view.CleanupFailures = append(policy.view.CleanupFailures, view.CleanupErrors...)
	policy.view.ResourcesComplete = view.OwnedResourcesComplete && view.BorrowedResourcesComplete
	if view.Absence == string(StateFileReadParentNotFound) || view.Absence == string(StateFileReadLeafNotFound) {
		if err := observation.ValidateAbsence(ctx, path, StateFileReadAbsence(view.Absence)); err != nil {
			policy.view.FirstCause = &StateFileReadFailureV1{Code: "POLICY_ABSENCE_UNQUALIFIED", Stage: "read", Slot: ""}
			return policy, errors.Join(readErr, err)
		}
		policy.view.IntentState = "absent"
		policy.view.EffectiveStrict = envStrict
		policy.view.BootstrapReadComplete = true
		if err := policy.Validate(ctx, policy.view.EnvIdentitySHA256); err != nil {
			return policy, err
		}
		return policy, nil
	}
	if readErr != nil {
		policy.view.FirstCause = &StateFileReadFailureV1{Code: "POLICY_BOOTSTRAP_READ_FAILED", Stage: "read", Slot: ""}
		return policy, readErr
	}
	defer zeroStateSecretBytes(raw)
	var intent SupervisorIntentFile
	if err := json.Unmarshal(raw, &intent); err != nil {
		policy.view.FirstCause = &StateFileReadFailureV1{Code: "POLICY_INTENT_PARSE_FAILED", Stage: "parse", Slot: ""}
		return policy, err
	}
	policy.view.IntentState = "parsed"
	policy.view.EffectiveStrict = envStrict || intent.StrictMode
	policy.view.BootstrapReadComplete = view.Complete
	if err := policy.Validate(ctx, policy.view.EnvIdentitySHA256); err != nil {
		return policy, err
	}
	return policy, nil
}

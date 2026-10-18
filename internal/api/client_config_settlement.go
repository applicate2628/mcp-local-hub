package api

import (
	"errors"
	"fmt"
	"path/filepath"

	"mcp-local-hub/internal/clients"
)

const (
	clientConfigSettlementSchemaV1 = "client-config-settlement-v1"
	clientConfigSettlementSchemaV2 = "client-config-settlement-v2"
)

var (
	ErrClientConfigSettlementInvalid     = errors.New("CLIENT_CONFIG_SETTLEMENT_INVALID")
	ErrClientConfigSettlementEventFailed = errors.New("CLIENT_CONFIG_SETTLEMENT_EVENT_FAILED")
)

type ClientConfigOperationV1 string

const (
	ClientConfigOperationInstall ClientConfigOperationV1 = "install"
	ClientConfigOperationAdopt   ClientConfigOperationV1 = "adopt"
)

// ClientConfigSettlementV1 is the portable exact-readback outcome of one
// client-config mutation. It contains logical identities and classes only.
type ClientConfigSettlementV1 struct {
	SchemaVersion         string `json:"schema_version"`
	Operation             string `json:"operation"`
	Phase                 string `json:"phase"`
	Client                string `json:"client"`
	LogicalSource         string `json:"logical_source"`
	TargetEntry           string `json:"target_entry"`
	WriteTarget           string `json:"write_target"`
	DesiredTransport      string `json:"desired_transport"`
	CollisionReason       string `json:"collision_reason,omitempty"`
	Action                string `json:"action"`
	Outcome               string `json:"outcome"`
	Readback              string `json:"readback"`
	ProjectIdentitySHA256 string `json:"project_identity_sha256,omitempty"`
}

// IsCommittedWrite reports whether this exact-readback receipt may be exposed
// as a settled client-config event. It intentionally does not constrain the
// logical source and target names to be equal: a collision-resolved target
// (such as the Codex H4 alias) is the receipt's truthful identity.
func (s ClientConfigSettlementV1) IsCommittedWrite() bool {
	return s.isValid() && s.Outcome == "committed"
}

// IsSettled reports whether the row was produced by exact client-config
// readback. It is independent of the wider install/adopt outcome.
func (s ClientConfigSettlementV1) IsSettled() bool {
	return s.isValid() && (s.Outcome == "committed" || s.Outcome == "already_configured")
}

func (s ClientConfigSettlementV1) isValid() bool {
	common := (s.Operation == string(ClientConfigOperationInstall) || s.Operation == string(ClientConfigOperationAdopt)) &&
		s.Phase == "settled" && s.Client == "codex-cli" && s.LogicalSource != "" && s.TargetEntry != "" &&
		s.DesiredTransport == string(clients.CodexTransportHTTP) && s.Readback == string(clients.CodexHTTPRelocationReadbackExact)
	if !common {
		return false
	}
	if s.SchemaVersion == clientConfigSettlementSchemaV1 {
		return s.WriteTarget == "codex_global" && s.CollisionReason == "cross_layer_opposite_transport" &&
			s.Action == "relocate" && s.ProjectIdentitySHA256 == ""
	}
	_, validFingerprint := decodeLowerProjectAdoptSHA256(s.ProjectIdentitySHA256)
	return s.SchemaVersion == clientConfigSettlementSchemaV2 && s.WriteTarget == "codex_project" &&
		s.CollisionReason == "" && s.Action == "replace" && validFingerprint
}

type clientConfigSettlementEmitter func(ClientConfigSettlementV1) error

// ClientConfigSettlementOwnerV1 is the sole API boundary that translates an
// exact Codex lower result and publishes the generic settlement event.
type ClientConfigSettlementOwnerV1 struct {
	emit clientConfigSettlementEmitter
}

func NewClientConfigSettlementOwnerV1() ClientConfigSettlementOwnerV1 {
	return ClientConfigSettlementOwnerV1{emit: emitClientConfigSettled}
}

// Accept validates every lower fact before creating an API row. A committed
// mutation emits exactly once; an event failure retains the valid row and
// fails loudly so callers can preserve evidence but must withhold success.
func (o ClientConfigSettlementOwnerV1) Accept(operation ClientConfigOperationV1, lower clients.CodexHTTPRelocationResult) (ClientConfigSettlementV1, error) {
	return o.accept(operation, lower, "")
}

func (o ClientConfigSettlementOwnerV1) AcceptProject(operation ClientConfigOperationV1, lower clients.CodexHTTPRelocationResult, projectIdentitySHA256 string) (ClientConfigSettlementV1, error) {
	return o.accept(operation, lower, projectIdentitySHA256)
}

func (o ClientConfigSettlementOwnerV1) accept(operation ClientConfigOperationV1, lower clients.CodexHTTPRelocationResult, projectIdentitySHA256 string) (ClientConfigSettlementV1, error) {
	project := projectIdentitySHA256 != ""
	validShape := lower.WriteTarget == "codex_global" && lower.CollisionReason == "cross_layer_opposite_transport" && lower.Action == "relocate"
	schemaVersion := clientConfigSettlementSchemaV1
	if project {
		_, validFingerprint := decodeLowerProjectAdoptSHA256(projectIdentitySHA256)
		validShape = validFingerprint && lower.WriteTarget == "codex_project" && lower.CollisionReason == "" && lower.Action == "replace"
		schemaVersion = clientConfigSettlementSchemaV2
	}
	if operation != ClientConfigOperationInstall && operation != ClientConfigOperationAdopt || lower.LogicalSource == "" || lower.TargetEntry == "" ||
		lower.DesiredTransport != clients.CodexTransportHTTP || lower.Readback != clients.CodexHTTPRelocationReadbackExact || !validShape ||
		(lower.Outcome != clients.CodexHTTPRelocationCommitted && lower.Outcome != clients.CodexHTTPRelocationAlreadyConfigured) {
		return ClientConfigSettlementV1{}, ErrClientConfigSettlementInvalid
	}
	row := ClientConfigSettlementV1{
		SchemaVersion: schemaVersion, Operation: string(operation), Phase: "settled",
		Client: "codex-cli", LogicalSource: lower.LogicalSource, TargetEntry: lower.TargetEntry,
		WriteTarget: lower.WriteTarget, DesiredTransport: string(lower.DesiredTransport),
		CollisionReason: lower.CollisionReason, Action: lower.Action, Outcome: string(lower.Outcome),
		Readback: string(lower.Readback), ProjectIdentitySHA256: projectIdentitySHA256,
	}
	if lower.Outcome != clients.CodexHTTPRelocationCommitted {
		return row, nil
	}
	emit := o.emit
	if emit == nil {
		emit = emitClientConfigSettled
	}
	if err := emit(row); err != nil {
		return row, fmt.Errorf("%w: %v", ErrClientConfigSettlementEventFailed, err)
	}
	return row, nil
}

// emitClientConfigSettled is the generic downstream event producer. The
// allowlisted portable row is the only input; no config bytes or paths cross
// the client/API boundary. It performs no retry because the append may have
// reached durable storage before returning an error.
func emitClientConfigSettled(settlement ClientConfigSettlementV1) error {
	stateDir, err := DaemonStateDir()
	if err != nil {
		return err
	}
	logger, err := OpenSupervisorEventLog(filepath.Join(stateDir, SupervisorEventLogFileLeaf))
	if err != nil {
		return err
	}
	body := map[string]any{
		"schema_version": settlement.SchemaVersion, "operation": settlement.Operation,
		"phase": settlement.Phase, "client": settlement.Client,
		"logical_source": settlement.LogicalSource, "target_entry": settlement.TargetEntry,
		"write_target": settlement.WriteTarget, "desired_transport": settlement.DesiredTransport,
		"collision_reason": settlement.CollisionReason, "action": settlement.Action,
		"outcome": settlement.Outcome, "readback": settlement.Readback,
	}
	if settlement.ProjectIdentitySHA256 != "" {
		body["project_identity_sha256"] = settlement.ProjectIdentitySHA256
	}
	emitErr := logger.Emit(SupervisorEvent{
		SchemaVersion: SupervisorEventSchemaVersion,
		Severity:      SupervisorEventSeverityInfo,
		Source:        settlement.Operation,
		Event:         "client-config-settled",
		Body:          body,
	})
	if closeErr := logger.Close(); closeErr != nil && emitErr == nil {
		return closeErr
	}
	return emitErr
}

package kimi

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
)

const (
	providerResultPrefix   = "ORCHESTRARIUM_PROVIDER_RESULT_V2="
	providerResultSchemaV2 = "orchestrarium.provider-result.v2"
)

var errReceiptInvalid = errors.New("invalid Kimi provider receipt")

type providerReceipt struct {
	Payload           string
	Provider          string
	Model             string
	Effort            string
	ResultText        string
	ExitCode          int
	Status            string
	Gate              string
	Cancelled         bool
	TimedOut          bool
	CleanupStatus     string
	CleanupIssueCount int
}

func (r providerReceipt) IsError() bool {
	return r.ExitCode != 0 || r.Status != "completed" || r.Gate != "PASS" || r.Cancelled || r.TimedOut ||
		r.CleanupStatus != "complete" || r.CleanupIssueCount != 0
}

type receiptEnvelope struct {
	Schema                          string          `json:"schema"`
	Provider                        string          `json:"provider"`
	Model                           string          `json:"model"`
	Effort                          string          `json:"effort"`
	ResultText                      *string         `json:"resultText"`
	ExitCode                        *int            `json:"exitCode"`
	Status                          string          `json:"status"`
	Gate                            string          `json:"gate"`
	Cancelled                       *bool           `json:"cancelled"`
	TimedOut                        *bool           `json:"timedOut"`
	CleanupStatus                   string          `json:"cleanupStatus"`
	CleanupIssueCount               *int            `json:"cleanupIssueCount"`
	PrimaryOutcome                  json.RawMessage `json:"primaryOutcome"`
	LaunchFlags                     json.RawMessage `json:"launchFlags"`
	Selected                        json.RawMessage `json:"selected"`
	Observed                        json.RawMessage `json:"observed"`
	Authorizing                     *bool           `json:"authorizing"`
	ClosesRunIDs                    []string        `json:"closesRunIds"`
	IndependentVerificationRequired *bool           `json:"independentVerificationRequired"`
	TerminalClass                   string          `json:"terminalClass"`
	ActualExecutionPath             string          `json:"actualExecutionPath"`
	AssignedRole                    string          `json:"assignedRole"`
	ExecutionRole                   string          `json:"executionRole"`
}

type receiptPrimaryOutcome struct {
	ExitCode *int   `json:"exitCode"`
	Status   string `json:"status"`
	Gate     string `json:"gate"`
}

type receiptSelected struct {
	Tools       []string `json:"tools"`
	MCPNames    []string `json:"mcpNames"`
	Subagents   []string `json:"subagents"`
	Permission  string   `json:"permission"`
	CWDSelected *bool    `json:"cwdSelected"`
}

type receiptObserved struct {
	ToolCalls           []json.RawMessage `json:"toolCalls"`
	PermissionDecisions []json.RawMessage `json:"permissionDecisions"`
	ObservationsOmitted *bool             `json:"observationsOmitted,omitempty"`
}

type receiptToolCall struct {
	ID     string `json:"id"`
	Title  string `json:"title"`
	Status string `json:"status"`
}

type receiptPermissionDecision struct {
	ID       string `json:"id"`
	Title    string `json:"title"`
	Decision string `json:"decision"`
}

func parseProviderReceipt(record []byte, requestedEffort, requestedRole string, capabilityChoice CapabilityChoice) (providerReceipt, error) {
	if bytes.ContainsRune(record, '\r') || len(record) == 0 || record[len(record)-1] != '\n' || bytes.Count(record, []byte{'\n'}) != 1 {
		return providerReceipt{}, errReceiptInvalid
	}
	line := record[:len(record)-1]
	if !bytes.HasPrefix(line, []byte(providerResultPrefix)) {
		return providerReceipt{}, errReceiptInvalid
	}
	payload := line[len(providerResultPrefix):]
	decoder := json.NewDecoder(bytes.NewReader(payload))
	var envelope receiptEnvelope
	if err := decoder.Decode(&envelope); err != nil {
		return providerReceipt{}, errReceiptInvalid
	}
	if decoder.InputOffset() != int64(len(payload)) {
		return providerReceipt{}, errReceiptInvalid
	}
	if envelope.Schema != providerResultSchemaV2 || envelope.Provider != "kimi" || envelope.Model != "kimi-code/k3" ||
		envelope.Effort != requestedEffort || envelope.ResultText == nil || envelope.ExitCode == nil ||
		envelope.Cancelled == nil || envelope.TimedOut == nil || envelope.CleanupIssueCount == nil ||
		envelope.Status == "" || envelope.Gate == "" ||
		(envelope.CleanupStatus != "complete" && envelope.CleanupStatus != "incomplete") || *envelope.CleanupIssueCount < 0 ||
		envelope.Authorizing == nil || *envelope.Authorizing || envelope.ClosesRunIDs == nil || len(envelope.ClosesRunIDs) != 0 ||
		envelope.IndependentVerificationRequired == nil || !*envelope.IndependentVerificationRequired ||
		envelope.TerminalClass != "external-nonauthorizing" || envelope.ActualExecutionPath != "direct-external-cli" ||
		envelope.AssignedRole != requestedRole || !validExecutionRole(requestedRole, envelope.ExecutionRole) {
		return providerReceipt{}, errReceiptInvalid
	}
	var flags []string
	if len(envelope.LaunchFlags) == 0 || json.Unmarshal(envelope.LaunchFlags, &flags) != nil || len(flags) != 0 {
		return providerReceipt{}, errReceiptInvalid
	}
	if err := validatePrimaryOutcome(envelope.PrimaryOutcome, *envelope.ExitCode, envelope.Status, envelope.Gate); err != nil {
		return providerReceipt{}, errReceiptInvalid
	}
	if err := validateCapabilityEvidence(envelope.Selected, envelope.Observed); err != nil {
		return providerReceipt{}, errReceiptInvalid
	}
	if capabilityChoice.Kind == "none" && !validateNoneCapabilityBinding(envelope.Selected, envelope.Observed) {
		return providerReceipt{}, errReceiptInvalid
	}
	return providerReceipt{
		Payload: string(payload), Provider: envelope.Provider, Model: envelope.Model,
		Effort: envelope.Effort, ResultText: *envelope.ResultText, ExitCode: *envelope.ExitCode,
		Status: envelope.Status, Gate: envelope.Gate, Cancelled: *envelope.Cancelled,
		TimedOut: *envelope.TimedOut, CleanupStatus: envelope.CleanupStatus,
		CleanupIssueCount: *envelope.CleanupIssueCount,
	}, nil
}

func validateNoneCapabilityBinding(selectedRaw, observedRaw json.RawMessage) bool {
	var selected receiptSelected
	var observed receiptObserved
	if json.Unmarshal(selectedRaw, &selected) != nil || json.Unmarshal(observedRaw, &observed) != nil {
		return false
	}
	return len(selected.Tools) == 0 && len(selected.MCPNames) == 0 && len(selected.Subagents) == 0 &&
		selected.Permission == "reject" && selected.CWDSelected != nil && !*selected.CWDSelected &&
		len(observed.ToolCalls) == 0 && len(observed.PermissionDecisions) == 0 && observed.ObservationsOmitted == nil
}

func validatePrimaryOutcome(raw json.RawMessage, exitCode int, status, gate string) error {
	var primary receiptPrimaryOutcome
	if len(raw) == 0 || json.Unmarshal(raw, &primary) != nil || primary.ExitCode == nil || primary.Status == "" || primary.Gate == "" ||
		*primary.ExitCode != exitCode || primary.Status != status || primary.Gate != gate {
		return errReceiptInvalid
	}
	return nil
}

func validateCapabilityEvidence(selectedRaw, observedRaw json.RawMessage) error {
	var selected receiptSelected
	var observed receiptObserved
	var selectedFields, observedFields map[string]json.RawMessage
	if len(selectedRaw) == 0 || len(observedRaw) == 0 || json.Unmarshal(selectedRaw, &selected) != nil || json.Unmarshal(observedRaw, &observed) != nil ||
		json.Unmarshal(selectedRaw, &selectedFields) != nil || json.Unmarshal(observedRaw, &observedFields) != nil ||
		!exactKeys(selectedFields, "tools", "mcpNames", "subagents", "permission", "cwdSelected") ||
		!(exactKeys(observedFields, "toolCalls", "permissionDecisions") || exactKeys(observedFields, "toolCalls", "permissionDecisions", "observationsOmitted")) ||
		selected.Tools == nil || selected.MCPNames == nil || selected.Subagents == nil || selected.CWDSelected == nil ||
		(selected.Permission != "reject" && selected.Permission != "approve_once" && selected.Permission != "approve_always") ||
		observed.ToolCalls == nil || observed.PermissionDecisions == nil ||
		(observed.ObservationsOmitted != nil && !*observed.ObservationsOmitted) {
		return errReceiptInvalid
	}
	if duplicateOrInvalidNames(selected.Tools) || duplicateOrInvalidNames(selected.MCPNames) || duplicateOrInvalidNames(selected.Subagents) {
		return errReceiptInvalid
	}
	toolIDs := make(map[string]struct{}, len(observed.ToolCalls))
	for _, raw := range observed.ToolCalls {
		var item receiptToolCall
		var fields map[string]json.RawMessage
		if json.Unmarshal(raw, &item) != nil || json.Unmarshal(raw, &fields) != nil ||
			!exactKeys(fields, "id", "title", "status") || !validReceiptName(item.ID) || !validReceiptName(item.Title) ||
			(item.Status != "pending" && item.Status != "in_progress" && item.Status != "completed" && item.Status != "failed") {
			return errReceiptInvalid
		}
		if _, duplicate := toolIDs[item.ID]; duplicate {
			return errReceiptInvalid
		}
		toolIDs[item.ID] = struct{}{}
	}
	for _, raw := range observed.PermissionDecisions {
		var item receiptPermissionDecision
		var fields map[string]json.RawMessage
		if json.Unmarshal(raw, &item) != nil || json.Unmarshal(raw, &fields) != nil ||
			!exactKeys(fields, "id", "title", "decision") || !validReceiptName(item.ID) || !validReceiptName(item.Title) ||
			(item.Decision != "reject" && item.Decision != "approve_once" && item.Decision != "approve_always") {
			return errReceiptInvalid
		}
	}
	return nil
}

func exactKeys(fields map[string]json.RawMessage, keys ...string) bool {
	if len(fields) != len(keys) {
		return false
	}
	for _, key := range keys {
		if _, found := fields[key]; !found {
			return false
		}
	}
	return true
}

func duplicateOrInvalidNames(values []string) bool {
	seen := make(map[string]struct{}, len(values))
	for _, value := range values {
		if !validReceiptName(value) {
			return true
		}
		if _, found := seen[value]; found {
			return true
		}
		seen[value] = struct{}{}
	}
	return false
}

func validReceiptName(value string) bool {
	if value == "" {
		return false
	}
	for _, r := range value {
		if r < 32 || r == 127 {
			return false
		}
	}
	return true
}

func receiptInvalid(cause error) error {
	return fmt.Errorf("%w: %v", errReceiptInvalid, cause)
}

// Role-to-lane binding follows Orchestrarium's external-role-taxonomy.v1.json
// at 8964852bb0c0a55d2fe2a19fe8475b3b55ca5bd3. This adapter also accepts
// explicit external-reviewer/worker assignments used by the legacy direct
// wrapper contract, but never promotes them to another execution lane.
func validExecutionRole(assigned, execution string) bool {
	switch assigned {
	case "consultant":
		return execution == "consultant"
	case "external-reviewer", "qa-engineer", "architecture-reviewer",
		"security-reviewer", "performance-reviewer", "accessibility-reviewer",
		"ux-reviewer", "ui-test-engineer":
		return execution == "external-reviewer"
	case "external-worker", "explorer", "product-analyst", "analyst", "architect",
		"ux-designer", "planner", "algorithm-scientist", "computational-scientist",
		"security-engineer", "performance-engineer", "reliability-engineer",
		"backend-engineer", "frontend-engineer", "qt-ui-engineer", "model-view-engineer",
		"data-engineer", "platform-engineer", "toolchain-engineer",
		"scientific-software-engineer", "geometry-engineer", "graphics-engineer",
		"visualization-engineer":
		return execution == "external-worker"
	default:
		return false
	}
}

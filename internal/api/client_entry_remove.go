package api

import (
	"context"
	"errors"
	"strings"

	"mcp-local-hub/internal/clients"
)

// ClientEntryRemovalRequest carries explicit selection; paths never come from HTTP.
type ClientEntryRemovalRequest struct {
	Apply       bool   `json:"apply"`
	Server      string `json:"server"`
	Client      string `json:"client"`
	SourceToken string `json:"source_token"`
}

type ClientEntryRemovalReceipt struct {
	Version     int    `json:"version"`
	BeforeToken string `json:"before_token"`
	AfterToken  string `json:"after_token"`
	SourceCount int    `json:"source_count"`
}

type ClientEntryRemovalResult struct {
	Operation       string                     `json:"operation"`
	Server          string                     `json:"server"`
	Client          string                     `json:"client"`
	Outcome         string                     `json:"outcome"`
	EffectiveAbsent bool                       `json:"effective_absent"`
	Code            string                     `json:"code"`
	Receipt         *ClientEntryRemovalReceipt `json:"receipt,omitempty"`
}

var ErrClientEntryRemovalInput = errors.New("invalid client entry removal selection")

const ClientEntryRemovalApplyRequired = "REMOVAL_APPLY_REQUIRED"

func applyEntryRemovalCapabilities(entries map[string]*ScanEntry, paths map[string]string) {
	fleet := clients.AllClients()
	for id, adapter := range fleet {
		path := paths[id]
		if path == "" {
			delete(fleet, id)
			continue
		}
		bound, ok := clients.EntryRemovalClientForScan(adapter, path)
		if !ok {
			delete(fleet, id)
			continue
		}
		fleet[id] = bound
	}
	for name, row := range entries {
		for id, cell := range row.ClientPresence {
			cap := clients.EntryRemovalCapability{Version: 1, Reason: "REMOVAL_UNSUPPORTED_SOURCE"}
			if adapter := fleet[id]; adapter != nil {
				cap = clients.DescribeEntryRemoval(adapter, name)
				if cap.Supported {
					if clients.EntryRemovalScopeGuard(adapter, fleet, name) != nil {
						cap.Supported = false
						cap.Reason = "REMOVAL_SHARED_SOURCE_SCOPE"
						cap.SourceToken = ""
					}
				}
			}
			cell.Removal = &cap
			row.ClientPresence[id] = cell
		}
	}
}

func ValidateClientEntryRemovalRequest(req ClientEntryRemovalRequest) error {
	if strings.TrimSpace(req.Server) == "" || strings.TrimSpace(req.Client) == "" ||
		(req.Apply && strings.TrimSpace(req.SourceToken) == "") {
		return ErrClientEntryRemovalInput
	}
	for _, name := range clients.SupportedClientNames() {
		if name == req.Client {
			return nil
		}
	}
	return ErrClientEntryRemovalInput
}

func (a *API) RemoveClientEntry(ctx context.Context, req ClientEntryRemovalRequest) (*ClientEntryRemovalResult, error) {
	fleet, failed := clients.AllClientsWithErrors()
	if len(failed) != 0 {
		if err := ValidateClientEntryRemovalRequest(req); err != nil {
			return nil, err
		}
		return &ClientEntryRemovalResult{Operation: "remove-client-entry", Server: req.Server, Client: req.Client, Outcome: "refused", Code: "REMOVAL_UNSUPPORTED_SOURCE"}, nil
	}
	return a.removeClientEntry(ctx, req, fleet)
}

func (a *API) removeClientEntry(ctx context.Context, req ClientEntryRemovalRequest, fleet map[string]clients.Client) (*ClientEntryRemovalResult, error) {
	if err := ValidateClientEntryRemovalRequest(req); err != nil {
		return nil, err
	}
	result := &ClientEntryRemovalResult{Operation: "remove-client-entry", Server: req.Server, Client: req.Client, Outcome: "refused", Code: "REMOVAL_UNSUPPORTED_SOURCE"}
	adapter := fleet[req.Client]
	if adapter == nil {
		return result, nil
	}
	if !req.Apply {
		capability, inspectErr := clients.DescribeEntryRemovalContext(ctx, adapter, req.Server)
		if inspectErr != nil && !errors.Is(inspectErr, clients.ErrEntryRemovalUnsupportedSource) && !errors.Is(inspectErr, clients.ErrEntryMutationPreconditionConflict) {
			result.Outcome, result.Code = "failed", "REMOVAL_INSPECTION_FAILED"
			if errors.Is(inspectErr, context.Canceled) {
				result.Code = "REMOVAL_CANCELLED"
			}
			if errors.Is(inspectErr, context.DeadlineExceeded) {
				result.Code = "REMOVAL_TIMEOUT"
			}
		} else {
			result.Code = capability.Reason
			if capability.Supported {
				result.Code = ClientEntryRemovalApplyRequired
			}
		}
		return result, nil
	}
	guard, err := acquireAttachmentBindingGuard(req.Server, req.Client, adapter, false)
	if err != nil {
		result.Code = "REMOVAL_SOURCE_CONFLICT"
		return result, nil
	}
	removed := clients.RemoveEntrySelected(ctx, adapter, req.Server, req.SourceToken, a.effectiveBackupKeepN(), func() error { return clients.EntryRemovalScopeGuard(adapter, fleet, req.Server) })
	result.Outcome, result.Code, result.EffectiveAbsent = removed.Outcome, removed.Code, removed.EffectiveAbsent
	if removed.Outcome == "removed" {
		result.Receipt = &ClientEntryRemovalReceipt{Version: 1, BeforeToken: req.SourceToken, AfterToken: removed.AfterToken, SourceCount: 1}
	}
	if err := guard.release(); err != nil {
		result.Outcome, result.Code = "applied-unverified", "REMOVAL_SETTLEMENT_UNVERIFIED"
		result.Receipt = nil
	}
	return result, nil
}

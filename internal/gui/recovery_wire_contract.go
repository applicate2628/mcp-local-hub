package gui

import "mcp-local-hub/internal/daemonrecovery"

// RecoveryWireContractSnapshot is the read-only recovery enum membership
// exposed solely to the frontend contract test.
type RecoveryWireContractSnapshot struct {
	RecoveryErrorCodes         []string `json:"recovery_error_codes"`
	AuditLockReceiptStatuses   []string `json:"audit_lock_receipt_statuses"`
	AuditLockAuthorizations    []string `json:"audit_lock_authorizations"`
	AuditLockTerminationStates []string `json:"audit_lock_termination_states"`
	PortOwnerChecks            []string `json:"port_owner_checks"`
	PortWaitOutcomes           []string `json:"port_wait_outcomes"`
	AuditHandoffs              []string `json:"audit_handoffs"`
}

// RecoveryWireContract copies the behavior-owning registries without adding a
// third membership owner or any recovery behavior.
func RecoveryWireContract() RecoveryWireContractSnapshot {
	portOwnerChecks := daemonrecovery.PortOwnerChecks()
	portWaitOutcomes := daemonrecovery.PortWaitOutcomes()
	auditHandoffs := daemonrecovery.AuditHandoffs()

	snapshot := RecoveryWireContractSnapshot{
		RecoveryErrorCodes:         daemonRecoverErrorCodes(),
		AuditLockReceiptStatuses:   auditLockOccurrenceStatusValues(),
		AuditLockAuthorizations:    auditLockAuthorizationValues(),
		AuditLockTerminationStates: auditLockTerminationStateValues(),
		PortOwnerChecks:            make([]string, len(portOwnerChecks)),
		PortWaitOutcomes:           make([]string, len(portWaitOutcomes)),
		AuditHandoffs:              make([]string, len(auditHandoffs)),
	}
	for i, value := range portOwnerChecks {
		snapshot.PortOwnerChecks[i] = string(value)
	}
	for i, value := range portWaitOutcomes {
		snapshot.PortWaitOutcomes[i] = string(value)
	}
	for i, value := range auditHandoffs {
		snapshot.AuditHandoffs[i] = string(value)
	}
	return snapshot
}

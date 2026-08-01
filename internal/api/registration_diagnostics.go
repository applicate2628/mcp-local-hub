package api

import (
	"crypto/sha256"
	"fmt"
	"strings"
)

// RegistrationDiagnosticCode is a stable internal registration diagnostic id.
// It is intentionally not serialized by RegisterReport or UnregisterReport.
type RegistrationDiagnosticCode string

const (
	RegistrationCodeSchedulerUnavailable     RegistrationDiagnosticCode = "REG_SCHEDULER_UNAVAILABLE"
	RegistrationCodeRelayStdioSkipped        RegistrationDiagnosticCode = "REG_RELAY_STDIO_SKIPPED"
	RegistrationCodeWeeklyRefreshFailed      RegistrationDiagnosticCode = "REG_WEEKLY_REFRESH_FAILED"
	RegistrationCodeTrustedRootRecordFailed  RegistrationDiagnosticCode = "REG_TRUSTED_ROOT_RECORD_FAILED"
	RegistrationCodePostCommitObserverFailed RegistrationDiagnosticCode = "REG_POST_COMMIT_OBSERVER_FAILED"

	RegistrationCodeCleanupUnsupported        RegistrationDiagnosticCode = "REG_CLEANUP_UNSUPPORTED"
	RegistrationCodeCleanupScanFailed         RegistrationDiagnosticCode = "REG_CLEANUP_SCAN_FAILED"
	RegistrationCodeCleanupSurvivorScanFailed RegistrationDiagnosticCode = "REG_CLEANUP_SURVIVOR_SCAN_FAILED"
	RegistrationCodeCleanupCASConflict        RegistrationDiagnosticCode = "REG_CLEANUP_CAS_CONFLICT"
	RegistrationCodeCleanupBackupFailed       RegistrationDiagnosticCode = "REG_CLEANUP_BACKUP_FAILED"
	RegistrationCodeCleanupRemoveFailed       RegistrationDiagnosticCode = "REG_CLEANUP_REMOVE_FAILED"
	RegistrationCodeRouterProofFailed         RegistrationDiagnosticCode = "REG_ROUTER_PROOF_FAILED"
	RegistrationCodeRouteProofFailed          RegistrationDiagnosticCode = "REG_ROUTE_PROOF_FAILED"

	RegistrationCodeUnregisterSchedulerUnavailable RegistrationDiagnosticCode = "UNREG_SCHEDULER_UNAVAILABLE"
	RegistrationCodeUnregisterLanguageMissing      RegistrationDiagnosticCode = "UNREG_LANGUAGE_NOT_REGISTERED"
	RegistrationCodeUnregisterIntentRemoveFailed   RegistrationDiagnosticCode = "UNREG_INTENT_REMOVE_FAILED"
	RegistrationCodeUnregisterReconcileFailed      RegistrationDiagnosticCode = "UNREG_RECONCILE_FAILED"
	RegistrationCodeUnregisterProxyForeignOwner    RegistrationDiagnosticCode = "UNREG_PROXY_FOREIGN_OWNER"
	RegistrationCodeUnregisterProxyKillFailed      RegistrationDiagnosticCode = "UNREG_PROXY_KILL_FAILED"
	RegistrationCodeUnregisterTaskDeleteFailed     RegistrationDiagnosticCode = "UNREG_TASK_DELETE_FAILED"
	RegistrationCodeUnregisterClientRemoveFailed   RegistrationDiagnosticCode = "UNREG_CLIENT_ENTRY_REMOVE_FAILED"

	RegistrationCodeLSPEnsureFailed RegistrationDiagnosticCode = "LSP_ENSURE_FAILED"
	RegistrationCodeUnknown         RegistrationDiagnosticCode = "REG_UNKNOWN"
)

// RegistrationDiagnosticSeverity is deliberately small: callers decide
// status/exit policy separately, while the diagnostic keeps meaning stable.
type RegistrationDiagnosticSeverity string

const (
	RegistrationSeverityWarning RegistrationDiagnosticSeverity = "warning"
	RegistrationSeverityError   RegistrationDiagnosticSeverity = "error"
)

type registrationDiagnosticDefinition struct {
	severity RegistrationDiagnosticSeverity
}

// registrationDiagnosticRegistry is the single owner of every admitted code.
var registrationDiagnosticRegistry = map[RegistrationDiagnosticCode]registrationDiagnosticDefinition{
	RegistrationCodeSchedulerUnavailable:           {severity: RegistrationSeverityWarning},
	RegistrationCodeRelayStdioSkipped:              {severity: RegistrationSeverityWarning},
	RegistrationCodeWeeklyRefreshFailed:            {severity: RegistrationSeverityWarning},
	RegistrationCodeTrustedRootRecordFailed:        {severity: RegistrationSeverityWarning},
	RegistrationCodePostCommitObserverFailed:       {severity: RegistrationSeverityWarning},
	RegistrationCodeCleanupUnsupported:             {severity: RegistrationSeverityWarning},
	RegistrationCodeCleanupScanFailed:              {severity: RegistrationSeverityWarning},
	RegistrationCodeCleanupSurvivorScanFailed:      {severity: RegistrationSeverityWarning},
	RegistrationCodeCleanupCASConflict:             {severity: RegistrationSeverityWarning},
	RegistrationCodeCleanupBackupFailed:            {severity: RegistrationSeverityWarning},
	RegistrationCodeCleanupRemoveFailed:            {severity: RegistrationSeverityWarning},
	RegistrationCodeRouterProofFailed:              {severity: RegistrationSeverityWarning},
	RegistrationCodeRouteProofFailed:               {severity: RegistrationSeverityWarning},
	RegistrationCodeUnregisterSchedulerUnavailable: {severity: RegistrationSeverityWarning},
	RegistrationCodeUnregisterLanguageMissing:      {severity: RegistrationSeverityWarning},
	RegistrationCodeUnregisterIntentRemoveFailed:   {severity: RegistrationSeverityWarning},
	RegistrationCodeUnregisterReconcileFailed:      {severity: RegistrationSeverityWarning},
	RegistrationCodeUnregisterProxyForeignOwner:    {severity: RegistrationSeverityWarning},
	RegistrationCodeUnregisterProxyKillFailed:      {severity: RegistrationSeverityWarning},
	RegistrationCodeUnregisterTaskDeleteFailed:     {severity: RegistrationSeverityWarning},
	RegistrationCodeUnregisterClientRemoveFailed:   {severity: RegistrationSeverityWarning},
	RegistrationCodeLSPEnsureFailed:                {severity: RegistrationSeverityError},
	RegistrationCodeUnknown:                        {severity: RegistrationSeverityError},
}

// RegistrationDiagnostic carries private causal data. Only read-only accessors
// cross the API boundary; JSON compatibility remains owned by the reports.
type RegistrationDiagnostic struct {
	code         RegistrationDiagnosticCode
	severity     RegistrationDiagnosticSeverity
	planIdentity string
	participant  string
	cause        error
}

func (d RegistrationDiagnostic) Code() RegistrationDiagnosticCode         { return d.code }
func (d RegistrationDiagnostic) Severity() RegistrationDiagnosticSeverity { return d.severity }
func (d RegistrationDiagnostic) PlanIdentity() string                     { return d.planIdentity }
func (d RegistrationDiagnostic) Participant() string                      { return d.participant }
func (d RegistrationDiagnostic) Cause() error                             { return d.cause }

// NewRegistrationDiagnostic validates code membership and sanitizes context.
// An unknown code is deliberately collapsed to REG_UNKNOWN.
func NewRegistrationDiagnostic(code RegistrationDiagnosticCode, planIdentity, participant string, cause error) RegistrationDiagnostic {
	definition, ok := registrationDiagnosticRegistry[code]
	if !ok {
		code = RegistrationCodeUnknown
		definition = registrationDiagnosticRegistry[code]
	}
	return RegistrationDiagnostic{
		code:         code,
		severity:     definition.severity,
		planIdentity: sanitizeRegistrationDiagnosticIdentity(planIdentity),
		participant:  sanitizeRegistrationDiagnosticIdentity(participant),
		cause:        cause,
	}
}

// ClassifyLSPEnsureError is the sole legacy boundary for
// EnsureLSPRegistered failures.
func ClassifyLSPEnsureError(err error, planIdentity, participant string) RegistrationDiagnostic {
	if err == nil {
		return RegistrationDiagnostic{}
	}
	return NewRegistrationDiagnostic(RegistrationCodeLSPEnsureFailed, planIdentity, participant, err)
}

// ClassifyRegistrationError converts an otherwise untyped legacy operation
// error without exposing its text to public consumers.
func ClassifyRegistrationError(err error, planIdentity, participant string) RegistrationDiagnostic {
	if err == nil {
		return RegistrationDiagnostic{}
	}
	return NewRegistrationDiagnostic(RegistrationCodeUnknown, planIdentity, participant, err)
}

// RegisteredRegistrationDiagnosticCodes returns a copy for projector
// exhaustiveness tests. Registry order is stable and source-owned.
func RegisteredRegistrationDiagnosticCodes() []RegistrationDiagnosticCode {
	return []RegistrationDiagnosticCode{
		RegistrationCodeSchedulerUnavailable,
		RegistrationCodeRelayStdioSkipped,
		RegistrationCodeWeeklyRefreshFailed,
		RegistrationCodeTrustedRootRecordFailed,
		RegistrationCodePostCommitObserverFailed,
		RegistrationCodeCleanupUnsupported,
		RegistrationCodeCleanupScanFailed,
		RegistrationCodeCleanupSurvivorScanFailed,
		RegistrationCodeCleanupCASConflict,
		RegistrationCodeCleanupBackupFailed,
		RegistrationCodeCleanupRemoveFailed,
		RegistrationCodeRouterProofFailed,
		RegistrationCodeRouteProofFailed,
		RegistrationCodeUnregisterSchedulerUnavailable,
		RegistrationCodeUnregisterLanguageMissing,
		RegistrationCodeUnregisterIntentRemoveFailed,
		RegistrationCodeUnregisterReconcileFailed,
		RegistrationCodeUnregisterProxyForeignOwner,
		RegistrationCodeUnregisterProxyKillFailed,
		RegistrationCodeUnregisterTaskDeleteFailed,
		RegistrationCodeUnregisterClientRemoveFailed,
		RegistrationCodeLSPEnsureFailed,
		RegistrationCodeUnknown,
	}
}

// RegistrationCompatibilityText is the one cause-free legacy string
// projector used only to populate the existing Warnings JSON field.
func RegistrationCompatibilityText(d RegistrationDiagnostic) string {
	context := registrationDiagnosticContext(d)
	switch d.code {
	case RegistrationCodeSchedulerUnavailable:
		return "scheduler unavailable; using supervised LSP proxy path and skipping legacy task handling"
	case RegistrationCodeRelayStdioSkipped:
		return "relay-stdio clients were skipped because workspace registration requires URL-capable clients" + context
	case RegistrationCodeWeeklyRefreshFailed:
		return "ensure shared weekly-refresh task failed; registration continued"
	case RegistrationCodeTrustedRootRecordFailed:
		return "could not record workspace as an LSP trusted root; sibling-workspace auto-registration may require explicit register"
	case RegistrationCodePostCommitObserverFailed:
		return "register-post-commit-observer-failed: registration succeeded, but a completion record could not be written"
	case RegistrationCodeCleanupUnsupported:
		return "direct-cleanup-unsupported" + context + ": matching direct LSP entries were kept"
	case RegistrationCodeCleanupScanFailed:
		return "direct-scan-failed" + context + ": matching direct LSP entries were kept"
	case RegistrationCodeCleanupSurvivorScanFailed:
		return "survivor-scan-failed" + context + ": matching direct LSP entries were kept"
	case RegistrationCodeCleanupCASConflict:
		return "direct-cleanup-cas-conflict" + context + ": matching direct LSP entries were kept"
	case RegistrationCodeCleanupBackupFailed:
		return "direct-cleanup-backup-failed" + context + ": matching direct LSP entries were kept"
	case RegistrationCodeCleanupRemoveFailed:
		return "direct-cleanup-remove-failed" + context + ": use the printed backup path if recovery is needed"
	case RegistrationCodeRouterProofFailed:
		prefix := d.participant
		if prefix == "" {
			prefix = "router-proof-failed"
		}
		return prefix + context + ": keeping matching direct LSP entries"
	case RegistrationCodeRouteProofFailed:
		prefix := d.participant
		if prefix == "" {
			prefix = "route-proof-failed"
		}
		return prefix + context + ": keeping matching direct LSP entries"
	case RegistrationCodeUnregisterSchedulerUnavailable:
		return "scheduler unavailable; skipping legacy task deletion"
	case RegistrationCodeUnregisterLanguageMissing:
		return "language not registered" + context
	case RegistrationCodeUnregisterIntentRemoveFailed:
		return "remove supervisor intent failed" + context
	case RegistrationCodeUnregisterReconcileFailed:
		return "supervisor reconcile failed" + context
	case RegistrationCodeUnregisterProxyForeignOwner:
		return "proxy port owned by foreign process; not killing" + context
	case RegistrationCodeUnregisterProxyKillFailed:
		return "kill proxy failed" + context
	case RegistrationCodeUnregisterTaskDeleteFailed:
		return "delete task failed" + context
	case RegistrationCodeUnregisterClientRemoveFailed:
		return "remove client entry failed" + context
	case RegistrationCodeLSPEnsureFailed:
		return "LSP registration failed" + context
	case RegistrationCodeUnknown:
		return "registration operation failed"
	default:
		return "registration operation failed"
	}
}

func registrationDiagnosticContext(d RegistrationDiagnostic) string {
	parts := make([]string, 0, 2)
	if d.planIdentity != "" {
		parts = append(parts, "plan="+d.planIdentity)
	}
	if d.participant != "" {
		parts = append(parts, "participant="+d.participant)
	}
	if len(parts) == 0 {
		return ""
	}
	return ": " + strings.Join(parts, ",")
}

func sanitizeRegistrationDiagnosticIdentity(value string) string {
	const maxIdentityLen = 64
	value = strings.TrimSpace(value)
	if value == "" {
		return ""
	}
	valid := len(value) <= maxIdentityLen
	for _, r := range value {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '-' || r == '_' || r == '.' {
			continue
		}
		valid = false
		break
	}
	if valid {
		return value
	}
	sum := sha256.Sum256([]byte(value))
	return fmt.Sprintf("sanitized-%x", sum[:8])
}

func (r *RegisterReport) addDiagnostic(d RegistrationDiagnostic) {
	if r == nil || d.code == "" {
		return
	}
	r.diagnostics = append(r.diagnostics, d)
}

func (r *UnregisterReport) addDiagnostic(d RegistrationDiagnostic) {
	if r == nil || d.code == "" {
		return
	}
	r.diagnostics = append(r.diagnostics, d)
}

func (r *RegisterReport) Diagnostics() []RegistrationDiagnostic {
	if r == nil {
		return nil
	}
	return append([]RegistrationDiagnostic(nil), r.diagnostics...)
}

func (r *UnregisterReport) Diagnostics() []RegistrationDiagnostic {
	if r == nil {
		return nil
	}
	return append([]RegistrationDiagnostic(nil), r.diagnostics...)
}

func (r *RegisterReport) projectCompatibilityWarnings() {
	if r == nil {
		return
	}
	r.Warnings = projectRegistrationCompatibilityWarnings(r.diagnostics)
}

func (r *UnregisterReport) projectCompatibilityWarnings() {
	if r == nil {
		return
	}
	r.Warnings = projectRegistrationCompatibilityWarnings(r.diagnostics)
}

func projectRegistrationCompatibilityWarnings(diagnostics []RegistrationDiagnostic) []string {
	var warnings []string
	for _, diagnostic := range diagnostics {
		if diagnostic.severity != RegistrationSeverityWarning {
			continue
		}
		warnings = append(warnings, RegistrationCompatibilityText(diagnostic))
	}
	return warnings
}

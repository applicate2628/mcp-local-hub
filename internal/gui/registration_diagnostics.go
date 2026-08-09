package gui

import (
	"errors"
	"log"
	"net/http"

	"mcp-local-hub/internal/api"
)

const (
	lspRegisterTrustedRootRecordFailedPublic = "trusted-root-record-failed: registration succeeded, but sibling-workspace auto-registration may require an explicit register; verify the mcphub state directory is writable and retry explicit registration"
	lspRegisterUnknownWarningPublic          = "lsp-register-warning: registration succeeded with a warning; retry explicit registration and verify the mcphub state directory is writable"
	registrationUnknownErrorPublic           = "registration operation could not be completed; retry and inspect local mcphub logs"
)

// registrationDiagnosticPublicText is the single GUI/public projector. It
// never reads Cause, PlanIdentity, or Participant.
func registrationDiagnosticPublicText(d api.RegistrationDiagnostic) string {
	switch d.Code() {
	case api.RegistrationCodeSchedulerUnavailable:
		return "The local scheduler is unavailable; registration continued through the supervised proxy path."
	case api.RegistrationCodeRelayStdioSkipped:
		return "Some stdio-relay clients were skipped because workspace registration requires URL-capable clients."
	case api.RegistrationCodeWeeklyRefreshFailed:
		return "Registration succeeded, but the shared weekly-refresh task could not be updated."
	case api.RegistrationCodeTrustedRootRecordFailed:
		return lspRegisterTrustedRootRecordFailedPublic
	case api.RegistrationCodePostCommitObserverFailed:
		return "Registration succeeded, but its completion record could not be written."
	case api.RegistrationCodeCleanupUnsupported:
		return "Registration succeeded, but direct LSP cleanup was unsupported; existing direct entries were kept."
	case api.RegistrationCodeCleanupScanFailed:
		return "Registration succeeded, but direct LSP entries could not be inspected; existing entries were kept."
	case api.RegistrationCodeCleanupSurvivorScanFailed:
		return "Registration succeeded, but direct LSP survivor checks failed; existing entries were kept."
	case api.RegistrationCodeCleanupCASConflict:
		return "Registration succeeded, but direct LSP cleanup observed a concurrent configuration change; existing entries were kept."
	case api.RegistrationCodeCleanupBackupFailed:
		return "Registration succeeded, but direct LSP cleanup could not create its backup; existing entries were kept."
	case api.RegistrationCodeCleanupRemoveFailed:
		return "Registration succeeded, but a direct LSP entry could not be removed; use the local backup information for recovery."
	case api.RegistrationCodeRouterProofFailed:
		return "Registration succeeded, but managed-router ownership could not be proven; existing direct LSP entries were kept."
	case api.RegistrationCodeRouteProofFailed:
		return "Registration succeeded, but the managed language route could not be proven; existing direct LSP entries were kept."
	case api.RegistrationCodeUnregisterSchedulerUnavailable:
		return "The local scheduler is unavailable; legacy task deletion was skipped."
	case api.RegistrationCodeUnregisterLanguageMissing:
		return "The requested language was not registered for this workspace."
	case api.RegistrationCodeUnregisterIntentRemoveFailed:
		return "A supervisor intent could not be removed; that registration was kept."
	case api.RegistrationCodeUnregisterReconcileFailed:
		return "The supervisor could not apply the unregister operation."
	case api.RegistrationCodeUnregisterProxyForeignOwner:
		return "A proxy port belongs to another process; that process was not stopped."
	case api.RegistrationCodeUnregisterProxyKillFailed:
		return "A managed proxy could not be stopped during unregister."
	case api.RegistrationCodeUnregisterTaskDeleteFailed:
		return "A legacy scheduler task could not be deleted during unregister."
	case api.RegistrationCodeUnregisterClientRemoveFailed:
		return "A client registration entry could not be removed during unregister."
	case api.RegistrationCodeLSPEnsureFailed:
		return "unknown LSP language or LSP registration failed for the requested language."
	case api.RegistrationCodeUnknown:
		if d.Severity() == api.RegistrationSeverityWarning {
			return lspRegisterUnknownWarningPublic
		}
		return registrationUnknownErrorPublic
	default:
		if d.Severity() == api.RegistrationSeverityWarning {
			return lspRegisterUnknownWarningPublic
		}
		return registrationUnknownErrorPublic
	}
}

func projectRegistrationWarnings(diagnostics []api.RegistrationDiagnostic) []string {
	var warnings []string
	for _, diagnostic := range diagnostics {
		if diagnostic.Severity() == api.RegistrationSeverityWarning {
			warnings = append(warnings, registrationDiagnosticPublicText(diagnostic))
		}
	}
	return warnings
}

func writeRegistrationDiagnosticError(
	w http.ResponseWriter,
	rawErr error,
	diagnostic api.RegistrationDiagnostic,
	status int,
	code string,
	logContext string,
) {
	if rawErr != nil {
		log.Printf("%s: %v", logContext, rawErr)
	}
	writeAPIError(w, errors.New(registrationDiagnosticPublicText(diagnostic)), status, code)
}

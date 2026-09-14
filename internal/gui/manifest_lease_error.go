package gui

import (
	"errors"
	"net/http"

	"mcp-local-hub/internal/api"
)

const manifestLeaseBusyCode = "MANIFEST_LEASE_BUSY"

func writeRetryableManifestLeaseConflict(w http.ResponseWriter, err error) bool {
	var failure *api.LeaseFailure
	if !errors.As(err, &failure) || failure == nil || !failure.Retryable {
		return false
	}
	writeJSON(w, http.StatusConflict, map[string]any{
		"error":      "another operation is changing this manifest; retry after it completes",
		"code":       manifestLeaseBusyCode,
		"failure_id": failure.FailureID,
		"retryable":  true,
	})
	return true
}

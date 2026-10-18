package api

// ManifestRestoreError deliberately carries only a stable public code; its
// cause remains private to the API layer and is never printed by the CLI.
type ManifestRestoreError struct {
	Code  string
	cause error
}

func (e *ManifestRestoreError) Error() string { return e.Code }
func (e *ManifestRestoreError) Unwrap() error { return e.cause }

func restoreFailure(code string, err error) error {
	return &ManifestRestoreError{Code: code, cause: err}
}

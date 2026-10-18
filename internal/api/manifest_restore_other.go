//go:build !windows

package api

// The no-replace filesystem oracle for this slice is Windows-specific.
// Other platforms must refuse rather than silently substitute Rename.
func (a *API) manifestRestoreExactInWithAdoptLeaseHeld(dir, name, yaml string) error {
	return restoreFailure("ATTACH_MANIFEST_RESTORE_UNSUPPORTED_FS", nil)
}

func (a *API) manifestRestoreExactInWithAdoptLeaseHeldWithGate(dir, name, yaml string, beforeGate func(), gate func() error) error {
	return restoreFailure("ATTACH_MANIFEST_RESTORE_UNSUPPORTED_FS", nil)
}

func (a *API) settlePublishedRestoreUnderLease(dir, name, expectedHash string) error {
	return nil
}

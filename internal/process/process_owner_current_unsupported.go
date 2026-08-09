//go:build !windows && !linux

package process

// ProcessOwnerMatchesCurrent fails closed on platforms without an admitted
// current-user process-owner proof.
func ProcessOwnerMatchesCurrent(int) (bool, error) {
	return false, ErrProcessOwnerUnsupported
}

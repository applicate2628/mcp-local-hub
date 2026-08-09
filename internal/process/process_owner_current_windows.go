//go:build windows

package process

// ProcessOwnerMatchesCurrent delegates to the existing Windows SID proof.
func ProcessOwnerMatchesCurrent(pid int) (bool, error) {
	return ProcessOwnerSIDMatchesCurrent(pid)
}

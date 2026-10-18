//go:build !windows

package cli

// LaunchBareGUI preserves the existing inline Cobra GUI route off Windows.
func LaunchBareGUI() (bool, error) { return false, nil }

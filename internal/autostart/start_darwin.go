//go:build darwin

package autostart

import "fmt"

// StartOwner starts the installed LaunchAgent autostart owner immediately.
func StartOwner() error {
	target := "gui/" + currentUIDFn() + "/" + DarwinLabel
	if _, _, err := launchctlFn([]string{"kickstart", "-k", target}); err != nil {
		return fmt.Errorf("autostart owner start: launchctl kickstart -k %s: %w", target, err)
	}
	return nil
}

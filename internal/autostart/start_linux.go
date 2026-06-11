//go:build linux

package autostart

import "fmt"

// StartOwner starts the installed systemd-user autostart owner immediately.
func StartOwner() error {
	if _, _, err := systemctlFn([]string{"--user", "start", linuxUnitName}); err != nil {
		return fmt.Errorf("autostart owner start: systemctl --user start %s: %w", linuxUnitName, err)
	}
	return nil
}

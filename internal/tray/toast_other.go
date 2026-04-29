// internal/tray/toast_other.go
//go:build !windows

package tray

// ShowToast is a no-op on non-Windows. Toast notifications are
// Windows-only per spec §2.2 (cross-platform tray is an explicit
// non-goal). Returning nil rather than an error so callers don't
// log noise about every toast event on Linux/macOS dev builds.
func ShowToast(title, body string) error {
	_ = title
	_ = body
	return nil
}

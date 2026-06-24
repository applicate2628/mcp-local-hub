// internal/gui/separate_process_detect_other.go
//go:build !windows

package gui

// detectSeparateProcessOnce is a no-op off-Windows. The SeparateProcess
// registry key and the persistent-explorer reveal behavior it controls
// are Windows-only (POSIX open/xdg-open delegate to the file manager and
// spawn no per-reveal process), so the detection is inert here.
func detectSeparateProcessOnce(s *Server) {
	_ = s
}

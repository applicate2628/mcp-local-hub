package daemon

// filepath_Dir returns the directory component of p. Tiny local wrapper
// kept independent of path/filepath so the daemon package has no
// conditional import dependency on it (matches the historical shape
// from the now-deleted launcher.go).
func filepath_Dir(p string) string {
	for i := len(p) - 1; i >= 0; i-- {
		if p[i] == '/' || p[i] == '\\' {
			return p[:i]
		}
	}
	return "."
}

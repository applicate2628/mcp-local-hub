//go:build !windows

package oneapi

// SetvarsEnv is a no-op on non-Windows hosts: oneAPI's setvars.bat is a Windows
// batch script, and the oneapi-run / drmemory daemons that consume the full
// environment are Windows-focused. Returns (nil, false) so callers fall back to
// os.Environ() (+ DLLDirs, also a Windows no-op here).
func SetvarsEnv() ([]string, bool) {
	return nil, false
}

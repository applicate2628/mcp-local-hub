//go:build !windows

package oneapirun

// captureVSEnvCached is the non-Windows stub: vcvars64.bat is a Windows
// Visual-Studio batch file, so there is no VS environment to capture off
// Windows. Returns ok=false so computeRunEnv falls back to the
// oneapi-only / plain composition (and, since DetectRoot also returns no
// root off Windows, the env_source resolves to "plain"). The package still
// compiles and runs cross-platform — the run handler simply executes the
// command with the inherited environment.
func captureVSEnvCached() ([]string, bool) {
	return nil, false
}

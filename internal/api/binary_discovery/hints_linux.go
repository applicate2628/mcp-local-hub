//go:build linux

package binary_discovery

// DefaultHints returns the ordered list of directories the discoverer
// probes on Linux. The shipped list covers standard system bin paths
// plus per-user toolchain caches (.local/bin for pipx/pip --user
// installs, .cargo/bin for rustup, go/bin for `go install` output).
//
// $HOME is expanded by Discover via os.ExpandEnv at probe time, so the
// returned list uses environment-variable references rather than
// resolving the user's home directory eagerly at package init.
func DefaultHints() []string {
	return []string{
		"/usr/local/bin",
		"/usr/bin",
		"$HOME/.local/bin",
		"$HOME/.cargo/bin",
		"$HOME/go/bin",
	}
}

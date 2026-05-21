//go:build darwin

package binary_discovery

// DefaultHints returns the ordered list of directories the discoverer
// probes on macOS. The list prepends the macOS-specific package-manager
// bin directories (Homebrew on Apple Silicon at /opt/homebrew/bin,
// Homebrew on Intel at /usr/local/bin, MacPorts at /opt/local/bin) and
// then continues with the same per-user toolchain caches used on Linux
// (.local/bin, .cargo/bin, go/bin).
//
// $HOME is expanded by Discover via os.ExpandEnv at probe time.
func DefaultHints() []string {
	return []string{
		"/opt/homebrew/bin",
		"/usr/local/bin",
		"/opt/local/bin",
		"/usr/bin",
		"$HOME/.local/bin",
		"$HOME/.cargo/bin",
		"$HOME/go/bin",
	}
}

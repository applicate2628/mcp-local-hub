// internal/gui/probe_windows_unsupported_arch.go
//go:build windows && !amd64

// Stubs for non-amd64 Windows builds. The real probe_windows.go
// implementation embeds amd64-specific PEB struct offsets
// (PEB.ProcessParameters at 0x20, RTL_USER_PROCESS_PARAMETERS.CommandLine
// at 0x70) and would silently read the wrong memory on x86/arm64
// — which would mis-classify legitimate stuck mcphub gui holders
// as PID-recycled and refuse --force --kill with exit 7. We ship
// an explicit "arch not supported" sentinel instead so the
// identity gate refuses the kill with a clear reason.
//
// Future per-arch implementations: replace this stub with
// probe_windows_386.go / probe_windows_arm64.go each pinning the
// correct PEB layout for that architecture.

package gui

// errWindowsArchUnsupported is defined in probe.go (cross-platform
// surface) so the probeOnce classifier can errors.Is against it on
// every platform without splitting the arch-handling logic across
// build-tagged files. This stub-arch impl just returns the shared
// sentinel.

func processIDImpl(pid int) (ProcessIdentity, error) {
	return ProcessIdentity{}, errWindowsArchUnsupported
}

func killProcessImpl(pid int) error {
	return errWindowsArchUnsupported
}

// closeProcessHandle no-op (this build never populates Handle).
func closeProcessHandle(_ uintptr) {}

// matchBasename: case-insensitive `mcphub.exe` match. Identical to
// the amd64 implementation; an arch-stub is allowed to keep this
// since matchBasename only inspects a string and doesn't depend on
// PEB layout. Keeps build-tag-split tests happy across arches.
func matchBasename(path string) bool {
	if path == "" {
		return false
	}
	// Inline the Windows logic without depending on filepath here
	// (filepath import not needed): take the trailing component
	// after the last '\\' or '/' and case-insensitively compare to
	// "mcphub.exe".
	last := -1
	for i := len(path) - 1; i >= 0; i-- {
		if path[i] == '\\' || path[i] == '/' {
			last = i
			break
		}
	}
	base := path[last+1:]
	const want = "mcphub.exe"
	if len(base) != len(want) {
		return false
	}
	for i := 0; i < len(want); i++ {
		bc := base[i]
		if bc >= 'A' && bc <= 'Z' {
			bc += 32
		}
		if bc != want[i] {
			return false
		}
	}
	return true
}

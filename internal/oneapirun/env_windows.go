//go:build windows

package oneapirun

import "strings"

// defaultPathExt is the PATHEXT fallback used when the child env carries no
// PATHEXT entry. Matches the cmd.exe default set of executable extensions.
const defaultPathExt = ".COM;.EXE;.BAT;.CMD"

// commandExtensions returns the executable-extension probe list for a bare
// command name on Windows, read from the CHILD env's PATHEXT (so a host that
// customizes PATHEXT — e.g. adds .PS1 — is honored, and the value the child
// would actually see drives resolution). The leading "" entry lets a command
// that already carries its own extension (e.g. "icx-cl.exe") resolve as-is
// before any PATHEXT extension is appended.
//
// If command already ends in one of the PATHEXT extensions (case-insensitive),
// ONLY the bare-name probe is returned so we never produce "icx-cl.exe.EXE".
func commandExtensions(command string, env []string) []string {
	pathExt, ok := envValue(env, "PATHEXT")
	if !ok || strings.TrimSpace(pathExt) == "" {
		pathExt = defaultPathExt
	}

	var exts []string
	for _, e := range strings.Split(pathExt, ";") {
		e = strings.TrimSpace(e)
		if e == "" {
			continue
		}
		exts = append(exts, e)
	}

	// If the command already has one of these extensions, the bare name is
	// the only correct probe — appending another extension would never match.
	for _, e := range exts {
		if strings.HasSuffix(strings.ToUpper(command), strings.ToUpper(e)) {
			return []string{""}
		}
	}

	// Bare name first (an explicit "cmd.exe" should win), then each PATHEXT
	// extension in order.
	return append([]string{""}, exts...)
}

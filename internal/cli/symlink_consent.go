// internal/cli/symlink_consent.go
//
// A3 PR-2 CLI surface (architect SEAM-C, design B): during an INTERACTIVE
// `mcphub install` / `mcphub install --reconcile-hub-mode`, when a client-
// config write hits a SYMLINKED destination, prompt the operator
//
//   <client> config is a symlink -> real target is X. Write there? [y/N]
//
// DEFAULT N (Enter = refuse). On `y` the write follows the symlink to its
// resolved target through the scoped-consent pipeline; on anything else (or a
// NON-interactive run) the existing default refusal stands so automation is
// never silently redirected.
//
// Wiring: the install command sets api.InteractiveSymlinkConsent (the injected
// consent port, nil in production) to promptInteractiveSymlinkConsent ONLY when
// stdin is a terminal, and CLEARS it via the returned restore func (defer). The
// port is consulted at the single client-config write choke point inside
// package api; the prompt only PRODUCES the consent — package api re-resolves
// and re-verifies the pin under the held handle, and strict mode refuses
// regardless (the port is never consulted under strict).
package cli

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"strings"

	"mcp-local-hub/internal/api"

	"golang.org/x/term"
)

// installInteractiveSymlinkConsent installs the interactive consent port when
// stdin is a terminal, prompting on `out`. It returns a restore func that
// MUST be deferred to clear the port (production default nil — a leaked port
// would make a later non-interactive process in the same image prompt-or-block,
// which the nil default exists to prevent). In a NON-interactive context it
// installs nothing and returns a no-op restore, so the existing refusal stands
// and automation is never redirected.
func installInteractiveSymlinkConsent(out io.Writer, in *os.File) (restore func()) {
	if in == nil || !term.IsTerminal(int(in.Fd())) {
		return func() {}
	}
	prev := api.InteractiveSymlinkConsent
	reader := bufio.NewReader(in)
	api.InteractiveSymlinkConsent = func(client, originalPath, pinnedParent string) bool {
		return promptInteractiveSymlinkConsent(out, reader, client, originalPath, pinnedParent)
	}
	return func() { api.InteractiveSymlinkConsent = prev }
}

// promptInteractiveSymlinkConsent prints the [y/N] prompt and reads one line.
// Returns true ONLY on an explicit y/yes (case-insensitive); Enter, n, EOF, or
// a read error all return false (default refuse). Split from the port wiring so
// it is unit-testable with an in-memory reader (no real TTY). `pinnedParent` is
// the resolved real target's parent directory the write is pinned to — shown to
// the operator so they consent to a concrete location, not an abstract
// "follow".
func promptInteractiveSymlinkConsent(out io.Writer, in *bufio.Reader, client, originalPath, pinnedParent string) bool {
	// Lead with the client name when known; fall back to a generic "Client"
	// when the destination could not be attributed to a known adapter, so the
	// line never opens with a stray leading space (F2). The client name is the
	// load-bearing first token an operator reads.
	subject := client
	if subject == "" {
		subject = "Client"
	}
	fmt.Fprintf(out,
		"%s config %s is a symlink -> real target directory is %s. Write there? [y/N] ",
		subject, originalPath, pinnedParent)
	line, err := in.ReadString('\n')
	if err != nil && err != io.EOF {
		// A read fault is treated as a refusal — never follow on an ambiguous
		// input (default-N posture).
		fmt.Fprintln(out)
		return false
	}
	switch strings.ToLower(strings.TrimSpace(line)) {
	case "y", "yes":
		return true
	default:
		// Empty (bare Enter), n/no, EOF, or anything else → refuse.
		return false
	}
}

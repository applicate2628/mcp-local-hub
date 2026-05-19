// parent_path_expand.go — Phase 2 Task 2.5 of the v0.5.x Servers
// matrix revamp. Supervisor-side helper that expands the literal
// ${parent_path} token in per-daemon env overlay values into the
// parent process's PATH value at spawn time.
//
// The token is the ONLY supported placeholder. The supervisor calls
// ExpandParentPath once per overlay row, after parsing the overlay
// and before the merged env reaches exec.Command. Per the spec, the
// expanded value is treated as opaque bytes — values go to
// exec.Command's env block, NOT a shell — so there is no second-pass
// re-scan after substitution.
//
// Behavior summary (matches spec §"${parent_path} token semantics"
// and §"Observability"):
//
//   - Returns a NEW map; the input is never mutated. The caller owns
//     the input map; treating it as immutable here keeps the surface
//     safe for concurrent overlay scans.
//   - Windows: case-insensitive lookup for the parent's PATH key
//     (`Path`, `PATH`, `path` all map to the same logical key).
//     POSIX: exact "PATH" match.
//   - When the parent has no PATH key, the token expands to "" so
//     the overlay row becomes the only PATH source (no error).
//   - Single-pass expansion: the substituted text is NOT re-scanned
//     for tokens.
//   - Any ${...} placeholder other than ${parent_path} is rejected.
//   - When an overlay's logical PATH key is set but the value does
//     NOT contain ${parent_path}, emit
//     `daemon-env-overlay-path-no-parent-token` (info) so operator
//     intent (deliberate parent-PATH drop) is auditable via the
//     hub-mcp event log.
//
// Spec: docs/superpowers/specs/2026-05-19-servers-matrix-lsp-and-env-revamp-design.md
// §"${parent_path} token semantics" + §"Observability".
// Plan: Task 2.5.

package daemon_env_overlay

import (
	"fmt"
	"regexp"
	"runtime"
	"strings"

	"mcp-local-hub/internal/api"
)

// parentPathToken is the only template token expansion supports.
const parentPathToken = "${parent_path}"

// tokenRe matches any ${...} placeholder; used to detect unknown
// tokens that must be rejected up-front. Allows letters, digits,
// underscores, hyphens, and dots inside braces — the full envelope
// of possible future tokens; we still reject anything but the one
// supported name today.
var tokenRe = regexp.MustCompile(`\$\{[A-Za-z0-9_.\-]+\}`)

// ExpandParentPath returns a new map identical to env except that
// every value's ${parent_path} literal is replaced by the parent
// process's PATH (or the empty string if the parent has no PATH).
//
// Errors:
//   - The value of any key contains a ${...} placeholder other than
//     ${parent_path}.
//
// Side effects:
//   - Best-effort emit of `daemon-env-overlay-path-no-parent-token`
//     (info) to hub-mcp.log when the overlay's logical PATH key is
//     set but the value does NOT contain ${parent_path}. Errors from
//     the emit are swallowed: observability is best-effort, not a
//     load-bearing data path. The event records the operator's
//     deliberate decision to drop the parent PATH for that daemon.
func ExpandParentPath(env map[string]string, parentEnv []string) (map[string]string, error) {
	parentPath := lookupParentPath(parentEnv)

	out := make(map[string]string, len(env))
	for k, v := range env {
		// Reject unknown ${...} tokens BEFORE substitution so an
		// unknown token can't be hidden inside a string that also
		// contains ${parent_path} (e.g. "${unknown};${parent_path}").
		if err := rejectUnknownTokens(k, v); err != nil {
			return nil, err
		}
		// Single-pass replace: strings.Replace with -1 substitutes
		// every occurrence; the substituted parent-PATH bytes are
		// NOT re-scanned (the regex above already ran on the input).
		out[k] = strings.Replace(v, parentPathToken, parentPath, -1)
	}

	// No-token check: if the overlay sets a logical PATH key whose
	// value does NOT contain ${parent_path}, the operator deliberately
	// dropped parent PATH for that daemon. Emit the info event so the
	// decision is auditable via hub-mcp.log.
	if pathKey, pathVal, ok := lookupLogicalPath(env); ok {
		if !strings.Contains(pathVal, parentPathToken) {
			// Best-effort emit. The fields stay light — the operator's
			// taskName is added by the supervisor call site (this
			// helper has no taskName context).
			_ = api.LogHubMcpEvent("info", "daemon-env-overlay-path-no-parent-token", map[string]any{
				"key": pathKey,
			})
		}
	}

	return out, nil
}

// lookupParentPath returns the value of the parent's PATH variable.
// Windows: case-insensitive match on `PATH`. POSIX: exact match. When
// no PATH entry is present, returns "" — the spec contract is that
// the overlay row becomes the only PATH source in that case (not an
// error, per §"${parent_path} token semantics").
func lookupParentPath(parentEnv []string) string {
	for _, kv := range parentEnv {
		// Split at the first '=' only; values can legitimately
		// contain '=' (e.g. `PATH=A=B;C`).
		eq := strings.IndexByte(kv, '=')
		if eq < 0 {
			continue
		}
		key := kv[:eq]
		if isPathKey(key) {
			return kv[eq+1:]
		}
	}
	return ""
}

// lookupLogicalPath finds the overlay's PATH-shaped key and returns
// (canonical-name-as-stored, value, true). Windows: any case-folded
// match of "PATH" qualifies. POSIX: exact "PATH" only. When no PATH
// key is present, returns ("", "", false).
//
// We return the key as the OVERLAY stored it (not normalized) so the
// emit-event field can show operators which spelling they used.
func lookupLogicalPath(env map[string]string) (string, string, bool) {
	for k, v := range env {
		if isPathKey(k) {
			return k, v, true
		}
	}
	return "", "", false
}

// isPathKey reports whether key designates the PATH variable for the
// current platform. The Windows clause matches `Path`, `PATH`, `path`,
// and any other case folding; POSIX requires exact `PATH`.
func isPathKey(key string) bool {
	if runtime.GOOS == "windows" {
		return strings.EqualFold(key, "PATH")
	}
	return key == "PATH"
}

// rejectUnknownTokens returns an error if `value` contains any
// ${...} placeholder other than ${parent_path}. The key argument is
// only used to make the error message actionable (so operators can
// jump to the offending row in the YAML editor).
func rejectUnknownTokens(key, value string) error {
	for _, match := range tokenRe.FindAllString(value, -1) {
		if match != parentPathToken {
			return fmt.Errorf("daemon_env_overlay: key %q contains unknown token %q (only %q is supported)", key, match, parentPathToken)
		}
	}
	return nil
}

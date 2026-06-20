// internal/api/secrets_placeholder.go — G6 ${secret:KEY} expander.
//
// Spec: docs/superpowers/specs/2026-05-12-g6-remote-mcp-manifests-design.md
// §"Secret-reference placeholders".
//
// The ${secret:KEY} placeholder is a NEW expansion class distinct from
// the existing ${env:VAR} / ${workspaceFolder} / ${userHome} /
// ${pathSeparator} patterns used by G5 marketplace + G7 vscode import.
// Those classes expand at parse / generate time against the host env;
// ${secret:KEY} resolves at INSTALL time from the encrypted vault so
// the manifest YAML stays cleartext-free on disk and in git.
//
// Threat model (spec §"Threat model"):
//   - Manifest never carries cleartext — only the placeholder string.
//   - Vault is the single source of truth; rotation via
//     `mcphub secrets edit` + `mcphub install` re-resolves.
//   - Expanded values containing \r or \n are REJECTED before they
//     reach client config writers — defeats CRLF header injection.
//
// Scope: Headers map (and the URL field for path-segment tokens) on
// remote-http manifests. NOT used by stdio-bridge / native-http
// manifests; those have no secret-bearing surface yet.

package api

import (
	"fmt"
	"strings"

	"mcp-local-hub/internal/secrets"
)

// SecretPlaceholderRE matches well-formed `${secret:KEY}` tokens.
// Capture group 1 is the KEY; allowed characters mirror the vault's
// key-name policy (alphanumeric + underscore + hyphen + period).
// The vault's own Get/Set surface is the authoritative gate on key
// shape — this regex defines what counts as a placeholder to expand.
var SecretPlaceholderRE = secrets.SecretPlaceholderRE

// malformedSecretPrefixRE matches `${secret:` followed by anything
// that ISN'T a well-formed key terminated by `}`. Used to detect
// strings that intended a placeholder but ended up with an invalid
// key shape (e.g. `${secret:BAD KEY}` with a space, or unterminated
// `${secret:FOO`). Without this check, ExpandSecrets would silently
// pass through the malformed token and let runtime auth failures
// downstream be the first signal an operator sees.
//
// codex bot r7 P2 closure (PR #169).
var malformedSecretPrefixRE = secrets.MalformedSecretPrefixRE

// SecretLookup is the vault accessor contract used by ExpandSecrets.
// The default production resolver opens the encrypted vault at
// secrets.DefaultVaultPath() with the key at secrets.DefaultKeyPath().
// Tests inject a deterministic resolver to avoid touching the on-disk
// vault.
type SecretLookup func(key string) (string, error)

// DefaultSecretLookup opens the vault once per call and returns the
// requested key. Suitable for batch operations (expand a small map of
// headers in one install transaction); for high-frequency use callers
// should cache an opened *secrets.Vault and bind the lookup to its
// Get method.
func DefaultSecretLookup(key string) (string, error) {
	v, err := secrets.OpenVault(secrets.DefaultKeyPath(), secrets.DefaultVaultPath())
	if err != nil {
		return "", fmt.Errorf("secret %q: open vault: %w", key, err)
	}
	defer func() {
		// Best-effort close; vault has no Close method today but the
		// pattern lets us add cleanup later without changing callers.
		_ = v
	}()
	return v.Get(key)
}

// ExpandSecrets walks `s`, resolves every ${secret:KEY} via lookup,
// and returns the expanded string. Errors short-circuit:
//
//   - Missing key (lookup returns error): return that error wrapped
//     with the key name.
//   - Expanded value contains \r or \n: return CRLF-injection error
//     naming the key. The check happens AFTER lookup so the operator
//     sees the offending key without exposing the cleartext value.
//   - FINAL string contains \r or \n (even if no placeholder was
//     expanded): rejected as a literal-newline header. This catches
//     the no-placeholder bypass — a hostile manifest writing
//     `Authorization: "Bearer abc\nX-Evil: 1"` directly with no
//     placeholder still gets refused.
//
// If lookup is nil, DefaultSecretLookup is used.
//
// codex bot r1 P1 closure (PR #169): the no-placeholder fast path
// previously returned `s` unchanged, bypassing the CRLF guard. Now
// every return goes through the final-string check.
func ExpandSecrets(s string, lookup SecretLookup) (string, error) {
	final := s
	if strings.Contains(s, "${secret:") {
		// codex bot r7 P2 closure (PR #169): detect malformed
		// placeholders BEFORE expansion. Count `${secret:` prefixes
		// vs well-formed `${secret:KEY}` matches; any unmatched
		// prefix is a malformed placeholder (invalid key shape,
		// unterminated, etc.) and gets a clear error instead of
		// silently passing through to corrupt downstream client
		// config.
		wellFormed := SecretPlaceholderRE.FindAllString(s, -1)
		allPrefixes := malformedSecretPrefixRE.FindAllString(s, -1)
		if len(allPrefixes) > len(wellFormed) {
			return "", fmt.Errorf("expand secrets: input contains malformed ${secret:KEY} placeholder — keys must match [A-Za-z0-9_.-]+ and be terminated with `}` (input had %d well-formed + %d total `${secret:` prefixes; %d malformed)",
				len(wellFormed), len(allPrefixes), len(allPrefixes)-len(wellFormed))
		}

		if lookup == nil {
			lookup = DefaultSecretLookup
		}
		var firstErr error
		final = SecretPlaceholderRE.ReplaceAllStringFunc(s, func(match string) string {
			if firstErr != nil {
				return match
			}
			sub := SecretPlaceholderRE.FindStringSubmatch(match)
			if len(sub) < 2 {
				firstErr = fmt.Errorf("secret placeholder malformed: %q", match)
				return match
			}
			key := sub[1]
			val, err := lookup(key)
			if err != nil {
				firstErr = fmt.Errorf("expand ${secret:%s}: %w", key, err)
				return match
			}
			if strings.ContainsAny(val, "\r\n") {
				// Reject CRLF injection at the SECRET level so the
				// operator gets a key-named error. The final-string
				// check below is the broader guard; this gives
				// better diagnostics when the offender is a secret.
				firstErr = fmt.Errorf("expand ${secret:%s}: value contains CR or LF — refusing to write (header / URL injection guard); rotate the secret to a single-line value", key)
				return match
			}
			return val
		})
		if firstErr != nil {
			return "", firstErr
		}
	}
	// Defense-in-depth: even on the no-placeholder fast path, refuse
	// to return a string with literal CR/LF. A hostile manifest could
	// embed newlines directly in headers/URLs WITHOUT any
	// ${secret:KEY} placeholder; bot r1 P1 closure (PR #169).
	if strings.ContainsAny(final, "\r\n") {
		return "", fmt.Errorf("expand secrets: result contains CR or LF — refusing to write (header / URL injection guard); inspect the manifest for literal newline characters")
	}
	return final, nil
}

// ExpandSecretsMap walks every value in m, expands ${secret:KEY}
// placeholders, and returns the resulting map. Errors short-circuit
// with the first failing key. The input map is NOT mutated.
//
// Used by the install path to resolve `headers:` for remote-http
// manifests before writing client configs.
func ExpandSecretsMap(m map[string]string, lookup SecretLookup) (map[string]string, error) {
	if len(m) == 0 {
		return m, nil
	}
	out := make(map[string]string, len(m))
	for k, v := range m {
		expanded, err := ExpandSecrets(v, lookup)
		if err != nil {
			return nil, fmt.Errorf("expand header %q: %w", k, err)
		}
		out[k] = expanded
	}
	return out, nil
}

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
	"regexp"
	"strings"

	"mcp-local-hub/internal/secrets"
)

// SecretPlaceholderRE matches `${secret:KEY}` tokens. Capture group 1
// is the KEY; allowed characters mirror the vault's key-name policy
// (alphanumeric + underscore + hyphen + period). The vault's own
// Get/Set surface is the authoritative gate on key shape — this regex
// just defines what counts as a placeholder to expand.
var SecretPlaceholderRE = regexp.MustCompile(`\$\{secret:([A-Za-z0-9_\-.]+)\}`)

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
//
// If lookup is nil, DefaultSecretLookup is used. Strings without any
// placeholder pass through unchanged (cheap allocation-free path).
func ExpandSecrets(s string, lookup SecretLookup) (string, error) {
	if !strings.Contains(s, "${secret:") {
		return s, nil
	}
	if lookup == nil {
		lookup = DefaultSecretLookup
	}
	var firstErr error
	expanded := SecretPlaceholderRE.ReplaceAllStringFunc(s, func(match string) string {
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
			// Reject CRLF injection. Do NOT include the value in the
			// error — that would leak cleartext into logs.
			firstErr = fmt.Errorf("expand ${secret:%s}: value contains CR or LF — refusing to write (header / URL injection guard); rotate the secret to a single-line value", key)
			return match
		}
		return val
	})
	if firstErr != nil {
		return "", firstErr
	}
	return expanded, nil
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

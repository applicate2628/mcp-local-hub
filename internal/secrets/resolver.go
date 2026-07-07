package secrets

import (
	"fmt"
	"os"
	"regexp"
	"strings"
)

// SecretPlaceholderRE matches well-formed `${secret:KEY}` tokens used by
// remote-http URL and header placeholders. Capture group 1 is the vault key.
var SecretPlaceholderRE = regexp.MustCompile(`\$\{secret:([A-Za-z0-9_\-.]+)\}`)

var envPlaceholderRE = regexp.MustCompile(`\$\{env:([A-Za-z_][A-Za-z0-9_]*)\}`)

// MalformedSecretPrefixRE matches any `${secret:` prefix so callers can detect
// intended placeholders that failed the stricter SecretPlaceholderRE shape.
var MalformedSecretPrefixRE = regexp.MustCompile(`\$\{secret:`)

// SettableKeyNameRE is the SINGLE OWNER of the "is this a settable vault key
// name" predicate — the shape a key must have for `mcphub secrets set` /
// POST /api/secrets to be able to write it. It lives in this lowest layer
// (imported by BOTH internal/api and internal/config) so the secret-set path
// and the manifest required_secrets gate share one definition and can never
// disagree on which key names an operator can actually create.
//
// It is DELIBERATELY stricter than SecretPlaceholderRE above (which allows `-`
// and `.`): the placeholder regex describes the broader resolvable-REF shape
// inside a `${secret:KEY}` token, while this regex describes the narrower
// SETTABLE key-name shape. A key matching the placeholder regex but not this
// one (e.g. `bad-key`, `1foo`) is referenceable in env yet un-settable — which
// is exactly the trap required_secrets must reject so the install gate cannot
// block forever on a key the operator literally cannot set.
var SettableKeyNameRE = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_]*$`)

// ValidateSettableKeyName returns nil when name is a settable vault key name
// (matches SettableKeyNameRE), or an error naming the required shape otherwise.
// PURE: regex match only — it does NOT enforce caller-side policy such as the
// `init` reserved-name collision or a non-empty value, which stay with the
// secret-set/rotate callers in internal/api (those are HTTP-route / set-op
// concerns, not key-name shape). The error text embeds the regex source so the
// api call sites preserve their existing operator-visible SECRETS_INVALID_NAME
// wording.
func ValidateSettableKeyName(name string) error {
	if !SettableKeyNameRE.MatchString(name) {
		return fmt.Errorf("name %q does not match %s", name, SettableKeyNameRE.String())
	}
	return nil
}

// Resolver turns manifest env values (which may contain prefixes like `secret:`,
// `file:`, or `$VAR`) into plaintext for use in a child process environment.
// Resolution order per spec §3.8:
//
//	secret:<key> → vault.Get
//	file:<key>   → local config map
//	$VAR         → os.Getenv (fails if unset)
//	<literal>    → returned as-is
type Resolver struct {
	vault *Vault
	local map[string]string
}

// NewResolver builds a Resolver. Either argument may be nil if the caller knows
// that prefix is not in use; in that case, matching-prefix lookups return errors.
func NewResolver(v *Vault, local map[string]string) *Resolver {
	return &Resolver{vault: v, local: local}
}

// Resolve returns the resolved value for a manifest-style reference string.
func (r *Resolver) Resolve(ref string) (string, error) {
	switch {
	case strings.HasPrefix(ref, "secret:"):
		if r.vault == nil {
			return "", fmt.Errorf("resolve %q: vault not available", ref)
		}
		key := strings.TrimPrefix(ref, "secret:")
		return r.vault.Get(key)
	case strings.HasPrefix(ref, "file:"):
		key := strings.TrimPrefix(ref, "file:")
		if r.local == nil {
			return "", fmt.Errorf("resolve %q: local config not available", ref)
		}
		v, ok := r.local[key]
		if !ok {
			return "", fmt.Errorf("resolve %q: key %q not in local config", ref, key)
		}
		return v, nil
	case strings.HasPrefix(ref, "${env:") && envPlaceholderRE.FindString(ref) == ref:
		name := strings.TrimSuffix(strings.TrimPrefix(ref, "${env:"), "}")
		if name == "" {
			return "", fmt.Errorf("resolve %q: environment variable name is empty", ref)
		}
		v, ok := os.LookupEnv(name)
		if !ok {
			return "", fmt.Errorf("resolve %q: environment variable %q not set", ref, name)
		}
		return v, nil
	case strings.Contains(ref, "${env:"):
		var resolveErr error
		resolved := envPlaceholderRE.ReplaceAllStringFunc(ref, func(token string) string {
			if resolveErr != nil {
				return token
			}
			name := strings.TrimSuffix(strings.TrimPrefix(token, "${env:"), "}")
			v, ok := os.LookupEnv(name)
			if !ok {
				resolveErr = fmt.Errorf("resolve %q: environment variable %q not set", ref, name)
				return token
			}
			return v
		})
		if resolveErr != nil {
			return "", resolveErr
		}
		return resolved, nil
	case strings.HasPrefix(ref, "$"):
		name := strings.TrimPrefix(ref, "$")
		v, ok := os.LookupEnv(name)
		if !ok {
			return "", fmt.Errorf("resolve %q: environment variable %q not set", ref, name)
		}
		return v, nil
	default:
		return ref, nil
	}
}

// ResolveMap resolves every value in a manifest env map and returns a new map.
// If any resolution fails, an error is returned referencing the offending key.
func (r *Resolver) ResolveMap(env map[string]string) (map[string]string, error) {
	out := make(map[string]string, len(env))
	for k, v := range env {
		resolved, err := r.Resolve(v)
		if err != nil {
			return nil, fmt.Errorf("env[%s]: %w", k, err)
		}
		out[k] = resolved
	}
	return out, nil
}

// ResolveMapBestEffort resolves every value in a manifest env map, OMITTING an
// unresolvable `secret:` ref instead of failing the whole map. It returns the
// resolved subset, a map of omitted keys to their original `secret:` ref (so
// the caller can log + explicitly UNSET them), and an error for any
// NON-secret resolution failure.
//
// This is the daemon-launch path for OPTIONAL secrets (install-and-it-works:
// secrets are optional by default). A server whose `secret:` ref is not set in
// the vault still spawns — with that env var unset — so the server reports its
// own "missing API key" instead of mcphub failing the spawn cryptically.
//
// ONLY `secret:` refs are optional. `$VAR` and `file:` refs are documented
// REQUIRED resolver inputs (resolver doc + Resolve); a missing one stays
// fail-fast (returns an error) so it surfaces loudly rather than silently
// starting the daemon without required configuration (Codex #377). The
// caller MUST treat the returned omitted keys as explicit unsets in the child
// environment, NOT merely "absent from the resolved map" — otherwise a skipped
// secret whose key also exists in the parent process env would be inherited
// ambiently (Codex #377).
func (r *Resolver) ResolveMapBestEffort(env map[string]string) (resolved, omitted map[string]string, err error) {
	resolved = make(map[string]string, len(env))
	for k, v := range env {
		val, e := r.Resolve(v)
		if e != nil {
			if strings.HasPrefix(v, "secret:") {
				if omitted == nil {
					omitted = make(map[string]string, 1)
				}
				omitted[k] = v
				continue
			}
			return nil, nil, fmt.Errorf("env[%s]: %w", k, e)
		}
		resolved[k] = val
	}
	return resolved, omitted, nil
}

// HasSecretRef reports whether any value in a manifest env map is a `secret:`
// reference. Used to gate vault-read strictness: a server with no secret refs
// must not be blocked by a corrupt vault it never touches (Codex #377 r5).
func HasSecretRef(env map[string]string) bool {
	for _, v := range env {
		if strings.HasPrefix(v, "secret:") {
			return true
		}
	}
	return false
}

// OpenVaultOptional opens the vault, distinguishing an ABSENT vault (no
// secrets configured → returns nil, nil; secret refs are then optional/omitted)
// from one that EXISTS but is unreadable/undecryptable (returns nil, error).
// A caller whose manifest uses NO secret refs ignores the error and proceeds
// with a nil vault; a caller whose manifest DOES use secret refs treats the
// error as fatal (daemon) / blocking (readiness) rather than silently omitting
// secrets the operator may have set (Codex #377 r5). Single owner of the
// absent-vs-unreadable distinction shared by the daemon launch + readiness.
func OpenVaultOptional(keyPath, vaultPath string) (*Vault, error) {
	vault, err := OpenVault(keyPath, vaultPath)
	if err != nil {
		// ABSENT (→ optional) ONLY when stat proves the file does not exist.
		// Stat SUCCESS (file present) OR a non-not-exist stat error
		// (permission denied, broken mount) means the vault may exist but be
		// inaccessible — treat as UNREADABLE, never silently as "no secrets"
		// (Codex #377 r6).
		if _, statErr := os.Stat(vaultPath); statErr != nil && os.IsNotExist(statErr) {
			return nil, nil // genuinely absent
		}
		return nil, fmt.Errorf("vault exists but unreadable: %w", err)
	}
	return vault, nil
}

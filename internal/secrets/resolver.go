package secrets

import (
	"fmt"
	"os"
	"strings"
)

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

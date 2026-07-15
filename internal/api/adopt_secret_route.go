package api

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"regexp"
	"sort"
	"strings"

	"mcp-local-hub/internal/secrets"
)

var (
	adoptBareShellEnvRefRE  = regexp.MustCompile(`^\$([A-Za-z_][A-Za-z0-9_]*)$`)
	adoptBraceShellEnvRefRE = regexp.MustCompile(`^\$\{([A-Za-z_][A-Za-z0-9_]*)\}$`)
)

func rewriteAdoptSensitiveEnv(manifestName string, env map[string]string) (routedKeys []string, secretValues map[string]string, err error) {
	if len(env) == 0 {
		return nil, nil, nil
	}
	keys := make([]string, 0, len(env))
	for key := range env {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	routedSourceEnv := map[string]string{}
	for _, key := range keys {
		value := env[key]
		if name, ok := adoptShellEnvReferenceName(value); ok {
			env[key] = "${env:" + name + "}"
			continue
		}
		secretPrefixed := isSecretPrefixedAdoptEnvValue(value)
		if !secretPrefixed && (!IsSensitiveEnvName(key) || !isLiteralAdoptEnvValue(value)) {
			continue
		}
		if secretValues == nil {
			secretValues = map[string]string{}
		}
		vaultKey := adoptVaultKey(manifestName, key)
		if prior, exists := routedSourceEnv[vaultKey]; exists {
			return nil, nil, fmt.Errorf("adopt secret routing: env keys %s and %s both map to vault key %q after sanitizing; rename one before adopting", prior, key, vaultKey)
		}
		routedSourceEnv[vaultKey] = key
		secretValues[vaultKey] = value
		env[key] = "secret:" + vaultKey
		routedKeys = append(routedKeys, vaultKey)
	}
	return routedKeys, secretValues, nil
}

func adoptVaultKey(manifestName, envName string) string {
	var b strings.Builder
	b.Grow(len(manifestName) + 1 + len(envName) + len("MCP_"))
	appendAdoptVaultKeySanitized(&b, manifestName)
	b.WriteByte('_')
	appendAdoptVaultKeySanitized(&b, envName)
	key := b.String()
	if key == "" || key[0] < 'A' || key[0] > 'Z' {
		key = "MCP_" + key
	}
	return key
}

func appendAdoptVaultKeySanitized(b *strings.Builder, s string) {
	for i := 0; i < len(s); i++ {
		ch := s[i]
		switch {
		case ch >= 'a' && ch <= 'z':
			b.WriteByte(ch - 'a' + 'A')
		case (ch >= 'A' && ch <= 'Z') || (ch >= '0' && ch <= '9') || ch == '_':
			b.WriteByte(ch)
		default:
			b.WriteByte('_')
		}
	}
}

func adoptShellEnvReferenceName(value string) (string, bool) {
	trimmed := strings.TrimSpace(value)
	if m := adoptBareShellEnvRefRE.FindStringSubmatch(trimmed); len(m) == 2 {
		return m[1], true
	}
	if m := adoptBraceShellEnvRefRE.FindStringSubmatch(trimmed); len(m) == 2 {
		return m[1], true
	}
	return "", false
}

func isLiteralAdoptEnvValue(value string) bool {
	trimmed := strings.TrimSpace(value)
	if _, ok := adoptShellEnvReferenceName(trimmed); ok {
		return false
	}
	return !strings.HasPrefix(trimmed, "secret:") && !strings.HasPrefix(trimmed, "${env:")
}

func isSecretPrefixedAdoptEnvValue(value string) bool {
	return strings.HasPrefix(strings.TrimSpace(value), "secret:")
}

func persistAdoptRoutedSecrets(secretValues map[string]string) error {
	if len(secretValues) == 0 {
		return nil
	}
	keys := make([]string, 0, len(secretValues))
	for key := range secretValues {
		keys = append(keys, key)
	}
	sort.Strings(keys)

	vaultMutex.Lock()
	defer vaultMutex.Unlock()
	return secrets.WithVaultLock(secrets.DefaultVaultPath(), func() error {
		vault, err := secrets.OpenVault(secrets.DefaultKeyPath(), secrets.DefaultVaultPath())
		if err != nil {
			return fmt.Errorf("adopt secret routing: open vault: %w", err)
		}
		for _, key := range keys {
			if err := secrets.ValidateSettableKeyName(key); err != nil {
				return fmt.Errorf("adopt secret routing: derived vault key %q is not manageable by secrets set/rotate: %w", key, err)
			}
			if _, err := vault.Get(key); err == nil {
				return fmt.Errorf("adopt secret routing: vault key %q already exists; refusing to overwrite an existing secret", key)
			}
		}
		var written []string
		for _, key := range keys {
			if err := vault.Set(key, secretValues[key]); err != nil {
				if cleanupErr := deleteAdoptRoutedSecretsLocked(written); cleanupErr != nil {
					return fmt.Errorf("adopt secret routing: set %s: %w; failed to remove already-written routed vault keys %s: %v", key, err, strings.Join(sortedAdoptStrings(written), ","), cleanupErr)
				}
				return fmt.Errorf("adopt secret routing: set %s: %w", key, err)
			}
			written = append(written, key)
		}
		return nil
	})
}

func deleteAdoptRoutedSecrets(keys []string) error {
	if len(keys) == 0 {
		return nil
	}
	keys = append([]string(nil), keys...)
	sort.Strings(keys)

	vaultMutex.Lock()
	defer vaultMutex.Unlock()
	return secrets.WithVaultLock(secrets.DefaultVaultPath(), func() error {
		return deleteAdoptRoutedSecretsLocked(keys)
	})
}

func deleteAdoptRoutedSecretsLocked(keys []string) error {
	if len(keys) == 0 {
		return nil
	}
	vault, err := secrets.OpenVault(secrets.DefaultKeyPath(), secrets.DefaultVaultPath())
	if err != nil {
		if adoptRoutedSecretVaultAbsent() {
			return nil
		}
		return fmt.Errorf("adopt secret routing cleanup: open vault: %w", err)
	}
	for _, key := range keys {
		if err := vault.Delete(key); err != nil {
			return fmt.Errorf("adopt secret routing cleanup: delete %s: %w", key, err)
		}
	}
	return nil
}

// adoptRoutedSecretVaultAbsent distinguishes a missing vault blob from other
// OpenVault failures. OpenVault can also report fs.ErrNotExist for a missing
// identity, so callers must verify the vault path itself before treating routed
// secret cleanup as complete.
func adoptRoutedSecretVaultAbsent() bool {
	_, err := os.Stat(secrets.DefaultVaultPath())
	return errors.Is(err, fs.ErrNotExist)
}

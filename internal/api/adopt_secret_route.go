package api

import (
	"fmt"
	"sort"
	"strings"

	"mcp-local-hub/internal/secrets"
)

func rewriteAdoptSensitiveEnv(manifestName string, env map[string]string) (routedKeys []string, secretValues map[string]string) {
	if len(env) == 0 {
		return nil, nil
	}
	keys := make([]string, 0, len(env))
	for key := range env {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		value := env[key]
		if !IsSensitiveEnvName(key) || !isLiteralAdoptEnvValue(value) {
			continue
		}
		if secretValues == nil {
			secretValues = map[string]string{}
		}
		vaultKey := adoptVaultKey(manifestName, key)
		secretValues[vaultKey] = value
		env[key] = "secret:" + vaultKey
		routedKeys = append(routedKeys, vaultKey)
	}
	return routedKeys, secretValues
}

func adoptVaultKey(manifestName, envName string) string {
	return manifestName + "." + envName
}

func isLiteralAdoptEnvValue(value string) bool {
	trimmed := strings.TrimSpace(value)
	return !strings.HasPrefix(trimmed, "secret:") && !strings.HasPrefix(trimmed, "${env:")
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
			if _, err := vault.Get(key); err == nil {
				return fmt.Errorf("adopt secret routing: vault key %q already exists; refusing to overwrite an existing secret", key)
			}
		}
		for _, key := range keys {
			if err := vault.Set(key, secretValues[key]); err != nil {
				return fmt.Errorf("adopt secret routing: set %s: %w", key, err)
			}
		}
		return nil
	})
}

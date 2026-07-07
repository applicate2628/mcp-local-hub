package api

import (
	"fmt"
	"sort"
	"strings"

	"mcp-local-hub/internal/secrets"
)

func rewriteAdoptSensitiveEnv(env map[string]string) (routedKeys []string, secretValues map[string]string) {
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
		secretValues[key] = value
		env[key] = "secret:" + key
		routedKeys = append(routedKeys, key)
	}
	return routedKeys, secretValues
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
			if err := vault.Set(key, secretValues[key]); err != nil {
				return fmt.Errorf("adopt secret routing: set %s: %w", key, err)
			}
		}
		return nil
	})
}

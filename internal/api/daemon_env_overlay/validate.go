package daemon_env_overlay

import (
	"fmt"
	"regexp"
)

var envKeyPattern = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// ValidEnvKey reports whether key is a portable environment-variable key.
func ValidEnvKey(key string) bool {
	return envKeyPattern.MatchString(key)
}

// HasControlChar reports whether s contains a newline, NUL, or any
// non-tab control character. Env override values are persisted and echoed
// through logs/UI, so multiline/control payloads are rejected before write.
func HasControlChar(s string) bool {
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c == '\t' {
			continue
		}
		if c < 0x20 || c == 0x7f {
			return true
		}
	}
	return false
}

// ValidateEnvMap checks the write-time env override contract shared by
// CLI and GUI surfaces.
func ValidateEnvMap(env map[string]string) error {
	for k, v := range env {
		if !ValidEnvKey(k) {
			return fmt.Errorf("invalid env key %q: must match [A-Za-z_][A-Za-z0-9_]*", k)
		}
		if HasControlChar(v) {
			return fmt.Errorf("invalid env value for key %q: contains newline/NUL/control char", k)
		}
	}
	return nil
}

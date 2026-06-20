package api

import (
	"os"
	"path/filepath"
	"strings"

	"mcp-local-hub/internal/secrets"
)

func init() {
	secrets.SetVaultFileReader(ReadStateFileInodeAnchored)
	secrets.SetVaultFileWriter(func(path string, data []byte, _ os.FileMode) error {
		return WriteStateFileBytesLockHeld(path, data)
	})
}

// ReadStateFileInodeAnchored exposes the internal state-file anchored reader to
// sibling packages that own trust-sensitive state reads but cannot import
// unexported api helpers. It preserves os.ErrNotExist and all verifier errors so
// each caller can keep its existing missing-file semantics.
func ReadStateFileInodeAnchored(path string) ([]byte, error) {
	return readStateFileInodeAnchored(path)
}

// ReadStateFileInodeAnchoredEnvStrictOnly is the bootstrap variant for recovery
// paths that must not consult persisted supervisor-intent strict_mode while
// reading the state needed to repair that same persisted posture. It still
// honors MCPHUB_REQUIRE_SINGLE_USER_HOME and keeps the inode/symlink checks.
func ReadStateFileInodeAnchoredEnvStrictOnly(path string) ([]byte, error) {
	return readStateFileInodeAnchoredEnvStrictOnly(path)
}

func readStateFileInodeAnchoredEnvStrictOnly(path string) ([]byte, error) {
	return readStateFileInodeAnchoredWithStrictPolicy(path, operatorRequiresSingleUserHomeEnvOnly)
}

func readStateFileInodeAnchoredEnvStrictOnlyNoAudit(path string) ([]byte, error) {
	return readStateFileInodeAnchoredWithStrictPolicyNoAudit(path, operatorRequiresSingleUserHomeEnvOnly)
}

func isSecretBearingStateFilePath(path string) bool {
	base := strings.ToLower(filepath.Base(path))
	switch base {
	case hubMcpTokensFileLeaf,
		hubMcpControlTokenFileLeaf,
		"hub-mcp-control.tok",
		"secrets.age",
		".age-key":
		return true
	}
	return strings.Contains(base, "token") ||
		strings.Contains(base, "secret") ||
		strings.Contains(base, "credential") ||
		strings.Contains(base, "vault")
}

func operatorRequiresSingleUserHomeEnvOnly() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv(RequireSingleUserHomeEnv))) {
	case "1", "true":
		return true
	default:
		return false
	}
}

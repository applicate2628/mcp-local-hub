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
		hubMcpEndpointFileLeaf,
		hubMcpControlTokenFileLeaf,
		"hub-mcp-control.tok",
		"secrets.age",
		".age-key":
		return true
	}
	// Adopt pre-adopt provenance snapshots (<state-dir>/adopt-provenance/<manifest>/
	// <client>.snapshot) pin the client's WHOLE pre-adopt config, which may embed
	// literal secret env values (adopted_entries.go writeAdoptClientSnapshot). Treat
	// them as secret-bearing so a read-broadened snapshot hard-fails like secrets.age
	// instead of warn-and-proceeding (bug 2026-07-11 P2-2) — the compensating control
	// for the snapshots the classifier fix deliberately retains longer.
	if strings.HasSuffix(base, adoptSnapshotFileSuffix) {
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

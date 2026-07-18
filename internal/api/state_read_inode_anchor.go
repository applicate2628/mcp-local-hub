package api

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"mcp-local-hub/internal/secrets"
)

// GUIRestartNonceFileLeaf is the single state-file leaf authorized for the
// restart-child readiness credential.
const GUIRestartNonceFileLeaf = "gui-restart-nonce"

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

// ConsumeStateSecretFileInodeAnchored opens an owner-only secret state file,
// unlinks the verified entry while retaining the opened file identity, and
// returns its bytes. The caller owns the returned buffer and must zero it when
// finished. Verification and unlink are performed by the platform-specific
// inode-anchored reader so no post-read path re-walk can leave the credential
// at its original entry.
func ConsumeStateSecretFileInodeAnchored(path string, expectedBytes int64) ([]byte, error) {
	if expectedBytes <= 0 {
		return nil, fmt.Errorf("consume state secret %s: expected size must be positive", path)
	}
	value, err := readStateFileInodeAnchoredWithOptions(
		path,
		func() bool { return true },
		expectedBytes,
		false,
		true,
	)
	if err != nil {
		return nil, err
	}
	if int64(len(value)) != expectedBytes {
		actual := len(value)
		zeroStateSecretBytes(value)
		return nil, fmt.Errorf("consume state secret %s: size = %d, want %d bytes", path, actual, expectedBytes)
	}
	return value, nil
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
		GUIRestartNonceFileLeaf,
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

func zeroStateSecretBytes(value []byte) {
	for i := range value {
		value[i] = 0
	}
}

func operatorRequiresSingleUserHomeEnvOnly() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv(RequireSingleUserHomeEnv))) {
	case "1", "true":
		return true
	default:
		return false
	}
}

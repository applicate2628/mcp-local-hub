package api

import (
	"os"
	"strings"
)

// ReadStateFileInodeAnchored exposes the internal state-file anchored reader to
// sibling packages that own trust-sensitive state reads but cannot import
// unexported api helpers. It preserves os.ErrNotExist and all verifier errors so
// each caller can keep its existing missing-file semantics.
func ReadStateFileInodeAnchored(path string) ([]byte, error) {
	return readStateFileInodeAnchored(path)
}

func readStateFileInodeAnchoredEnvStrictOnly(path string) ([]byte, error) {
	return readStateFileInodeAnchoredWithStrictPolicy(path, operatorRequiresSingleUserHomeEnvOnly)
}

func operatorRequiresSingleUserHomeEnvOnly() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv(RequireSingleUserHomeEnv))) {
	case "1", "true":
		return true
	default:
		return false
	}
}

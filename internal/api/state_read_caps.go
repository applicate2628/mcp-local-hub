package api

import (
	"path/filepath"
	"strings"
)

const (
	// maxStateFileBytes caps ordinary hub/state control files. These files
	// should stay small; the cap is OOM protection for swapped-in payloads.
	maxStateFileBytes = 1 << 20
	// maxIntentFileBytes is the daemon/supervisor intent read ceiling. Intent
	// files are per-task maps/lists and can legitimately exceed the small
	// hub-state cap.
	maxIntentFileBytes = 16 << 20
	// maxAgeKeyFileBytes bounds the age identity file. Real X25519 identity
	// files are tiny; 64 KiB leaves room for comments / future multi-identity
	// forms without allowing an unbounded key read.
	maxAgeKeyFileBytes = 64 << 10
	// maxVaultBlobFileBytes bounds the encrypted secrets vault. The vault can
	// legitimately exceed the small hub-state cap as operators add secrets.
	maxVaultBlobFileBytes = 16 << 20
	// maxWorkspaceRegistryFileBytes bounds workspaces.yaml. The registry can
	// legitimately exceed the small hub-state cap on hosts with many workspaces.
	maxWorkspaceRegistryFileBytes = 16 << 20
)

func stateFileReadCapBytes(path string) int64 {
	base := strings.ToLower(filepath.Base(path))
	// Adopt pre-adopt provenance snapshots pin whole client configs, which can
	// exceed the ordinary state-file ceiling.
	if strings.HasSuffix(filepath.Base(path), ".snapshot") {
		return maxIntentFileBytes
	}
	switch base {
	case intentFileLeaf, supervisorIntentFileLeaf:
		return maxIntentFileBytes
	case ".age-key":
		return maxAgeKeyFileBytes
	case "secrets.age":
		return maxVaultBlobFileBytes
	case "workspaces.yaml":
		return maxWorkspaceRegistryFileBytes
	default:
		return maxStateFileBytes
	}
}

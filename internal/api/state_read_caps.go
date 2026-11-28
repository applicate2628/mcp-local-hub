package api

import (
	"errors"
	"path/filepath"
	"strings"
)

const stateFileReadPathCapacityError = "positive non-overflowing observed read path/cap required"

// ValidateStateFileReadCapacity checks the existing observed reader's capacity
// domain. The allocation headroom is arithmetic safety, not a read allowance:
// callers still own their permitted byte count, path, policy and authority.
func ValidateStateFileReadCapacity(maxBytes int64) error {
	maximum := int64(int(^uint(0) >> 1))
	if maxBytes <= 0 || maxBytes > maximum-4096 {
		return errors.New(stateFileReadPathCapacityError)
	}
	return nil
}

const (
	// maxStateFileBytes caps ordinary hub/state control files. These files
	// should stay small; the cap is OOM protection for swapped-in payloads.
	maxStateFileBytes = 1 << 20
	// maxIntentFileBytes is the daemon/supervisor intent read ceiling. Intent
	// files are per-task maps/lists and can legitimately exceed the small
	// hub-state cap.
	maxIntentFileBytes = 16 << 20
	// MaxClientConfigBackupBytes bounds a rolling client-config backup before it
	// becomes a retained reconcile pin. Client configs can legitimately exceed
	// ordinary hub-state files, but a pin must remain bounded before a client
	// mutation can proceed.
	MaxClientConfigBackupBytes = maxIntentFileBytes
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
	if strings.HasSuffix(base, ".snapshot") {
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

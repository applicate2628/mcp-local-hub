package api

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// InstallTaskRecoveryError reports a failed replacement even when the prior
// definition was registered again. A recovery error may follow an applied
// registration; it is not proof of absence. SnapshotPath names retained private
// recovery material, never XML or credentials in the diagnostic itself.
type InstallTaskRecoveryError struct {
	TaskName     string
	SnapshotPath string
	Cause        error
	Recovery     error
}

func (e *InstallTaskRecoveryError) Error() string {
	if e.Recovery != nil {
		return fmt.Sprintf("create task %s: %v; restore prior definition without replacement: %v; recovery snapshot retained at %q", e.TaskName, e.Cause, e.Recovery, e.SnapshotPath)
	}
	return fmt.Sprintf("create task %s: %v; prior definition registered again; recovery snapshot retained at %q", e.TaskName, e.Cause, e.SnapshotPath)
}

func (e *InstallTaskRecoveryError) Unwrap() []error {
	if e.Recovery == nil {
		return []error{e.Cause}
	}
	return []error{e.Cause, e.Recovery}
}

// installTaskRecoverySnapshot records provenance, not a transaction outcome.
// XML is base64 in JSON so its original encoding and bytes survive unchanged.
// It does not contain separately stored scheduler credentials or object ACLs.
type installTaskRecoverySnapshot struct {
	Version       int    `json:"version"`
	TaskName      string `json:"task_name"`
	CapturedAtUTC string `json:"captured_at_utc"`
	XML           []byte `json:"xml"`
	XMLSHA256     string `json:"xml_sha256"`
}

func retainInstallTaskSnapshot(name string, xml []byte) (string, error) {
	dir, err := DaemonStateDir()
	if err != nil {
		return "", err
	}
	return saveInstallTaskSnapshot(dir, "scheduler-recovery-"+rand.Text()+".json", name, xml)
}

// saveInstallTaskSnapshot deliberately retains every admitted snapshot, even
// after successful replacement. Later install steps can still fail. There is
// no automatic pruning or path-based cleanup of these recovery files.
func saveInstallTaskSnapshot(dir, leaf, name string, xml []byte) (string, error) {
	if err := validateStateFileName(leaf); err != nil {
		return "", err
	}
	path := filepath.Join(dir, leaf)
	if len(xml) == 0 || len(xml) > maxStateFileBytes || len(name) > maxStateFileBytes {
		return path, fmt.Errorf("recovery snapshot input is empty or exceeds the state-file limit")
	}
	xmlHash := sha256.Sum256(xml)
	raw, err := json.MarshalIndent(installTaskRecoverySnapshot{
		Version: 1, TaskName: name, CapturedAtUTC: time.Now().UTC().Format(time.RFC3339Nano),
		XML: xml, XMLSHA256: hex.EncodeToString(xmlHash[:]),
	}, "", "  ")
	if err != nil {
		return path, err
	}
	raw = append(raw, '\n')
	if len(raw) > maxStateFileBytes {
		return path, fmt.Errorf("serialized recovery snapshot exceeds the %d-byte state-file limit", maxStateFileBytes)
	}
	// This generic byte writer creates a protected owner-only file and enforces
	// the parent permission gate. Do not relax it for sensitive task arguments.
	created, err := SecureCreateClientConfigIfMissing(path, raw)
	if err != nil {
		return path, err
	}
	if !created {
		return path, fmt.Errorf("recovery snapshot path is already occupied: %w", os.ErrExist)
	}
	fileHash := sha256.Sum256(raw)
	if _, err := ReadStateFileBeneathRootNoFollow(context.Background(), dir, []string{leaf}, hex.EncodeToString(fileHash[:])); err != nil {
		return path, fmt.Errorf("verify newly created recovery snapshot: %w", err)
	}
	return path, nil
}

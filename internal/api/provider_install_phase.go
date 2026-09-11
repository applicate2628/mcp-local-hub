package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
)

const (
	providerInstallPhaseLeaf       = "provider-install-phase.json"
	providerInstallPhaseVersion    = 1
	providerInstallPhaseNotStarted = "not_started"
	providerInstallPhaseStarted    = "started"
)

type providerInstallPhaseV1 struct {
	Version int    `json:"version"`
	Phase   string `json:"phase"`
}

func providerInstallPhasePath(manifestName string) (string, error) {
	dir, err := adoptSnapshotDir(manifestName)
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, providerInstallPhaseLeaf), nil
}

func encodeProviderInstallPhase(phase string) ([]byte, error) {
	if phase != providerInstallPhaseNotStarted && phase != providerInstallPhaseStarted {
		return nil, fmt.Errorf("E_PROVIDER_LIFECYCLE_UNSUPPORTED")
	}
	return json.Marshal(providerInstallPhaseV1{Version: providerInstallPhaseVersion, Phase: phase})
}

func readProviderInstallPhase(manifestName string) (string, error) {
	path, err := providerInstallPhasePath(manifestName)
	if err != nil {
		return "", err
	}
	raw, err := ReadStateFileInodeAnchored(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			// Missing is deliberately UNKNOWN, never equivalent to not_started.
			// Provider provenance written before this marker existed therefore
			// stays fail-closed rather than acquiring synthetic history.
			return "", fmt.Errorf("E_PROVIDER_LIFECYCLE_UNSUPPORTED")
		}
		return "", fmt.Errorf("E_PROVIDER_LIFECYCLE_UNSUPPORTED")
	}
	var record providerInstallPhaseV1
	if err := json.Unmarshal(raw, &record); err != nil || record.Version != providerInstallPhaseVersion {
		return "", fmt.Errorf("E_PROVIDER_LIFECYCLE_UNSUPPORTED")
	}
	if record.Phase != providerInstallPhaseNotStarted && record.Phase != providerInstallPhaseStarted {
		return "", fmt.Errorf("E_PROVIDER_LIFECYCLE_UNSUPPORTED")
	}
	return record.Phase, nil
}

// initializeProviderInstallPhase is called before the provider activation CAS.
// The marker lives inside the provenance snapshot directory, so the existing
// snapshots-first abort/close owner removes it with the rest of the recovery
// state. Existing bytes are accepted only when they are the exact initial state;
// an existing corrupt/link/unreadable marker is never rewritten into invented
// history.
func initializeProviderInstallPhase(manifestName string) error {
	path, err := providerInstallPhasePath(manifestName)
	if err != nil {
		return err
	}
	_, statErr := os.Lstat(path)
	switch {
	case statErr == nil:
		phase, readErr := readProviderInstallPhase(manifestName)
		if readErr != nil || phase != providerInstallPhaseNotStarted {
			return fmt.Errorf("E_PROVIDER_LIFECYCLE_UNSUPPORTED")
		}
		return nil
	case !errors.Is(statErr, fs.ErrNotExist):
		return fmt.Errorf("E_PROVIDER_LIFECYCLE_UNSUPPORTED")
	}

	raw, err := encodeProviderInstallPhase(providerInstallPhaseNotStarted)
	if err != nil {
		return err
	}
	if err := WriteStateFileBytesAtomic(path, raw); err != nil {
		return fmt.Errorf("E_PROVIDER_LIFECYCLE_UNSUPPORTED: initialize provider install phase: %w", err)
	}
	phase, err := readProviderInstallPhase(manifestName)
	if err != nil || phase != providerInstallPhaseNotStarted {
		return fmt.Errorf("E_PROVIDER_LIFECYCLE_UNSUPPORTED")
	}
	return nil
}

// markProviderInstallStartedForTask is invoked only after the fail-closed
// server-install audit append succeeds and before executeInstallTo may mutate a
// scheduler/client/intent surface. It scans the durable adopting provider rows
// instead of parsing a potentially ambiguous hyphenated task name; an exact
// canonical task match is the authority. No matching provider adoption is a
// normal non-provider install and therefore a no-op.
func markProviderInstallStartedForTask(taskName string) error {
	canonical := canonicalIntentTaskKey(taskName)
	store, err := readAdoptedEntries()
	if err != nil {
		return fmt.Errorf("E_PROVIDER_LIFECYCLE_UNSUPPORTED")
	}
	var manifestName string
	for i := range store.Records {
		rec := &store.Records[i]
		if rec.ProviderSource == nil || rec.OperationState != AdoptOperationStateAdopting {
			continue
		}
		expected := canonicalIntentTaskKey("mcp-local-hub-" + rec.ManifestName + "-" + adoptDefaultDaemonName)
		if expected != canonical {
			continue
		}
		if manifestName != "" {
			return fmt.Errorf("E_PROVIDER_LIFECYCLE_UNSUPPORTED")
		}
		manifestName = rec.ManifestName
	}
	if manifestName == "" {
		return nil
	}
	phase, err := readProviderInstallPhase(manifestName)
	if err != nil {
		return err
	}
	if phase == providerInstallPhaseStarted {
		return nil
	}
	if phase != providerInstallPhaseNotStarted {
		return fmt.Errorf("E_PROVIDER_LIFECYCLE_UNSUPPORTED")
	}
	path, err := providerInstallPhasePath(manifestName)
	if err != nil {
		return err
	}
	raw, err := encodeProviderInstallPhase(providerInstallPhaseStarted)
	if err != nil {
		return err
	}
	if err := WriteStateFileBytesAtomic(path, raw); err != nil {
		return fmt.Errorf("E_PROVIDER_LIFECYCLE_UNSUPPORTED: mark provider install started: %w", err)
	}
	phase, err = readProviderInstallPhase(manifestName)
	if err != nil || phase != providerInstallPhaseStarted {
		return fmt.Errorf("E_PROVIDER_LIFECYCLE_UNSUPPORTED")
	}
	return nil
}

func providerInstallNeverStarted(rec *AdoptProvenanceRecord) (bool, error) {
	if rec == nil || rec.ProviderSource == nil {
		return false, fmt.Errorf("E_PROVIDER_SOURCE_CHANGED")
	}
	phase, err := readProviderInstallPhase(rec.ManifestName)
	if err != nil {
		return false, err
	}
	return phase == providerInstallPhaseNotStarted, nil
}

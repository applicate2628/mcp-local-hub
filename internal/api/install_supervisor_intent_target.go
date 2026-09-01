package api

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
)

// InstallSupervisorIntentTarget is the one daemon row an intent-only install
// refresh owns. It deliberately excludes scheduler, reconcile, autostart and
// client configuration lifecycles.
type InstallSupervisorIntentTarget struct {
	Row         *SupervisorDaemon
	Fingerprint string
}

// BuildInstallSupervisorIntentTarget parses the exact manifest bytes, builds
// the normal frozen install plan with client writes disabled, binds its hash,
// and extracts exactly the provenance-owned daemon row.
func buildInstallSupervisorIntentTarget(manifestName string, manifestYAML []byte, port int) (InstallSupervisorIntentTarget, InstallMutationReceiptV1, error) {
	m, err := parseManifestForName(manifestName, manifestYAML)
	if err != nil {
		return InstallSupervisorIntentTarget{}, InstallMutationReceiptV1{}, err
	}
	daemonName := ""
	for _, d := range m.Daemons {
		if d.Port == port {
			if daemonName != "" {
				return InstallSupervisorIntentTarget{}, InstallMutationReceiptV1{}, fmt.Errorf("manifest port %d is ambiguous", port)
			}
			daemonName = d.Name
		}
	}
	if daemonName == "" {
		return InstallSupervisorIntentTarget{}, InstallMutationReceiptV1{}, fmt.Errorf("manifest has no daemon on provenance port %d", port)
	}
	plan, err := BuildPlanWithOpts(m, BuildPlanOpts{DaemonFilter: daemonName, SkipClientConfigWrites: true})
	if err != nil {
		return InstallSupervisorIntentTarget{}, InstallMutationReceiptV1{}, err
	}
	bindPlanSupervisorIntentManifestHash(plan, ManifestHashContent(manifestYAML))
	rows, err := supervisorDaemonsFromPlan(m, plan, daemonName)
	if err != nil || len(rows) != 1 {
		if err == nil {
			err = fmt.Errorf("expected exactly one supervisor row, got %d", len(rows))
		}
		return InstallSupervisorIntentTarget{}, InstallMutationReceiptV1{}, err
	}
	row := rows[0]
	receipt := frozenInstallMutationReceipt(plan, InstallOpts{Server: manifestName, DaemonFilter: daemonName, SkipClientConfigWrites: true})
	return InstallSupervisorIntentTarget{Row: &row, Fingerprint: supervisorIntentTargetFingerprint(&row)}, receipt, nil
}

// ReadInstallSupervisorIntentTarget reads only the exact task row selected by
// the frozen target. It never writes or starts any process.
func readInstallSupervisorIntentTarget(taskName string) (InstallSupervisorIntentTarget, error) {
	path, err := DefaultSupervisorIntentPath()
	if err != nil {
		return InstallSupervisorIntentTarget{}, err
	}
	intent, err := ReadSupervisorIntent(path)
	if err != nil {
		return InstallSupervisorIntentTarget{}, err
	}
	return supervisorIntentTargetFromFile(intent, taskName)
}

// MaterializeInstallSupervisorIntentTarget applies one frozen target under the
// canonical intent lock. It is intentionally intent-only: no admission probe,
// reconcile, autostart, scheduler, nudge, kill, start or stop is reachable.
func materializeInstallSupervisorIntentTarget(expected, desired InstallSupervisorIntentTarget, receipt InstallMutationReceiptV1) (InstallMutationReceiptV1, error) {
	if len(receipt.ClientConfigSettlements) != 0 {
		return receipt, fmt.Errorf("intent-only receipt carries client settlements")
	}
	if desired.Row == nil || desired.Fingerprint == "" {
		return receipt, fmt.Errorf("desired supervisor intent target is empty")
	}
	path, err := DefaultSupervisorIntentPath()
	if err != nil {
		return receipt, err
	}
	result, err := MutateSupervisorIntentIfChangedReturning(path, func(intent *SupervisorIntentFile) (bool, error) {
		current, currentErr := supervisorIntentTargetFromFile(intent, desired.Row.TaskName)
		if currentErr != nil {
			return false, currentErr
		}
		if current.Fingerprint != expected.Fingerprint {
			return false, ErrInstallSupervisorIntentTargetConflict
		}
		return mergeInstallSupervisorIntentTargetRow(intent, desired.Row)
	})
	if err != nil {
		return receipt, err
	}
	actual, err := supervisorIntentTargetFromFile(result.Intent, desired.Row.TaskName)
	if err != nil {
		return receipt, err
	}
	if actual.Fingerprint != desired.Fingerprint {
		return receipt, ErrInstallSupervisorIntentTargetConflict
	}
	receipt.ClientConfigSettlements = nil
	receipt.Committed = true
	return receipt, nil
}

// RestoreInstallSupervisorIntentTarget restores exactly the pre-update target
// row after confirming the current target fingerprint. Sibling rows remain in
// the fresh locked document untouched.
func restoreInstallSupervisorIntentTarget(expected, restore InstallSupervisorIntentTarget) error {
	if expected.Row == nil || expected.Fingerprint == "" {
		return ErrInstallSupervisorIntentTargetConflict
	}
	path, err := DefaultSupervisorIntentPath()
	if err != nil {
		return err
	}
	result, err := MutateSupervisorIntentIfChangedReturning(path, func(intent *SupervisorIntentFile) (bool, error) {
		current, currentErr := supervisorIntentTargetFromFile(intent, expected.Row.TaskName)
		if currentErr != nil {
			return false, currentErr
		}
		if current.Fingerprint != expected.Fingerprint {
			return false, ErrInstallSupervisorIntentTargetConflict
		}
		return mergeInstallSupervisorIntentTargetRow(intent, restore.Row)
	})
	if err != nil {
		return err
	}
	actual, err := supervisorIntentTargetFromFile(result.Intent, expected.Row.TaskName)
	if err != nil || actual.Fingerprint != restore.Fingerprint {
		return ErrInstallSupervisorIntentTargetConflict
	}
	return nil
}

// ensureInstallSupervisorIntentTarget classifies the current fresh target
// before mutation: expected means apply, desired means idempotent success, and
// every third fingerprint is a conflict that preserves the caller's journal.
func ensureInstallSupervisorIntentTarget(expected, desired InstallSupervisorIntentTarget, receipt InstallMutationReceiptV1) (InstallMutationReceiptV1, error) {
	if desired.Row == nil {
		return receipt, ErrInstallSupervisorIntentTargetConflict
	}
	current, err := readInstallSupervisorIntentTarget(desired.Row.TaskName)
	if err != nil {
		return receipt, err
	}
	if current.Fingerprint == desired.Fingerprint {
		receipt.ClientConfigSettlements = nil
		receipt.Committed = true
		return receipt, nil
	}
	if current.Fingerprint != expected.Fingerprint {
		return receipt, ErrInstallSupervisorIntentTargetConflict
	}
	return materializeInstallSupervisorIntentTarget(expected, desired, receipt)
}

var ErrInstallSupervisorIntentTargetConflict = fmt.Errorf("install supervisor intent target conflict")

func supervisorIntentTargetFromFile(intent *SupervisorIntentFile, taskName string) (InstallSupervisorIntentTarget, error) {
	if intent == nil {
		return InstallSupervisorIntentTarget{}, fmt.Errorf("supervisor intent missing")
	}
	var row *SupervisorDaemon
	for i := range intent.Daemons {
		if intent.Daemons[i].TaskName != taskName {
			continue
		}
		if row != nil {
			return InstallSupervisorIntentTarget{}, ErrInstallSupervisorIntentTargetConflict
		}
		copy := intent.Daemons[i]
		row = &copy
	}
	if row == nil {
		return InstallSupervisorIntentTarget{Fingerprint: supervisorIntentTargetFingerprint(nil)}, nil
	}
	return InstallSupervisorIntentTarget{Row: row, Fingerprint: supervisorIntentTargetFingerprint(row)}, nil
}

// mergeInstallSupervisorIntentTargetRow is the target-row extraction of the
// canonical buildMergedSupervisorIntent replacement rule. It replaces only the
// exact task identity and preserves every sibling descriptor and intent field.
func mergeInstallSupervisorIntentTargetRow(intent *SupervisorIntentFile, desired *SupervisorDaemon) (bool, error) {
	if intent == nil || desired == nil || desired.TaskName == "" {
		return false, fmt.Errorf("invalid supervisor intent target")
	}
	kept := make([]SupervisorDaemon, 0, len(intent.Daemons))
	found := false
	for _, row := range intent.Daemons {
		if row.TaskName == desired.TaskName {
			if found {
				return false, ErrInstallSupervisorIntentTargetConflict
			}
			found = true
			continue
		}
		kept = append(kept, row)
	}
	kept = append(kept, *desired)
	if found && supervisorIntentTargetFingerprint(intent.FindSupervisorDaemonByTaskName(desired.TaskName)) == supervisorIntentTargetFingerprint(desired) {
		return false, nil
	}
	intent.Daemons = kept
	return true, nil
}

func supervisorIntentTargetFingerprint(row *SupervisorDaemon) string {
	if row == nil {
		return "absent"
	}
	raw, _ := json.Marshal(row)
	sum := sha256.Sum256(bytes.TrimSpace(raw))
	return hex.EncodeToString(sum[:])
}

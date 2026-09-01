package api

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"mcp-local-hub/internal/config"
	"mcp-local-hub/internal/mcpcompat"

	"gopkg.in/yaml.v3"
)

// AdoptProfileUpdateFailure is a stable, path-free failure projection for the
// hidden provenance repair command. Its cause is deliberately retained only for
// callers which need errors.Is/As; CLI output uses FailureID.
type AdoptProfileUpdateFailure struct {
	FailureID string
	Stage     string
	cause     error
}

func (e *AdoptProfileUpdateFailure) Error() string { return e.FailureID }
func (e *AdoptProfileUpdateFailure) Unwrap() error { return e.cause }

const (
	AdoptProfileUpdateInvalidProfile = "ADOPT_PROFILE_INVALID"
	AdoptProfileUpdateNotAdopted     = "ADOPT_PROFILE_NOT_ADOPTED"
	AdoptProfileUpdateIdentity       = "ADOPT_PROFILE_IDENTITY_MISMATCH"
	AdoptProfileUpdateManifestCAS    = "ADOPT_PROFILE_MANIFEST_CAS"
	AdoptProfileUpdateRecovery       = "ADOPT_PROFILE_RECOVERY_REFUSED"
	AdoptProfileUpdateRefresh        = "ADOPT_PROFILE_INTENT_REFRESH_FAILED"
	AdoptProfileUpdateSettlement     = "ADOPT_PROFILE_CLIENT_SETTLEMENT_UNEXPECTED"
)

func adoptProfileFailure(id, stage string, cause error) error {
	return &AdoptProfileUpdateFailure{FailureID: id, Stage: stage, cause: cause}
}

func asAdoptProfileFailure(id, stage string, err error) error {
	if err == nil {
		return nil
	}
	var typed *AdoptProfileUpdateFailure
	if errors.As(err, &typed) {
		return err
	}
	return adoptProfileFailure(id, stage, err)
}

// AdoptProfileUpdatePlan is the redacted, side-effect-free projection used by
// `mcphub adopt-provenance update-profile`. RestartRequired is intentionally
// true: the command refreshes desired supervisor intent but never kills or
// starts a daemon itself.
type AdoptProfileUpdatePlan struct {
	ManifestName    string
	Profile         string
	OldProfile      string
	OldManifestHash string
	NewManifestHash string
	RestartRequired bool
	Noop            bool
}

type AdoptProfileUpdateOpts struct {
	LeaseOwner AdoptLeaseOwner
}

// AdoptProfileUpdateJournal is short-lived recovery state stored inside the
// existing adopted-entries atomic document. It is never a second file and is
// cleared only once manifest, provenance and intent have converged.
type AdoptProfileUpdateJournal struct {
	ManifestName    string                        `json:"manifest_name"`
	Profile         string                        `json:"profile"`
	OldProfile      string                        `json:"old_profile,omitempty"`
	OldManifestHash string                        `json:"old_manifest_hash"`
	NewManifestHash string                        `json:"new_manifest_hash"`
	OldManifestYAML string                        `json:"old_manifest_yaml"`
	NewManifestYAML string                        `json:"new_manifest_yaml"`
	OldTarget       InstallSupervisorIntentTarget `json:"old_target"`
	NewTarget       InstallSupervisorIntentTarget `json:"new_target"`
	Rollback        bool                          `json:"rollback,omitempty"`
}

func (a *API) BuildAdoptProfileUpdatePlan(manifestName, profile string) (*AdoptProfileUpdatePlan, error) {
	return buildAdoptProfileUpdatePlan(manifestName, profile)
}

func buildAdoptProfileUpdatePlan(manifestName, profile string) (*AdoptProfileUpdatePlan, error) {
	if err := CheckManifestName(manifestName); err != nil {
		return nil, adoptProfileFailure(AdoptProfileUpdateIdentity, "plan", err)
	}
	if _, err := mcpcompat.ResolveProtocolCompatibilityProfile(profile); err != nil || profile == "" {
		if err == nil {
			err = errors.New("profile is required")
		}
		return nil, adoptProfileFailure(AdoptProfileUpdateInvalidProfile, "plan", err)
	}
	rec, found, err := ReadAdoptProvenance(manifestName)
	if err != nil {
		return nil, adoptProfileFailure(AdoptProfileUpdateNotAdopted, "provenance-read", err)
	}
	if !found || rec.OperationState != AdoptOperationStateAdopted {
		return nil, adoptProfileFailure(AdoptProfileUpdateNotAdopted, "provenance-read", errors.New("no adopted provenance row"))
	}
	old, err := os.ReadFile(filepath.Join(adoptCommittedManifestDir(), manifestName, "manifest.yaml"))
	if err != nil {
		return nil, adoptProfileFailure(AdoptProfileUpdateIdentity, "manifest-read", err)
	}
	oldHash := ManifestHashContent(old)
	if rec.ExpectedManifestHash == "" || oldHash != rec.ExpectedManifestHash || rec.AdoptManifestHash == "" {
		return nil, adoptProfileFailure(AdoptProfileUpdateIdentity, "manifest-read", errors.New("manifest and provenance receipt disagree"))
	}
	updated, err := updateAdoptProfileManifest(old, rec.Port, profile)
	if err != nil {
		return nil, err
	}
	return &AdoptProfileUpdatePlan{ManifestName: manifestName, Profile: profile, OldProfile: rec.MCPProtocolCompatibilityProfile, OldManifestHash: oldHash, NewManifestHash: ManifestHashContent(updated), RestartRequired: true, Noop: rec.MCPProtocolCompatibilityProfile == profile && bytes.Equal(old, updated)}, nil
}

// ExecuteAdoptProfileUpdate holds ONE per-manifest lease across journal
// recovery, manifest CAS, provenance update and intent refresh. It deliberately
// settles with Unlock (not ReleaseAndRemove): the manifest's adopt lease is a
// namespace serialisation resource, not an adopt operation being completed.
func (a *API) ExecuteAdoptProfileUpdate(plan *AdoptProfileUpdatePlan, opts AdoptProfileUpdateOpts) (err error) {
	if plan == nil {
		return adoptProfileFailure(AdoptProfileUpdateIdentity, "plan", errors.New("nil plan"))
	}
	owner := opts.LeaseOwner
	if owner == nil {
		owner = NewAdoptLeaseOwner()
	}
	lease, acquired, acquireErr := owner.AcquireAdoptLease(plan.ManifestName)
	if acquireErr != nil {
		return adoptProfileFailure(AdoptProfileUpdateRecovery, "lease-acquire", acquireErr)
	}
	if !acquired {
		return adoptProfileFailure(AdoptProfileUpdateRecovery, "lease-acquire", errors.New("lease busy"))
	}
	defer func() {
		if releaseErr := lease.Unlock(); releaseErr != nil {
			err = errors.Join(err, adoptProfileFailure(AdoptProfileUpdateRecovery, "lease-release", releaseErr))
		}
	}()

	if err := a.recoverAdoptProfileUpdateUnderLease(plan.ManifestName); err != nil {
		return asAdoptProfileFailure(AdoptProfileUpdateRecovery, "recovery", err)
	}
	fresh, err := buildAdoptProfileUpdatePlan(plan.ManifestName, plan.Profile)
	if err != nil {
		return err
	}
	if fresh.OldManifestHash != plan.OldManifestHash || fresh.NewManifestHash != plan.NewManifestHash {
		return adoptProfileFailure(AdoptProfileUpdateIdentity, "identity", errors.New("reviewed plan changed; re-run dry-run"))
	}
	if fresh.Noop {
		return nil
	}

	oldManifest, err := os.ReadFile(filepath.Join(adoptCommittedManifestDir(), fresh.ManifestName, "manifest.yaml"))
	if err != nil {
		return adoptProfileFailure(AdoptProfileUpdateIdentity, "manifest-read", err)
	}
	rec, found, err := ReadAdoptProvenance(fresh.ManifestName)
	if err != nil || !found {
		return adoptProfileFailure(AdoptProfileUpdateNotAdopted, "provenance-read", errors.New("provenance row disappeared"))
	}
	newManifest, err := updateAdoptProfileManifest(oldManifest, rec.Port, fresh.Profile)
	if err != nil {
		return err
	}
	newTarget, receipt, err := buildInstallSupervisorIntentTarget(fresh.ManifestName, newManifest, rec.Port)
	if err != nil {
		return adoptProfileFailure(AdoptProfileUpdateRefresh, "intent-plan", err)
	}
	oldTarget, err := readInstallSupervisorIntentTarget(newTarget.Row.TaskName)
	if err != nil {
		return adoptProfileFailure(AdoptProfileUpdateRefresh, "intent-snapshot", err)
	}
	j := &AdoptProfileUpdateJournal{ManifestName: fresh.ManifestName, Profile: fresh.Profile, OldProfile: fresh.OldProfile, OldManifestHash: fresh.OldManifestHash, NewManifestHash: fresh.NewManifestHash, OldManifestYAML: string(oldManifest), NewManifestYAML: string(newManifest), OldTarget: oldTarget, NewTarget: newTarget}
	if err := beginAdoptProfileUpdateJournal(fresh.ManifestName, j); err != nil {
		return asAdoptProfileFailure(AdoptProfileUpdateRecovery, "journal", err)
	}
	if err := a.finishAdoptProfileUpdateForwardUnderLease(fresh.ManifestName, j, receipt); err != nil {
		rollbackErr := a.rollbackAdoptProfileUpdateUnderLease(fresh.ManifestName, j)
		return errors.Join(err, rollbackErr)
	}
	return nil
}

func (a *API) finishAdoptProfileUpdateForwardUnderLease(manifestName string, j *AdoptProfileUpdateJournal, receipt InstallMutationReceiptV1) error {
	if _, err := a.ManifestEditInWithHash(adoptCommittedManifestDir(), manifestName, j.NewManifestYAML, j.OldManifestHash); err != nil {
		if errors.Is(err, ErrManifestHashMismatch) {
			return adoptProfileFailure(AdoptProfileUpdateManifestCAS, "manifest-cas", err)
		}
		return adoptProfileFailure(AdoptProfileUpdateManifestCAS, "manifest-write", err)
	}
	actualReceipt, err := ensureInstallSupervisorIntentTarget(j.OldTarget, j.NewTarget, receipt)
	if err != nil {
		return adoptProfileFailure(AdoptProfileUpdateRefresh, "intent-refresh", err)
	}
	if !actualReceipt.Committed || len(actualReceipt.ClientConfigSettlements) != 0 {
		return adoptProfileFailure(AdoptProfileUpdateSettlement, "intent-refresh", errors.New("intent-only materializer receipt is not committed or has client settlements"))
	}
	if err := commitAdoptProfileUpdate(manifestName, j); err != nil {
		return asAdoptProfileFailure(AdoptProfileUpdateRecovery, "provenance-commit", err)
	}
	return nil
}

func (a *API) recoverAdoptProfileUpdateUnderLease(manifestName string) error {
	j, err := readAdoptProfileUpdateJournal(manifestName)
	if err != nil || j == nil {
		return asAdoptProfileFailure(AdoptProfileUpdateRecovery, "recovery-journal", err)
	}
	actual, err := os.ReadFile(filepath.Join(adoptCommittedManifestDir(), manifestName, "manifest.yaml"))
	if err != nil {
		return adoptProfileFailure(AdoptProfileUpdateRecovery, "recovery-read", err)
	}
	rec, found, err := ReadAdoptProvenance(manifestName)
	if err != nil || !found {
		return adoptProfileFailure(AdoptProfileUpdateRecovery, "recovery-read", errors.New("provenance row missing"))
	}
	manifestHash := ManifestHashContent(actual)
	if j.Rollback {
		switch {
		case manifestHash == j.NewManifestHash && rec.ExpectedManifestHash == j.NewManifestHash:
			return a.rollbackAdoptProfileUpdateUnderLease(manifestName, j)
		case manifestHash == j.OldManifestHash && rec.ExpectedManifestHash == j.NewManifestHash:
			if _, err := ensureInstallSupervisorIntentTarget(j.NewTarget, j.OldTarget, InstallMutationReceiptV1{}); err != nil {
				return adoptProfileFailure(AdoptProfileUpdateRecovery, "recovery-rollback-intent", err)
			}
			return rollbackAdoptProfileUpdateRecord(manifestName, j)
		case manifestHash == j.OldManifestHash && rec.ExpectedManifestHash == j.OldManifestHash:
			if _, targetErr := ensureInstallSupervisorIntentTarget(j.NewTarget, j.OldTarget, InstallMutationReceiptV1{}); targetErr != nil {
				return adoptProfileFailure(AdoptProfileUpdateRecovery, "recovery-rollback-verify", ErrInstallSupervisorIntentTargetConflict)
			}
			return clearAdoptProfileUpdateJournal(manifestName)
		default:
			return adoptProfileFailure(AdoptProfileUpdateRecovery, "recovery-rollback", errors.New("unrecognized manifest/provenance state"))
		}
	}
	switch {
	case manifestHash == j.OldManifestHash && rec.ExpectedManifestHash == j.OldManifestHash:
		receipt := InstallMutationReceiptV1{Committed: true}
		return a.finishAdoptProfileUpdateForwardUnderLease(manifestName, j, receipt)
	case manifestHash == j.NewManifestHash && rec.ExpectedManifestHash == j.OldManifestHash:
		receipt := InstallMutationReceiptV1{Committed: true}
		if actual, refreshErr := ensureInstallSupervisorIntentTarget(j.OldTarget, j.NewTarget, receipt); refreshErr != nil || !actual.Committed || len(actual.ClientConfigSettlements) != 0 {
			return adoptProfileFailure(AdoptProfileUpdateRefresh, "recovery-refresh", errors.Join(refreshErr, ErrInstallSupervisorIntentTargetConflict))
		}
		if _, targetErr := ensureInstallSupervisorIntentTarget(j.OldTarget, j.NewTarget, InstallMutationReceiptV1{}); targetErr != nil {
			return adoptProfileFailure(AdoptProfileUpdateRecovery, "recovery-forward-verify", ErrInstallSupervisorIntentTargetConflict)
		}
		return commitAdoptProfileUpdate(manifestName, j)
	case manifestHash == j.NewManifestHash && rec.ExpectedManifestHash == j.NewManifestHash:
		if _, targetErr := ensureInstallSupervisorIntentTarget(j.OldTarget, j.NewTarget, InstallMutationReceiptV1{}); targetErr != nil {
			return adoptProfileFailure(AdoptProfileUpdateRecovery, "recovery-forward-verify", ErrInstallSupervisorIntentTargetConflict)
		}
		return clearAdoptProfileUpdateJournal(manifestName)
	default:
		return adoptProfileFailure(AdoptProfileUpdateRecovery, "recovery-forward", errors.New("unrecognized manifest/provenance state"))
	}
}

func (a *API) rollbackAdoptProfileUpdateUnderLease(manifestName string, j *AdoptProfileUpdateJournal) error {
	if err := markAdoptProfileUpdateRollback(manifestName); err != nil {
		return asAdoptProfileFailure(AdoptProfileUpdateRecovery, "rollback-journal", err)
	}
	actual, err := os.ReadFile(filepath.Join(adoptCommittedManifestDir(), manifestName, "manifest.yaml"))
	if err != nil {
		return adoptProfileFailure(AdoptProfileUpdateRecovery, "rollback-read", err)
	}
	if ManifestHashContent(actual) == j.NewManifestHash {
		if _, err := a.ManifestEditInWithHash(adoptCommittedManifestDir(), manifestName, j.OldManifestYAML, j.NewManifestHash); err != nil {
			return adoptProfileFailure(AdoptProfileUpdateManifestCAS, "rollback-manifest", err)
		}
	} else if ManifestHashContent(actual) != j.OldManifestHash {
		return adoptProfileFailure(AdoptProfileUpdateRecovery, "rollback-manifest", ErrManifestHashMismatch)
	}
	if _, err := ensureInstallSupervisorIntentTarget(j.NewTarget, j.OldTarget, InstallMutationReceiptV1{}); err != nil {
		return adoptProfileFailure(AdoptProfileUpdateRefresh, "rollback-intent", err)
	}
	return asAdoptProfileFailure(AdoptProfileUpdateRecovery, "rollback-provenance", rollbackAdoptProfileUpdateRecord(manifestName, j))
}

func updateAdoptProfileManifest(raw []byte, port int, profile string) ([]byte, error) {
	var doc yaml.Node
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		return nil, adoptProfileFailure(AdoptProfileUpdateIdentity, "manifest-parse", err)
	}
	if len(doc.Content) != 1 || doc.Content[0].Kind != yaml.MappingNode {
		return nil, adoptProfileFailure(AdoptProfileUpdateIdentity, "manifest-parse", errors.New("manifest root is not a mapping"))
	}
	root := doc.Content[0]
	transport := yamlMapValue(root, "transport")
	if transport == nil || transport.Value != "stdio-bridge" {
		return nil, adoptProfileFailure(AdoptProfileUpdateInvalidProfile, "manifest-validate", errors.New("profile applies only to stdio-bridge"))
	}
	daemons := yamlMapValue(root, "daemons")
	if daemons == nil || daemons.Kind != yaml.SequenceNode {
		return nil, adoptProfileFailure(AdoptProfileUpdateIdentity, "manifest-identity", errors.New("manifest has no daemon sequence"))
	}
	matched := 0
	changed := false
	for _, daemon := range daemons.Content {
		if daemon.Kind != yaml.MappingNode {
			continue
		}
		portNode := yamlMapValue(daemon, "port")
		if portNode == nil {
			continue
		}
		parsedPort, parseErr := strconv.Atoi(portNode.Value)
		if parseErr == nil && parsedPort == port {
			profileNode := yamlMapValue(daemon, "mcp_protocol_compatibility_profile")
			if profileNode == nil {
				daemon.Content = append(daemon.Content, &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: "mcp_protocol_compatibility_profile"}, &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: profile})
				changed = true
			} else if profileNode.Value != profile {
				profileNode.Value = profile
				profileNode.Tag = "!!str"
				changed = true
			}
			matched++
		}
	}
	if matched != 1 {
		return nil, adoptProfileFailure(AdoptProfileUpdateIdentity, "manifest-identity", errors.New("provenance port does not identify exactly one daemon"))
	}
	if !changed {
		return raw, nil
	}
	out, err := yaml.Marshal(&doc)
	if err != nil {
		return nil, adoptProfileFailure(AdoptProfileUpdateIdentity, "manifest-marshal", err)
	}
	if _, err := config.ParseManifest(bytes.NewReader(out)); err != nil {
		return nil, adoptProfileFailure(AdoptProfileUpdateInvalidProfile, "manifest-validate", err)
	}
	return out, nil
}

func yamlMapValue(node *yaml.Node, key string) *yaml.Node {
	if node == nil || node.Kind != yaml.MappingNode {
		return nil
	}
	for i := 0; i+1 < len(node.Content); i += 2 {
		if node.Content[i].Value == key {
			return node.Content[i+1]
		}
	}
	return nil
}

func beginAdoptProfileUpdateJournal(manifestName string, j *AdoptProfileUpdateJournal) error {
	return withAdoptedEntriesLock(func() error {
		s, err := readAdoptedEntries()
		if err != nil {
			return err
		}
		for _, pending := range s.ProtocolProfileUpdates {
			if pending.ManifestName == manifestName {
				return adoptProfileFailure(AdoptProfileUpdateRecovery, "journal", errors.New("profile update recovery is pending"))
			}
		}
		for i := range s.Records {
			if s.Records[i].ManifestName == manifestName {
				r := &s.Records[i]
				if r.OperationState != AdoptOperationStateAdopted || r.ExpectedManifestHash != j.OldManifestHash {
					return adoptProfileFailure(AdoptProfileUpdateIdentity, "journal", errors.New("provenance changed"))
				}
				s.ProtocolProfileUpdates = append(s.ProtocolProfileUpdates, *j)
				return writeAdoptedEntries(s)
			}
		}
		return adoptProfileFailure(AdoptProfileUpdateNotAdopted, "journal", errors.New("provenance missing"))
	})
}

func readAdoptProfileUpdateJournal(manifestName string) (*AdoptProfileUpdateJournal, error) {
	var out *AdoptProfileUpdateJournal
	err := withAdoptedEntriesLock(func() error {
		s, err := readAdoptedEntries()
		if err != nil {
			return err
		}
		for i := range s.ProtocolProfileUpdates {
			if s.ProtocolProfileUpdates[i].ManifestName == manifestName {
				cp := s.ProtocolProfileUpdates[i]
				out = &cp
				break
			}
		}
		return nil
	})
	return out, err
}

func commitAdoptProfileUpdate(manifestName string, j *AdoptProfileUpdateJournal) error {
	return withAdoptedEntriesLock(func() error {
		s, err := readAdoptedEntries()
		if err != nil {
			return err
		}
		for i := range s.Records {
			r := &s.Records[i]
			if r.ManifestName == manifestName {
				if !hasAdoptProfileUpdateJournal(s.ProtocolProfileUpdates, manifestName, j.NewManifestHash) || r.ExpectedManifestHash != j.OldManifestHash {
					return adoptProfileFailure(AdoptProfileUpdateRecovery, "provenance-commit", errors.New("journal/provenance changed"))
				}
				r.MCPProtocolCompatibilityProfile = j.Profile
				r.ExpectedManifestHash = j.NewManifestHash
				r.UpdatedAt = time.Now().UTC()
				s.ProtocolProfileUpdates = withoutAdoptProfileUpdateJournal(s.ProtocolProfileUpdates, manifestName)
				return writeAdoptedEntries(s)
			}
		}
		return adoptProfileFailure(AdoptProfileUpdateNotAdopted, "provenance-commit", errors.New("provenance missing"))
	})
}

func markAdoptProfileUpdateRollback(manifestName string) error {
	return withAdoptedEntriesLock(func() error {
		s, err := readAdoptedEntries()
		if err != nil {
			return err
		}
		for i := range s.ProtocolProfileUpdates {
			if s.ProtocolProfileUpdates[i].ManifestName == manifestName {
				s.ProtocolProfileUpdates[i].Rollback = true
				return writeAdoptedEntries(s)
			}
		}
		return adoptProfileFailure(AdoptProfileUpdateRecovery, "rollback-journal", errors.New("journal missing"))
	})
}

func rollbackAdoptProfileUpdateRecord(manifestName string, j *AdoptProfileUpdateJournal) error {
	return withAdoptedEntriesLock(func() error {
		s, err := readAdoptedEntries()
		if err != nil {
			return err
		}
		for i := range s.Records {
			r := &s.Records[i]
			if r.ManifestName == manifestName {
				if !hasAdoptProfileUpdateJournal(s.ProtocolProfileUpdates, manifestName, j.NewManifestHash) || r.ExpectedManifestHash != j.NewManifestHash {
					return adoptProfileFailure(AdoptProfileUpdateRecovery, "rollback-provenance", errors.New("journal/provenance changed"))
				}
				r.MCPProtocolCompatibilityProfile = j.OldProfile
				r.ExpectedManifestHash = j.OldManifestHash
				r.UpdatedAt = time.Now().UTC()
				s.ProtocolProfileUpdates = withoutAdoptProfileUpdateJournal(s.ProtocolProfileUpdates, manifestName)
				return writeAdoptedEntries(s)
			}
		}
		return adoptProfileFailure(AdoptProfileUpdateNotAdopted, "rollback-provenance", errors.New("provenance missing"))
	})
}

func clearAdoptProfileUpdateJournal(manifestName string) error {
	return withAdoptedEntriesLock(func() error {
		s, err := readAdoptedEntries()
		if err != nil {
			return err
		}
		for i := range s.Records {
			if s.Records[i].ManifestName == manifestName {
				s.ProtocolProfileUpdates = withoutAdoptProfileUpdateJournal(s.ProtocolProfileUpdates, manifestName)
				return writeAdoptedEntries(s)
			}
		}
		return adoptProfileFailure(AdoptProfileUpdateNotAdopted, "journal-clear", errors.New("provenance missing"))
	})
}

func hasAdoptProfileUpdateJournal(in []AdoptProfileUpdateJournal, manifestName, hash string) bool {
	for _, j := range in {
		if j.ManifestName == manifestName && j.NewManifestHash == hash {
			return true
		}
	}
	return false
}

func withoutAdoptProfileUpdateJournal(in []AdoptProfileUpdateJournal, manifestName string) []AdoptProfileUpdateJournal {
	out := in[:0]
	for _, j := range in {
		if j.ManifestName != manifestName {
			out = append(out, j)
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

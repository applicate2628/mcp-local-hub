package cli

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"time"

	"mcp-local-hub/internal/api"
	"mcp-local-hub/internal/binaryadmission"
)

const windowsCanonicalEntrySetSchemaV1 = binaryadmission.WindowsCanonicalEntrySetSchemaV1
const UpgradeAdmissionCanonicalEntryProductV1 = "canonical-entry-product-v1"

// Only an original binary-admission staging owner can supply this carrier.
// The detached snapshot is journal DATA, never a constructor for this owner.
type stagedWindowsCanonicalEntrySetV1 struct {
	original       *binaryadmission.StagedWindowsCanonicalEntrySetV1
	prior          *binaryadmission.WindowsCanonicalEntryPriorSetV1
	priorAttempted bool
	priorError     error
}

func (s *stagedWindowsCanonicalEntrySetV1) acquirePrior(ctx context.Context) (*binaryadmission.WindowsCanonicalEntryPriorSetV1, error) {
	if s == nil || s.original == nil {
		return nil, errors.New("original staged entry set required")
	}
	if s.prior != nil || s.priorAttempted {
		return s.prior, s.priorError
	}
	s.priorAttempted = true
	s.prior, s.priorError = s.original.AcquireOriginalPriorSet(ctx)
	if s.prior == nil && s.priorError == nil {
		s.priorError = errors.New("original prior-set acquisition returned no owner")
	}
	return s.prior, s.priorError
}

type windowsCanonicalEntryDirectoryV1 struct {
	State           string                                                                     `json:"state,omitempty"`
	Absence         *binaryadmission.WindowsCanonicalEntryPayloadDirectoryAbsenceObservationV1 `json:"absence,omitempty"`
	CreationEntered bool                                                                       `json:"creation_entered,omitempty"`
	Original        *binaryadmission.WindowsCanonicalEntryPayloadDirectorySnapshotV1           `json:"original,omitempty"`
	Disposition     *windowsCanonicalEntryMoveRecordV1                                         `json:"disposition,omitempty"`
	Path            string                                                                     `json:"path"`
	PriorPresent    bool                                                                       `json:"prior_present"`
	Created         bool                                                                       `json:"created"`
}

type windowsCanonicalEntrySetJournalV1 struct {
	Cleanup          []windowsCanonicalEntryCleanupV1                   `json:"cleanup,omitempty"`
	Schema           string                                             `json:"schema"`
	TransactionID    string                                             `json:"transaction_id"`
	CanonicalEntry   windowsProductRecoveryArtifactV3                   `json:"canonical_entry"`
	Identity         windowsProductRecoveryArtifactV3                   `json:"identity"`
	StagedEntry      windowsProductRecoveryStagedV3                     `json:"staged_entry"`
	StagedIdentity   windowsProductRecoveryStagedV3                     `json:"staged_identity"`
	Expected         binaryadmission.WindowsCanonicalEntrySetSnapshotV1 `json:"expected"`
	PayloadDirectory windowsCanonicalEntryDirectoryV1                   `json:"payload_directory"`
	Moves            []windowsCanonicalEntryMoveRecordV1                `json:"moves,omitempty"`
	PriorRuntime     *windowsCanonicalEntryRuntimePreimageV1            `json:"prior_runtime,omitempty"`
}

type windowsCanonicalEntryCleanupV1 struct {
	TransactionID  string `json:"transaction_id"`
	Side           string `json:"side"`
	Member         string `json:"member"`
	SourceRelative string `json:"source_relative"`
	ExpectedSHA256 string `json:"expected_sha256"`
	State          string `json:"state"`
}
type windowsCanonicalEntryRuntimePreimageV1 struct {
	Path    string `json:"path"`
	Present bool   `json:"present"`
	SHA256  string `json:"sha256"`
}

type windowsCanonicalEntrySetReceiptV1 struct {
	Schema                   string `json:"schema"`
	TransactionID            string `json:"transaction_id"`
	BuildID                  string `json:"build_id"`
	RegistryProjectionSHA256 string `json:"registry_projection_sha256"`
	CanonicalEntryPath       string `json:"canonical_entry_path"`
	CanonicalEntrySHA256     string `json:"canonical_entry_sha256"`
	ProductPath              string `json:"product_path"`
	IdentityPath             string `json:"identity_path"`
	IdentitySHA256           string `json:"identity_sha256"`
	Machine                  uint16 `json:"machine"`
}

type windowsCanonicalEntrySetPrepared struct {
	directory                         *binaryadmission.WindowsCanonicalEntryPayloadDirectoryV1
	source                            *stagedWindowsCanonicalEntrySetV1
	snapshot                          binaryadmission.WindowsCanonicalEntrySetSnapshotV1
	stages                            binaryadmission.WindowsCanonicalEntrySetTargetsV1
	targets                           binaryadmission.WindowsCanonicalEntrySetTargetsV1
	entryPrior, identityPrior         productPairFileSnapshot
	entryPromotion, identityPromotion *WindowsProductPairPromotion
	entryWithdrawn                    string
	stagedLocations, priorLocations   map[string]string
	nativeResults                     []binaryadmission.WindowsCanonicalEntryMoveResultV1
	nativeErrors                      []error
}

func canonicalEntrySetJournalTargets(j windowsProductRecoveryJournalV3) (binaryadmission.WindowsCanonicalEntrySetTargetsV1, error) {
	if j.EntrySet == nil {
		return binaryadmission.WindowsCanonicalEntrySetTargetsV1{}, errors.New("canonical entry-set journal arm is required")
	}
	targets, err := binaryadmission.CanonicalEntryTargetsV1(j.EntrySet.CanonicalEntry.Target)
	if err != nil {
		return targets, err
	}
	if !sameWindowsProductPath(targets.ProductPath, j.Product.Target) || !sameWindowsProductPath(targets.IdentityPath, j.EntrySet.Identity.Target) {
		return targets, errors.New("canonical entry-set journal targets disagree with registry")
	}
	return targets, nil
}

func validateCanonicalEntrySetJournal(j windowsProductRecoveryJournalV3) error {
	if err := validateOriginalPayloadDirectory(j); err != nil {
		return err
	}
	if err := validateCanonicalCleanup(j); err != nil {
		return err
	}
	e := j.EntrySet
	if e == nil {
		return nil
	}
	targets, err := canonicalEntrySetJournalTargets(j)
	if err != nil {
		return err
	}
	if j.Mode == WindowsProductPairModeUpgrade {
		r := e.PriorRuntime
		if r == nil {
			return errors.New("complete-set upgrade requires original prior runtime projection")
		}
		var prior windowsProductPreimageV1
		if sameWindowsProductPath(r.Path, e.CanonicalEntry.Target) {
			prior = windowsProductPreimageV1{Present: e.CanonicalEntry.PriorPresent, SHA256: e.CanonicalEntry.PriorSHA256}
		} else if sameWindowsProductPath(r.Path, j.Product.Target) {
			prior = windowsProductPreimageV1{Present: j.Product.PriorPresent, SHA256: j.Product.PriorSHA256}
		} else {
			return errors.New("prior runtime escaped exact entry-set targets")
		}
		if prior != (windowsProductPreimageV1{Present: r.Present, SHA256: r.SHA256}) {
			return errors.New("prior runtime does not match original member preimage")
		}
		if !r.Present && (e.CanonicalEntry.PriorPresent || j.Product.PriorPresent || e.Identity.PriorPresent) {
			return errors.New("partial entry set is not prior runtime absence")
		}
	}
	if e.Schema != windowsCanonicalEntrySetSchemaV1 || e.Expected.Schema != e.Schema || e.TransactionID != j.TransactionID || strings.TrimSpace(e.Expected.BuildID) == "" || !validLowerHexSHA256(e.Expected.RegistryProjectionSHA256) {
		return errors.New("invalid canonical entry-set journal identity")
	}
	for _, member := range []windowsProductRecoveryArtifactV3{e.CanonicalEntry, e.Identity} {
		if !validRecoveryPreimage(member.PriorPresent, member.PriorSHA256) || !validLowerHexSHA256(member.NewSHA256) {
			return errors.New("invalid canonical entry-set member preimage")
		}
		if err := validateWindowsProductRecoveryAsides(member.Target, member.BaselineAsides); err != nil {
			return err
		}
	}
	if !cleanAbsolutePath(e.StagedEntry.Path) || !cleanAbsolutePath(e.StagedIdentity.Path) || e.StagedEntry.SHA256 != e.CanonicalEntry.NewSHA256 || e.StagedIdentity.SHA256 != e.Identity.NewSHA256 {
		return errors.New("invalid canonical entry-set stages")
	}
	paths := []string{j.Staged.Path, e.StagedEntry.Path, e.StagedIdentity.Path, targets.CanonicalEntryPath, targets.ProductPath, targets.IdentityPath}
	for i, path := range paths {
		for k := 0; k < i; k++ {
			if sameWindowsProductPath(path, paths[k]) {
				return errors.New("canonical entry-set paths alias")
			}
		}
	}
	if e.Expected.ProductArtifact.Role != binaryadmission.WindowsArtifactRoleProduct || e.Expected.ProductArtifact.Subsystem != binaryadmission.WindowsGUISubsystem || e.Expected.ProductArtifact.Machine == 0 || e.Expected.ProductArtifact.SHA256 != j.Product.NewSHA256 || !sameWindowsProductPath(e.Expected.ProductArtifact.Path, j.Product.Target) {
		return errors.New("invalid canonical entry-set Product binding")
	}
	if fmt.Sprintf("%x", sha256.Sum256(e.Expected.IdentityBytes)) != e.Identity.NewSHA256 {
		return errors.New("canonical entry-set identity bytes differ")
	}
	if e.Expected.CanonicalEntry.SHA256 != e.CanonicalEntry.NewSHA256 || e.Expected.Product.SHA256 != j.Product.NewSHA256 || e.Expected.Identity.SHA256 != e.Identity.NewSHA256 || e.Expected.Machine != e.Expected.ProductArtifact.Machine {
		return errors.New("canonical entry-set expected member identities differ")
	}
	if !sameWindowsProductPath(e.PayloadDirectory.Path, filepath.Dir(targets.ProductPath)) || (e.PayloadDirectory.PriorPresent && e.PayloadDirectory.Created) {
		return errors.New("invalid canonical payload directory preimage")
	}
	return nil
}

func canonicalEntrySnapshotFromJournal(j windowsProductRecoveryJournalV3) binaryadmission.WindowsCanonicalEntrySetSnapshotV1 {
	s := j.EntrySet.Expected
	s.CanonicalEntry.Path = j.EntrySet.CanonicalEntry.Target
	s.Product.Path = j.Product.Target
	s.Identity.Path = j.EntrySet.Identity.Target
	s.ProductArtifact.Path = j.Product.Target
	s.IdentityBytes = append([]byte(nil), s.IdentityBytes...)
	return s
}
func readCanonicalEntrySet(ctx context.Context, targets binaryadmission.WindowsCanonicalEntrySetTargetsV1, expected binaryadmission.WindowsCanonicalEntrySetSnapshotV1) (actual binaryadmission.WindowsCanonicalEntrySetSnapshotV1, retErr error) {
	owner, err := binaryadmission.AcquireWindowsCanonicalEntrySetV1(ctx, targets, expected)
	if owner != nil {
		defer func() { retErr = errors.Join(retErr, owner.Close()) }()
	}
	if err != nil {
		return actual, err
	}
	if owner == nil {
		return actual, errors.New("canonical entry-set reader returned no original owner")
	}
	return owner.Snapshot()
}

func canonicalEntrySetReceipt(j windowsProductRecoveryJournalV3) *windowsCanonicalEntrySetReceiptV1 {
	if j.EntrySet == nil {
		return nil
	}
	e := j.EntrySet
	return &windowsCanonicalEntrySetReceiptV1{Schema: e.Schema, TransactionID: j.TransactionID, BuildID: e.Expected.BuildID, RegistryProjectionSHA256: e.Expected.RegistryProjectionSHA256, CanonicalEntryPath: e.CanonicalEntry.Target, CanonicalEntrySHA256: e.CanonicalEntry.NewSHA256, ProductPath: j.Product.Target, IdentityPath: e.Identity.Target, IdentitySHA256: e.Identity.NewSHA256, Machine: e.Expected.ProductArtifact.Machine}
}

func validateCanonicalEntrySetReceipt(receipt UpgradeReceiptV3) error {
	e := receipt.EntrySet
	if receipt.Admission == UpgradeAdmissionLocalProductV3 {
		if e != nil {
			return errors.New("legacy product receipt cannot certify an entry set")
		}
		return nil
	}
	if receipt.Admission != UpgradeAdmissionCanonicalEntryProductV1 || e == nil {
		return errors.New("canonical entry-set admission and arm disagree")
	}
	targets, err := binaryadmission.CanonicalEntryTargetsV1(e.CanonicalEntryPath)
	if err != nil {
		return err
	}
	if e.Schema != windowsCanonicalEntrySetSchemaV1 || e.TransactionID != receipt.TransactionID || strings.TrimSpace(e.BuildID) == "" || !validLowerHexSHA256(e.RegistryProjectionSHA256) || !validLowerHexSHA256(e.CanonicalEntrySHA256) || !validLowerHexSHA256(e.IdentitySHA256) || e.Machine != receipt.Artifact.Machine || !sameWindowsProductPath(targets.ProductPath, e.ProductPath) || !sameWindowsProductPath(targets.IdentityPath, e.IdentityPath) {
		return errors.New("invalid canonical entry-set receipt")
	}
	return nil
}

func prepareCanonicalEntrySet(o WindowsProductTxnOpts, d WindowsProductTxnDeps, id string) (*windowsCanonicalEntrySetPrepared, *windowsCanonicalEntrySetJournalV1, error) {
	if o.EntrySet == nil {
		return nil, nil, nil
	}
	if o.Context == nil || o.EntrySet.original == nil {
		return nil, nil, errors.New("original staged canonical entry set and context are required")
	}
	if err := o.EntrySet.original.VerifyUnchanged(); err != nil {
		return nil, nil, err
	}
	stages, err := o.EntrySet.original.Targets()
	if err != nil {
		return nil, nil, err
	}
	targets, err := o.EntrySet.original.DestinationTargets()
	if err != nil {
		return nil, nil, err
	}
	snapshot, err := o.EntrySet.original.Snapshot()
	if err != nil {
		return nil, nil, err
	}
	if !sameWindowsProductPath(targets.CanonicalEntryPath, o.CanonicalEntryPath) || !sameWindowsProductPath(targets.ProductPath, o.ProductPath) || !sameWindowsProductPath(targets.IdentityPath, o.IdentityPath) || !sameWindowsProductPath(stages.ProductPath, o.StagedProduct) {
		return nil, nil, errors.New("staged canonical entry set disagrees with transaction paths")
	}
	entryPrior, err := canonicalPriorSnapshot(o.EntrySet.prior, "launcher", targets.CanonicalEntryPath)
	if err != nil {
		return nil, nil, err
	}
	identityPrior, err := canonicalPriorSnapshot(o.EntrySet.prior, "identity", targets.IdentityPath)
	if err != nil {
		return nil, nil, err
	}
	payloadParentAbsent, err := o.EntrySet.prior.ObservePayloadParentAbsent(o.Context)
	if err != nil {
		return nil, nil, fmt.Errorf("observe original canonical payload parent: %w", err)
	}
	dir := windowsCanonicalEntryDirectoryV1{Path: filepath.Dir(targets.ProductPath), PriorPresent: !payloadParentAbsent}
	entryAsides, err := snapshotWindowsProductAsides(targets.CanonicalEntryPath, d)
	if err != nil {
		return nil, nil, err
	}
	identityAsides, err := snapshotWindowsProductAsides(targets.IdentityPath, d)
	if err != nil {
		return nil, nil, err
	}
	expected := snapshot
	expected.CanonicalEntry.Path = targets.CanonicalEntryPath
	expected.Product.Path = targets.ProductPath
	expected.ProductArtifact.Path = targets.ProductPath
	expected.Identity.Path = targets.IdentityPath
	state := &windowsCanonicalEntrySetPrepared{source: o.EntrySet, snapshot: snapshot, stages: stages, targets: targets, entryPrior: entryPrior, identityPrior: identityPrior}
	arm := &windowsCanonicalEntrySetJournalV1{Schema: windowsCanonicalEntrySetSchemaV1, TransactionID: id, Expected: expected,
		CanonicalEntry: windowsProductRecoveryArtifactV3{Target: targets.CanonicalEntryPath, PriorPresent: entryPrior.present, PriorSHA256: windowsProductPreimage(entryPrior).SHA256, NewSHA256: snapshot.CanonicalEntry.SHA256, BaselineAsides: entryAsides},
		Identity:       windowsProductRecoveryArtifactV3{Target: targets.IdentityPath, PriorPresent: identityPrior.present, PriorSHA256: windowsProductPreimage(identityPrior).SHA256, NewSHA256: snapshot.Identity.SHA256, BaselineAsides: identityAsides},
		StagedEntry:    windowsProductRecoveryStagedV3{Path: stages.CanonicalEntryPath, SHA256: snapshot.CanonicalEntry.SHA256}, StagedIdentity: windowsProductRecoveryStagedV3{Path: stages.IdentityPath, SHA256: snapshot.Identity.SHA256}, PayloadDirectory: dir}
	if o.Mode == WindowsProductPairModeUpgrade {
		view, err := o.EntrySet.prior.RuntimeProduct(o.Context)
		if err != nil {
			return nil, nil, err
		}
		runtime := &windowsCanonicalEntryRuntimePreimageV1{Path: targets.CanonicalEntryPath}
		if view != nil {
			if err := view.ValidateOriginal(o.Context); err != nil {
				return nil, nil, err
			}
			artifact := view.Artifact()
			runtime.Path, runtime.Present, runtime.SHA256 = artifact.Path, true, artifact.SHA256
		}
		arm.PriorRuntime = runtime
	}
	return state, arm, nil
}

func (t *WindowsProductTxn) receiveOriginalPayloadDirectory() error {
	p := t.prepared
	if p.entrySet == nil {
		return nil
	}
	owner, ensureErr := t.Opts.EntrySet.original.EnsureOriginalPayloadDirectory(t.Opts.Context)
	p.entrySet.directory = owner
	if owner == nil {
		return errors.Join(ensureErr, errors.New("original payload directory owner missing"))
	}
	observation, readErr := owner.Snapshot()
	if ensureErr != nil || readErr != nil {
		return errors.Join(ensureErr, readErr)
	}
	record := &p.journal.EntrySet.PayloadDirectory
	if observation.Schema != binaryadmission.WindowsCanonicalEntryPayloadDirectorySchemaV1 || !sameWindowsProductPath(observation.Root, filepath.Dir(t.Opts.CanonicalEntryPath)) || observation.RelativePath != filepath.ToSlash(filepath.Dir(binaryadmission.WindowsCanonicalProductRelativePathV1)) || observation.Created == record.PriorPresent {
		return errors.New("payload directory original creation/preimage disagrees")
	}
	record.Created = observation.Created
	record.Original = &observation
	if observation.Created {
		record.State = "created_identity_received"
	}
	return t.persistProductJournal()
}

func (t *WindowsProductTxn) ensureCanonicalPayloadDirectory() error {
	p := t.prepared
	if p.entrySet == nil {
		return nil
	}
	if p.entrySet.directory == nil || p.journal.EntrySet.PayloadDirectory.Original == nil {
		return errors.New("payload directory native custody was not durably received")
	}
	observation, err := p.entrySet.directory.Snapshot()
	if err != nil {
		return err
	}
	if !reflect.DeepEqual(observation, *p.journal.EntrySet.PayloadDirectory.Original) {
		return errors.New("payload directory native observation changed")
	}
	return nil
}

type windowsCanonicalEntryMoveRecordV1 struct {
	Member              string `json:"member"`
	Prior               bool   `json:"prior"`
	SourceRelative      string `json:"source_relative"`
	DestinationRelative string `json:"destination_relative"`
	Applied             bool   `json:"applied"`
	Readback            bool   `json:"readback"`
	NativeStatusKnown   bool   `json:"native_status_known"`
	NativeStatus        uint32 `json:"native_status"`
}

func canonicalPriorSnapshot(owner *binaryadmission.WindowsCanonicalEntryPriorSetV1, member, path string) (productPairFileSnapshot, error) {
	if owner == nil {
		return productPairFileSnapshot{}, errors.New("original canonical prior owner required")
	}
	present, body, digest, err := owner.ReadOriginal(member)
	if err != nil {
		return productPairFileSnapshot{}, err
	}
	if present && fmt.Sprintf("%x", sha256.Sum256(body)) != digest {
		return productPairFileSnapshot{}, errors.New("original canonical prior readback differs")
	}
	return productPairFileSnapshot{path: path, present: present, body: append([]byte(nil), body...)}, nil
}

func (t *WindowsProductTxn) entryRelative(path string) (string, error) {
	relative, err := filepath.Rel(filepath.Dir(t.Opts.CanonicalEntryPath), path)
	if err != nil || relative == "." || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) || filepath.IsAbs(relative) {
		return "", errors.New("canonical member target escaped original root")
	}
	return filepath.ToSlash(relative), nil
}

func (t *WindowsProductTxn) moveEntryOriginal(ctx context.Context, prior bool, member, target string) error {
	p := t.prepared
	e := p.entrySet
	relative, err := t.entryRelative(target)
	if err != nil {
		return err
	}
	p.journal.EntrySet.Moves = append(p.journal.EntrySet.Moves, windowsCanonicalEntryMoveRecordV1{Member: member, Prior: prior, DestinationRelative: relative})
	index := len(p.journal.EntrySet.Moves) - 1
	if err := t.persistProductJournal(); err != nil {
		return err
	}
	var result binaryadmission.WindowsCanonicalEntryMoveResultV1
	if prior {
		result, err = t.Opts.EntrySet.prior.MoveOriginal(ctx, member, relative)
	} else {
		result, err = t.Opts.EntrySet.original.MoveOriginal(ctx, member, relative)
	}
	e.nativeResults = append(e.nativeResults, result)
	e.nativeErrors = append(e.nativeErrors, err)
	record := &p.journal.EntrySet.Moves[index]
	record.SourceRelative = result.SourceRelative
	record.Applied = result.Applied
	record.Readback = result.Readback
	record.NativeStatusKnown = result.NativeStatusKnown
	record.NativeStatus = result.NativeStatus
	if result.Applied {
		if prior {
			if e.priorLocations == nil {
				e.priorLocations = make(map[string]string)
			}
			e.priorLocations[member] = target
		} else {
			if e.stagedLocations == nil {
				e.stagedLocations = make(map[string]string)
			}
			e.stagedLocations[member] = target
		}
	}
	received := canonicalEntryMoveResultError(result, err, member, relative)
	return errors.Join(received, t.persistProductJournal())
}

func (t *WindowsProductTxn) canonicalAside(target string) string {
	return target + ".old-" + t.prepared.deps.Now().UTC().Add(time.Duration(len(t.prepared.journal.EntrySet.Moves))*time.Second).Format("20060102T150405Z")
}

func (t *WindowsProductTxn) promoteCanonicalMember(ctx context.Context, member, target string, prior productPairFileSnapshot, newSHA string) (WindowsProductPairPromotion, error) {
	result := WindowsProductPairPromotion{Target: target, PriorPresent: prior.present, NewSHA256: newSHA}
	if prior.present {
		result.RetainedPrior = t.canonicalAside(target)
		if err := t.moveEntryOriginal(ctx, true, member, result.RetainedPrior); err != nil {
			return result, err
		}
	}
	return result, t.moveEntryOriginal(ctx, false, member, target)
}

func (t *WindowsProductTxn) promoteProductOriginal(ctx context.Context) (WindowsProductPairPromotion, error) {
	p := t.prepared
	if p.entrySet == nil {
		return p.deps.Promote(t.Opts.StagedProduct, t.Opts.ProductPath, p.staged.SHA256)
	}
	return t.promoteCanonicalMember(ctx, "product", t.Opts.ProductPath, p.productPrior, p.staged.SHA256)
}

func (t *WindowsProductTxn) promoteCanonicalEntryCompanions(ctx context.Context) error {
	p := t.prepared
	if p.entrySet == nil {
		return nil
	}
	e := p.entrySet
	j := p.journal.EntrySet
	for _, member := range []struct {
		name, target, sha string
		prior             productPairFileSnapshot
		promotion         **WindowsProductPairPromotion
		fault             WindowsProductStage
	}{
		{"identity", e.targets.IdentityPath, j.Identity.NewSHA256, e.identityPrior, &e.identityPromotion, WindowsProductStageIdentityPromoted},
		{"launcher", e.targets.CanonicalEntryPath, j.CanonicalEntry.NewSHA256, e.entryPrior, &e.entryPromotion, WindowsProductStageCanonicalEntryPromoted},
	} {
		if windowsProductTargetDisposition(member.prior, member.sha) != windowsProductTargetIdentical {
			promotion, err := t.promoteCanonicalMember(ctx, member.name, member.target, member.prior, member.sha)
			*member.promotion = rollbackProductPairPromotion(promotion, member.prior)
			if err != nil {
				return err
			}
		}
		if p.deps.Fault != nil {
			if err := p.deps.Fault(member.fault); err != nil {
				return err
			}
		}
	}
	return t.verifyLiveCanonicalEntrySet()
}

func (t *WindowsProductTxn) verifyLiveCanonicalEntrySet() error {
	p := t.prepared
	e := p.entrySet
	if err := t.Opts.EntrySet.original.VerifyUnchanged(); err != nil {
		return err
	}
	current, err := t.Opts.EntrySet.original.Targets()
	if err != nil {
		return err
	}
	for _, member := range []struct{ name, current, target, want string }{
		{"launcher", current.CanonicalEntryPath, e.targets.CanonicalEntryPath, e.snapshot.CanonicalEntry.SHA256},
		{"product", current.ProductPath, e.targets.ProductPath, e.snapshot.Product.SHA256},
		{"identity", current.IdentityPath, e.targets.IdentityPath, e.snapshot.Identity.SHA256},
	} {
		if location := e.stagedLocations[member.name]; location != "" {
			if !sameWindowsProductPath(location, member.target) || !sameWindowsProductPath(member.current, member.target) {
				return errors.New("canonical original member name differs after promotion")
			}
		} else {
			if e.priorLocations[member.name] != "" {
				return errors.New("identical canonical member prior moved unexpectedly")
			}
			present, _, digest, err := t.Opts.EntrySet.prior.ReadOriginal(member.name)
			if err != nil || !present || digest != member.want {
				return errors.Join(err, errors.New("identical canonical member no longer matches original"))
			}
		}
	}
	return nil
}

func (t *WindowsProductTxn) withdrawCanonicalSuccessorEntry() error {
	p := t.prepared
	if p.entrySet == nil || p.entrySet.stagedLocations["launcher"] == "" {
		return nil
	}
	e := p.entrySet
	target := e.targets.CanonicalEntryPath
	if !sameWindowsProductPath(e.stagedLocations["launcher"], target) {
		return nil
	}
	e.entryWithdrawn = t.canonicalAside(target)
	return t.moveEntryOriginal(context.WithoutCancel(t.Opts.Context), false, "launcher", e.entryWithdrawn)
}

func (t *WindowsProductTxn) restoreCanonicalMember(member, target string, prior productPairFileSnapshot) error {
	e := t.prepared.entrySet
	if location := e.stagedLocations[member]; location != "" && sameWindowsProductPath(location, target) {
		if err := t.moveEntryOriginal(context.WithoutCancel(t.Opts.Context), false, member, t.canonicalAside(target)); err != nil {
			return err
		}
	}
	if location := e.priorLocations[member]; location != "" {
		if !prior.present {
			return errors.New("prior-absent canonical member unexpectedly acquired an original")
		}
		return t.moveEntryOriginal(context.WithoutCancel(t.Opts.Context), true, member, target)
	}
	return nil
}

func (t *WindowsProductTxn) restoreCanonicalIdentity() error {
	if t.prepared.entrySet == nil {
		return nil
	}
	return t.restoreCanonicalMember("identity", t.Opts.IdentityPath, t.prepared.entrySet.identityPrior)
}

func (t *WindowsProductTxn) restoreCanonicalPublicEntryLast() error {
	if t.prepared.entrySet == nil {
		return nil
	}
	return t.restoreCanonicalMember("launcher", t.Opts.CanonicalEntryPath, t.prepared.entrySet.entryPrior)
}

func (t *WindowsProductTxn) discardEntryOriginal(ctx context.Context, prior bool, member, path string) error {
	relative, err := t.entryRelative(path)
	if err != nil {
		return err
	}
	var result binaryadmission.WindowsCanonicalEntryMoveResultV1
	if prior {
		result, err = t.Opts.EntrySet.prior.DiscardOriginal(ctx, member, relative)
	} else {
		result, err = t.Opts.EntrySet.original.DiscardOriginal(ctx, member, relative)
	}
	t.prepared.entrySet.nativeResults = append(t.prepared.entrySet.nativeResults, result)
	t.prepared.entrySet.nativeErrors = append(t.prepared.entrySet.nativeErrors, err)
	return canonicalEntryDiscardResultError(result, err, member, relative)
}

func (t *WindowsProductTxn) cleanupCanonicalOwnedFiles(committed bool) error {
	p := t.prepared
	if p.entrySet == nil {
		return nil
	}
	e := p.entrySet
	for _, member := range []struct{ name, stage, target string }{
		{"launcher", e.stages.CanonicalEntryPath, e.targets.CanonicalEntryPath}, {"product", e.stages.ProductPath, e.targets.ProductPath}, {"identity", e.stages.IdentityPath, e.targets.IdentityPath},
	} {
		location := e.stagedLocations[member.name]
		if location == "" {
			location = member.stage
		}
		if !sameWindowsProductPath(location, member.target) {
			var disposeErr error
			if committed {
				disposeErr = t.receiveCommittedCanonicalDiscard(context.WithoutCancel(t.Opts.Context), "unused-successor", member.name, location)
			} else {
				disposeErr = t.discardEntryOriginal(context.WithoutCancel(t.Opts.Context), false, member.name, location)
			}
			if err := disposeErr; err != nil {
				return err
			}
		}
		if committed {
			if prior := e.priorLocations[member.name]; prior != "" && !sameWindowsProductPath(prior, member.target) {
				if err := t.receiveCommittedCanonicalDiscard(context.WithoutCancel(t.Opts.Context), "prior", member.name, prior); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

func (t *WindowsProductTxn) removeCreatedPayloadDirectoryAfterInverse() error {
	p := t.prepared
	if p.entrySet == nil {
		return nil
	}
	record := &p.journal.EntrySet.PayloadDirectory
	if p.entrySet.directory == nil {
		return errors.New("payload directory original owner unavailable")
	}
	if record.PriorPresent {
		return p.entrySet.directory.Close()
	}
	if !record.Created || record.Original == nil {
		return errors.New("payload directory original creation identity is unconfirmed")
	}
	record.State = "dispose_intent"
	if err := t.persistProductJournal(); err != nil {
		return err
	}
	result, err := p.entrySet.directory.DisposeOriginalIfEmpty(context.WithoutCancel(t.Opts.Context))
	p.entrySet.nativeResults = append(p.entrySet.nativeResults, result)
	p.entrySet.nativeErrors = append(p.entrySet.nativeErrors, err)
	record.Disposition = &windowsCanonicalEntryMoveRecordV1{Member: result.Member, SourceRelative: result.SourceRelative, Applied: result.Applied, Readback: result.Readback, NativeStatusKnown: result.NativeStatusKnown, NativeStatus: result.NativeStatus}
	if received := canonicalEntryDiscardResultError(result, err, "payload-directory", record.Original.RelativePath); received != nil {
		return errors.Join(received, ErrWindowsProductRecoveryPending)
	}
	record.State = "absence_received"
	return errors.Join(t.persistProductJournal(), p.entrySet.directory.Close())
}
func windowsCanonicalSetDisposition(productPrior productPairFileSnapshot, e *windowsCanonicalEntrySetPrepared, productSHA string) windowsProductTargetDispositionV1 {
	values := []windowsProductTargetDispositionV1{windowsProductTargetDisposition(productPrior, productSHA), windowsProductTargetDisposition(e.entryPrior, e.snapshot.CanonicalEntry.SHA256), windowsProductTargetDisposition(e.identityPrior, e.snapshot.Identity.SHA256)}
	result := windowsProductTargetIdentical
	for _, value := range values {
		if value == windowsProductTargetReplace {
			return value
		}
		if value == windowsProductTargetCreate {
			result = value
		}
	}
	return result
}

func validateWindowsProductTxnReplacementAuthority(o WindowsProductTxnOpts, p *windowsProductPreparedTxn) error {
	if p.entrySet == nil {
		return validateWindowsSingleProductReplacementAuthority(o, p.disposition, p.productPrior, p.legacyPrior)
	}
	if o.Mode != WindowsProductPairModeUpgrade {
		return validateWindowsSingleProductReplacementAuthority(o, p.disposition, p.entrySet.entryPrior, p.legacyPrior)
	}
	runtime := p.journal.EntrySet.PriorRuntime
	if runtime == nil {
		return ErrWindowsProductPairQuiescenceRequired
	}
	prior := p.entrySet.entryPrior
	if sameWindowsProductPath(runtime.Path, o.ProductPath) {
		prior = p.productPrior
	} else if !sameWindowsProductPath(runtime.Path, o.CanonicalEntryPath) {
		return errors.New("prior runtime is outside original complete-set targets")
	}
	if windowsProductPreimage(prior) != (windowsProductPreimageV1{Present: runtime.Present, SHA256: runtime.SHA256}) {
		return errors.New("original prior runtime preimage changed")
	}
	projected := o
	// Existing legacy authority names the stopped runtime as CLI; the new public
	// entry and GUI Product remain distinct on the actual transaction options.
	projected.EntrySet = nil
	projected.ProductPath = runtime.Path
	return validateWindowsSingleProductReplacementAuthority(projected, p.disposition, prior, p.legacyPrior)
}
func verifyLegacyProductPromotion(p *windowsProductPreparedTxn, promotion WindowsProductPairPromotion) error {
	if p.entrySet != nil {
		return nil
	}
	return verifyProductPairPromotion(promotion, p.productPrior)
}

// Check the reserved member before encoding/json can case-fold a field name
// or let a later duplicate erase an already observed complete-set arm.
func validateCanonicalEntryArmJSON(raw []byte) error {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	start, err := decoder.Token()
	if err != nil {
		return err
	}
	if start != json.Delim('{') {
		return errors.New("product record must be a JSON object")
	}
	seen := false
	for decoder.More() {
		token, err := decoder.Token()
		if err != nil {
			return err
		}
		key, ok := token.(string)
		if !ok {
			return errors.New("product record member is not a string")
		}
		var value json.RawMessage
		if err := decoder.Decode(&value); err != nil {
			return err
		}
		if !strings.EqualFold(key, "entry_set") {
			continue
		}
		if key != "entry_set" {
			return errors.New("reserved entry_set member spelling is not exact")
		}
		if seen {
			return errors.New("duplicate decoded entry_set member")
		}
		seen = true
		if bytes.Equal(bytes.TrimSpace(value), []byte("null")) {
			return errors.New("entry_set must be absent for legacy or a complete object")
		}
	}
	end, err := decoder.Token()
	if err != nil {
		return err
	}
	if end != json.Delim('}') {
		return errors.New("product record object is incomplete")
	}
	if err := decoder.Decode(new(any)); !errors.Is(err, io.EOF) {
		return errors.Join(err, errors.New("product record has trailing JSON"))
	}
	return nil
}

// OriginalOwnerPending describes retained custody, not completion of this
// particular native operation. Final owner Close remains a separate phase.
func canonicalEntryMoveResultError(result binaryadmission.WindowsCanonicalEntryMoveResultV1, callErr error, member, destination string) error {
	err := errors.Join(callErr, result.Error, result.CloseError)
	if !result.Applied || !result.Readback || !result.NativeStatusKnown || result.Member != member || result.DestinationRelative != destination {
		err = errors.Join(err, errors.New("canonical original move not fully received"))
	}
	return err
}

func canonicalEntryDiscardResultError(result binaryadmission.WindowsCanonicalEntryMoveResultV1, callErr error, member, source string) error {
	err := errors.Join(callErr, result.Error, result.CloseError)
	if !result.Applied || !result.Readback || !result.NativeStatusKnown || result.Member != member || result.SourceRelative != source {
		err = errors.Join(err, errors.New("canonical original discard or final absence unreceived"))
	}
	return err
}

func validateGUIOwnedStopPreimage(journal windowsProductRecoveryJournalV3, receipt windowsProductQuiescenceReceiptV1) error {
	if journal.EntrySet != nil && journal.EntrySet.PriorRuntime == nil {
		return errors.New("complete-set prior runtime projection is missing")
	}
	runtime := canonicalRecoveryRuntimeArtifact(journal)
	if !sameWindowsProductPath(receipt.CanonicalCLIPath, runtime.Target) ||
		!sameWindowsProductPath(receipt.CanonicalWindowlessPath, journal.LegacyWindowless.Target) ||
		receipt.PriorCLI != (windowsProductPreimageV1{Present: runtime.PriorPresent, SHA256: runtime.PriorSHA256}) ||
		receipt.PriorWindowless != (windowsProductPreimageV1{Present: journal.LegacyWindowless.PriorPresent, SHA256: journal.LegacyWindowless.PriorSHA256}) {
		return errors.New("GUI-owned settlement changed original runtime preimage")
	}
	return nil
}

func cleanupCanonicalEntryLegacyFiles(o WindowsProductTxnOpts, p *windowsProductPreparedTxn, forward bool) error {
	if p.entrySet == nil {
		for _, stage := range []string{p.journal.Staged.Path, p.journal.EntrySet.StagedEntry.Path, p.journal.EntrySet.StagedIdentity.Path} {
			if _, err := os.Lstat(stage); !errors.Is(err, os.ErrNotExist) {
				return fmt.Errorf("%w: cold original stage absence unconfirmed", ErrWindowsProductCleanupPending)
			}
		}
		for _, target := range []string{o.ProductPath, p.journal.EntrySet.CanonicalEntry.Target, p.journal.EntrySet.Identity.Target} {
			paths, err := p.deps.ListGeneratedAsides(target)
			if err != nil || len(paths) != 0 {
				return fmt.Errorf("%w: cold original asides require receiving", ErrWindowsProductCleanupPending)
			}
		}
	}
	if p.legacyRetained != "" {
		if err := removeBoundWindowsProductFileIfPresent(p.legacyRetained, p.journal.LegacyWindowless.PriorSHA256, p.deps); err != nil {
			return err
		}
	}
	if len(p.journal.LegacyWindowless.BaselineAsides) != 0 {
		return fmt.Errorf("%w: legacy original aside custody requires receiving", ErrWindowsProductCleanupPending)
	}
	if _, err := os.Lstat(o.LegacyWindowlessPath); !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("%w: legacy windowless remains", ErrWindowsProductCleanupPending)
	}
	helper := filepath.Join(filepath.Dir(o.ProductPath), "mcphub-pe-admit.exe")
	if _, err := os.Lstat(helper); !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("%w: helper absence unconfirmed", ErrWindowsProductCleanupPending)
	}
	return nil
}

func reconcileCanonicalEntryCommitReceipt(ctx context.Context, stateDir string, j windowsProductRecoveryJournalV3, d WindowsProductTxnDeps) (bool, error) {
	if j.EntrySet == nil {
		return false, errors.New("canonical entry-set journal arm required")
	}
	d = withWindowsProductDefaults(d)
	raw, err := d.ReadReceipt(filepath.Join(stateDir, UpgradeReceiptSchemaV3+".json"))
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	decoded, err := DecodeUpgradeReceipt(raw)
	if err != nil {
		return false, err
	}
	r := decoded.V3
	if r == nil || r.EntrySet == nil || r.Admission != UpgradeAdmissionCanonicalEntryProductV1 {
		return false, nil
	}
	expected := canonicalEntrySetReceipt(j)
	if r.TransactionID != j.TransactionID || r.Artifact.SHA256 != j.Product.NewSHA256 || r.TaskSetSHA256 != j.TaskSetSHA256 || !reflect.DeepEqual(r.EntrySet, expected) {
		return false, errors.New("canonical entry-set committed receipt differs from original journal")
	}
	targets, err := canonicalEntrySetJournalTargets(j)
	if err != nil {
		return false, err
	}
	if _, err := d.AdmitEntrySet(ctx, targets, canonicalEntrySnapshotFromJournal(j)); err != nil {
		return false, err
	}
	receiptSHA := fmt.Sprintf("%x", sha256.Sum256(raw))
	if d.PublishCommitted == nil {
		return false, errors.New("canonical entry-set committed publisher missing")
	}
	_, err = d.PublishCommitted(WindowsProductCommitEvent{Schema: r.Schema, TransactionID: r.TransactionID, Mode: r.Mode, Artifact: r.Artifact, EntrySet: r.EntrySet, TaskSetSHA256: r.TaskSetSHA256, Retired: r.Retired, ReceiptSHA256: receiptSHA, InstalledAt: r.InstalledAt})
	return err == nil, err
}

func canonicalRecoveryRuntimeArtifact(j windowsProductRecoveryJournalV3) windowsProductPairRecoveryArtifact {
	if j.EntrySet != nil && j.EntrySet.PriorRuntime != nil {
		r := j.EntrySet.PriorRuntime
		return windowsProductPairRecoveryArtifact{Target: r.Path, PriorPresent: r.Present, PriorSHA256: r.SHA256, NewSHA256: j.Product.NewSHA256}
	}
	return windowsProductPairRecoveryArtifact{Target: j.Product.Target, PriorPresent: j.Product.PriorPresent, PriorSHA256: j.Product.PriorSHA256, NewSHA256: j.Product.NewSHA256}
}

func reconcileStagedCanonicalEntryNoop(ctx context.Context, stateDir string, mode WindowsProductPairMode, source *stagedWindowsCanonicalEntrySetV1, reconcile func(UpgradeReceiptV3) error) (bool, error) {
	if source == nil || source.original == nil || reconcile == nil {
		return false, errors.New("original set and committed receiving owner required")
	}
	if err := source.original.VerifyUnchanged(); err != nil {
		return false, err
	}
	snapshot, err := source.original.Snapshot()
	if err != nil {
		return false, err
	}
	targets, err := source.original.DestinationTargets()
	if err != nil {
		return false, err
	}
	prior, err := source.acquirePrior(ctx)
	if err != nil {
		return false, err
	}
	for _, member := range []struct{ name, sha string }{{"launcher", snapshot.CanonicalEntry.SHA256}, {"product", snapshot.Product.SHA256}, {"identity", snapshot.Identity.SHA256}} {
		present, _, digest, err := prior.ReadOriginal(member.name)
		if err != nil {
			return false, err
		}
		if !present || digest != member.sha {
			return false, nil
		}
	}
	d := withWindowsProductDefaults(WindowsProductTxnDeps{})
	raw, err := d.ReadReceipt(filepath.Join(stateDir, UpgradeReceiptSchemaV3+".json"))
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	decoded, err := DecodeUpgradeReceipt(raw)
	if err != nil {
		return false, err
	}
	r := decoded.V3
	if r == nil || r.EntrySet == nil || r.Admission != UpgradeAdmissionCanonicalEntryProductV1 {
		return false, nil
	}
	e := r.EntrySet
	if !windowsProductPairReceiptModeMatches(r.Mode, mode) || r.Artifact.SHA256 != snapshot.Product.SHA256 || e.BuildID != snapshot.BuildID || e.RegistryProjectionSHA256 != snapshot.RegistryProjectionSHA256 || e.CanonicalEntrySHA256 != snapshot.CanonicalEntry.SHA256 || e.IdentitySHA256 != snapshot.Identity.SHA256 || !sameWindowsProductPath(e.CanonicalEntryPath, targets.CanonicalEntryPath) || !sameWindowsProductPath(e.ProductPath, targets.ProductPath) || !sameWindowsProductPath(e.IdentityPath, targets.IdentityPath) {
		return false, nil
	}
	if err := reconcile(*r); err != nil {
		return false, err
	}
	return true, nil
}

func canonicalRollbackRuntime(o WindowsProductTxnOpts, p *windowsProductPreparedTxn) (string, productPairFileSnapshot) {
	if p.journal.EntrySet != nil && p.journal.EntrySet.PriorRuntime != nil {
		path := p.journal.EntrySet.PriorRuntime.Path
		if sameWindowsProductPath(path, o.CanonicalEntryPath) {
			return path, p.entrySet.entryPrior
		}
	}
	return o.ProductPath, p.productPrior
}

func canonicalCleanupMember(j windowsProductRecoveryJournalV3, side, member string) (target, stage, expected string, err error) {
	if j.EntrySet == nil {
		return "", "", "", errors.New("cleanup entry-set arm missing")
	}
	var artifact windowsProductRecoveryArtifactV3
	switch member {
	case "launcher":
		artifact = j.EntrySet.CanonicalEntry
		stage = j.EntrySet.StagedEntry.Path
	case "product":
		artifact = j.Product
		stage = j.Staged.Path
	case "identity":
		artifact = j.EntrySet.Identity
		stage = j.EntrySet.StagedIdentity.Path
	default:
		return "", "", "", errors.New("unknown cleanup member")
	}
	switch side {
	case "prior":
		if !artifact.PriorPresent {
			return "", "", "", errors.New("absent prior cannot be cleanup original")
		}
		expected = artifact.PriorSHA256
	case "unused-successor":
		expected = artifact.NewSHA256
	default:
		return "", "", "", errors.New("unknown cleanup side")
	}
	return artifact.Target, stage, expected, nil
}

func validateCanonicalCleanup(j windowsProductRecoveryJournalV3) error {
	if j.EntrySet == nil || len(j.EntrySet.Cleanup) == 0 {
		return nil
	}
	if j.Phase != WindowsProductRecoveryCommitDecided && j.Phase != WindowsProductRecoverySettledCommit {
		return errors.New("irreversible cleanup records require committed-forward decision")
	}
	seen := make(map[string]bool)
	root := filepath.Dir(j.EntrySet.CanonicalEntry.Target)
	for _, row := range j.EntrySet.Cleanup {
		target, stage, expected, err := canonicalCleanupMember(j, row.Side, row.Member)
		if err != nil {
			return err
		}
		key := row.Side + "\x00" + row.Member
		if seen[key] || row.TransactionID != j.TransactionID || row.ExpectedSHA256 != expected || !validLowerHexSHA256(expected) {
			return errors.New("cleanup record disagrees with original transaction/member")
		}
		seen[key] = true
		if row.State != "retained" && row.State != "discard_intent" && row.State != "absence_received" {
			return errors.New("unknown cleanup receiving state")
		}
		source := filepath.Join(root, filepath.FromSlash(row.SourceRelative))
		relative, err := filepath.Rel(root, source)
		if err != nil || relative == "." || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) || filepath.IsAbs(row.SourceRelative) || filepath.ToSlash(relative) != row.SourceRelative {
			return errors.New("cleanup source escaped original root")
		}
		for _, final := range []string{j.EntrySet.CanonicalEntry.Target, j.Product.Target, j.EntrySet.Identity.Target} {
			if sameWindowsProductPath(source, final) {
				return errors.New("canonical successor is never disposable cleanup")
			}
		}
		if !api.IsGeneratedRenameAsideForTarget(target, source) && (row.Side != "unused-successor" || !sameWindowsProductPath(source, stage)) {
			return errors.New("cleanup location is not exact original stage/aside")
		}
	}
	return nil
}

func canonicalCleanupRow(j windowsProductRecoveryJournalV3, side, member string) (int, *windowsCanonicalEntryCleanupV1) {
	if j.EntrySet == nil {
		return -1, nil
	}
	for i := range j.EntrySet.Cleanup {
		row := &j.EntrySet.Cleanup[i]
		if row.Side == side && row.Member == member {
			return i, row
		}
	}
	return -1, nil
}

func cleanupAbsenceAuthorized(row *windowsCanonicalEntryCleanupV1) bool {
	return row != nil && (row.State == "discard_intent" || row.State == "absence_received")
}

func (t *WindowsProductTxn) receiveCommittedCanonicalDiscard(ctx context.Context, side, member, path string) error {
	j := &t.prepared.journal
	if j.Phase != WindowsProductRecoveryCommitDecided && j.Phase != WindowsProductRecoverySettledCommit {
		return errors.New("original committed cleanup decision missing")
	}
	relative, err := t.entryRelative(path)
	if err != nil {
		return err
	}
	_, _, expected, err := canonicalCleanupMember(*j, side, member)
	if err != nil {
		return err
	}
	index, row := canonicalCleanupRow(*j, side, member)
	if row == nil {
		j.EntrySet.Cleanup = append(j.EntrySet.Cleanup, windowsCanonicalEntryCleanupV1{TransactionID: j.TransactionID, Side: side, Member: member, SourceRelative: relative, ExpectedSHA256: expected, State: "retained"})
		index = len(j.EntrySet.Cleanup) - 1
		row = &j.EntrySet.Cleanup[index]
	} else if row.SourceRelative != relative || row.ExpectedSHA256 != expected {
		return errors.New("cleanup original location changed")
	}
	if row.State == "absence_received" {
		if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
			return errors.Join(err, ErrWindowsProductCleanupPending)
		}
		return nil
	}
	if row.State == "discard_intent" {
		if _, err := os.Lstat(path); errors.Is(err, os.ErrNotExist) {
			j.EntrySet.Cleanup[index].State = "absence_received"
			return t.persistProductJournal()
		} else if err != nil {
			return err
		}
	}
	row.State = "discard_intent"
	if err := t.persistProductJournal(); err != nil {
		return err
	}
	if err := t.discardEntryOriginal(ctx, side == "prior", member, path); err != nil {
		return err
	}
	j.EntrySet.Cleanup[index].State = "absence_received"
	return t.persistProductJournal()
}

func closeCanonicalEntryFileOwners(o WindowsProductTxnOpts, p *windowsProductPreparedTxn) error {
	var causes []error
	if o.EntrySet != nil {
		if o.EntrySet.original != nil {
			causes = append(causes, o.EntrySet.original.Close())
		}
		if o.EntrySet.prior != nil {
			causes = append(causes, o.EntrySet.prior.Close())
		}
	}
	return errors.Join(causes...)
}

func validateOriginalPayloadDirectory(j windowsProductRecoveryJournalV3) error {
	if j.EntrySet == nil {
		return nil
	}
	d := j.EntrySet.PayloadDirectory
	if d.Created && d.Original == nil {
		return errors.New("payload directory Created flag has no native creation identity")
	}
	if d.State != "" && d.State != "created_identity_received" && d.State != "dispose_intent" && d.State != "absence_received" {
		return errors.New("unknown payload directory disposition state")
	}
	if (d.State == "dispose_intent" || d.State == "absence_received") && (d.PriorPresent || !d.Created || d.Original == nil || j.Phase == WindowsProductRecoveryCommitDecided || j.Phase == WindowsProductRecoverySettledCommit) {
		return errors.New("payload directory disposal is not original-created rollback")
	}
	if d.Absence != nil && (d.State != "absence_received" || !d.Absence.Absent || d.Original == nil || d.Absence.ParentIdentity != d.Original.ParentIdentity || !sameWindowsProductPath(d.Absence.Root, d.Original.Root) || d.Absence.RelativePath != d.Original.RelativePath) {
		return errors.New("payload directory absence observation disagrees")
	}
	if d.Original == nil {
		if j.Phase != WindowsProductRecoveryPrepared && j.Phase != WindowsProductRecoveryGUIOwnedStopPending {
			return errors.New("payload directory original receiving is incomplete")
		}
		return nil
	}
	expected := d.Original
	if !d.CreationEntered || expected.Schema != binaryadmission.WindowsCanonicalEntryPayloadDirectorySchemaV1 || expected.Created != d.Created || expected.Created == d.PriorPresent || !sameWindowsProductPath(expected.Root, filepath.Dir(j.EntrySet.CanonicalEntry.Target)) || expected.RelativePath != filepath.ToSlash(filepath.Dir(binaryadmission.WindowsCanonicalProductRelativePathV1)) {
		return errors.New("payload directory native snapshot disagrees with original transaction")
	}
	return nil
}

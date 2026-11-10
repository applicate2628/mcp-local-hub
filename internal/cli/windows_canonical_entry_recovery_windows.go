//go:build windows

package cli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"mcp-local-hub/internal/binaryadmission"
)

type canonicalRecoveryOriginalOwner interface {
	ReadOriginal(string) (bool, []byte, string, error)
	MoveOriginal(context.Context, string, string) (binaryadmission.WindowsCanonicalEntryMoveResultV1, error)
	DiscardOriginal(context.Context, string, string) (binaryadmission.WindowsCanonicalEntryMoveResultV1, error)
	RuntimeProduct(context.Context) (*binaryadmission.WindowsCanonicalEntryPriorRuntimeV1, error)
	Close() error
}

// Constructed only by the existing fenced V3 journal receiver. The callback
// lends immutable classified inputs to native acquisition; it cannot mutate
// the journal, authorize a different root, or fabricate a native owner.
type windowsCanonicalRecoveryPurpose string

const (
	canonicalRecoveryRollback         windowsCanonicalRecoveryPurpose = "rollback"
	canonicalRecoveryCommittedCleanup windowsCanonicalRecoveryPurpose = "committed-forward"
)

type windowsCanonicalJournalRecoverySource struct {
	purpose           windowsCanonicalRecoveryPurpose
	directory         *binaryadmission.WindowsCanonicalEntryPayloadDirectoryV1
	directoryAbsence  *binaryadmission.WindowsCanonicalEntryPayloadDirectoryAbsenceObservationV1
	authorizedAbsence map[string]bool
	self              *windowsCanonicalJournalRecoverySource
	req               windowsProductRecoveryRequest
	deps              windowsProductRecoveryDeps
	journal           windowsProductRecoveryJournalV3
	raw               []byte
	path              string
	active            bool
	prior, successor  binaryadmission.WindowsCanonicalRecoveryLocationsV1
}

func (s *windowsCanonicalJournalRecoverySource) ValidateOriginalRecovery(ctx context.Context) error {
	if s == nil || s.self != s || !s.active || ctx == nil {
		return ErrWindowsProductRecoveryIdentity
	}
	if s.purpose == canonicalRecoveryCommittedCleanup && s.journal.Phase != WindowsProductRecoveryCommitDecided && s.journal.Phase != WindowsProductRecoverySettledCommit {
		return ErrWindowsProductRecoveryIdentity
	}
	current, err := s.deps.ReadJournal(s.path)
	if err != nil {
		return err
	}
	if !bytes.Equal(current, s.raw) {
		return fmt.Errorf("%w: original recovery journal changed", ErrWindowsProductRecoveryIdentity)
	}
	return nil
}

func (s *windowsCanonicalJournalRecoverySource) WithOriginalRecoveryInput(ctx context.Context, side binaryadmission.WindowsCanonicalRecoverySideV1, use func(binaryadmission.WindowsCanonicalRecoveryTargetsV1, binaryadmission.WindowsCanonicalRecoveryLocationsV1) error) error {
	if err := s.ValidateOriginalRecovery(ctx); err != nil {
		return err
	}
	if use == nil {
		return ErrWindowsProductRecoveryIdentity
	}
	targets, err := canonicalEntrySetJournalTargets(s.journal)
	if err != nil {
		return err
	}
	root := filepath.Dir(targets.CanonicalEntryPath)
	relative := func(path string) string { r, _ := filepath.Rel(root, path); return filepath.ToSlash(r) }
	nativeTargets := binaryadmission.WindowsCanonicalRecoveryTargetsV1{Root: root, LauncherRelative: relative(targets.CanonicalEntryPath), ProductRelative: relative(targets.ProductPath), IdentityRelative: relative(targets.IdentityPath)}
	switch string(side) {
	case "prior":
		return use(nativeTargets, s.prior)
	case "successor":
		return use(nativeTargets, s.successor)
	default:
		return ErrWindowsProductRecoveryIdentity
	}
}

func canonicalRecoveryArtifact(member windowsProductRecoveryArtifactV3) windowsProductPairRecoveryArtifact {
	return windowsProductPairRecoveryArtifact{Target: member.Target, PriorPresent: member.PriorPresent, PriorSHA256: member.PriorSHA256, NewSHA256: member.NewSHA256, BaselineAsideBasenames: recoveryAsideBasenames(member.BaselineAsides)}
}

func prepareCanonicalRecoverySource(req windowsProductRecoveryRequest, d windowsProductRecoveryDeps, path string, j windowsProductRecoveryJournalV3, purpose windowsCanonicalRecoveryPurpose) (*windowsCanonicalJournalRecoverySource, error) {
	raw, err := d.ReadJournal(path)
	if err != nil {
		return nil, err
	}
	decoded, err := decodeWindowsProductRecoveryJournalV3(raw)
	if err != nil {
		return nil, err
	}
	left, err := encodeWindowsProductRecoveryJournalV3(decoded)
	if err != nil {
		return nil, err
	}
	right, err := encodeWindowsProductRecoveryJournalV3(j)
	if err != nil {
		return nil, err
	}
	if !bytes.Equal(left, right) {
		return nil, ErrWindowsProductRecoveryIdentity
	}
	if purpose == canonicalRecoveryCommittedCleanup {
		if j.Phase != WindowsProductRecoveryCommitDecided && j.Phase != WindowsProductRecoverySettledCommit {
			return nil, ErrWindowsProductRecoveryIdentity
		}
		if err := readRecoveryCanonicalEntrySet(req.Context, j, d); err != nil {
			return nil, err
		}
	} else if purpose != canonicalRecoveryRollback || j.Phase == WindowsProductRecoveryCommitDecided || j.Phase == WindowsProductRecoverySettledCommit {
		return nil, ErrWindowsProductRecoveryIdentity
	}
	source := &windowsCanonicalJournalRecoverySource{req: req, deps: d, journal: j, raw: append([]byte(nil), raw...), path: path, active: true, purpose: purpose, authorizedAbsence: make(map[string]bool)}
	source.self = source
	root := filepath.Dir(j.EntrySet.CanonicalEntry.Target)
	relative := func(path string) (string, error) {
		r, e := filepath.Rel(root, path)
		if e != nil || r == "." || r == ".." || strings.HasPrefix(r, ".."+string(filepath.Separator)) || filepath.IsAbs(r) {
			return "", ErrWindowsProductRecoveryIdentity
		}
		return filepath.ToSlash(r), nil
	}
	// A disposed payload parent must be received through its original native
	// identity before the product's aside inventory can treat that parent as absent.
	if _, err := source.observeDisposedPayloadDirectory(req.Context); err != nil {
		return nil, err
	}
	if purpose == canonicalRecoveryCommittedCleanup {
		for _, row := range j.EntrySet.Cleanup {
			location := filepath.Join(root, filepath.FromSlash(row.SourceRelative))
			_, statErr := os.Lstat(location)
			if errors.Is(statErr, os.ErrNotExist) {
				if !cleanupAbsenceAuthorized(&row) {
					return nil, fmt.Errorf("%w: cleanup absence has no durable intent", ErrWindowsProductCleanupPending)
				}
				source.authorizedAbsence[row.Side+"\x00"+row.Member] = true
				continue
			}
			if statErr != nil {
				return nil, statErr
			}
			if row.State == "absence_received" {
				return nil, fmt.Errorf("%w: terminal cleanup location is occupied", ErrWindowsProductCleanupPending)
			}
			actual, err := d.HashFile(location)
			if err != nil || actual != row.ExpectedSHA256 {
				return nil, errors.Join(err, ErrWindowsProductCleanupPending)
			}
		}
	}
	for _, member := range []struct {
		name, stage      string
		artifact         windowsProductRecoveryArtifactV3
		prior, successor *binaryadmission.WindowsCanonicalRecoveryMemberV1
	}{
		{"launcher", j.EntrySet.StagedEntry.Path, j.EntrySet.CanonicalEntry, &source.prior.Launcher, &source.successor.Launcher},
		{"product", j.Staged.Path, j.Product, &source.prior.Product, &source.successor.Product},
		{"identity", j.EntrySet.StagedIdentity.Path, j.EntrySet.Identity, &source.prior.Identity, &source.successor.Identity},
	} {
		artifactDeps := pairRecoveryDepsFromProduct(d)
		if member.name == "product" && source.directoryAbsence != nil {
			artifactDeps.ListAsides = func(target string) ([]string, error) {
				if !sameWindowsProductPath(target, j.Product.Target) || !sameWindowsProductPath(filepath.Dir(target), j.EntrySet.PayloadDirectory.Path) {
					return nil, ErrWindowsProductRecoveryIdentity
				}
				if _, err := os.Lstat(j.EntrySet.PayloadDirectory.Path); !errors.Is(err, os.ErrNotExist) {
					return nil, errors.Join(err, ErrWindowsProductRecoveryPending)
				}
				return []string{}, nil
			}
		}
		var state windowsProductPairRecoveryArtifactState
		if purpose == canonicalRecoveryCommittedCleanup {
			state, err = observeWindowsProductPairRecoveryArtifact(canonicalRecoveryArtifact(member.artifact), artifactDeps)
			if err == nil && state.canonical != member.artifact.NewSHA256 {
				err = fmt.Errorf("%w: committed canonical %s is not its successor", ErrWindowsProductRecoveryIdentity, member.name)
			}
		} else {
			state, err = inspectWindowsProductPairRecoveryArtifact(canonicalRecoveryArtifact(member.artifact), artifactDeps)
		}
		if err != nil {
			return nil, fmt.Errorf("%w: classify %s originals: %w", ErrWindowsProductRecoveryPending, member.name, err)
		}
		if member.artifact.PriorPresent {
			original := state.priorAside
			if state.canonical == member.artifact.PriorSHA256 {
				original = member.artifact.Target
			}
			if original == "" {
				if purpose != canonicalRecoveryCommittedCleanup || !source.authorizedAbsence["prior\x00"+member.name] {
					return nil, fmt.Errorf("%w: unique original %s prior absent", ErrWindowsProductRecoveryPending, member.name)
				}
			} else if purpose != canonicalRecoveryCommittedCleanup || !sameWindowsProductPath(original, member.artifact.Target) {
				rel, err := relative(original)
				if err != nil {
					return nil, err
				}
				*member.prior = binaryadmission.WindowsCanonicalRecoveryMemberV1{Present: true, SourceRelative: rel, ExpectedSHA256: member.artifact.PriorSHA256}
			}
		}
		currentNew := ""
		if purpose == canonicalRecoveryCommittedCleanup {
			if _, row := canonicalCleanupRow(j, "unused-successor", member.name); row != nil {
				if !source.authorizedAbsence["unused-successor\x00"+member.name] {
					currentNew = filepath.Join(root, filepath.FromSlash(row.SourceRelative))
				}
			} else {
				currentNew = state.newAside
				stageSHA, stageErr := d.HashFile(member.stage)
				if stageErr == nil {
					if stageSHA != member.artifact.NewSHA256 || currentNew != "" {
						return nil, ErrWindowsProductCleanupPending
					}
					currentNew = member.stage
				} else if !errors.Is(stageErr, os.ErrNotExist) {
					return nil, stageErr
				}
			}
		} else {
			if state.canonical == member.artifact.NewSHA256 && (!member.artifact.PriorPresent || member.artifact.PriorSHA256 != member.artifact.NewSHA256) {
				currentNew = member.artifact.Target
			}
			if currentNew == "" {
				currentNew = state.newAside
			}
			stageSHA, stageErr := d.HashFile(member.stage)
			if stageErr == nil {
				if stageSHA != member.artifact.NewSHA256 || currentNew != "" {
					return nil, fmt.Errorf("%w: ambiguous %s successor", ErrWindowsProductRecoveryPending, member.name)
				}
				currentNew = member.stage
			} else if !errors.Is(stageErr, os.ErrNotExist) {
				return nil, stageErr
			}
		}
		if currentNew != "" {
			rel, err := relative(currentNew)
			if err != nil {
				return nil, err
			}
			*member.successor = binaryadmission.WindowsCanonicalRecoveryMemberV1{Present: true, SourceRelative: rel, ExpectedSHA256: member.artifact.NewSHA256}
		}
	}
	if source.directoryAbsence != nil && (source.prior.Product.Present || source.successor.Product.Present) {
		return nil, ErrWindowsProductRecoveryPending
	}
	return source, nil
}

func (s *windowsCanonicalJournalRecoverySource) persistCleanupState(side, member, relative, state string) error {
	if s.purpose != canonicalRecoveryCommittedCleanup {
		return ErrWindowsProductRecoveryIdentity
	}
	if err := s.ValidateOriginalRecovery(s.req.Context); err != nil {
		return err
	}
	next := s.journal
	arm := *next.EntrySet
	arm.Cleanup = append([]windowsCanonicalEntryCleanupV1(nil), arm.Cleanup...)
	next.EntrySet = &arm
	_, _, expected, err := canonicalCleanupMember(next, side, member)
	if err != nil {
		return err
	}
	index, row := canonicalCleanupRow(next, side, member)
	if row == nil {
		arm.Cleanup = append(arm.Cleanup, windowsCanonicalEntryCleanupV1{TransactionID: next.TransactionID, Side: side, Member: member, SourceRelative: relative, ExpectedSHA256: expected, State: "retained"})
		index = len(arm.Cleanup) - 1
	} else if row.SourceRelative != relative || row.ExpectedSHA256 != expected {
		return ErrWindowsProductRecoveryIdentity
	}
	arm.Cleanup[index].State = state
	if err := persistWindowsProductRecoveryJournalV3(s.path, next, s.deps.WriteJournal, s.deps.ReadJournal); err != nil {
		return err
	}
	raw, err := s.deps.ReadJournal(s.path)
	if err != nil {
		return err
	}
	s.journal = next
	s.raw = append([]byte(nil), raw...)
	return nil
}

func (s *windowsCanonicalJournalRecoverySource) WithOriginalPayloadDirectoryRecoveryInput(ctx context.Context, use func(binaryadmission.WindowsCanonicalEntryPayloadDirectorySnapshotV1) error) error {
	if err := s.ValidateOriginalRecovery(ctx); err != nil {
		return err
	}
	if s.journal.EntrySet == nil || s.journal.EntrySet.PayloadDirectory.Original == nil || use == nil {
		return fmt.Errorf("%w: native payload directory identity was not durably received", ErrWindowsProductRecoveryPending)
	}
	return use(*s.journal.EntrySet.PayloadDirectory.Original)
}

func (s *windowsCanonicalJournalRecoverySource) OriginalPayloadDirectory(ctx context.Context) (*binaryadmission.WindowsCanonicalEntryPayloadDirectoryV1, error) {
	if err := s.ValidateOriginalRecovery(ctx); err != nil {
		return nil, err
	}
	if s.directory == nil {
		if s.directoryAbsence != nil {
			return nil, nil
		}
		return nil, fmt.Errorf("%w: original directory owner not acquired", ErrWindowsProductRecoveryPending)
	}
	return s.directory, nil
}

func (s *windowsCanonicalJournalRecoverySource) WithOriginalPayloadDirectoryAbsenceInput(ctx context.Context, use func(binaryadmission.WindowsCanonicalEntryPayloadDirectorySnapshotV1) error) error {
	if err := s.ValidateOriginalRecovery(ctx); err != nil {
		return err
	}
	d := s.journal.EntrySet.PayloadDirectory
	if s.purpose != canonicalRecoveryRollback || d.PriorPresent || !d.Created || d.Original == nil || (d.State != "dispose_intent" && d.State != "absence_received") || use == nil {
		return ErrWindowsProductRecoveryPending
	}
	return use(*d.Original)
}

func (s *windowsCanonicalJournalRecoverySource) observeDisposedPayloadDirectory(ctx context.Context) (bool, error) {
	if s.directoryAbsence != nil {
		return true, s.ValidateOriginalRecovery(ctx)
	}
	d := s.journal.EntrySet.PayloadDirectory
	if s.purpose == canonicalRecoveryRollback && !d.PriorPresent && d.Created && d.Original != nil && (d.State == "dispose_intent" || d.State == "absence_received") {
		if _, err := os.Lstat(d.Path); errors.Is(err, os.ErrNotExist) {
			if s.journal.Product.PriorPresent {
				return false, ErrWindowsProductRecoveryPending
			}
			observed, err := binaryadmission.ObserveWindowsCanonicalEntryPayloadDirectoryAbsenceV1(ctx, s)
			if err != nil {
				return false, err
			}
			if !observed.Absent {
				return false, ErrWindowsProductRecoveryPending
			}
			s.directoryAbsence = &observed
			return true, nil
		} else if err != nil {
			return false, err
		}
	}
	return false, nil
}

func (s *windowsCanonicalJournalRecoverySource) acquireDirectory(ctx context.Context) error {
	absent, err := s.observeDisposedPayloadDirectory(ctx)
	if err != nil {
		return err
	}
	if absent {
		if s.prior.Product.Present || s.successor.Product.Present {
			return ErrWindowsProductRecoveryPending
		}
		return nil
	}
	owner, err := binaryadmission.AcquireWindowsCanonicalEntryPayloadDirectoryForRecoveryV1(ctx, s)
	s.directory = owner
	if err != nil {
		return err
	}
	if owner == nil {
		return ErrWindowsProductRecoveryPending
	}
	return nil
}
func rollbackCanonicalEntryRecovery(req windowsProductRecoveryRequest, d windowsProductRecoveryDeps, path string, j windowsProductRecoveryJournalV3, tasks WindowsProductTaskTxn) (result *WindowsProductPairRecoverySettled, retErr error) {
	source, err := prepareCanonicalRecoverySource(req, d, path, j, canonicalRecoveryRollback)
	if err != nil {
		return nil, err
	}
	defer func() { source.active = false }()
	if err := source.acquireDirectory(req.Context); err != nil {
		if source.directory != nil {
			return nil, errors.Join(err, source.directory.Close())
		}
		return nil, err
	}
	defer func() {
		if source.directory != nil {
			retErr = errors.Join(retErr, source.directory.Close())
		}
	}()
	prior, err := binaryadmission.AcquireWindowsCanonicalEntryForRecoveryV1(req.Context, source, binaryadmission.WindowsCanonicalRecoverySideV1("prior"))
	if prior != nil {
		defer func() { retErr = errors.Join(retErr, prior.Close()) }()
	}
	if err != nil {
		return nil, err
	}
	successor, err := binaryadmission.AcquireWindowsCanonicalEntryForRecoveryV1(req.Context, source, binaryadmission.WindowsCanonicalRecoverySideV1("successor"))
	if successor != nil {
		defer func() { retErr = errors.Join(retErr, successor.Close()) }()
	}
	if err != nil {
		return nil, err
	}
	if prior == nil || successor == nil {
		return nil, ErrWindowsProductRecoveryIdentity
	}
	root := filepath.Dir(j.EntrySet.CanonicalEntry.Target)
	sequence := 0
	rel := func(target string) string { value, _ := filepath.Rel(root, target); return filepath.ToSlash(value) }
	locations := map[string]string{"launcher": source.successor.Launcher.SourceRelative, "product": source.successor.Product.SourceRelative, "identity": source.successor.Identity.SourceRelative}
	move := func(owner canonicalRecoveryOriginalOwner, member, target string) error {
		if err := source.ValidateOriginalRecovery(req.Context); err != nil {
			return err
		}
		actual, err := owner.MoveOriginal(context.WithoutCancel(req.Context), member, rel(target))
		if received := canonicalEntryMoveResultError(actual, err, member, rel(target)); received != nil {
			return errors.Join(received, ErrWindowsProductRecoveryPending)
		}
		return nil
	}
	withdraw := func(member, target string) error {
		if locations[member] == "" || locations[member] != rel(target) {
			return nil
		}
		sequence++
		aside := target + ".old-" + d.Now().UTC().Add(time.Duration(sequence)*time.Second).Format("20060102T150405Z")
		if err := move(successor, member, aside); err != nil {
			return err
		}
		locations[member] = rel(aside)
		return nil
	}
	restore := func(member, target string, original binaryadmission.WindowsCanonicalRecoveryMemberV1) error {
		if err := withdraw(member, target); err != nil {
			return err
		}
		if original.Present && original.SourceRelative != rel(target) {
			return move(prior, member, target)
		}
		return nil
	}
	if err := withdraw("launcher", j.EntrySet.CanonicalEntry.Target); err != nil {
		return nil, err
	}
	if err := restore("product", j.Product.Target, source.prior.Product); err != nil {
		return nil, err
	}
	if err := restore("identity", j.EntrySet.Identity.Target, source.prior.Identity); err != nil {
		return nil, err
	}
	if tasks != nil {
		if err := tasks.RestoreImport(); err != nil {
			return nil, fmt.Errorf("%w: task inverse: %w", ErrWindowsProductPairRecoveryRollback, err)
		}
	} else if j.Phase != WindowsProductRecoverySettledRollback {
		return nil, ErrWindowsProductRecoveryIdentity
	}
	legacy := windowsProductPairRecoveryArtifact{Target: j.LegacyWindowless.Target, PriorPresent: j.LegacyWindowless.PriorPresent, PriorSHA256: j.LegacyWindowless.PriorSHA256, BaselineAsideBasenames: recoveryAsideBasenames(j.LegacyWindowless.BaselineAsides)}
	legacyState, err := inspectWindowsProductLegacyRecoveryArtifact(legacy, d)
	if err != nil {
		return nil, err
	}
	if err := restoreWindowsProductLegacyRecoveryArtifact(legacy, legacyState, d); err != nil {
		return nil, err
	}
	for _, receipt := range []windowsProductPairRecoveryReceipt{j.Receipts.V3, j.Receipts.V2} {
		if err := restoreWindowsProductPairRecoveryReceipt(receipt, pairRecoveryDepsFromProduct(d)); err != nil {
			return nil, err
		}
	}
	if err := restore("launcher", j.EntrySet.CanonicalEntry.Target, source.prior.Launcher); err != nil {
		return nil, err
	}
	if req.RecoverPriorSettlementOriginal == nil {
		return nil, fmt.Errorf("%w: original prior runtime settlement callback unavailable", ErrWindowsProductRecoveryPending)
	}
	view, err := prior.RuntimeProduct(req.Context)
	if err != nil {
		return nil, err
	}
	if err := source.ValidateOriginalRecovery(req.Context); err != nil {
		return nil, err
	}
	if err := req.RecoverPriorSettlementOriginal(req.Context, j, view); err != nil {
		return nil, err
	}
	for _, member := range []string{"launcher", "product", "identity"} {
		location := locations[member]
		if location == "" {
			continue
		}
		actual, err := successor.DiscardOriginal(context.WithoutCancel(req.Context), member, location)
		if err = canonicalEntryDiscardResultError(actual, err, member, location); err != nil {
			return nil, errors.Join(err, ErrWindowsProductRecoveryPending)
		}
	}
	if err := errors.Join(prior.Close(), successor.Close()); err != nil {
		return nil, err
	}
	if !j.EntrySet.PayloadDirectory.PriorPresent {
		if source.directoryAbsence != nil {
			j.EntrySet.PayloadDirectory.State = "absence_received"
			j.EntrySet.PayloadDirectory.Absence = source.directoryAbsence
		} else {
			j.EntrySet.PayloadDirectory.State = "dispose_intent"
			if err := persistWindowsProductRecoveryJournalV3(path, j, d.WriteJournal, d.ReadJournal); err != nil {
				return nil, err
			}
			current, err := d.ReadJournal(path)
			if err != nil {
				return nil, err
			}
			source.journal = j
			source.raw = append([]byte(nil), current...)
			result, disposeErr := source.directory.DisposeOriginalIfEmpty(context.WithoutCancel(req.Context))
			if err := canonicalEntryDiscardResultError(result, disposeErr, "payload-directory", j.EntrySet.PayloadDirectory.Original.RelativePath); err != nil {
				return nil, errors.Join(err, ErrWindowsProductRecoveryPending)
			}
			j.EntrySet.PayloadDirectory.State = "absence_received"
			j.EntrySet.PayloadDirectory.Disposition = &windowsCanonicalEntryMoveRecordV1{Member: result.Member, SourceRelative: result.SourceRelative, Applied: result.Applied, Readback: result.Readback, NativeStatusKnown: result.NativeStatusKnown, NativeStatus: result.NativeStatus}
		}
	}
	if source.directory != nil {
		if err := source.directory.Close(); err != nil {
			return nil, err
		}
	}
	j.Phase = WindowsProductRecoverySettledRollback
	j.TaskSetSHA256 = ""
	if err := persistWindowsProductRecoveryJournalV3(path, j, d.WriteJournal, d.ReadJournal); err != nil {
		return nil, err
	}
	if err := d.DiscardTaskStore(j.Tasks); err != nil {
		return nil, err
	}
	if err := d.RemoveJournal(path); err != nil {
		return nil, err
	}
	return &WindowsProductPairRecoverySettled{TransactionID: j.TransactionID, Outcome: WindowsProductPairRecoverySettledRollback}, nil
}

func readRecoveryCanonicalEntrySet(ctx context.Context, j windowsProductRecoveryJournalV3, d windowsProductRecoveryDeps) error {
	if j.EntrySet == nil {
		return nil
	}
	targets, err := canonicalEntrySetJournalTargets(j)
	if err != nil {
		return err
	}
	_, err = d.AdmitEntrySet(ctx, targets, canonicalEntrySnapshotFromJournal(j))
	return err
}

func resolveCanonicalRecoveryRequest(req *windowsProductRecoveryRequest, j windowsProductRecoveryJournalV3) error {
	if j.EntrySet == nil {
		if req.CanonicalEntryPath != "" {
			req.ProductPath = req.CanonicalEntryPath
		}
		return nil
	}
	targets, err := canonicalEntrySetJournalTargets(j)
	if err != nil {
		return err
	}
	if req.CanonicalEntryPath != "" && !sameWindowsRecoveryPath(req.CanonicalEntryPath, targets.CanonicalEntryPath) {
		return ErrWindowsProductRecoveryIdentity
	}
	if req.IdentityPath != "" && !sameWindowsRecoveryPath(req.IdentityPath, targets.IdentityPath) {
		return ErrWindowsProductRecoveryIdentity
	}
	if !sameWindowsRecoveryPath(req.ProductPath, targets.CanonicalEntryPath) && !sameWindowsRecoveryPath(req.ProductPath, targets.ProductPath) {
		return ErrWindowsProductRecoveryIdentity
	}
	req.ProductPath = targets.ProductPath
	req.CanonicalEntryPath = targets.CanonicalEntryPath
	req.IdentityPath = targets.IdentityPath
	return nil
}

func cleanupColdCanonicalOriginals(req windowsProductRecoveryRequest, d windowsProductRecoveryDeps, path string, j *windowsProductRecoveryJournalV3) (retErr error) {
	source, err := prepareCanonicalRecoverySource(req, d, path, *j, canonicalRecoveryCommittedCleanup)
	if err != nil {
		return err
	}
	defer func() { *j = source.journal; source.active = false }()
	if err := source.acquireDirectory(req.Context); err != nil {
		if source.directory != nil {
			return errors.Join(err, source.directory.Close())
		}
		return err
	}
	defer func() {
		if source.directory != nil {
			retErr = errors.Join(retErr, source.directory.Close())
		}
	}()
	for _, row := range append([]windowsCanonicalEntryCleanupV1(nil), source.journal.EntrySet.Cleanup...) {
		if source.authorizedAbsence[row.Side+"\x00"+row.Member] && row.State == "discard_intent" {
			if err := source.persistCleanupState(row.Side, row.Member, row.SourceRelative, "absence_received"); err != nil {
				return err
			}
		}
	}
	root := filepath.Dir(j.EntrySet.CanonicalEntry.Target)
	relative := func(path string) string { value, _ := filepath.Rel(root, path); return filepath.ToSlash(value) }
	prior, err := binaryadmission.AcquireWindowsCanonicalEntryForRecoveryV1(req.Context, source, binaryadmission.WindowsCanonicalRecoverySideV1("prior"))
	if prior != nil {
		defer func() { retErr = errors.Join(retErr, prior.Close()) }()
	}
	if err != nil {
		return err
	}
	successor, err := binaryadmission.AcquireWindowsCanonicalEntryForRecoveryV1(req.Context, source, binaryadmission.WindowsCanonicalRecoverySideV1("successor"))
	if successor != nil {
		defer func() { retErr = errors.Join(retErr, successor.Close()) }()
	}
	if err != nil {
		return err
	}
	if prior == nil || successor == nil {
		return ErrWindowsProductRecoveryIdentity
	}
	dispose := func(owner canonicalRecoveryOriginalOwner, side, member, location string) error {
		if err := source.persistCleanupState(side, member, location, "discard_intent"); err != nil {
			return err
		}
		actual, callErr := owner.DiscardOriginal(context.WithoutCancel(req.Context), member, location)
		if err := canonicalEntryDiscardResultError(actual, callErr, member, location); err != nil {
			return errors.Join(err, ErrWindowsProductCleanupPending)
		}
		return source.persistCleanupState(side, member, location, "absence_received")
	}
	for _, member := range []struct {
		name, target  string
		original, new binaryadmission.WindowsCanonicalRecoveryMemberV1
	}{
		{"launcher", j.EntrySet.CanonicalEntry.Target, source.prior.Launcher, source.successor.Launcher},
		{"product", j.Product.Target, source.prior.Product, source.successor.Product},
		{"identity", j.EntrySet.Identity.Target, source.prior.Identity, source.successor.Identity},
	} {
		if member.original.Present && member.original.SourceRelative != relative(member.target) {
			if err := dispose(prior, "prior", member.name, member.original.SourceRelative); err != nil {
				return err
			}
		}
		if member.new.Present {
			if err := dispose(successor, "unused-successor", member.name, member.new.SourceRelative); err != nil {
				return err
			}
		}
	}
	if err := errors.Join(prior.Close(), successor.Close()); err != nil {
		return err
	}
	return source.directory.Close()
}

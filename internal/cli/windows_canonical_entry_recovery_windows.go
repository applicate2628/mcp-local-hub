//go:build windows

package cli

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync"
	"time"

	"mcp-local-hub/internal/api"
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
	canonicalRecoveryRollbackCleanup  windowsCanonicalRecoveryPurpose = "rollback-cleanup"
)

type windowsCanonicalJournalRecoverySource struct {
	beforeCurrentInverse     func(context.Context) error
	cleanEffect              currentCleanEffectState
	inverseMovesReceived     int
	memberDirectories        []*binaryadmission.WindowsCanonicalEntryPayloadDirectoryV1
	memberDirectorySnapshots map[*binaryadmission.WindowsCanonicalEntryPayloadDirectoryV1]binaryadmission.WindowsCanonicalEntryMemberDirectorySnapshotV2
	memberResults            []binaryadmission.WindowsCanonicalEntryMoveResultV1
	memberErrors             []error
	purpose                  windowsCanonicalRecoveryPurpose
	directory                *binaryadmission.WindowsCanonicalEntryPayloadDirectoryV1
	directoryAbsence         *binaryadmission.WindowsCanonicalEntryPayloadDirectoryAbsenceObservationV1
	authorizedAbsence        map[string]bool
	self                     *windowsCanonicalJournalRecoverySource
	req                      windowsProductRecoveryRequest
	deps                     windowsProductRecoveryDeps
	journal                  windowsProductRecoveryJournalV3
	raw                      []byte
	path                     string
	active                   bool
	prior, successor         binaryadmission.WindowsCanonicalRecoveryLocationsV1
}

func (s *windowsCanonicalJournalRecoverySource) ValidateOriginalRecovery(ctx context.Context) error {
	if err := s.validateOriginalRecoveryState(ctx); err != nil {
		return err
	}
	return s.validateOriginalRecoveryBytes(ctx, s.path, s.raw)
}

func (s *windowsCanonicalJournalRecoverySource) validateOriginalRecoveryBytes(ctx context.Context, path string, raw []byte) error {
	current, err := s.deps.readJournalContext(ctx, path)
	if err != nil {
		return err
	}
	if !bytes.Equal(current, raw) {
		return fmt.Errorf("%w: original recovery journal changed", ErrWindowsProductRecoveryIdentity)
	}
	return ctx.Err()
}

func (s *windowsCanonicalJournalRecoverySource) validateOriginalRecoveryState(ctx context.Context) error {
	if s == nil || s.self != s || !s.active || ctx == nil {
		return ErrWindowsProductRecoveryIdentity
	}
	if s.purpose == canonicalRecoveryCommittedCleanup && s.journal.Phase != WindowsProductRecoveryCommitDecided && s.journal.Phase != WindowsProductRecoverySettledCommit {
		return ErrWindowsProductRecoveryIdentity
	}
	if s.journal.EntryBundle != nil {
		if err := ctx.Err(); err != nil {
			return err
		}
		switch s.purpose {
		case canonicalRecoveryCommittedCleanup:
		case canonicalRecoveryRollback:
			if s.journal.Phase == WindowsProductRecoveryCommitDecided || s.journal.Phase == WindowsProductRecoverySettledCommit {
				return ErrWindowsProductRecoveryIdentity
			}
		case canonicalRecoveryRollbackCleanup:
			state := s.journal.EntryBundle.Bundle.Disposition.State
			if s.journal.Phase == WindowsProductRecoveryCommitDecided || s.journal.Phase == WindowsProductRecoverySettledCommit || (state != "rolled-back" && state != "cleanup-pending" && state != "settled") {
				return ErrWindowsProductRecoveryIdentity
			}
		default:
			return ErrWindowsProductRecoveryIdentity
		}
	}
	return nil
}

// The checker is live state validation for one synchronous read-only operation.
// Filesystem currentness is received by full protected reads at both boundaries;
// this is neither an atomic journal snapshot nor credit for any native effect.
func (s *windowsCanonicalJournalRecoverySource) WithCanonicalRecoveryReadScope(ctx context.Context, body func(func(context.Context) error) error) error {
	return s.withCanonicalRecoveryReadScope(ctx, s.ValidateOriginalRecovery, nil, body)
}

// WithCanonicalRecoveryCleanupReadScope projects the requested bank's cleanup
// input inside the original journal read window. It grants no effect authority.
func (s *windowsCanonicalJournalRecoverySource) WithCanonicalRecoveryCleanupReadScope(ctx context.Context, side binaryadmission.WindowsCanonicalRecoverySideV1, purpose binaryadmission.WindowsCanonicalEntryRecoveryPurposeV2, body func(func(context.Context) error, binaryadmission.WindowsCanonicalEntryMembersRecoveryInputV2) error) error {
	if s == nil || body == nil || (side != binaryadmission.WindowsCanonicalRecoveryPriorV1 && side != binaryadmission.WindowsCanonicalRecoverySuccessorV1) ||
		(purpose != binaryadmission.WindowsCanonicalRecoveryCommittedForwardV2 && (purpose != binaryadmission.WindowsCanonicalRecoveryRollbackCleanupV2 || side != binaryadmission.WindowsCanonicalRecoverySuccessorV1)) {
		return ErrWindowsProductRecoveryIdentity
	}
	journal, path, raw := s.journal, s.path, s.raw
	pinned := func() error {
		// The source replaces its owned raw bytes and detached journal together
		// after successful persistence. Each installed raw generation is immutable.
		if s.path != path || len(raw) == 0 || len(s.raw) != len(raw) || &s.raw[0] != &raw[0] ||
			s.journal.EntryBundle != journal.EntryBundle || s.journal.Phase != journal.Phase ||
			s.journal.TransactionID != journal.TransactionID || s.purpose != windowsCanonicalRecoveryPurpose(purpose) {
			return ErrWindowsProductRecoveryIdentity
		}
		return nil
	}
	boundaryRead := func(ctx context.Context) error {
		if err := s.validateOriginalRecoveryState(ctx); err != nil {
			return err
		}
		return s.validateOriginalRecoveryBytes(ctx, path, raw)
	}
	return s.withCanonicalRecoveryReadScope(ctx, boundaryRead, pinned, func(check func(context.Context) error) error {
		if err := check(ctx); err != nil {
			return err
		}
		input, err := projectCurrentRecoveryJournal(journal, purpose)
		if err != nil {
			return err
		}
		if err = check(ctx); err != nil {
			return err
		}
		return body(check, input)
	})
}

func (s *windowsCanonicalJournalRecoverySource) withCanonicalRecoveryReadScope(ctx context.Context, boundaryRead func(context.Context) error, pinned func() error, body func(func(context.Context) error) error) error {
	if err := boundaryRead(ctx); err != nil {
		return err
	}
	if body == nil || s.journal.EntryBundle == nil {
		return ErrWindowsProductRecoveryIdentity
	}
	version, phase, purpose := s.journal.EntryBundle, s.journal.Phase, s.purpose
	var mu sync.Mutex
	active := true
	var checkErr error
	defer func() {
		mu.Lock()
		active = false
		mu.Unlock()
	}()
	state := func(useCtx context.Context) error {
		if err := s.validateOriginalRecoveryState(useCtx); err != nil {
			return err
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if s.req.Context == nil {
			return ErrWindowsProductRecoveryIdentity
		}
		if err := s.req.Context.Err(); err != nil {
			return err
		}
		if s.journal.EntryBundle != version || s.journal.Phase != phase || s.purpose != purpose {
			return ErrWindowsProductRecoveryIdentity
		}
		if pinned != nil {
			return pinned()
		}
		return nil
	}
	check := func(useCtx context.Context) error {
		mu.Lock()
		defer mu.Unlock()
		if !active {
			return ErrWindowsProductRecoveryIdentity
		}
		err := state(useCtx)
		checkErr = errors.Join(checkErr, err)
		return checkErr
	}
	bodyErr := body(check)
	mu.Lock()
	active = false
	stateErr := state(ctx)
	receivedCheckErr := checkErr
	mu.Unlock()
	closeErr := boundaryRead(ctx)
	if pinned != nil {
		stateErr = errors.Join(stateErr, state(ctx))
	}
	return errors.Join(bodyErr, receivedCheckErr, stateErr, closeErr)
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

func prepareCanonicalRecoverySource(req windowsProductRecoveryRequest, d windowsProductRecoveryDeps, path string, j windowsProductRecoveryJournalV3, purpose windowsCanonicalRecoveryPurpose) (result *windowsCanonicalJournalRecoverySource, retErr error) {
	if req.journalReadEvidence == nil {
		req.journalReadEvidence = &windowsProductJournalReadEvidence{}
	}
	defer func() { retErr = withWindowsProductJournalEvidence(retErr, req.journalReadEvidence) }()
	d.journal = windowsProductJournalBinding{ctx: req.Context, allowance: req.JournalAllowance, evidence: req.journalReadEvidence, phase: func() string { return string(j.Phase) }}
	d = withWindowsProductRecoveryDefaults(d)
	if j.EntryBundle != nil {
		return prepareCurrentCanonicalRecoverySource(req, d, path, j, purpose)
	}
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
	if err := persistWindowsProductRecoveryJournalV3(context.WithoutCancel(s.req.Context), s.req.JournalAllowance, s.path, next, s.deps.WriteJournal, s.deps.ReadJournal); err != nil {
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
		return fmt.Errorf("%w: legacy checkpoint has no durable payload directory identity; preserve the journal and payload directory for manual recovery", ErrWindowsProductRecoveryPending)
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

// Validation borrows the same original objects as rollback, but never moves or
// disposes them, settles runtime, or discards the retained task store.
func validateCanonicalEntryPendingRecovery(req windowsProductRecoveryRequest, d windowsProductRecoveryDeps, j windowsProductRecoveryJournalV3) (retErr error) {
	if j.EntryBundle != nil {
		return validateCurrentCanonicalEntryPendingRecovery(req, d, j)
	}
	source, err := prepareCanonicalRecoverySource(req, d, filepath.Join(req.StateDir, WindowsProductRecoveryFileLeafV3), j, canonicalRecoveryRollback)
	if err != nil {
		return err
	}
	defer func() { source.active = false }()
	defer func() {
		if source.directory != nil {
			retErr = errors.Join(retErr, source.directory.Close())
		}
	}()
	if err := source.acquireDirectory(req.Context); err != nil {
		return err
	}
	prior, err := binaryadmission.AcquireWindowsCanonicalEntryForRecoveryV1(req.Context, source, binaryadmission.WindowsCanonicalRecoveryPriorV1)
	if prior != nil {
		defer func() { retErr = errors.Join(retErr, prior.Close()) }()
	}
	if err != nil {
		return err
	}
	successor, err := binaryadmission.AcquireWindowsCanonicalEntryForRecoveryV1(req.Context, source, binaryadmission.WindowsCanonicalRecoverySuccessorV1)
	if successor != nil {
		defer func() { retErr = errors.Join(retErr, successor.Close()) }()
	}
	if err != nil {
		return err
	}
	if prior == nil || successor == nil {
		return ErrWindowsProductRecoveryIdentity
	}
	// Setup/canonicalize can fill missing members of an otherwise identical
	// partial set. They neither stop a runtime nor journal a runtime projection.
	if j.Mode == WindowsProductPairModeUpgrade {
		runtime, err := prior.RuntimeProduct(req.Context)
		if err != nil {
			return fmt.Errorf("%w: original prior runtime admission: %w", ErrWindowsProductRecoveryIdentity, err)
		}
		if expected := j.EntrySet.PriorRuntime; expected != nil {
			if expected.Present != (runtime != nil) {
				return ErrWindowsProductRecoveryIdentity
			}
			if runtime != nil {
				member := source.prior.Product
				if sameWindowsProductPath(expected.Path, j.EntrySet.CanonicalEntry.Target) {
					member = source.prior.Launcher
				}
				artifact := runtime.Artifact()
				originalPath := filepath.Join(filepath.Dir(j.EntrySet.CanonicalEntry.Target), filepath.FromSlash(member.SourceRelative))
				if !member.Present || !sameWindowsProductPath(artifact.Path, originalPath) || artifact.SHA256 != expected.SHA256 || artifact.Role != binaryadmission.WindowsArtifactRoleProduct || artifact.Subsystem != binaryadmission.WindowsGUISubsystem {
					return ErrWindowsProductRecoveryIdentity
				}
			}
		}
	}
	legacy := windowsProductPairRecoveryArtifact{Target: j.LegacyWindowless.Target, PriorPresent: j.LegacyWindowless.PriorPresent, PriorSHA256: j.LegacyWindowless.PriorSHA256, BaselineAsideBasenames: recoveryAsideBasenames(j.LegacyWindowless.BaselineAsides)}
	if _, err := inspectWindowsProductLegacyRecoveryArtifact(legacy, d); err != nil {
		return fmt.Errorf("%w: legacy inverse: %w", ErrWindowsProductRecoveryIdentity, err)
	}
	// Settled rollback has already restored its tasks, and its retained store
	// may have been discarded before the journal's final removal was interrupted.
	if j.Phase == WindowsProductRecoverySettledRollback {
		return source.ValidateOriginalRecovery(req.Context)
	}
	if d.ReopenEntrySetTasks == nil {
		return fmt.Errorf("%w: complete-set task inverse validation owner unavailable", ErrWindowsProductRecoveryIdentity)
	}
	targets, err := canonicalEntrySetJournalTargets(j)
	if err != nil {
		return err
	}
	tasks, err := d.ReopenEntrySetTasks(req.Context, j.Tasks, targets)
	if tasks != nil {
		defer func() { retErr = errors.Join(retErr, tasks.Close()) }()
	}
	if err != nil {
		return fmt.Errorf("%w: complete-set task inverse validation: %w", ErrWindowsProductRecoveryIdentity, err)
	}
	if tasks == nil {
		return fmt.Errorf("%w: complete-set task inverse validation returned no owner", ErrWindowsProductRecoveryIdentity)
	}
	return source.ValidateOriginalRecovery(req.Context)
}

func rollbackCanonicalEntryRecovery(req windowsProductRecoveryRequest, d windowsProductRecoveryDeps, path string, j windowsProductRecoveryJournalV3, tasks WindowsProductTaskTxn) (result *WindowsProductPairRecoverySettled, retErr error) {
	if j.EntryBundle != nil {
		return rollbackCurrentCanonicalEntryRecovery(req, d, path, j, tasks)
	}
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
	var view *binaryadmission.WindowsCanonicalEntryPriorRuntimeV1
	if j.Mode == WindowsProductPairModeUpgrade {
		view, err = prior.RuntimeProduct(req.Context)
		if err != nil {
			return nil, err
		}
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
			if err := persistWindowsProductRecoveryJournalV3(context.WithoutCancel(req.Context), req.JournalAllowance, path, j, d.WriteJournal, d.ReadJournal); err != nil {
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
	if err := persistWindowsProductRecoveryJournalV3(context.WithoutCancel(req.Context), req.JournalAllowance, path, j, d.WriteJournal, d.ReadJournal); err != nil {
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
	if j.EntryBundle != nil {
		targets, err := binaryadmission.CanonicalEntryTargetsV2(j.EntryBundle.CanonicalEntry.Target)
		if err != nil {
			return err
		}
		actual, err := d.AdmitEntryBundle(ctx, targets, j.EntryBundle.Expected)
		if err != nil {
			return err
		}
		if !reflect.DeepEqual(actual, j.EntryBundle.Expected) {
			return errors.New("current complete cohort recovery readback differs")
		}
		return nil
	}
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
	if j.EntryBundle != nil {
		if j.EntrySet != nil {
			return ErrWindowsProductRecoveryIdentity
		}
		targets, err := binaryadmission.CanonicalEntryTargetsV2(j.EntryBundle.CanonicalEntry.Target)
		if err != nil {
			return err
		}
		if req.CanonicalEntryPath != "" && !sameWindowsRecoveryPath(req.CanonicalEntryPath, targets.CanonicalEntryPath) || req.IdentityPath != "" && !sameWindowsRecoveryPath(req.IdentityPath, targets.IdentityPath) || !sameWindowsRecoveryPath(req.ProductPath, targets.CanonicalEntryPath) && !sameWindowsRecoveryPath(req.ProductPath, targets.ProductPath) {
			return ErrWindowsProductRecoveryIdentity
		}
		req.CanonicalEntryPath, req.ProductPath, req.IdentityPath = targets.CanonicalEntryPath, targets.ProductPath, targets.IdentityPath
		return nil
	}
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
	if j.EntryBundle != nil {
		return cleanupColdCurrentCanonicalOriginals(req, d, path, j, nil)
	}
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

func cloneCurrentRecoveryJournal(j windowsProductRecoveryJournalV3) (windowsProductRecoveryJournalV3, error) {
	raw, err := encodeReceivedWindowsProductRecoveryJournalV3(j)
	if err != nil {
		return windowsProductRecoveryJournalV3{}, err
	}
	return decodeWindowsProductRecoveryJournalV3(raw)
}

// After a successful persistCurrent, raw and journal are detached, matching
// snapshots. Decode the retained bytes to detach the next edit again.
func (s *windowsCanonicalJournalRecoverySource) clonePersistedCurrentJournal() (windowsProductRecoveryJournalV3, error) {
	return decodeWindowsProductRecoveryJournalV3(s.raw)
}

// These two callback boundaries lend detached data, never the accepted graph.
func (s *windowsCanonicalJournalRecoverySource) acquireCurrentSuccessorQuiescence(ctx context.Context, successor *binaryadmission.WindowsCanonicalEntryRecoverySetV2) (*windowsProductSuccessorQuiescence, error) {
	journal, err := cloneCurrentRecoveryJournal(s.journal)
	if err != nil {
		return nil, err
	}
	return s.req.AcquireSuccessorQuiescence(ctx, journal, s, successor)
}

func (s *windowsCanonicalJournalRecoverySource) recoverCurrentPriorSettlementOriginal(ctx context.Context, runtime *binaryadmission.WindowsCanonicalEntryPriorRuntimeV1) error {
	journal, err := cloneCurrentRecoveryJournal(s.journal)
	if err != nil {
		return err
	}
	return s.req.RecoverPriorSettlementOriginal(ctx, journal, runtime)
}

func prepareCurrentCanonicalRecoverySource(req windowsProductRecoveryRequest, d windowsProductRecoveryDeps, path string, j windowsProductRecoveryJournalV3, purpose windowsCanonicalRecoveryPurpose) (result *windowsCanonicalJournalRecoverySource, retErr error) {
	if req.journalReadEvidence == nil {
		req.journalReadEvidence = &windowsProductJournalReadEvidence{}
	}
	defer func() { retErr = withWindowsProductJournalEvidence(retErr, req.journalReadEvidence) }()
	d.journal = windowsProductJournalBinding{ctx: req.Context, allowance: req.JournalAllowance, evidence: req.journalReadEvidence, phase: func() string { return string(j.Phase) }}
	d = withWindowsProductRecoveryDefaults(d)
	if req.Context == nil || j.EntryBundle == nil || j.EntrySet != nil {
		return nil, ErrWindowsProductRecoveryIdentity
	}
	raw, err := d.ReadJournal(path)
	if err != nil {
		return nil, err
	}
	decoded, err := decodeWindowsProductRecoveryJournalV3(raw)
	if err != nil {
		return nil, err
	}
	left, err := encodeReceivedWindowsProductRecoveryJournalV3(decoded)
	if err != nil {
		return nil, err
	}
	right, err := encodeReceivedWindowsProductRecoveryJournalV3(j)
	if err != nil {
		return nil, err
	}
	if !bytes.Equal(left, right) {
		return nil, ErrWindowsProductRecoveryIdentity
	}
	s := &windowsCanonicalJournalRecoverySource{req: req, deps: d, journal: decoded, raw: append([]byte(nil), raw...), path: path, active: true, purpose: purpose, memberDirectorySnapshots: map[*binaryadmission.WindowsCanonicalEntryPayloadDirectoryV1]binaryadmission.WindowsCanonicalEntryMemberDirectorySnapshotV2{}}
	s.self = s
	if err := s.ValidateOriginalRecovery(req.Context); err != nil {
		s.active = false
		return nil, err
	}
	if purpose == canonicalRecoveryCommittedCleanup {
		if err := readRecoveryCanonicalEntrySet(req.Context, decoded, d); err != nil {
			s.active = false
			return nil, err
		}
	}
	return s, nil
}

func (s *windowsCanonicalJournalRecoverySource) WithOriginalMemberRecoveryInput(ctx context.Context, side binaryadmission.WindowsCanonicalRecoverySideV1, use func(binaryadmission.WindowsCanonicalEntryMembersRecoveryInputV2) error) error {
	if err := s.ValidateOriginalRecovery(ctx); err != nil {
		return err
	}
	if use == nil || s.journal.EntryBundle == nil || (string(side) != "prior" && string(side) != "successor") {
		return ErrWindowsProductRecoveryIdentity
	}
	input, err := projectCurrentRecoveryJournal(s.journal, binaryadmission.WindowsCanonicalEntryRecoveryPurposeV2(s.purpose))
	if err != nil {
		return err
	}
	// Exactly one synchronous callback receives a detached projection. Its row
	// order, unknown flags/statuses and nullable diagnostic strings stay intact.
	err = use(input)
	return errors.Join(err, s.ValidateOriginalRecovery(ctx))
}

func (s *windowsCanonicalJournalRecoverySource) OriginalMemberDirectories(ctx context.Context) ([]*binaryadmission.WindowsCanonicalEntryPayloadDirectoryV1, error) {
	if err := s.ValidateOriginalRecovery(ctx); err != nil {
		return nil, err
	}
	if s.journal.EntryBundle == nil {
		return nil, ErrWindowsProductRecoveryIdentity
	}
	return append([]*binaryadmission.WindowsCanonicalEntryPayloadDirectoryV1{}, s.memberDirectories...), nil
}

func (s *windowsCanonicalJournalRecoverySource) registerCurrentDirectories(owners []*binaryadmission.WindowsCanonicalEntryPayloadDirectoryV1) error {
	var causes []error
	for _, owner := range owners {
		if owner == nil {
			continue
		}
		known := false
		for _, prior := range s.memberDirectories {
			if prior == owner {
				known = true
				break
			}
		}
		if !known {
			s.memberDirectories = append(s.memberDirectories, owner)
		}
		original, err := owner.SnapshotForEntryMembersV2()
		causes = append(causes, err)
		// The original snapshot remains ordering data when validation fails.
		// It supplies no journal, borrow or disposal authority.
		if original.Schema != binaryadmission.WindowsCanonicalEntryMemberDirectorySchemaV2 || original.Root == "" || !currentEntryRelative(original.RelativePath) {
			causes = append(causes, fmt.Errorf("%w: current directory ordering metadata unavailable", ErrWindowsProductRecoveryIdentity))
			continue
		}
		if previous, ok := s.memberDirectorySnapshots[owner]; ok {
			if previous != original {
				causes = append(causes, ErrWindowsProductRecoveryIdentity)
			}
			continue
		}
		if s.memberDirectorySnapshots == nil {
			s.memberDirectorySnapshots = map[*binaryadmission.WindowsCanonicalEntryPayloadDirectoryV1]binaryadmission.WindowsCanonicalEntryMemberDirectorySnapshotV2{}
		}
		s.memberDirectorySnapshots[owner] = original
	}
	return errors.Join(causes...)
}

func (s *windowsCanonicalJournalRecoverySource) acquireCurrentSide(ctx context.Context, side binaryadmission.WindowsCanonicalRecoverySideV1) (*binaryadmission.WindowsCanonicalEntryRecoverySetV2, error) {
	owner, err := binaryadmission.AcquireWindowsCanonicalEntryForRecoveryV2(ctx, s, side)
	if err != nil {
		s.memberErrors = append(s.memberErrors, err)
	}
	if owner != nil {
		err = errors.Join(err, s.registerCurrentDirectories(owner.Directories()))
	} else if err == nil {
		err = errors.New("current original recovery returned no native owner")
	}
	return owner, err
}

func (s *windowsCanonicalJournalRecoverySource) persistCurrent(next windowsProductRecoveryJournalV3) error {
	return s.persistCurrentWithArchive(next, receivedRolledBackCurrentArchive{})
}

func (s *windowsCanonicalJournalRecoverySource) persistCurrentWithArchive(next windowsProductRecoveryJournalV3, verified receivedRolledBackCurrentArchive) error {
	if err := s.ValidateOriginalRecovery(context.WithoutCancel(s.req.Context)); err != nil {
		return err
	}
	if next.EntryBundle == nil || !reflect.DeepEqual(next.EntryBundle.Bundle.Retained, s.journal.EntryBundle.Bundle.Retained) {
		return fmt.Errorf("%w: original retained member witness changed", ErrWindowsProductRecoveryIdentity)
	}
	bound := s.journal.EntryBundle.CleanArchiveSHA256
	if next.EntryBundle.CleanArchiveSHA256 != bound {
		archive := verified.archive
		if bound != "" || !archive.matchesSource(s) || next.EntryBundle.CleanArchiveSHA256 != archive.digest() {
			return fmt.Errorf("%w: original clean archive binding changed", ErrWindowsProductRecoveryIdentity)
		}
	}
	owned, err := cloneCurrentRecoveryJournal(next)
	if err != nil {
		return err
	}
	raw, err := encodeReceivedWindowsProductRecoveryJournalV3(owned)
	if err != nil {
		return err
	}
	if err := persistEncodedWindowsProductRecoveryJournalV3(context.WithoutCancel(s.req.Context), s.req.JournalAllowance, s.path, owned, raw, s.deps.WriteJournal, s.deps.ReadJournal); err != nil {
		return err
	}
	s.journal = owned
	s.raw = append([]byte(nil), raw...)
	return nil
}

func (s *windowsCanonicalJournalRecoverySource) closeCurrentMembers(owners ...*binaryadmission.WindowsCanonicalEntryRecoverySetV2) error {
	var causes []error
	for _, owner := range owners {
		if owner == nil {
			continue
		}
		// Adopt any remaining directories before the first member close. A
		// repeated finalizer can run after those same directories have closed.
		if !owner.Disposition().CloseEntered {
			causes = append(causes, s.registerCurrentDirectories(owner.Directories()))
		}
		err := owner.Close()
		causes = append(causes, err)
		if err != nil {
			s.memberErrors = append(s.memberErrors, err)
		}
	}
	return errors.Join(causes...)
}

func (s *windowsCanonicalJournalRecoverySource) closeCurrentDirectories() error {
	var causes []error
	for _, owner := range orderedOriginalMemberDirectories(s.memberDirectories, s.memberDirectorySnapshots) {
		if owner != nil {
			err := owner.Close()
			causes = append(causes, err)
			if err != nil {
				s.memberErrors = append(s.memberErrors, err)
			}
		}
	}
	return errors.Join(causes...)
}

func currentRecoveryMember(ctx context.Context, owner *binaryadmission.WindowsCanonicalEntryRecoverySetV2, member string) (binaryadmission.WindowsCanonicalEntryMemberEvidenceV2, bool, error) {
	if owner == nil {
		return binaryadmission.WindowsCanonicalEntryMemberEvidenceV2{}, false, ErrWindowsProductRecoveryIdentity
	}
	members, err := owner.Members(ctx)
	if err != nil {
		return binaryadmission.WindowsCanonicalEntryMemberEvidenceV2{}, false, err
	}
	for _, value := range members {
		if value.Member == member {
			return value, true, nil
		}
	}
	return binaryadmission.WindowsCanonicalEntryMemberEvidenceV2{}, false, nil
}

type currentInverseStage string

const (
	currentInverseVerifyBeforeRollback currentInverseStage = "VERIFY_BEFORE_ROLLBACK_PENDING"
	currentInversePersistRollback      currentInverseStage = "PERSIST_ROLLBACK_PENDING"
	currentInverseVerifyAfterRollback  currentInverseStage = "VERIFY_AFTER_ROLLBACK_PENDING"
	currentInversePrepareMove          currentInverseStage = "PREPARE_MOVE"
	currentInverseVerifyBeforeIntent   currentInverseStage = "VERIFY_BEFORE_MOVE_INTENT"
	currentInversePersistIntent        currentInverseStage = "PERSIST_MOVE_INTENT"
	currentInverseVerifyAfterIntent    currentInverseStage = "VERIFY_AFTER_MOVE_INTENT"
	currentInverseNativeMove           currentInverseStage = "NATIVE_MOVE"
	currentInverseNativeReadback       currentInverseStage = "NATIVE_MOVE_READBACK"
	currentInverseReadResult           currentInverseStage = "RECEIVE_MOVE_RESULT"
	currentInversePersistResult        currentInverseStage = "PERSIST_MOVE_RESULT"
)

func isCurrentInverseStage(stage currentInverseStage) bool {
	switch stage {
	case currentInverseVerifyBeforeRollback, currentInversePersistRollback, currentInverseVerifyAfterRollback,
		currentInversePrepareMove, currentInverseVerifyBeforeIntent, currentInversePersistIntent, currentInverseVerifyAfterIntent,
		currentInverseNativeMove, currentInverseNativeReadback, currentInverseReadResult, currentInversePersistResult:
		return true
	default:
		return false
	}
}

type currentInverseRecoveryError struct {
	stage         currentInverseStage
	movesReceived int
	cause         error
}

func (e *currentInverseRecoveryError) Error() string { return e.cause.Error() }
func (e *currentInverseRecoveryError) Unwrap() error { return e.cause }

func (s *windowsCanonicalJournalRecoverySource) inverseFailure(stage currentInverseStage, cause error) error {
	return &currentInverseRecoveryError{stage: stage, movesReceived: s.inverseMovesReceived, cause: cause}
}

func (s *windowsCanonicalJournalRecoverySource) moveCurrentRecoveryOriginal(ctx context.Context, owner *binaryadmission.WindowsCanonicalEntryRecoverySetV2, prior bool, member, destination string) error {
	if err := s.ValidateOriginalRecovery(ctx); err != nil {
		return s.inverseFailure(currentInversePrepareMove, err)
	}
	observation, present, err := currentRecoveryMember(ctx, owner, member)
	if err != nil {
		return s.inverseFailure(currentInversePrepareMove, err)
	}
	if !present {
		return s.inverseFailure(currentInversePrepareMove, errors.New("current recovery move has no original member"))
	}
	plan := currentEntryMember(s.journal.EntryBundle, member)
	if plan == nil {
		return s.inverseFailure(currentInversePrepareMove, ErrWindowsProductRecoveryIdentity)
	}
	expected := plan.Staged
	if prior {
		expected = plan.Prior
	}
	if expected == nil || observation.NativeIdentity != expected.NativeIdentity || observation.File.SHA256 != expected.File.SHA256 || observation.Size != expected.Size || observation.Attributes != expected.Attributes {
		return s.inverseFailure(currentInversePrepareMove, ErrWindowsProductRecoveryIdentity)
	}
	next, err := cloneCurrentRecoveryJournal(s.journal)
	if err != nil {
		return s.inverseFailure(currentInversePrepareMove, err)
	}
	next.EntryBundle.Moves = append(next.EntryBundle.Moves, windowsCanonicalEntryMoveRecordV2{Member: member, Prior: prior, SourceRelative: observation.File.Path, DestinationRelative: destination})
	index := len(next.EntryBundle.Moves) - 1
	if err := s.persistCurrent(next); err != nil {
		return s.inverseFailure(currentInversePersistIntent, err)
	}
	if s.beforeCurrentInverse != nil {
		if err := s.beforeCurrentInverse(ctx); err != nil {
			return s.inverseFailure(currentInverseVerifyAfterIntent, err)
		}
	}
	actual, callErr := owner.MoveOriginal(ctx, member, destination)
	s.memberResults = append(s.memberResults, actual)
	s.memberErrors = append(s.memberErrors, callErr)
	next, cloneErr := s.clonePersistedCurrentJournal()
	if cloneErr != nil {
		return s.inverseFailure(currentInverseReadResult, errors.Join(callErr, actual.Error, actual.CloseError, cloneErr))
	}
	record := currentMoveRecord(actual, prior, observation.File.Path, destination)
	record.Member = member
	next.EntryBundle.Moves[index] = record
	received := canonicalEntryMoveResultError(actual, callErr, member, destination)
	if actual.SourceRelative != "" && actual.SourceRelative != observation.File.Path {
		received = errors.Join(received, ErrWindowsProductRecoveryIdentity)
	}
	persistErr := s.persistCurrent(next)
	if received != nil {
		stage := currentInverseNativeMove
		if actual.Applied {
			stage = currentInverseNativeReadback
		}
		received = s.inverseFailure(stage, received)
	}
	if persistErr != nil {
		persistErr = s.inverseFailure(currentInversePersistResult, persistErr)
	}
	if received == nil && persistErr == nil {
		s.inverseMovesReceived++
	}
	return errors.Join(received, persistErr)
}

func currentCleanupIndex(e *windowsCanonicalEntrySetJournalV2, side, member string) int {
	for i := range e.Cleanup {
		if e.Cleanup[i].Side == side && e.Cleanup[i].Member == member {
			return i
		}
	}
	return -1
}

func (s *windowsCanonicalJournalRecoverySource) cleanupCurrentSide(ctx context.Context, owner *binaryadmission.WindowsCanonicalEntryRecoverySetV2, prior bool) error {
	side := "unused-successor"
	nativeSide := binaryadmission.WindowsCanonicalRecoverySuccessorV1
	if prior {
		side = "prior"
		nativeSide = binaryadmission.WindowsCanonicalRecoveryPriorV1
	}
	purpose := binaryadmission.WindowsCanonicalEntryRecoveryPurposeV2(s.purpose)
	if purpose != binaryadmission.WindowsCanonicalRecoveryCommittedForwardV2 && purpose != binaryadmission.WindowsCanonicalRecoveryRollbackCleanupV2 {
		return ErrWindowsProductRecoveryIdentity
	}
	if prior && purpose == binaryadmission.WindowsCanonicalRecoveryRollbackCleanupV2 {
		return ErrWindowsProductRecoveryIdentity
	}
	plans := append([]windowsCanonicalEntryMemberJournalV2(nil), s.journal.EntryBundle.Bundle.Members...)
	for _, plan := range plans {
		expected := plan.Staged
		if prior {
			expected = plan.Prior
		}
		if expected == nil {
			continue
		}
		observation, present, err := owner.ObserveCleanupMember(ctx, nativeSide, purpose, *expected)
		if err != nil {
			return err
		}
		index := currentCleanupIndex(s.journal.EntryBundle, side, plan.Member)
		if !present {
			if index < 0 {
				return errors.New("missing current original has no durable cleanup intent")
			}
			row := s.journal.EntryBundle.Cleanup[index]
			if row.Purpose != purpose || row.TransactionID != s.journal.TransactionID || (row.State != "discard_intent" && row.State != "absence_received") {
				return ErrWindowsProductRecoveryIdentity
			}
			// The native owner received this exact purpose-bound
			// absence. No unavailable historical syscall/status is fabricated.
			if row.State != "absence_received" {
				next, err := cloneCurrentRecoveryJournal(s.journal)
				if err != nil {
					return err
				}
				next.EntryBundle.Cleanup[index].State = "absence_received"
				if err := s.persistCurrent(next); err != nil {
					return err
				}
			}
			continue
		}
		if observation.Member != plan.Member || observation.NativeIdentity != expected.NativeIdentity || observation.File.SHA256 != expected.File.SHA256 ||
			observation.Size != expected.Size || observation.Attributes != expected.Attributes {
			return ErrWindowsProductRecoveryIdentity
		}
		if observation.File.Path == plan.Member {
			if !prior && purpose == binaryadmission.WindowsCanonicalRecoveryCommittedForwardV2 {
				continue
			}
			return errors.New("cleanup original remains at a protected final name")
		}
		next, err := cloneCurrentRecoveryJournal(s.journal)
		if err != nil {
			return err
		}
		if index < 0 {
			next.EntryBundle.Cleanup = append(next.EntryBundle.Cleanup, windowsCanonicalEntryCleanupV2{TransactionID: next.TransactionID, Side: side, Member: plan.Member, SourceRelative: observation.File.Path, ExpectedSHA256: expected.File.SHA256, State: "retained", Purpose: purpose})
			index = len(next.EntryBundle.Cleanup) - 1
		} else {
			row := next.EntryBundle.Cleanup[index]
			if row.SourceRelative != observation.File.Path || row.Purpose != purpose || row.State == "absence_received" {
				return ErrWindowsProductRecoveryIdentity
			}
		}
		next.EntryBundle.Cleanup[index].State = "discard_intent"
		if err := s.persistCurrent(next); err != nil {
			return err
		}
		actual, callErr := owner.DiscardOriginal(ctx, plan.Member, observation.File.Path)
		s.memberResults = append(s.memberResults, actual)
		s.memberErrors = append(s.memberErrors, callErr)
		next, cloneErr := s.clonePersistedCurrentJournal()
		if cloneErr != nil {
			return errors.Join(callErr, actual.Error, actual.CloseError, cloneErr)
		}
		if actual.Applied && actual.Readback {
			next.EntryBundle.Cleanup[index].State = "absence_received"
		}
		if err := errors.Join(canonicalEntryDiscardResultError(actual, callErr, plan.Member, observation.File.Path), s.persistCurrent(next)); err != nil {
			return err
		}
	}
	return nil
}

func (s *windowsCanonicalJournalRecoverySource) cleanupCurrentMembers(ctx context.Context, prior, successor *binaryadmission.WindowsCanonicalEntryRecoverySetV2) error {
	purpose := binaryadmission.WindowsCanonicalEntryRecoveryPurposeV2(s.purpose)
	if err := errors.Join(prior.VerifyUnchanged(ctx), successor.VerifyUnchanged(ctx)); err != nil {
		return err
	}
	if purpose == binaryadmission.WindowsCanonicalRecoveryCommittedForwardV2 {
		if err := s.cleanupCurrentSide(ctx, prior, true); err != nil {
			return err
		}
	}
	if err := s.cleanupCurrentSide(ctx, successor, false); err != nil {
		return err
	}
	if err := errors.Join(prior.VerifyUnchanged(ctx), successor.VerifyUnchanged(ctx)); err != nil {
		return err
	}
	if purpose == binaryadmission.WindowsCanonicalRecoveryCommittedForwardV2 {
		if err := prior.VerifyCleanupAbsences(ctx, binaryadmission.WindowsCanonicalRecoveryPriorV1, purpose); err != nil {
			return err
		}
	}
	return successor.VerifyCleanupAbsences(ctx, binaryadmission.WindowsCanonicalRecoverySuccessorV1, purpose)
}

func (s *windowsCanonicalJournalRecoverySource) settleCurrentDirectories(ctx context.Context, dispose bool) error {
	var causes []error
	effectsHeld := false
	var closeErrors []error
	for _, owner := range orderedOriginalMemberDirectories(s.memberDirectories, s.memberDirectorySnapshots) {
		if owner == nil {
			continue
		}
		original, known := s.memberDirectorySnapshots[owner]
		if dispose && !effectsHeld && known && original.Created {
			index := -1
			for i := range s.journal.EntryBundle.Bundle.Directories {
				if s.journal.EntryBundle.Bundle.Directories[i].Original == original {
					index = i
					break
				}
			}
			var effectErr error
			if index < 0 {
				effectErr = ErrWindowsProductRecoveryIdentity
			} else if s.journal.EntryBundle.Bundle.Directories[index].State != "absence_received" {
				next, err := cloneCurrentRecoveryJournal(s.journal)
				effectErr = err
				if effectErr == nil {
					next.EntryBundle.Bundle.Directories[index].State = "dispose_intent"
					effectErr = s.persistCurrent(next)
				}
				if effectErr == nil {
					actual, callErr := owner.DisposeOriginalIfEmpty(ctx)
					s.memberResults = append(s.memberResults, actual)
					s.memberErrors = append(s.memberErrors, callErr)
					next, err := s.clonePersistedCurrentJournal()
					if err != nil {
						effectErr = errors.Join(callErr, actual.Error, actual.CloseError, err)
					} else {
						record := currentNativeResultRecord(actual)
						next.EntryBundle.Bundle.Directories[index].Disposition = &record
						if actual.Applied && actual.Readback {
							next.EntryBundle.Bundle.Directories[index].State = "absence_received"
						}
						effectErr = errors.Join(canonicalEntryDiscardResultError(actual, callErr, actual.Member, original.RelativePath), s.persistCurrent(next))
					}
				}
			}
			if effectErr != nil {
				causes = append(causes, effectErr)
				effectsHeld = true
			}
		}
		if err := owner.Close(); err != nil {
			causes = append(causes, err)
			closeErrors = append(closeErrors, err)
			s.memberErrors = append(s.memberErrors, err)
			effectsHeld = true
		}
	}
	if closeErr := errors.Join(closeErrors...); closeErr != nil {
		next, err := cloneCurrentRecoveryJournal(s.journal)
		if err == nil {
			rendered := closeErr.Error()
			next.EntryBundle.Bundle.Disposition.CloseError = &rendered
			next.EntryBundle.Bundle.Disposition.OriginalOwnerPending = true
			err = s.persistCurrent(next)
		}
		causes = append(causes, err)
	}
	return errors.Join(causes...)
}

func validateCurrentCanonicalEntryPendingRecovery(req windowsProductRecoveryRequest, d windowsProductRecoveryDeps, j windowsProductRecoveryJournalV3) (retErr error) {
	s, err := prepareCurrentCanonicalRecoverySource(req, d, filepath.Join(req.StateDir, WindowsProductRecoveryFileLeafV3), j, currentRollbackRecoveryPurpose(j))
	if err != nil {
		return err
	}
	var prior, successor *binaryadmission.WindowsCanonicalEntryRecoverySetV2
	defer func() {
		retErr = errors.Join(retErr, s.closeCurrentMembers(prior, successor), s.closeCurrentDirectories())
		s.active = false
	}()
	prior, err = s.acquireCurrentSide(req.Context, binaryadmission.WindowsCanonicalRecoveryPriorV1)
	if err != nil {
		return err
	}
	successor, err = s.acquireCurrentSide(req.Context, binaryadmission.WindowsCanonicalRecoverySuccessorV1)
	if err != nil {
		return err
	}
	view, err := prior.RuntimeProduct(req.Context)
	if err != nil {
		return err
	}
	expected := j.EntryBundle.PriorRuntime
	if expected != nil {
		if expected.Present != (view != nil) {
			return ErrWindowsProductRecoveryIdentity
		}
		if view != nil {
			artifact := view.Artifact()
			if artifact.SHA256 != expected.SHA256 || artifact.Role != binaryadmission.WindowsArtifactRoleProduct || artifact.Subsystem != binaryadmission.WindowsGUISubsystem {
				return ErrWindowsProductRecoveryIdentity
			}
			if err := view.ValidateOriginal(req.Context); err != nil {
				return err
			}
		}
	}
	legacy := windowsProductPairRecoveryArtifact{Target: j.LegacyWindowless.Target, PriorPresent: j.LegacyWindowless.PriorPresent, PriorSHA256: j.LegacyWindowless.PriorSHA256, BaselineAsideBasenames: recoveryAsideBasenames(j.LegacyWindowless.BaselineAsides)}
	if _, err := inspectWindowsProductLegacyRecoveryArtifact(legacy, d); err != nil {
		return err
	}
	if j.Phase == WindowsProductRecoverySettledRollback {
		return s.ValidateOriginalRecovery(req.Context)
	}
	if d.ReopenEntryBundleTasks == nil {
		return errors.Join(ErrWindowsProductRecoveryIdentity, errors.New("current task recovery projection unavailable"))
	}
	targets, err := binaryadmission.CanonicalEntryTargetsV2(j.EntryBundle.CanonicalEntry.Target)
	if err != nil {
		return err
	}
	tasks, err := d.ReopenEntryBundleTasks(req.Context, j.Tasks, targets)
	if tasks != nil {
		defer func() { retErr = errors.Join(retErr, tasks.Close()) }()
	}
	if err != nil {
		return err
	}
	if tasks == nil {
		return ErrWindowsProductRecoveryIdentity
	}
	return s.ValidateOriginalRecovery(req.Context)
}

type rolledBackCurrentEntryEvidence struct {
	TransactionID string
	JournalSHA256 string
	Successors    []binaryadmission.WindowsCanonicalEntryMemberEvidenceV2
}

// inspectRolledBackCurrentEntry receives detached evidence under the caller's
// Product upgrade fence. It never grants archive, journal or replacement custody.
func inspectRolledBackCurrentEntry(req windowsProductRecoveryRequest) (result rolledBackCurrentEntryEvidence, retErr error) {
	retErr = withRolledBackCurrentEntry(req, false, func(s *windowsCanonicalJournalRecoverySource, _, _ *binaryadmission.WindowsCanonicalEntryRecoverySetV2, store *windowsProductPairTaskStore, members []binaryadmission.WindowsCanonicalEntryMemberEvidenceV2) error {
		for _, backup := range s.journal.Tasks.Backups {
			if _, err := store.Load(req.Context, backup.Ref); err != nil {
				return fmt.Errorf("verify retained Task backup: %w", err)
			}
		}
		result = rolledBackCurrentEntryEvidence{TransactionID: s.journal.TransactionID, JournalSHA256: fmt.Sprintf("%x", sha256.Sum256(s.raw)), Successors: members}
		return s.ValidateOriginalRecovery(req.Context)
	})
	if retErr != nil {
		return rolledBackCurrentEntryEvidence{}, retErr
	}
	return result, nil
}

// withRolledBackCurrentEntry lends the exact old owner synchronously under the
// caller's already-acquired Product upgrade fence.
func withRolledBackCurrentEntry(req windowsProductRecoveryRequest, allowSettled bool, use func(*windowsCanonicalJournalRecoverySource, *binaryadmission.WindowsCanonicalEntryRecoverySetV2, *binaryadmission.WindowsCanonicalEntryRecoverySetV2, *windowsProductPairTaskStore, []binaryadmission.WindowsCanonicalEntryMemberEvidenceV2) error) (retErr error) {
	ctx := req.Context
	if err := validateWindowsProductJournalOperation(ctx, req.JournalAllowance); err != nil {
		return err
	}
	if !cleanAbsolutePath(req.StateDir) || !cleanAbsolutePath(req.CanonicalEntryPath) ||
		!cleanAbsolutePath(req.ProductPath) || !cleanAbsolutePath(req.LegacyWindowlessPath) {
		return ErrWindowsProductRecoveryIdentity
	}
	if err := requireWindowsProductWithoutBackendHold(ctx, req.StateDir); err != nil {
		return err
	}
	d := boundWindowsProductRecoveryDefaults(req)
	if _, err := d.readJournalContext(ctx, filepath.Join(req.StateDir, WindowsProductPairRecoveryFileLeaf)); !windowsProductJournalCleanAbsence(err) {
		return errors.Join(ErrWindowsProductRecoveryIdentity, err, errors.New("clean install requires an absent legacy recovery journal"))
	}
	path := filepath.Join(req.StateDir, WindowsProductRecoveryFileLeafV3)
	raw, err := d.readJournalContext(ctx, path)
	if err != nil {
		return err
	}
	j, err := decodeWindowsProductRecoveryJournalV3(raw)
	if err != nil {
		return err
	}
	settled := j.Phase == WindowsProductRecoverySettledRollback
	expectedState := "rolled-back"
	if allowSettled && settled {
		expectedState = "settled"
	}
	if !windowsProductPriorRecoveryPending(j) || settled && !allowSettled ||
		j.EntryBundle == nil || j.EntrySet != nil || j.EntryBundle.Bundle.Disposition.State != expectedState ||
		!j.EntryBundle.Bundle.Disposition.Transferred || j.EntryBundle.PriorRuntime == nil || !j.EntryBundle.PriorRuntime.Present {
		return errors.Join(ErrWindowsProductRecoveryPending, errors.New("clean install requires the exact rolled-back current Product journal and prior runtime"))
	}
	if err := resolveCanonicalRecoveryRequest(&req, j); err != nil {
		return err
	}
	if !sameWindowsRecoveryPath(j.Product.Target, req.ProductPath) || !sameWindowsRecoveryPath(j.LegacyWindowless.Target, req.LegacyWindowlessPath) ||
		!sameWindowsRecoveryPath(j.Receipts.V3.Path, filepath.Join(req.StateDir, UpgradeReceiptSchemaV3+".json")) ||
		!sameWindowsRecoveryPath(j.Receipts.V2.Path, filepath.Join(req.StateDir, UpgradeReceiptSchemaV2+".json")) {
		return ErrWindowsProductRecoveryIdentity
	}
	s, err := prepareCurrentCanonicalRecoverySource(req, d, path, j, canonicalRecoveryRollbackCleanup)
	if err != nil {
		return err
	}
	var prior, successor *binaryadmission.WindowsCanonicalEntryRecoverySetV2
	defer func() {
		retErr = errors.Join(retErr, s.closeCurrentMembers(prior, successor), s.closeCurrentDirectories())
		s.active = false
	}()
	if !bytes.Equal(raw, s.raw) {
		return ErrWindowsProductRecoveryIdentity
	}
	prior, err = s.acquireCurrentSide(ctx, binaryadmission.WindowsCanonicalRecoveryPriorV1)
	if err != nil {
		return err
	}
	successor, err = s.acquireCurrentSide(ctx, binaryadmission.WindowsCanonicalRecoverySuccessorV1)
	if err != nil {
		return err
	}
	runtime, err := prior.RuntimeProduct(ctx)
	if err != nil || runtime == nil {
		return errors.Join(ErrWindowsProductRecoveryIdentity, err)
	}
	artifact := runtime.Artifact()
	if want := j.EntryBundle.PriorRuntime; !sameWindowsRecoveryPath(artifact.Path, want.Path) || artifact.SHA256 != want.SHA256 {
		return errors.Join(ErrWindowsProductRecoveryIdentity, errors.New("restored prior runtime differs from original evidence"))
	}
	legacy := windowsProductPairRecoveryArtifact{Target: j.LegacyWindowless.Target, PriorPresent: j.LegacyWindowless.PriorPresent, PriorSHA256: j.LegacyWindowless.PriorSHA256}
	if err := verifyRecoveryArtifactPrior(legacy, pairRecoveryDepsFromProduct(s.deps)); err != nil {
		return fmt.Errorf("verify restored legacy executable: %w", err)
	}
	for _, receipt := range []windowsProductPairRecoveryReceipt{j.Receipts.V3, j.Receipts.V2} {
		actual, err := s.deps.ReadReceipt(receipt.Path)
		if receipt.PriorPresent {
			if err != nil || !bytes.Equal(actual, receipt.PriorBytes) {
				return errors.Join(ErrWindowsProductRecoveryIdentity, err, errors.New("restored receipt differs from original bytes"))
			}
		} else if !errors.Is(err, os.ErrNotExist) {
			return errors.Join(ErrWindowsProductRecoveryIdentity, err, errors.New("expected original receipt absence"))
		}
	}
	var store *windowsProductPairTaskStore
	if !settled {
		store, err = reopenWindowsProductPairTaskStore(req.StateDir, j.Tasks, true)
		if err != nil {
			return err
		}
	}
	members, err := successor.Members(ctx)
	if err != nil {
		return err
	}
	// Members verifies the whole bank, including the retained canonical files.
	// Only an issued staged original can enter the archive and discard plan.
	present := make(map[string]binaryadmission.WindowsCanonicalEntryMemberEvidenceV2, len(members))
	for _, member := range members {
		present[member.Member] = member
	}
	var changed []binaryadmission.WindowsCanonicalEntryMemberEvidenceV2
	for _, row := range s.journal.EntryBundle.Bundle.Members {
		if row.Staged == nil {
			continue
		}
		member, ok := present[row.Member]
		if !ok {
			// Acquisition checked all issued locations against this very journal
			// and admits absence only for an exact cleanup intent or readback.
			index := currentCleanupIndex(s.journal.EntryBundle, "unused-successor", row.Member)
			if index < 0 {
				return ErrWindowsProductRecoveryIdentity
			}
			cleanup := s.journal.EntryBundle.Cleanup[index]
			if cleanup.Purpose != binaryadmission.WindowsCanonicalRecoveryRollbackCleanupV2 ||
				cleanup.State != "discard_intent" && cleanup.State != "absence_received" {
				return ErrWindowsProductRecoveryIdentity
			}
			continue
		}
		if member.File.Path == row.Member || member.File.Path != row.Staged.File.Path &&
			(row.SuccessorAsideRelative == nil || member.File.Path != *row.SuccessorAsideRelative) {
			return errors.Join(ErrWindowsProductRecoveryIdentity, errors.New("rolled-back successor is outside its issued stage or aside"))
		}
		want := *row.Staged
		want.File.Path = member.File.Path // The issued stage and aside share the exact original parent.
		if member != want {
			return ErrWindowsProductRecoveryIdentity
		}
		changed = append(changed, member)
	}
	if err := s.ValidateOriginalRecovery(ctx); err != nil {
		return err
	}
	return use(s, prior, successor, store, changed)
}
func currentRollbackRecoveryPurpose(j windowsProductRecoveryJournalV3) windowsCanonicalRecoveryPurpose {
	if j.EntryBundle != nil {
		switch j.EntryBundle.Bundle.Disposition.State {
		case "rolled-back", "cleanup-pending", "settled":
			return canonicalRecoveryRollbackCleanup
		}
	}
	return canonicalRecoveryRollback
}

func cleanupColdCurrentCanonicalOriginals(req windowsProductRecoveryRequest, d windowsProductRecoveryDeps, path string, j *windowsProductRecoveryJournalV3, finish func(*windowsCanonicalJournalRecoverySource) error) (retErr error) {
	s, err := prepareCurrentCanonicalRecoverySource(req, d, path, *j, canonicalRecoveryCommittedCleanup)
	if err != nil {
		return err
	}
	var prior, successor *binaryadmission.WindowsCanonicalEntryRecoverySetV2
	defer func() {
		retErr = errors.Join(retErr, s.closeCurrentMembers(prior, successor), s.closeCurrentDirectories())
		*j = s.journal
		s.active = false
	}()
	prior, err = s.acquireCurrentSide(req.Context, binaryadmission.WindowsCanonicalRecoveryPriorV1)
	if err != nil {
		return err
	}
	successor, err = s.acquireCurrentSide(req.Context, binaryadmission.WindowsCanonicalRecoverySuccessorV1)
	if err != nil {
		return err
	}
	ctx := context.WithoutCancel(req.Context)
	if err := s.cleanupCurrentMembers(ctx, prior, successor); err != nil {
		return errors.Join(ErrWindowsProductCleanupPending, err)
	}
	if err := s.closeCurrentMembers(prior, successor); err != nil {
		return err
	}
	if err := s.settleCurrentDirectories(ctx, false); err != nil {
		return err
	}
	if finish != nil {
		return finish(s)
	}
	return nil
}

// The settled path already completed member cleanup. It still must receive
// the exact retained witness before reconciling the receipt or retiring state.
func verifySettledCurrentRetention(req windowsProductRecoveryRequest, d windowsProductRecoveryDeps, path string, j windowsProductRecoveryJournalV3) (retErr error) {
	s, err := prepareCurrentCanonicalRecoverySource(req, d, path, j, canonicalRecoveryCommittedCleanup)
	if err != nil {
		return err
	}
	successor, err := s.acquireCurrentSide(req.Context, binaryadmission.WindowsCanonicalRecoverySuccessorV1)
	defer func() {
		retErr = errors.Join(retErr, s.closeCurrentMembers(successor), s.closeCurrentDirectories())
		s.active = false
	}()
	if err != nil {
		return err
	}
	return s.ValidateOriginalRecovery(req.Context)
}

// This is a detached data projection only. The private fenced source validates
// original bytes/phase/purpose before calling it; it cannot issue a native owner.
func projectCurrentRecoveryJournal(j windowsProductRecoveryJournalV3, purpose binaryadmission.WindowsCanonicalEntryRecoveryPurposeV2) (binaryadmission.WindowsCanonicalEntryMembersRecoveryInputV2, error) {
	var result binaryadmission.WindowsCanonicalEntryMembersRecoveryInputV2
	if j.EntryBundle == nil || j.EntrySet != nil {
		return result, ErrWindowsProductRecoveryIdentity
	}
	switch purpose {
	case binaryadmission.WindowsCanonicalRecoveryRollbackV2, binaryadmission.WindowsCanonicalRecoveryCommittedForwardV2, binaryadmission.WindowsCanonicalRecoveryRollbackCleanupV2:
	default:
		return result, ErrWindowsProductRecoveryIdentity
	}
	e := j.EntryBundle
	targets, err := binaryadmission.CanonicalEntryTargetsV2(e.CanonicalEntry.Target)
	if err != nil {
		return result, err
	}
	text := func(value *string) *string {
		if value == nil {
			return nil
		}
		copy := *value
		return &copy
	}
	result = binaryadmission.WindowsCanonicalEntryMembersRecoveryInputV2{Targets: targets, BeforeKind: e.Bundle.BeforeKind, Expected: e.Expected, TransactionID: j.TransactionID, Purpose: purpose}
	if retained := e.Bundle.Retained; retained != nil {
		result.Retained = &binaryadmission.WindowsCanonicalEntryRetainedMembersV2{
			MembersSHA256: retained.MembersSHA256,
			Singletons:    slices.Clone(retained.Singletons),
		}
	}
	result.Expected.IdentityBytes = append([]byte(nil), e.Expected.IdentityBytes...)
	result.Expected.RuntimePayloadBytes = append([]byte(nil), e.Expected.RuntimePayloadBytes...)
	result.Expected.BundleInventory = append([]binaryadmission.RuntimeObjectV1(nil), e.Expected.BundleInventory...)
	for i := range result.Expected.BundleInventory {
		if value := result.Expected.BundleInventory[i].Size; value != nil {
			copy := *value
			result.Expected.BundleInventory[i].Size = &copy
		}
	}
	if e.Bundle.Members != nil {
		result.Members = make([]binaryadmission.WindowsCanonicalEntryMemberRecoveryPlanV2, len(e.Bundle.Members))
	}
	for i, row := range e.Bundle.Members {
		result.Members[i] = row
		if row.Prior != nil {
			value := *row.Prior
			result.Members[i].Prior = &value
		}
		if row.Staged != nil {
			value := *row.Staged
			result.Members[i].Staged = &value
		}
		if row.PriorAbsence != nil {
			value := *row.PriorAbsence
			result.Members[i].PriorAbsence = &value
		}
		result.Members[i].PriorAsideRelative = text(row.PriorAsideRelative)
		result.Members[i].SuccessorAsideRelative = text(row.SuccessorAsideRelative)
	}
	if e.Moves != nil {
		result.Moves = make([]binaryadmission.WindowsCanonicalEntryMemberMoveRecordV2, len(e.Moves))
	}
	for i, row := range e.Moves {
		result.Moves[i] = row
		result.Moves[i].Error = text(row.Error)
		result.Moves[i].CloseError = text(row.CloseError)
	}
	if e.Bundle.Directories != nil {
		result.Directories = make([]binaryadmission.WindowsCanonicalEntryMemberDirectoryRecordV2, len(e.Bundle.Directories))
	}
	for i, row := range e.Bundle.Directories {
		result.Directories[i] = row
		if row.Disposition != nil {
			copy := *row.Disposition
			copy.Error = text(copy.Error)
			copy.CloseError = text(copy.CloseError)
			result.Directories[i].Disposition = &copy
		}
	}
	if e.Cleanup != nil {
		result.Cleanup = append([]binaryadmission.WindowsCanonicalEntryMemberCleanupRecordV2{}, e.Cleanup...)
	}
	return result, nil
}

// Cold current recovery retains the same source/native union and receives
// absence/reservations before the first inverse intent or native file effect.
func rollbackCurrentCanonicalEntryRecovery(req windowsProductRecoveryRequest, d windowsProductRecoveryDeps, path string, j windowsProductRecoveryJournalV3, tasks WindowsProductTaskTxn) (result *WindowsProductPairRecoverySettled, retErr error) {
	if j.EntryBundle != nil && j.EntryBundle.CleanArchiveSHA256 != "" {
		return nil, fmt.Errorf("%w: resume the bound clean archive with setup --clean", ErrWindowsProductRecoveryPending)
	}
	s, err := prepareCurrentCanonicalRecoverySource(req, d, path, j, currentRollbackRecoveryPurpose(j))
	if err != nil {
		return nil, err
	}
	var prior, successor *binaryadmission.WindowsCanonicalEntryRecoverySetV2
	var q *windowsProductSuccessorQuiescence
	defer func() {
		if q != nil {
			retErr = errors.Join(retErr, q.Close())
		}
		retErr = errors.Join(retErr, s.closeCurrentMembers(prior, successor), s.closeCurrentDirectories())
		s.active = false
	}()
	prior, err = s.acquireCurrentSide(req.Context, binaryadmission.WindowsCanonicalRecoveryPriorV1)
	if err != nil {
		return nil, err
	}
	successor, err = s.acquireCurrentSide(req.Context, binaryadmission.WindowsCanonicalRecoverySuccessorV1)
	if err != nil {
		return nil, err
	}
	if prior == nil || successor == nil {
		return nil, ErrWindowsProductRecoveryIdentity
	}
	// A durable completed inverse may resume its selected cleanup without
	// inventing a new absence lease or rerunning predecessor startup.
	if s.purpose != canonicalRecoveryRollbackCleanup {
		if req.AcquireSuccessorQuiescence == nil {
			return nil, fmt.Errorf("%w: current successor quiescence factory unavailable", ErrWindowsProductRecoveryPending)
		}
		q, err = s.acquireCurrentSuccessorQuiescence(req.Context, successor)
		if err != nil || q == nil {
			return nil, errors.Join(ErrWindowsProductRecoveryPending, err, errors.New("current successor quiescence unavailable"))
		}
		if err = q.VerifyForInverse(req.Context); err != nil {
			return nil, errors.Join(ErrWindowsProductRecoveryPending, s.inverseFailure(currentInverseVerifyBeforeRollback, err))
		}
		s.beforeCurrentInverse = q.VerifyForInverse
		next, err := cloneCurrentRecoveryJournal(s.journal)
		if err != nil {
			return nil, s.inverseFailure(currentInversePersistRollback, err)
		}
		next.EntryBundle.Bundle.Disposition.State = "rollback-pending"
		if err = s.persistCurrent(next); err != nil {
			return nil, s.inverseFailure(currentInversePersistRollback, err)
		}
		if err = q.VerifyForInverse(req.Context); err != nil {
			return nil, errors.Join(ErrWindowsProductRecoveryPending, s.inverseFailure(currentInverseVerifyAfterRollback, err))
		}
		keys := currentPromotionOrder(s.journal.EntryBundle, false)
		for i := len(keys) - 1; i >= 0; i-- {
			key := keys[i]
			actual, present, err := currentRecoveryMember(req.Context, successor, key)
			if err != nil {
				return nil, s.inverseFailure(currentInversePrepareMove, err)
			}
			if !present {
				return nil, s.inverseFailure(currentInversePrepareMove, errors.New("rollback-needed successor original is missing"))
			}
			if actual.File.Path != key {
				continue
			}
			row := currentEntryMember(s.journal.EntryBundle, key)
			if row == nil || row.SuccessorAsideRelative == nil {
				return nil, s.inverseFailure(currentInversePrepareMove, ErrWindowsProductRecoveryIdentity)
			}
			if err = q.VerifyForInverse(req.Context); err != nil {
				return nil, errors.Join(ErrWindowsProductRecoveryPending, s.inverseFailure(currentInverseVerifyBeforeIntent, err))
			}
			if err = s.moveCurrentRecoveryOriginal(req.Context, successor, false, key, *row.SuccessorAsideRelative); err != nil {
				return nil, errors.Join(ErrWindowsProductRecoveryPending, err)
			}
		}
		keys = currentPromotionOrder(s.journal.EntryBundle, true)
		for i := len(keys) - 1; i >= 0; i-- {
			key := keys[i]
			actual, present, err := currentRecoveryMember(req.Context, prior, key)
			if err != nil {
				return nil, s.inverseFailure(currentInversePrepareMove, err)
			}
			if !present {
				return nil, s.inverseFailure(currentInversePrepareMove, errors.New("rollback-needed prior original is missing"))
			}
			if actual.File.Path == key {
				continue
			}
			if err = q.VerifyForInverse(req.Context); err != nil {
				return nil, errors.Join(ErrWindowsProductRecoveryPending, s.inverseFailure(currentInverseVerifyBeforeIntent, err))
			}
			if err = s.moveCurrentRecoveryOriginal(req.Context, prior, true, key, key); err != nil {
				return nil, errors.Join(ErrWindowsProductRecoveryPending, err)
			}
		}
		// This transaction never authored runtime intent/settings/OFF bytes:
		// the no-change intent case still releases the actual held lease. The
		// original prior-runtime owner receives any selected runtime duty later.
		if err = q.WithIntentInverse(req.Context, func(*api.StateFileObservedLease) error { return nil }); err != nil {
			return nil, errors.Join(ErrWindowsProductRecoveryPending, err)
		}
		s.beforeCurrentInverse = nil
		if tasks != nil {
			if err = tasks.RestoreImport(); err != nil {
				return nil, errors.Join(ErrWindowsProductPairRecoveryRollback, err)
			}
		} else if s.journal.Phase != WindowsProductRecoverySettledRollback {
			return nil, ErrWindowsProductRecoveryIdentity
		}
		legacy := windowsProductPairRecoveryArtifact{Target: s.journal.LegacyWindowless.Target, PriorPresent: s.journal.LegacyWindowless.PriorPresent, PriorSHA256: s.journal.LegacyWindowless.PriorSHA256, BaselineAsideBasenames: recoveryAsideBasenames(s.journal.LegacyWindowless.BaselineAsides)}
		legacyState, err := inspectWindowsProductLegacyRecoveryArtifact(legacy, d)
		if err != nil {
			return nil, err
		}
		if err = restoreWindowsProductLegacyRecoveryArtifact(legacy, legacyState, d); err != nil {
			return nil, err
		}
		for _, receipt := range []windowsProductPairRecoveryReceipt{s.journal.Receipts.V3, s.journal.Receipts.V2} {
			if err = restoreWindowsProductPairRecoveryReceipt(receipt, pairRecoveryDepsFromProduct(d)); err != nil {
				return nil, err
			}
		}
		if err = q.Close(); err != nil {
			return nil, errors.Join(ErrWindowsProductRecoveryPending, err)
		}
		if req.RecoverPriorSettlementOriginal == nil {
			return nil, fmt.Errorf("%w: original prior runtime settlement callback unavailable", ErrWindowsProductRecoveryPending)
		}
		view, err := prior.RuntimeProduct(req.Context)
		if err != nil {
			return nil, err
		}
		if err = s.ValidateOriginalRecovery(req.Context); err != nil {
			return nil, err
		}
		if err = s.recoverCurrentPriorSettlementOriginal(req.Context, view); err != nil {
			return nil, err
		}
		next, err = cloneCurrentRecoveryJournal(s.journal)
		if err != nil {
			return nil, err
		}
		next.EntryBundle.Bundle.Disposition.State = "rolled-back"
		if err = s.persistCurrent(next); err != nil {
			return nil, err
		}
		s.purpose = canonicalRecoveryRollbackCleanup
	}
	ctx := context.WithoutCancel(req.Context)
	if err = s.cleanupCurrentMembers(ctx, prior, successor); err != nil {
		return nil, errors.Join(ErrWindowsProductCleanupPending, err)
	}
	return s.finishCurrentRollback(ctx, prior, successor, tasks)
}

func (s *windowsCanonicalJournalRecoverySource) finishCurrentRollback(ctx context.Context, prior, successor *binaryadmission.WindowsCanonicalEntryRecoverySetV2, tasks WindowsProductTaskTxn) (*WindowsProductPairRecoverySettled, error) {
	if err := s.closeCurrentMembers(prior, successor); err != nil {
		return nil, err
	}
	if err := s.settleCurrentDirectories(ctx, true); err != nil {
		return nil, err
	}
	next, err := cloneCurrentRecoveryJournal(s.journal)
	if err != nil {
		return nil, err
	}
	next.Phase = WindowsProductRecoverySettledRollback
	next.TaskSetSHA256 = ""
	next.EntryBundle.Bundle.Disposition.State = "settled"
	next.EntryBundle.Bundle.Disposition.OriginalOwnerPending = false
	next.EntryBundle.Bundle.Disposition.DispositionPending = false
	if err = s.persistCurrent(next); err != nil {
		return nil, err
	}
	return s.retireCurrentRollback(tasks)
}

func (s *windowsCanonicalJournalRecoverySource) retireCurrentRollback(tasks WindowsProductTaskTxn) (*WindowsProductPairRecoverySettled, error) {
	if s.journal.Phase != WindowsProductRecoverySettledRollback {
		return nil, ErrWindowsProductRecoveryIdentity
	}
	if tasks != nil {
		if err := tasks.DiscardRecoveryStore(); err != nil {
			return nil, err
		}
	} else if err := s.deps.DiscardTaskStore(s.journal.Tasks); err != nil {
		return nil, err
	}
	if err := s.deps.RemoveJournal(s.path); err != nil {
		return nil, err
	}
	return &WindowsProductPairRecoverySettled{TransactionID: s.journal.TransactionID, Outcome: WindowsProductPairRecoverySettledRollback}, nil
}

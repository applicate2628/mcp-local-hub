//go:build windows

package cli

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"reflect"

	"mcp-local-hub/internal/api"
	"mcp-local-hub/internal/binaryadmission"
)

var errCleanArchiveExists = errors.New("original clean install archive already exists")

type rolledBackCurrentArchive struct {
	source        *windowsCanonicalJournalRecoverySource
	path          string
	journalSHA256 [sha256.Size]byte
	archiveSHA256 [sha256.Size]byte
	members       []binaryadmission.WindowsCanonicalEntryMemberEvidenceV2
}

// writeRolledBackCurrentArchive creates a private, never-replaced archive. Its
// returned material still requires an independently received disk readback.
func (s *windowsCanonicalJournalRecoverySource) writeRolledBackCurrentArchive(ctx context.Context, successor *binaryadmission.WindowsCanonicalEntryRecoverySetV2, store *windowsProductPairTaskStore, members []binaryadmission.WindowsCanonicalEntryMemberEvidenceV2) (rolledBackCurrentArchive, error) {
	if err := s.ValidateOriginalRecovery(ctx); err != nil {
		return rolledBackCurrentArchive{}, err
	}
	if s.purpose != canonicalRecoveryRollbackCleanup || s.journal.EntryBundle.Bundle.Disposition.State != "rolled-back" ||
		s.journal.EntryBundle.CleanArchiveSHA256 != "" {
		return rolledBackCurrentArchive{}, ErrWindowsProductRecoveryIdentity
	}
	path := filepath.Join(s.req.StateDir, "windows-product-clean-"+s.journal.TransactionID+".tar")
	hash := sha256.New()
	created, err := api.SecureCreateOwnerOnlyFileStream(ctx, path, func(out io.Writer) (retErr error) {
		archive := tar.NewWriter(io.MultiWriter(out, hash))
		defer func() { retErr = errors.Join(retErr, archive.Close()) }()
		data := func(name string, body []byte) error {
			if err := archive.WriteHeader(&tar.Header{Name: name, Mode: 0600, Size: int64(len(body))}); err != nil {
				return err
			}
			_, err := archive.Write(body)
			return err
		}
		if err := data(WindowsProductRecoveryFileLeafV3, s.raw); err != nil {
			return err
		}
		for _, member := range members {
			row := currentEntryMember(s.journal.EntryBundle, member.Member)
			if row == nil || row.Staged == nil || member.File.Path == member.Member {
				return ErrWindowsProductRecoveryIdentity
			}
			if err := archive.WriteHeader(&tar.Header{Name: "successors/" + member.Member, Mode: 0600, Size: member.Size}); err != nil {
				return err
			}
			actual, present, err := successor.CopyRollbackCleanupOriginal(ctx, *row.Staged, archive)
			if err != nil || !present || actual != member {
				return errors.Join(ErrWindowsProductRecoveryIdentity, err)
			}
		}
		written := make(map[string]bool, len(s.journal.Tasks.Backups))
		for _, backup := range s.journal.Tasks.Backups {
			if written[backup.Ref] {
				continue
			}
			raw, err := store.Load(ctx, backup.Ref)
			if err != nil {
				return err
			}
			if err := data("tasks/"+backup.Ref, raw); err != nil {
				return err
			}
			written[backup.Ref] = true
		}
		return s.ValidateOriginalRecovery(ctx)
	})
	if err != nil {
		return rolledBackCurrentArchive{}, err
	}
	if !created {
		return rolledBackCurrentArchive{}, errCleanArchiveExists
	}
	a := rolledBackCurrentArchive{source: s, path: path, journalSHA256: sha256.Sum256(s.raw), members: members}
	hash.Sum(a.archiveSHA256[:0])
	return a, nil
}

type receivedRolledBackCurrentArchive struct{ archive rolledBackCurrentArchive }

func (a rolledBackCurrentArchive) matchesSource(source *windowsCanonicalJournalRecoverySource) bool {
	return a.source == source && a.path != "" && a.journalSHA256 == sha256.Sum256(source.raw)
}

func (a rolledBackCurrentArchive) digest() string { return fmt.Sprintf("%x", a.archiveSHA256) }

func receiveRolledBackCurrentArchive(ctx context.Context, namespace *api.WindowsProductStateNamespace, archive rolledBackCurrentArchive) (receivedRolledBackCurrentArchive, error) {
	var received [sha256.Size]byte
	digest := sha256.New()
	if err := namespace.WithBoundFileReader(ctx, filepath.Base(archive.path), func(r io.Reader) error {
		_, err := io.Copy(digest, r)
		return err
	}); err != nil {
		return receivedRolledBackCurrentArchive{}, err
	}
	digest.Sum(received[:0])
	if received != archive.archiveSHA256 {
		return receivedRolledBackCurrentArchive{}, errors.Join(ErrWindowsProductRecoveryIdentity, errors.New("clean install archive readback differs"))
	}
	return receivedRolledBackCurrentArchive{archive}, nil
}

func (s *windowsCanonicalJournalRecoverySource) receiveExistingRolledBackArchive(ctx context.Context, namespace *api.WindowsProductStateNamespace, members []binaryadmission.WindowsCanonicalEntryMemberEvidenceV2) (receivedRolledBackCurrentArchive, error) {
	path := filepath.Join(s.req.StateDir, "windows-product-clean-"+s.journal.TransactionID+".tar")
	digest := sha256.New()
	err := namespace.WithBoundFileReader(ctx, filepath.Base(path), func(body io.Reader) error {
		body = io.TeeReader(body, digest)
		tarReader := tar.NewReader(body)
		h, err := tarReader.Next()
		limit := s.req.JournalAllowance.maxEncodedBytes
		if err != nil || h.Name != WindowsProductRecoveryFileLeafV3 || !cleanArchiveFile(h) || h.Size > limit {
			return errors.Join(ErrWindowsProductRecoveryIdentity, err)
		}
		oldRaw, err := io.ReadAll(tarReader)
		if err != nil || int64(len(oldRaw)) != h.Size {
			return errors.Join(ErrWindowsProductRecoveryIdentity, err)
		}
		original, err := decodeWindowsProductRecoveryJournalV3(oldRaw)
		if err != nil || !cleanArchiveJournalContinues(original, s.journal, bytes.Equal(oldRaw, s.raw)) {
			return errors.Join(ErrWindowsProductRecoveryIdentity, err, errors.New("archived V3 is not the original rollback checkpoint"))
		}
		type archiveOriginal struct {
			digest   string
			size     int64
			required bool
		}
		expected := make(map[string]archiveOriginal, len(original.EntryBundle.Bundle.Members)+len(original.Tasks.Backups))
		for _, row := range original.EntryBundle.Bundle.Members {
			if row.Staged == nil {
				continue
			}
			index := currentCleanupIndex(original.EntryBundle, "unused-successor", row.Member)
			required := index < 0
			if index >= 0 {
				state := original.EntryBundle.Cleanup[index].State
				if state == "absence_received" {
					continue
				}
				required = state != "discard_intent"
			}
			expected["successors/"+row.Member] = archiveOriginal{row.Staged.File.SHA256, row.Staged.Size, required}
		}
		for _, member := range members {
			key := "successors/" + member.Member
			e, ok := expected[key]
			if !ok {
				return ErrWindowsProductRecoveryIdentity
			}
			e.required = true
			expected[key] = e
		}
		for _, backup := range original.Tasks.Backups {
			expected["tasks/"+backup.Ref] = archiveOriginal{backup.SHA256, -1, true}
		}
		for {
			h, err := tarReader.Next()
			if errors.Is(err, io.EOF) {
				break
			}
			if err != nil {
				return err
			}
			fact, ok := expected[h.Name]
			if !ok || !cleanArchiveFile(h) || (fact.size >= 0 && h.Size != fact.size) {
				return errors.Join(ErrWindowsProductRecoveryIdentity, errors.New("clean archive contains an unissued, duplicate or inexact entry"))
			}
			hash := sha256.New()
			n, err := io.Copy(hash, tarReader)
			if err != nil || n != h.Size || fmt.Sprintf("%x", hash.Sum(nil)) != fact.digest {
				return errors.Join(ErrWindowsProductRecoveryIdentity, err, errors.New("clean archive bytes differ from the original evidence"))
			}
			delete(expected, h.Name)
		}
		for _, entry := range expected {
			if entry.required {
				return errors.Join(ErrWindowsProductRecoveryIdentity, errors.New("clean archive omits required original material"))
			}
		}
		if n, err := io.Copy(io.Discard, body); err != nil || n != 0 {
			return errors.Join(ErrWindowsProductRecoveryIdentity, err, errors.New("clean archive has trailing material"))
		}
		return nil
	})
	if err != nil {
		return receivedRolledBackCurrentArchive{}, err
	}
	if err := s.ValidateOriginalRecovery(ctx); err != nil {
		return receivedRolledBackCurrentArchive{}, err
	}
	archive := rolledBackCurrentArchive{source: s, path: path, journalSHA256: sha256.Sum256(s.raw), members: members}
	digest.Sum(archive.archiveSHA256[:0])
	if bound := s.journal.EntryBundle.CleanArchiveSHA256; bound != "" && bound != archive.digest() {
		return receivedRolledBackCurrentArchive{}, errors.Join(ErrWindowsProductRecoveryIdentity, errors.New("bound clean install archive digest differs"))
	}
	return receivedRolledBackCurrentArchive{archive}, nil
}

func cleanArchiveFile(h *tar.Header) bool {
	return h.Typeflag == tar.TypeReg && h.Mode == 0600 && h.Size >= 0 && h.Linkname == ""
}

// The archived rollback checkpoint keeps every original, Task and receipt
// binding. The only admissible changes are monotonic cleanup observations and
// the existing settled-rollback terminal.
func cleanArchiveJournalContinues(original, current windowsProductRecoveryJournalV3, sameRaw bool) bool {
	if original.EntryBundle == nil || current.EntryBundle == nil || original.EntryBundle.Bundle.Disposition.State != "rolled-back" ||
		original.EntryBundle.CleanArchiveSHA256 != "" || !windowsProductPriorRecoveryPending(original) ||
		original.Phase == WindowsProductRecoverySettledRollback {
		return false
	}
	if current.EntryBundle.CleanArchiveSHA256 == "" {
		return sameRaw
	}
	oldArm, currentArm := *original.EntryBundle, *current.EntryBundle
	currentArm.CleanArchiveSHA256 = ""
	oldCleanup, cleanup := oldArm.Cleanup, currentArm.Cleanup
	oldDirectories, directories := oldArm.Bundle.Directories, currentArm.Bundle.Directories
	if len(oldCleanup) > len(cleanup) || len(oldDirectories) != len(directories) {
		return false
	}
	if current.Phase == WindowsProductRecoverySettledRollback {
		if currentArm.Bundle.Disposition.State != "settled" || currentArm.Bundle.Disposition.OriginalOwnerPending ||
			currentArm.Bundle.Disposition.DispositionPending || currentArm.Bundle.Disposition.CloseError != nil {
			return false
		}
		original.Phase, original.TaskSetSHA256 = current.Phase, current.TaskSetSHA256
		oldArm.Bundle.Disposition = currentArm.Bundle.Disposition
	}
	oldArm.Cleanup, currentArm.Cleanup = nil, nil
	oldArm.Bundle.Directories, currentArm.Bundle.Directories = nil, nil
	original.EntryBundle, current.EntryBundle = &oldArm, &currentArm
	if !reflect.DeepEqual(original, current) {
		return false
	}
	for i, old := range oldCleanup {
		now := cleanup[i]
		if !cleanArchiveCleanupAdvances(old.State, now.State) {
			return false
		}
		old.State = now.State
		if old != now {
			return false
		}
	}
	for _, newRow := range cleanup[len(oldCleanup):] {
		if newRow.State != "discard_intent" && newRow.State != "absence_received" {
			return false
		}
	}
	for i, old := range oldDirectories {
		now := directories[i]
		if old.Original != now.Original || !reflect.DeepEqual(old, now) && (!old.Original.Created || !cleanArchiveDirectoryAdvances(old.State, now.State)) {
			return false
		}
	}
	return true
}

func cleanArchiveCleanupAdvances(old, now string) bool {
	return old == now || old == "retained" && (now == "discard_intent" || now == "absence_received") ||
		old == "discard_intent" && now == "absence_received"
}

func cleanArchiveDirectoryAdvances(old, now string) bool {
	return old == "created_identity_received" && (now == "dispose_intent" || now == "absence_received") ||
		old == "dispose_intent" && now == "absence_received"
}

// discardArchivedCurrentOriginals uses the still-live old V3 owner. It enters
// no native discard before one durable write admits every covered original.
func (s *windowsCanonicalJournalRecoverySource) discardArchivedCurrentOriginals(ctx context.Context, prior, successor *binaryadmission.WindowsCanonicalEntryRecoverySetV2, verified receivedRolledBackCurrentArchive) error {
	archive := verified.archive
	if !archive.matchesSource(s) {
		return ErrWindowsProductRecoveryIdentity
	}
	if err := s.ValidateOriginalRecovery(ctx); err != nil {
		return err
	}
	if err := errors.Join(prior.VerifyUnchanged(ctx), successor.VerifyUnchanged(ctx)); err != nil {
		return err
	}
	next, err := cloneCurrentRecoveryJournal(s.journal)
	if err != nil {
		return err
	}
	next.EntryBundle.CleanArchiveSHA256 = archive.digest()
	const side = "unused-successor"
	const nativeSide = binaryadmission.WindowsCanonicalRecoverySuccessorV1
	const purpose = binaryadmission.WindowsCanonicalRecoveryRollbackCleanupV2
	present := make(map[string]binaryadmission.WindowsCanonicalEntryMemberEvidenceV2, len(archive.members))
	for _, member := range archive.members {
		present[member.Member] = member
	}
	for _, plan := range s.journal.EntryBundle.Bundle.Members {
		if plan.Staged == nil {
			continue
		}
		member, covered := present[plan.Member]
		index := currentCleanupIndex(next.EntryBundle, side, plan.Member)
		if !covered {
			if _, found, err := successor.ObserveCleanupMember(ctx, nativeSide, purpose, *plan.Staged); err != nil || found || index < 0 {
				return errors.Join(ErrWindowsProductRecoveryIdentity, err)
			}
			next.EntryBundle.Cleanup[index].State = "absence_received"
			continue
		}
		if index < 0 {
			next.EntryBundle.Cleanup = append(next.EntryBundle.Cleanup, windowsCanonicalEntryCleanupV2{
				TransactionID: next.TransactionID, Side: side, Member: member.Member, SourceRelative: member.File.Path,
				ExpectedSHA256: member.File.SHA256, State: "discard_intent", Purpose: purpose,
			})
		} else {
			row := &next.EntryBundle.Cleanup[index]
			if row.SourceRelative != member.File.Path || row.State == "absence_received" {
				return ErrWindowsProductRecoveryIdentity
			}
			row.State = "discard_intent"
		}
	}
	if err := s.persistCurrentWithArchive(next, verified); err != nil {
		return err
	}
	for _, member := range archive.members {
		result, callErr := successor.DiscardOriginal(ctx, member.Member, member.File.Path)
		s.memberResults = append(s.memberResults, result)
		s.memberErrors = append(s.memberErrors, callErr)
		if result.Applied && result.Readback {
			index := currentCleanupIndex(next.EntryBundle, side, member.Member)
			next.EntryBundle.Cleanup[index].State = "absence_received"
		}
		if err := canonicalEntryDiscardResultError(result, callErr, member.Member, member.File.Path); err != nil {
			// The old V3 still carries exact intents for retry if readback fails.
			return errors.Join(err, s.persistCurrent(next))
		}
	}
	if err := s.persistCurrent(next); err != nil {
		return err
	}
	if err := errors.Join(prior.VerifyUnchanged(ctx), successor.VerifyUnchanged(ctx)); err != nil {
		return err
	}
	return successor.VerifyCleanupAbsences(ctx, nativeSide, purpose)
}

// retireRolledBackForCleanInstall runs while the setup command holds the
// ordinary Product upgrade fence. The old V3 owns every old cleanup effect.
func retireRolledBackForCleanInstall(req windowsProductRecoveryRequest, namespace *api.WindowsProductStateNamespace) (retErr error) {
	ctx := req.Context
	if req.AcquireSuccessorQuiescence == nil {
		return ErrWindowsProductRecoveryIdentity
	}
	if err := validateWindowsProductJournalOperation(ctx, req.JournalAllowance); err != nil {
		return err
	}
	d := boundWindowsProductRecoveryDefaults(req)
	_, oldV3 := d.readJournalContext(ctx, filepath.Join(req.StateDir, WindowsProductRecoveryFileLeafV3))
	if windowsProductJournalCleanAbsence(oldV3) {
		_, oldLegacy := d.readJournalContext(ctx, filepath.Join(req.StateDir, WindowsProductPairRecoveryFileLeaf))
		if !windowsProductJournalCleanAbsence(oldLegacy) {
			return errors.Join(ErrWindowsProductRecoveryPending, oldLegacy, errors.New("clean install requires an absent legacy recovery journal"))
		}
		// An earlier explicit clean invocation may have safely retired the old
		// transaction. The existing lifecycle still requires original absence.
		return nil
	}
	if oldV3 != nil {
		return oldV3
	}
	if namespace == nil {
		var err error
		namespace, err = api.OpenWindowsProductStateNamespace(ctx, req.StateDir)
		if namespace != nil {
			defer api.ReleaseAndJoin(&retErr, namespace.Close, "close clean archive namespace")
		}
		if err != nil {
			return err
		}
	}
	req = wireWindowsProductRecoveryRequest(req)
	return withRolledBackCurrentEntry(req, true, func(s *windowsCanonicalJournalRecoverySource, prior, successor *binaryadmission.WindowsCanonicalEntryRecoverySetV2, store *windowsProductPairTaskStore, members []binaryadmission.WindowsCanonicalEntryMemberEvidenceV2) (retErr error) {
		quiescence, err := req.AcquireSuccessorQuiescence(ctx, s.journal, s, successor)
		if quiescence != nil {
			defer api.ReleaseAndJoin(&retErr, quiescence.Close, "release old clean-install reservations")
		}
		if err != nil || quiescence == nil {
			return errors.Join(ErrWindowsProductRecoveryPending, err)
		}
		if s.journal.Phase == WindowsProductRecoverySettledRollback {
			if _, err := s.receiveExistingRolledBackArchive(ctx, namespace, members); err != nil {
				return err
			}
			if err := quiescence.VerifyForInverse(ctx); err != nil {
				return err
			}
			if err := successor.VerifyCleanupAbsences(ctx, binaryadmission.WindowsCanonicalRecoverySuccessorV1, binaryadmission.WindowsCanonicalRecoveryRollbackCleanupV2); err != nil {
				return err
			}
			if err := s.closeCurrentMembers(prior, successor); err != nil {
				return err
			}
			if err := s.settleCurrentDirectories(ctx, false); err != nil {
				return err
			}
			if _, err := s.retireCurrentRollback(nil); err != nil {
				return err
			}
			return verifyCleanRetirement(ctx, req, s.deps)
		}
		targets, err := binaryadmission.CanonicalEntryTargetsV2(s.journal.EntryBundle.CanonicalEntry.Target)
		if err != nil {
			return err
		}
		tasks, err := s.deps.ReopenEntryBundleTasks(ctx, s.journal.Tasks, targets)
		if tasks != nil {
			defer api.ReleaseAndJoin(&retErr, tasks.Close, "close restored Task owner")
		}
		restored, ok := tasks.(interface{ VerifyRestored() error })
		if err != nil || !ok {
			return errors.Join(ErrWindowsProductRecoveryIdentity, err)
		}
		if err := restored.VerifyRestored(); err != nil {
			return err
		}
		if err := quiescence.VerifyForInverse(ctx); err != nil {
			return err
		}
		var received receivedRolledBackCurrentArchive
		if s.journal.EntryBundle.CleanArchiveSHA256 != "" {
			received, err = s.receiveExistingRolledBackArchive(ctx, namespace, members)
		} else {
			archive, createErr := s.writeRolledBackCurrentArchive(ctx, successor, store, members)
			switch {
			case errors.Is(createErr, errCleanArchiveExists):
				received, err = s.receiveExistingRolledBackArchive(ctx, namespace, members)
			case createErr != nil:
				return createErr
			default:
				received, err = receiveRolledBackCurrentArchive(ctx, namespace, archive)
			}
		}
		if err != nil {
			return err
		}
		if err := quiescence.VerifyForInverse(ctx); err != nil {
			return err
		}
		if err := s.discardArchivedCurrentOriginals(ctx, prior, successor, received); err != nil {
			return err
		}
		if err := quiescence.VerifyForInverse(ctx); err != nil {
			return err
		}
		if err := restored.VerifyRestored(); err != nil {
			return err
		}
		if _, err := s.finishCurrentRollback(context.WithoutCancel(ctx), prior, successor, tasks); err != nil {
			return err
		}
		return verifyCleanRetirement(ctx, req, s.deps)
	})
}

func verifyCleanRetirement(ctx context.Context, req windowsProductRecoveryRequest, d windowsProductRecoveryDeps) error {
	for _, name := range []string{WindowsProductRecoveryFileLeafV3, WindowsProductPairRecoveryFileLeaf} {
		if _, err := d.readJournalContext(ctx, filepath.Join(req.StateDir, name)); !windowsProductJournalCleanAbsence(err) {
			return errors.Join(ErrWindowsProductRecoveryPending, err, fmt.Errorf("clean install journal retirement was not received for %s", name))
		}
	}
	return nil
}

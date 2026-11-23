//go:build windows

package cli

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"reflect"
	"slices"
	"sort"
	"strings"
	"testing"

	"golang.org/x/sys/windows"

	"mcp-local-hub/internal/api"
	"mcp-local-hub/internal/api/apitest"
	"mcp-local-hub/internal/binaryadmission"
)

type v2TxnFixtureTasks struct {
	root                        string
	currentXML, priorXML        string
	exports, rewrites, restores int
	closes, discards            int
}

func (f *v2TxnFixtureTasks) InventoryExport() error {
	f.exports++
	f.priorXML = f.currentXML
	return nil
}

func (f *v2TxnFixtureTasks) RecoveryDescriptor() (WindowsProductPairTaskRecovery, error) {
	return WindowsProductPairTaskRecovery{StoreRoot: f.root, Backups: []WindowsProductPairTaskBackup{}}, nil
}

func (f *v2TxnFixtureTasks) RewriteCommand() error {
	f.rewrites++
	f.currentXML = "<Task>new fixture command</Task>"
	return nil
}

func (f *v2TxnFixtureTasks) VerifyRuntimeSHA256() (string, error) {
	return v2TxnFixtureDigest([]byte(f.currentXML)), nil
}

func (f *v2TxnFixtureTasks) RestoreImport() error {
	f.restores++
	f.currentXML = f.priorXML
	return nil
}

func (f *v2TxnFixtureTasks) Close() error {
	f.closes++
	return nil
}

func (f *v2TxnFixtureTasks) DiscardRecoveryStore() error {
	f.discards++
	return os.Remove(f.root)
}

func v2TxnFixtureDigest(body []byte) string {
	sum := sha256.Sum256(body)
	return hex.EncodeToString(sum[:])
}

func v2TxnFixtureAbsent(path string) error {
	if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("expected fixture path absent: %s: %v", filepath.Base(path), err)
	}
	return nil
}

func v2TxnRequireOrdinaryDirectory(t *testing.T, path string) {
	t.Helper()
	if !filepath.IsAbs(path) || filepath.Clean(path) != path {
		t.Fatal("fixture requires an exact absolute private directory")
	}
	name, err := windows.UTF16PtrFromString(path)
	if err != nil {
		t.Fatal(err)
	}
	attrs, err := windows.GetFileAttributes(name)
	if err != nil || attrs&windows.FILE_ATTRIBUTE_DIRECTORY == 0 || attrs&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0 {
		t.Fatal("fixture requires an ordinary private directory", err)
	}
}

func v2TxnFixtureInput(t *testing.T) (binaryadmission.WindowsCanonicalEntrySetTargetsV2, binaryadmission.WindowsCanonicalEntrySetSnapshotV2, binaryadmission.WindowsCanonicalEntryBuildBindingV1) {
	t.Helper()
	path, supplied := os.LookupEnv("MCPHUB_V2_TXN_FIXTURE")
	pin, pinned := os.LookupEnv("MCPHUB_V2_TXN_FIXTURE_SHA256")
	if !supplied && !pinned {
		t.Skip("requires separately retained private V4 transaction fixture")
	}
	private := os.Getenv("MCPHUB_V2_TXN_PRIVATE_TEMP")
	exportRoot := os.Getenv("MCPHUB_V2_TXN_EXPORT_ROOT")
	v2TxnRequireOrdinaryDirectory(t, private)
	v2TxnRequireOrdinaryDirectory(t, exportRoot)
	tempPrefix := strings.ToLower(private) + string(filepath.Separator)
	exportPrefix := strings.ToLower(exportRoot) + string(filepath.Separator)
	if strings.HasPrefix(tempPrefix, exportPrefix) || strings.HasPrefix(exportPrefix, tempPrefix) ||
		private != os.TempDir() || private != os.Getenv("TEMP") || private != os.Getenv("TMP") ||
		private != os.Getenv("TMPDIR") || private != os.Getenv("GOTMPDIR") {
		t.Fatal("consumer requires aligned private TEMP values separate from the retained export")
	}
	cohortRoot := filepath.Join(exportRoot, "v2-producttxn-fixture")
	v2TxnRequireOrdinaryDirectory(t, cohortRoot)
	if path != filepath.Join(cohortRoot, "snapshot.json") || !validLowerHexSHA256(pin) {
		t.Fatal("fixture requires the exact retained exporter snapshot and its lowercase SHA-256")
	}
	if info, err := os.Lstat(path); err != nil || !info.Mode().IsRegular() {
		t.Fatal("retained fixture snapshot is not an ordinary file", err)
	}
	raw := mustReadFile(t, path)
	if v2TxnFixtureDigest(raw) != pin {
		t.Fatal("fixture snapshot differs from its supplied digest")
	}
	var snapshot binaryadmission.WindowsCanonicalEntrySetSnapshotV2
	if err := json.Unmarshal(raw, &snapshot); err != nil {
		t.Fatal(err)
	}
	targets, err := binaryadmission.CanonicalEntryTargetsV2(filepath.Join(filepath.Dir(path), "cohort", binaryadmission.WindowsCanonicalEntryRelativePathV1))
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.CanonicalEntry.Path != targets.CanonicalEntryPath || snapshot.Product.Path != targets.ProductPath || snapshot.Identity.Path != targets.IdentityPath {
		t.Fatal("fixture snapshot is not confined to the manifest's cohort")
	}
	// Every actual V2 move persists and rereads the whole journal. Admit the small
	// synthetic descriptor fixture only, comfortably above its few dozen rows.
	if len(snapshot.BundleInventory) == 0 || len(snapshot.BundleInventory) > 64 {
		t.Fatal("expected the small synthetic runtime cohort")
	}
	binding, err := binaryadmission.NewWindowsCanonicalEntryBuildBindingV1(snapshot.BuildID, snapshot.CanonicalEntry.SHA256, snapshot.ReverseProjection)
	if err != nil {
		t.Fatal(err)
	}
	return targets, snapshot, binding
}

// Copy only the immutable snapshot member plan while its reader holds the
// source. The copy has the same bytes and distinct Windows file identities.
func v2TxnSeedPrior(t *testing.T, src, dst binaryadmission.WindowsCanonicalEntrySetTargetsV2, snapshot binaryadmission.WindowsCanonicalEntrySetSnapshotV2) map[string]binaryadmission.WindowsCanonicalEntryFileIdentityV2 {
	t.Helper()
	originals := make(map[string]binaryadmission.WindowsCanonicalEntryFileIdentityV2)
	copyMember := func(source, target, digest string) {
		body := mustReadFile(t, source)
		if v2TxnFixtureDigest(body) != digest {
			t.Fatal("original fixture member differs from admitted bytes")
		}
		if err := os.MkdirAll(filepath.Dir(target), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(target, body, 0600); err != nil {
			t.Fatal(err)
		}
		originals[target] = v2TxnFixtureIdentity(t, target)
	}
	copyMember(src.CanonicalEntryPath, dst.CanonicalEntryPath, snapshot.CanonicalEntry.SHA256)
	for _, object := range snapshot.BundleInventory {
		if object.Attributes != 0 {
			t.Fatal("fixture must use ordinary, unadorned objects")
		}
		rel := filepath.FromSlash(object.Path)
		target := filepath.Join(dst.BundlePath, rel)
		switch object.Kind {
		case "directory":
			if err := os.MkdirAll(target, 0700); err != nil {
				t.Fatal(err)
			}
		case "file":
			copyMember(filepath.Join(src.BundlePath, rel), target, object.SHA256)
		default:
			t.Fatal("unexpected admitted fixture member kind")
		}
	}
	return originals
}

func v2TxnFixtureIdentity(t *testing.T, path string) binaryadmission.WindowsCanonicalEntryFileIdentityV2 {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	var info windows.ByHandleFileInformation
	observeErr := windows.GetFileInformationByHandle(windows.Handle(f.Fd()), &info)
	if err := errors.Join(observeErr, f.Close()); err != nil {
		t.Fatal(err)
	}
	if info.NumberOfLinks != 1 || info.FileAttributes&(windows.FILE_ATTRIBUTE_REPARSE_POINT|windows.FILE_ATTRIBUTE_DIRECTORY) != 0 {
		t.Fatal("expected an ordinary, single-link original")
	}
	return binaryadmission.WindowsCanonicalEntryFileIdentityV2{
		VolumeSerialNumber: info.VolumeSerialNumber,
		FileIndexHigh:      info.FileIndexHigh,
		FileIndexLow:       info.FileIndexLow,
		CreationTime:       uint64(info.CreationTime.HighDateTime)<<32 | uint64(info.CreationTime.LowDateTime),
	}
}

func v2TxnAssertFixtureRoot(t *testing.T, dir string, targets binaryadmission.WindowsCanonicalEntrySetTargetsV2) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]bool{
		filepath.Base(targets.CanonicalEntryPath): false,
		filepath.Base(targets.BundlePath):         true,
	}
	if len(entries) != len(want) {
		t.Fatal("private Product root retained unplanned member, stage or aside")
	}
	for _, entry := range entries {
		isDir, expected := want[entry.Name()]
		if !expected || entry.IsDir() != isDir {
			t.Fatal("unplanned private Product root entry", entry.Name())
		}
	}
}

func v2TxnReadFixtureJournal(path string) (windowsProductRecoveryJournalV3, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return windowsProductRecoveryJournalV3{}, err
	}
	journal, err := decodeWindowsProductRecoveryJournalV3(raw)
	if err == nil && (journal.EntryBundle == nil || journal.EntrySet != nil || journal.EntryBundle.TransactionID != journal.TransactionID) {
		err = errors.New("missing transaction-bound V2 journal arm")
	}
	return journal, err
}

func v2TxnPromotionNames(journal windowsProductRecoveryJournalV3) ([]string, error) {
	moves := make([]string, 0, len(journal.EntryBundle.Bundle.Members))
	for _, move := range journal.EntryBundle.Moves {
		if move.Prior {
			continue
		}
		if move.Member != move.DestinationRelative || !move.Attempted || !move.NativeStatusKnown || !move.Applied || !move.Readback || move.Error != nil {
			return nil, errors.New("unreceived or misdirected native V2 promotion")
		}
		moves = append(moves, move.Member)
	}
	return moves, nil
}

func v2TxnAssertPersistedJournalClone(t *testing.T, raw []byte) {
	t.Helper()

	// Preserve the valid V2 fixture while exercising both representations of
	// the replacement rune that the journal reader accepts.
	raw = bytes.Replace(raw, []byte("\"close_error\": null"), []byte("\"close_error\": \"fixture \xff \\ufffd\""), 1)
	raw = append([]byte(" \t"), raw...)
	accepted, err := decodeWindowsProductRecoveryJournalV3(raw)
	if err != nil {
		t.Fatal(err)
	}
	normalized := accepted.EntryBundle.Bundle.Disposition.CloseError
	if normalized == nil || *normalized != "fixture \ufffd \ufffd" {
		t.Fatal("fixture did not exercise diagnostic normalization")
	}

	source := windowsCanonicalJournalRecoverySource{journal: accepted, raw: raw}
	want, err := cloneCurrentRecoveryJournal(accepted)
	if err != nil {
		t.Fatal(err)
	}
	got, err := source.clonePersistedCurrentJournal()
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Fatal("persisted clone differs from the encode/decode clone", err)
	}
	wantBytes, err := encodeWindowsProductRecoveryJournalV3(want)
	if err != nil {
		t.Fatal(err)
	}
	gotBytes, err := encodeWindowsProductRecoveryJournalV3(got)
	if err != nil || !bytes.Equal(gotBytes, wantBytes) || bytes.Equal(gotBytes, raw) {
		t.Fatal("persisted clone changed the canonical encoding of the admitted input", err)
	}

	*got.EntryBundle.Bundle.Disposition.CloseError = "changed"
	got.EntryBundle.Expected.IdentityBytes[0] ^= 0xff
	again, err := source.clonePersistedCurrentJournal()
	if err != nil || !reflect.DeepEqual(again, want) || !reflect.DeepEqual(source.journal, want) {
		t.Fatal("caller edits escaped the detached journal clone", err)
	}
}

type v2TxnCleanupReadBoundaryProbe struct {
	*windowsCanonicalJournalRecoverySource
	afterSelected func() error
}

func (s *v2TxnCleanupReadBoundaryProbe) WithCanonicalRecoveryCleanupReadScope(ctx context.Context, side binaryadmission.WindowsCanonicalRecoverySideV1, purpose binaryadmission.WindowsCanonicalEntryRecoveryPurposeV2, body func(func(context.Context) error, binaryadmission.WindowsCanonicalEntryMembersRecoveryInputV2) error) error {
	return s.windowsCanonicalJournalRecoverySource.WithCanonicalRecoveryCleanupReadScope(ctx, side, purpose, func(check func(context.Context) error, input binaryadmission.WindowsCanonicalEntryMembersRecoveryInputV2) error {
		if err := body(check, input); err != nil {
			return err
		}
		return s.afterSelected()
	})
}

func v2TxnAssertCleanupScopeClosingDrift(t *testing.T, req windowsProductRecoveryRequest, journalPath string, decision windowsProductRecoveryJournalV3, expected binaryadmission.WindowsCanonicalEntryMemberEvidenceV2) {
	t.Helper()
	const side = binaryadmission.WindowsCanonicalRecoveryPriorV1
	const purpose = binaryadmission.WindowsCanonicalRecoveryCommittedForwardV2
	evidence := &windowsProductJournalReadEvidence{}
	req.journalReadEvidence = evidence
	s, err := prepareCurrentCanonicalRecoverySource(req, req.Deps, journalPath, decision, canonicalRecoveryCommittedCleanup)
	if err != nil {
		t.Fatal(err)
	}
	raw := bytes.Clone(s.raw)
	drift := append(bytes.Clone(raw), '\n')
	var owner *binaryadmission.WindowsCanonicalEntryRecoverySetV2
	var driftWritten bool
	defer func() {
		var restoreErr error
		if driftWritten {
			restoreErr = api.WriteStateFileBytesAtomic(journalPath, raw)
		}
		if err := errors.Join(restoreErr, s.closeCurrentMembers(owner), s.closeCurrentDirectories()); err != nil {
			t.Error(err)
		}
		s.active = false
	}()
	readsBefore, selected := 0, 0
	probe := &v2TxnCleanupReadBoundaryProbe{
		windowsCanonicalJournalRecoverySource: s,
		afterSelected: func() error {
			selected++
			if len(evidence.outcomes) != readsBefore+1 {
				return errors.New("native read used a nested protected journal read")
			}
			err := api.WriteStateFileBytesAtomic(journalPath, drift)
			driftWritten = err == nil
			return err
		},
	}
	owner, err = binaryadmission.AcquireWindowsCanonicalEntryForRecoveryV2(req.Context, probe, side)
	if err != nil {
		t.Fatal(err)
	}
	readsBefore = len(evidence.outcomes)
	got, present, err := owner.ObserveCleanupMember(req.Context, side, purpose, expected)
	if selected != 1 || err == nil || !errors.Is(err, ErrWindowsProductRecoveryIdentity) || !strings.Contains(err.Error(), "original recovery journal changed") ||
		present || got != (binaryadmission.WindowsCanonicalEntryMemberEvidenceV2{}) {
		t.Fatalf("closing read published a drifted observation: selected=%d present=%v err=%v", selected, present, err)
	}
	if err := s.cleanupCurrentSide(req.Context, owner, true); err == nil {
		t.Fatal("failed native observation allowed subsequent cleanup")
	}
	reads := evidence.outcomes[readsBefore:]
	if len(reads) != 2 || len(owner.Disposition().Cleanup) != 0 || !bytes.Equal(mustReadFile(t, journalPath), drift) {
		t.Fatalf("drift attempt advanced past the two protected reads or performed a cleanup effect: reads=%d", len(reads))
	}
	for _, observed := range reads {
		if observed.path != journalPath || observed.phase != string(decision.Phase) || observed.original == nil || observed.readError != nil {
			t.Fatal("missing original protected read evidence", observed.readError)
		}
	}
}

func v2TxnAssertColdCleanupExactSlots(t *testing.T, req windowsProductRecoveryRequest, journalPath string, decision windowsProductRecoveryJournalV3) {
	t.Helper()
	const member = binaryadmission.WindowsCanonicalEntryRelativePathV1
	const side = binaryadmission.WindowsCanonicalRecoveryPriorV1
	const purpose = binaryadmission.WindowsCanonicalRecoveryCommittedForwardV2
	plan := currentEntryMember(decision.EntryBundle, member)
	if plan == nil || plan.Prior == nil || plan.Staged == nil || plan.PriorAsideRelative == nil {
		t.Fatal("fixture did not publish over a distinct original launcher")
	}
	expected := *plan.Prior
	if !t.Run("closing_read_refuses_raw_drift", func(t *testing.T) {
		v2TxnAssertCleanupScopeClosingDrift(t, req, journalPath, decision, expected)
	}) {
		t.FailNow()
	}
	relative := *plan.PriorAsideRelative
	slot := filepath.Join(filepath.Dir(req.CanonicalEntryPath), relative)
	body := mustReadFile(t, slot)
	foreign := filepath.Join(filepath.Dir(req.StateDir), "same-byte-foreign")
	if err := os.WriteFile(foreign, body, 0600); err != nil {
		t.Fatal(err)
	}
	foreignID := v2TxnFixtureIdentity(t, foreign)
	if foreignID == expected.NativeIdentity || v2TxnFixtureDigest(body) != expected.File.SHA256 {
		t.Fatal("foreign fixture must have the same bytes and a distinct original identity")
	}

	withPrior := func(check func(*windowsCanonicalJournalRecoverySource, *binaryadmission.WindowsCanonicalEntryRecoverySetV2)) {
		t.Helper()
		j, err := v2TxnReadFixtureJournal(journalPath)
		if err != nil {
			t.Fatal(err)
		}
		s, err := prepareCurrentCanonicalRecoverySource(req, req.Deps, journalPath, j, canonicalRecoveryCommittedCleanup)
		if err != nil {
			t.Fatal(err)
		}
		owner, err := s.acquireCurrentSide(req.Context, side)
		defer func() {
			if err := errors.Join(s.closeCurrentMembers(owner), s.closeCurrentDirectories()); err != nil {
				t.Error(err)
			}
			s.active = false
		}()
		if err != nil {
			t.Fatal(err)
		}
		check(s, owner)
	}
	checkAbsent := func(owner *binaryadmission.WindowsCanonicalEntryRecoverySetV2) {
		t.Helper()
		got, present, err := owner.ObserveCleanupMember(req.Context, side, purpose, expected)
		if err != nil || present || got != (binaryadmission.WindowsCanonicalEntryMemberEvidenceV2{}) {
			t.Fatalf("exact original absence yielded %+v present=%v err=%v", got, present, err)
		}
	}
	withPrior(func(s *windowsCanonicalJournalRecoverySource, owner *binaryadmission.WindowsCanonicalEntryRecoverySetV2) {
		got, present, err := owner.ObserveCleanupMember(req.Context, side, purpose, expected)
		if err != nil || !present || got.NativeIdentity != expected.NativeIdentity || got.File.Path != relative {
			t.Fatalf("selected original aside yielded %+v present=%v err=%v", got, present, err)
		}
		next, err := cloneCurrentRecoveryJournal(s.journal)
		if err != nil {
			t.Fatal(err)
		}
		next.EntryBundle.Cleanup = append(next.EntryBundle.Cleanup, windowsCanonicalEntryCleanupV2{
			TransactionID: decision.TransactionID, Side: "prior", Member: member, SourceRelative: relative,
			ExpectedSHA256: expected.File.SHA256, State: "discard_intent", Purpose: purpose,
		})
		if err := s.persistCurrent(next); err != nil {
			t.Fatal(err)
		}
		result, err := owner.DiscardOriginal(req.Context, member, relative)
		if err := canonicalEntryDiscardResultError(result, err, member, relative); err != nil {
			t.Fatal(err)
		}
		checkAbsent(owner)
		// Leave the durable intent intact to exercise cold receiving of the
		// exact native absence after the first owner has closed.
	})
	withPrior(func(_ *windowsCanonicalJournalRecoverySource, owner *binaryadmission.WindowsCanonicalEntryRecoverySetV2) {
		checkAbsent(owner)
	})
	for _, change := range []func(*binaryadmission.WindowsCanonicalEntryMemberEvidenceV2){
		func(e *binaryadmission.WindowsCanonicalEntryMemberEvidenceV2) {
			e.NativeIdentity = plan.Staged.NativeIdentity
		},
		func(e *binaryadmission.WindowsCanonicalEntryMemberEvidenceV2) {
			e.File.SHA256 = strings.Repeat("0", 64)
		},
		func(e *binaryadmission.WindowsCanonicalEntryMemberEvidenceV2) { e.Member = "unissued.fixture" },
	} {
		withPrior(func(_ *windowsCanonicalJournalRecoverySource, owner *binaryadmission.WindowsCanonicalEntryRecoverySetV2) {
			wrong := expected
			change(&wrong)
			if got, present, err := owner.ObserveCleanupMember(req.Context, side, purpose, wrong); err == nil || present || got != (binaryadmission.WindowsCanonicalEntryMemberEvidenceV2{}) {
				t.Fatalf("wrong original was credited: %+v present=%v err=%v", got, present, err)
			}
		})
	}

	var priorAbsence string
	req.ExpectedJournalSHA256 = v2TxnFixtureDigest(mustReadFile(t, journalPath))
	req.Deps.WriteJournal = func(path string, raw []byte) error {
		if err := api.WriteStateFileBytesAtomic(path, raw); err != nil {
			return err
		}
		if priorAbsence != "" {
			return nil
		}
		j, err := v2TxnReadFixtureJournal(path)
		if err != nil {
			return err
		}
		index := currentCleanupIndex(j.EntryBundle, "prior", member)
		if index < 0 || j.EntryBundle.Cleanup[index].State != "absence_received" {
			return nil
		}
		for _, row := range j.EntryBundle.Cleanup {
			if row.Side == "prior" && row.Member != member && row.State == "absence_received" {
				if err := v2TxnFixtureAbsent(slot); err != nil {
					return err
				}
				if err := os.Rename(foreign, slot); err != nil {
					return err
				}
				priorAbsence = row.SourceRelative
				break
			}
		}
		return nil
	}
	settled, err := reconcileWindowsProductRecovery(req)
	if settled != nil || !errors.Is(err, ErrWindowsProductCleanupPending) || !strings.Contains(err.Error(), "cleanup slot is occupied after absence readback") || priorAbsence == "" {
		t.Fatalf("foreign replacement did not hold cold recovery pending: replaced=%t result=%+v err=%v", priorAbsence != "", settled, err)
	}
	if !bytes.Equal(mustReadFile(t, slot), body) || v2TxnFixtureIdentity(t, slot) != foreignID {
		t.Fatal("cleanup changed the same-byte foreign replacement")
	}
	if err := v2TxnFixtureAbsent(filepath.Join(filepath.Dir(slot), filepath.FromSlash(priorAbsence))); err != nil {
		t.Fatal("cleanup lost an earlier native absence", err)
	}
	pending, err := v2TxnReadFixtureJournal(journalPath)
	if err != nil || pending.Phase != WindowsProductRecoveryCommitDecided || pending.TransactionID != decision.TransactionID {
		t.Fatal("cleanup lost or settled its committed-decision journal", err)
	}
	index := currentCleanupIndex(pending.EntryBundle, "prior", member)
	if index < 0 || pending.EntryBundle.Cleanup[index].State != "absence_received" {
		t.Fatal("cleanup erased its earlier exact durable absence")
	}
	if v2TxnFixtureIdentity(t, req.CanonicalEntryPath) != plan.Staged.NativeIdentity {
		t.Fatal("cleanup touched the protected committed launcher")
	}
}

func TestWindowsV2ProductTxnNativeEntryBundle(t *testing.T) {
	input, seed, binding := v2TxnFixtureInput(t)
	deadline, finite := t.Deadline()
	if !finite {
		t.Fatal("native V2 transaction fixture requires a runner deadline")
	}
	for _, phase := range []string{
		"wrong_or_missing_admission_has_no_effects",
		"pretransfer_prior_closes_before_stage",
		"launcher_last_predecision_inverse",
		"current_inverse_after_intent_failure",
		"retained_runtime_requires_native_witness",
		"commit_decided_cold_forward_and_repeat",
		"commit_decided_cold_cleanup_exact_slots",
		"guard_veto",
		"guard_nil_without_write",
		"guard_writer_and_release_errors",
		"guard_release_error_after_commit",
	} {
		t.Run(phase, func(t *testing.T) {
			ctx, cancel := context.WithDeadline(t.Context(), deadline)
			t.Cleanup(cancel)
			root := apitest.HardenedTempDir(t)
			state := apitest.HardenedDir(t, filepath.Join(root, "state"))
			target := apitest.HardenedDir(t, filepath.Join(root, "target"))
			t.Cleanup(api.SetDaemonStateRootForTest(state))
			dst, err := binaryadmission.CanonicalEntryTargetsV2(filepath.Join(target, binaryadmission.WindowsCanonicalEntryRelativePathV1))
			if err != nil {
				t.Fatal(err)
			}
			allowance := productJournalFixtureAllowance()
			journalPath := filepath.Join(state, WindowsProductRecoveryFileLeafV3)
			receiptPath := filepath.Join(state, UpgradeReceiptSchemaV3+".json")
			eventPath := filepath.Join(state, api.SupervisorEventLogFileLeaf)
			convertedReceiptPath := filepath.Join(state, UpgradeReceiptSchemaV2+".json")
			priorTask := "<Task>fixture prior command</Task>"
			tasks := &v2TxnFixtureTasks{root: filepath.Join(root, "tasks"), currentXML: priorTask}
			apitest.HardenedDir(t, tasks.root)
			var events []WindowsProductCommitEvent
			publish := func(event WindowsProductCommitEvent) (string, error) {
				events = append(events, event)
				return windowsProductEventPublisher(state)(event)
			}
			txn := &WindowsProductTxn{Opts: WindowsProductTxnOpts{
				Context: ctx, JournalAllowance: allowance, EntryBundle: &stagedWindowsCanonicalEntrySetV2{},
				Mode: WindowsProductPairModeCanonicalize, StateDir: state, CanonicalEntryPath: dst.CanonicalEntryPath,
				ProductPath: dst.ProductPath, IdentityPath: dst.IdentityPath, StagedProduct: filepath.Join(target, "unavailable.stage"),
				LegacyWindowlessPath: filepath.Join(target, "mcphub-windowless.exe"), Tasks: tasks,
				Deps: WindowsProductTxnDeps{PublishCommitted: publish},
			}}
			if phase == "wrong_or_missing_admission_has_no_effects" {
				wrongSHA := strings.Repeat("0", 64)
				if wrongSHA == binding.ExpectedCanonicalEntrySHA256() {
					t.Fatal("fixture digest unexpectedly matches the wrong binding")
				}
				wrong, err := binaryadmission.NewWindowsCanonicalEntryBuildBindingV1(binding.BuildID(), wrongSHA, binding.ReverseProjection())
				if err != nil {
					t.Fatal(err)
				}
				refused, admissionErr := binaryadmission.AdmitWindowsCanonicalEntrySetV2(ctx, input.CanonicalEntryPath, wrong)
				if refused == nil {
					t.Fatal("current reader failed to return its partial owner", admissionErr)
				}
				if err := refused.Close(); err != nil {
					t.Fatal(err)
				}
				if admissionErr == nil || !strings.Contains(admissionErr.Error(), "binding disagrees") {
					t.Fatal("same BuildID with wrong launcher digest did not reach binding refusal", admissionErr)
				}
				_, err = txn.Run(ctx)
				var pre *WindowsProductPairPrePromotionError
				if !errors.As(err, &pre) || !strings.Contains(err.Error(), "original current context and stage required") {
					t.Fatal("missing original V2 stage was not refused before Prepare", err)
				}
				entries, err := os.ReadDir(target)
				if err != nil || len(entries) != 0 || tasks.exports != 0 || tasks.rewrites != 0 || tasks.restores != 0 || tasks.currentXML != priorTask || len(events) != 0 {
					t.Fatal("unadmitted V2 stage caused target, Task or event effects", err)
				}
				for _, path := range []string{journalPath, receiptPath, convertedReceiptPath, eventPath} {
					if err := v2TxnFixtureAbsent(path); err != nil {
						t.Fatal(err)
					}
				}
				return
			}

			source, err := binaryadmission.AcquireWindowsCanonicalEntrySetV2(ctx, input, seed)
			if source != nil {
				t.Cleanup(func() {
					if err := source.Close(); err != nil {
						t.Error(err)
					}
				})
			}
			if err != nil {
				t.Fatal(err)
			}

			inverse := phase == "launcher_last_predecision_inverse" || phase == "guard_veto" ||
				phase == "guard_nil_without_write" || phase == "guard_writer_and_release_errors"
			pretransferClose := phase == "pretransfer_prior_closes_before_stage"
			exactCleanupSlots := phase == "commit_decided_cold_cleanup_exact_slots"
			retainedRuntime := phase == "retained_runtime_requires_native_witness"
			var prior map[string]binaryadmission.WindowsCanonicalEntryFileIdentityV2
			if inverse || pretransferClose || exactCleanupSlots || retainedRuntime || phase == "current_inverse_after_intent_failure" {
				prior = v2TxnSeedPrior(t, input, dst, seed)
			}
			if inverse {
				if err := os.WriteFile(convertedReceiptPath, []byte("retained fixture receipt"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			stage, err := source.Stage(ctx, dst.CanonicalEntryPath)
			txn.Opts.EntryBundle.original = stage
			t.Cleanup(func() {
				if err := txn.Close(); err != nil {
					t.Error(err)
				}
			})
			if err != nil {
				t.Fatal(err)
			}
			if err := source.Close(); err != nil {
				t.Fatal(err)
			}
			stagedSnapshot, err := stage.Snapshot()
			if err != nil {
				t.Fatal(err)
			}
			members, err := stage.Members()
			if err != nil {
				t.Fatal(err)
			}
			stagedFiles := make(map[string]binaryadmission.WindowsCanonicalEntryMemberEvidenceV2, len(members))
			for _, member := range members {
				stagedFiles[member.Member] = member
				if member.Member == stagedSnapshot.ProductRelativePath {
					txn.Opts.StagedProduct = filepath.Join(target, filepath.FromSlash(member.File.Path))
				}
			}
			if _, recorded := stagedFiles[stagedSnapshot.ProductRelativePath]; !recorded {
				t.Fatal("Product stage member unavailable")
			}

			if retainedRuntime {
				v2TxnAssertRetainedRuntimeRequiresNativeWitness(t, ctx, txn, journalPath)
				return
			}
			if pretransferClose {
				carrier := txn.Opts.EntryBundle
				originalPrior, err := carrier.acquirePrior(ctx)
				if err != nil || originalPrior == nil {
					t.Fatal("original BUNDLE prior unavailable", err)
				}
				before, err := originalPrior.Observation(ctx)
				if err != nil || before.BeforeKind != binaryadmission.WindowsCanonicalEntryBeforeBundleV2 || len(stage.Directories()) == 0 {
					t.Fatal("pre-transfer fixture lacks an admitted original BUNDLE and lenders", err)
				}
				observations := 0
				closeErr := carrier.closeWithStageObservation(func() {
					observations++
					priorClose := originalPrior.Disposition()
					if !priorClose.CloseEntered || priorClose.CloseError != nil {
						t.Error("original prior did not close successfully before Stage", priorClose.CloseError)
					}
					if stageClose := stage.Disposition(); stageClose.CloseEntered || stageClose.Transferred {
						t.Error("fixture did not observe the unpublished Stage before its original Close")
					}
				})
				if closeErr != nil || observations != 1 {
					t.Fatalf("original carrier close observations=%d error=%v", observations, closeErr)
				}
				stageClose := stage.Disposition()
				if !stageClose.CloseEntered || stageClose.Transferred || stageClose.CloseError != nil ||
					stageClose.OriginalOwnerPending || len(stageClose.Cleanup) != len(stagedFiles) {
					t.Fatal("original Stage or its directory lenders did not settle", stageClose.CloseError)
				}
				for _, result := range stageClose.Cleanup {
					if !result.Applied || !result.Readback || result.Error != nil || result.CloseError != nil {
						t.Fatalf("original staged member %q failed native removal/readback: %v", result.Member, errors.Join(result.Error, result.CloseError))
					}
				}
				for member, staged := range stagedFiles {
					final := filepath.Join(target, filepath.FromSlash(member))
					identity, exists := prior[final]
					if !exists || identity == staged.NativeIdentity || v2TxnFixtureIdentity(t, final) != identity ||
						v2TxnFixtureDigest(mustReadFile(t, final)) != staged.File.SHA256 {
						t.Fatalf("pre-transfer close changed or aliased original final member %q", member)
					}
					if err := v2TxnFixtureAbsent(filepath.Join(target, filepath.FromSlash(staged.File.Path))); err != nil {
						t.Fatal(err)
					}
				}
				v2TxnAssertFixtureRoot(t, target, dst)
				return
			}

			if phase == "current_inverse_after_intent_failure" {
				v2TxnAssertInverseDiagnosticAfterPrefix(t, ctx, txn, stagedFiles)
				return
			}

			var txnID string
			cut := errors.New("stop private V2 transaction at selected boundary")
			var observed []WindowsProductStage
			veto := errors.New("refuse private V2 commit before the writer")
			writeFailure := errors.New("private commit writer kept the prior bytes")
			releaseFailure := errors.New("release private decision guard")
			decisionWrites := 0
			txn.Opts.Deps.WriteRecoveryJournal = func(path string, raw []byte) error {
				journal, err := decodeWindowsProductRecoveryJournalV3(raw)
				if err != nil {
					return err
				}
				if journal.Phase == WindowsProductRecoveryCommitDecided {
					decisionWrites++
					if phase == "guard_writer_and_release_errors" {
						return writeFailure
					}
				}
				return api.WriteStateFileBytesAtomic(path, raw)
			}
			var ready, retired bool
			var lateWrite func() error
			if strings.HasPrefix(phase, "guard_") {
				txn.aroundCommitDecision = func(_ context.Context, write func() error) error {
					journal, err := v2TxnReadFixtureJournal(journalPath)
					if err != nil || !ready || !retired || journal.Phase != WindowsProductRecoverySuccessorStarting ||
						journal.TransactionID != txnID || decisionWrites != 0 {
						t.Error("guard entered before readiness, retirement and the original decision", err)
						return errors.New("invalid private guard entry")
					}
					lateWrite = write
					switch phase {
					case "guard_veto":
						return veto
					case "guard_nil_without_write":
						return nil
					case "guard_writer_and_release_errors":
						if err := write(); !errors.Is(err, writeFailure) {
							t.Error("callback did not return the original writer cause", err)
						}
					case "guard_release_error_after_commit":
						if err := write(); err != nil {
							return err
						}
						decisionBytes := mustReadFile(t, journalPath)
						if err := write(); err == nil || decisionWrites != 1 ||
							!bytes.Equal(decisionBytes, mustReadFile(t, journalPath)) {
							t.Error("repeated callback changed the confirmed decision", err)
						}
					}
					return releaseFailure
				}
			}
			txn.Opts.Deps.Fault = func(at WindowsProductStage) error {
				switch at {
				case WindowsProductStageIdentityPromoted, WindowsProductStageProductPromoted:
					journal, err := v2TxnReadFixtureJournal(journalPath)
					if err != nil {
						return err
					}
					moves, err := v2TxnPromotionNames(journal)
					if err != nil {
						return err
					}
					want := stagedSnapshot.IdentityRelativePath
					if at == WindowsProductStageProductPromoted {
						want = stagedSnapshot.ProductRelativePath
					}
					if len(moves) == 0 || moves[len(moves)-1] != want {
						return errors.New("native member readback did not reach the expected publication")
					}
					observed = append(observed, at)
					return v2TxnFixtureAbsent(dst.CanonicalEntryPath)
				case WindowsProductStageCanonicalEntryPromoted:
					journal, err := v2TxnReadFixtureJournal(journalPath)
					if err != nil {
						return err
					}
					moves, err := v2TxnPromotionNames(journal)
					if err != nil {
						return err
					}
					if len(moves) != len(stagedFiles) || moves[len(moves)-1] != stagedSnapshot.CanonicalEntryRelativePath {
						return errors.New("native V2 launcher was published before the complete cohort")
					}
					seen := make(map[string]bool, len(moves))
					for _, member := range moves {
						if _, expected := stagedFiles[member]; !expected || seen[member] {
							return errors.New("native publication gained duplicate or foreign members")
						}
						seen[member] = true
					}
					if journal.TransactionID != txnID {
						return errors.New("native promotion changed the prepared transaction")
					}
					observed = append(observed, at)
				case WindowsProductStageTasksReadBack:
					if phase == "launcher_last_predecision_inverse" {
						return cut
					}
				case WindowsProductStageSupervisorReady:
					ready = true
				case WindowsProductStageLegacyRetired:
					retired = true
				case WindowsProductStageCommitDecided:
					if phase == "commit_decided_cold_forward_and_repeat" || exactCleanupSlots {
						return cut
					}
				}
				return nil
			}
			// Prepare freezes the dependencies, including the fault callback.
			if err := txn.Prepare(); err != nil {
				t.Fatal(err)
			}
			prepared, err := v2TxnReadFixtureJournal(journalPath)
			if err != nil || prepared.Phase != WindowsProductRecoveryPrepared || len(prepared.EntryBundle.Moves) != 0 ||
				tasks.exports != 1 || tasks.rewrites != 0 {
				t.Fatal("native V2 Prepare did not persist the exact pre-move journal", err)
			}
			if phase == "commit_decided_cold_forward_and_repeat" {
				t.Run("persisted_journal_clone", func(t *testing.T) {
					v2TxnAssertPersistedJournalClone(t, mustReadFile(t, journalPath))
				})
			}
			txnID = prepared.TransactionID
			_, runErr := txn.Run(ctx)
			wantOrder := []WindowsProductStage{WindowsProductStageIdentityPromoted, WindowsProductStageProductPromoted, WindowsProductStageCanonicalEntryPromoted}
			if !reflect.DeepEqual(observed, wantOrder) || txnID == "" || tasks.exports != 1 || tasks.rewrites != 1 {
				t.Fatal("real V2 publication/readback or Task rewrite was not reached", observed, runErr)
			}
			wantDecisionWrites := 1
			if inverse && phase != "guard_writer_and_release_errors" {
				wantDecisionWrites = 0
			}
			if lateWrite != nil {
				if err := lateWrite(); err == nil {
					t.Fatal("saved callback remained usable after the guard returned")
				}
			}
			if decisionWrites != wantDecisionWrites || (strings.HasPrefix(phase, "guard_") && lateWrite == nil) {
				t.Fatalf("original commit writer entered %d times, want %d: %v", decisionWrites, wantDecisionWrites, runErr)
			}
			switch phase {
			case "launcher_last_predecision_inverse":
				if !errors.Is(runErr, cut) {
					t.Fatal("unguarded predecision failure lost its cause", runErr)
				}
			case "guard_veto":
				if !errors.Is(runErr, veto) {
					t.Fatal("guard veto lost its cause", runErr)
				}
			case "guard_nil_without_write":
				if runErr == nil || !strings.Contains(runErr.Error(), "guard did not invoke the original writer") {
					t.Fatal("guard returned success without writing the decision", runErr)
				}
			case "guard_writer_and_release_errors":
				if !errors.Is(runErr, writeFailure) || !errors.Is(runErr, releaseFailure) {
					t.Fatal("inverse lost a writer or release cause", runErr)
				}
			case "guard_release_error_after_commit":
				if !errors.Is(runErr, ErrWindowsProductRecoveryPending) || !errors.Is(runErr, releaseFailure) {
					t.Fatal("confirmed decision lost forward recovery or the release cause", runErr)
				}
			case "commit_decided_cold_forward_and_repeat", "commit_decided_cold_cleanup_exact_slots":
				if !errors.Is(runErr, ErrWindowsProductCleanupPending) || !strings.Contains(runErr.Error(), cut.Error()) {
					t.Fatal("unguarded confirmed decision did not retain its cleanup cause", runErr)
				}
			}
			if inverse {
				if errors.Is(runErr, ErrWindowsProductRecoveryPending) ||
					tasks.restores != 1 || tasks.currentXML != priorTask || tasks.discards != 1 || len(events) != 0 {
					t.Fatal("predecision did not restore the original V2 Task state and settle", runErr)
				}
				for member, staged := range stagedFiles {
					path := filepath.Join(target, filepath.FromSlash(member))
					original, existed := prior[path]
					if !existed || original == staged.NativeIdentity || v2TxnFixtureIdentity(t, path) != original ||
						v2TxnFixtureDigest(mustReadFile(t, path)) != staged.File.SHA256 {
						t.Fatalf("predecision inverse lost the distinct prior member identity or bytes: %s", member)
					}
				}
				if got := mustReadFile(t, convertedReceiptPath); !bytes.Equal(got, []byte("retained fixture receipt")) {
					t.Fatal("predecision inverse changed the original receipt")
				}
				for _, p := range []string{journalPath, receiptPath, eventPath} {
					if err := v2TxnFixtureAbsent(p); err != nil {
						t.Fatal(err)
					}
				}
				v2TxnAssertFixtureRoot(t, target, dst)
				t.Log("real V2 launcher-last publication and exact prior member/Task inverse received")
				return
			}

			if tasks.restores != 0 || tasks.currentXML == priorTask || tasks.discards != 0 || len(events) != 0 {
				t.Fatal("durable decision did not retain one forward transaction", runErr)
			}
			decision, err := v2TxnReadFixtureJournal(journalPath)
			if err != nil || decision.Phase != WindowsProductRecoveryCommitDecided || decision.TransactionID != txnID {
				t.Fatal("actual durable decision differs from the native publication transaction", err)
			}
			if err := v2TxnFixtureAbsent(receiptPath); err != nil {
				t.Fatal(err)
			}
			for member, staged := range stagedFiles {
				path := filepath.Join(target, filepath.FromSlash(member))
				if v2TxnFixtureIdentity(t, path) != staged.NativeIdentity || v2TxnFixtureDigest(mustReadFile(t, path)) != staged.File.SHA256 {
					t.Fatalf("commit decision did not retain the moved native source and bytes %s", member)
				}
			}

			cold := &v2TxnFixtureTasks{root: tasks.root, currentXML: tasks.currentXML}
			reopens := 0
			req := windowsProductRecoveryRequest{
				Context: ctx, JournalAllowance: allowance, StateDir: state, CanonicalEntryPath: dst.CanonicalEntryPath,
				ProductPath: dst.ProductPath, IdentityPath: dst.IdentityPath, LegacyWindowlessPath: txn.Opts.LegacyWindowlessPath,
				Legacy:                windowsProductPairRecoveryRequest{Context: ctx, JournalAllowance: allowance, StateDir: state},
				ExpectedJournalSHA256: v2TxnFixtureDigest(mustReadFile(t, journalPath)),
				ObserveSuccessorReady: func(_ context.Context, path, digest string) (bool, error) {
					if path != dst.ProductPath || digest != stagedSnapshot.Product.SHA256 {
						return false, errors.New("cold observer received a different successor")
					}
					return true, nil
				},
				StartSupervisor: func(string) (SupervisorLaunch, error) {
					t.Error("cold private fixture unexpectedly tried to launch a Product")
					return nil, errors.New("unexpected Product launch")
				},
				WaitSupervisorReady: func(context.Context, string, string) error {
					t.Error("cold private fixture unexpectedly waited for a Product launch")
					return errors.New("unexpected Product readiness wait")
				},
				Deps: windowsProductRecoveryDeps{
					ReopenEntryBundleTasks: func(_ context.Context, desc WindowsProductPairTaskRecovery, targets binaryadmission.WindowsCanonicalEntrySetTargetsV2) (WindowsProductTaskTxn, error) {
						reopens++
						if targets != dst || !reflect.DeepEqual(desc, decision.Tasks) || desc.StoreRoot != cold.root {
							return nil, errors.New("cold V2 Task owner received a different transaction descriptor")
						}
						return cold, nil
					},
					DiscardTaskStore: func(WindowsProductPairTaskRecovery) error {
						return errors.New("cold commit must finish through its reopened V2 Task transaction")
					},
					PublishCommitted: publish,
				},
			}
			if exactCleanupSlots {
				v2TxnAssertColdCleanupExactSlots(t, req, journalPath, decision)
				if reopens != 1 || cold.discards != 0 || cold.closes != 1 || len(events) != 0 {
					t.Fatal("pending cleanup consumed Task custody or emitted a committed event")
				}
				if err := v2TxnFixtureAbsent(receiptPath); err != nil {
					t.Fatal(err)
				}
				return
			}
			settled, err := reconcileWindowsProductRecovery(req)
			if err != nil || settled == nil || settled.Outcome != WindowsProductPairRecoverySettledCommit ||
				settled.TransactionID != txnID || reopens != 1 || cold.discards != 1 || cold.closes != 1 ||
				cold.restores != 0 || cold.exports != 0 || cold.rewrites != 0 || len(events) != 1 {
				t.Fatal("cold V2 recovery did not settle the same decision exactly once", err)
			}
			receipt := mustReadFile(t, receiptPath)
			decoded, err := DecodeUpgradeReceipt(receipt)
			if err != nil || decoded.V3 == nil || decoded.V3.Admission != UpgradeAdmissionCanonicalEntryProductV2 ||
				decoded.V3.EntryBundle == nil || decoded.V3.EntrySet != nil || decoded.V3.TransactionID != txnID ||
				decoded.V3.EntryBundle.TransactionID != txnID || decoded.V3.TaskSetSHA256 != decision.TaskSetSHA256 ||
				events[0].TransactionID != txnID || events[0].EntryBundle == nil || events[0].ReceiptSHA256 != v2TxnFixtureDigest(receipt) {
				t.Fatal("cold V2 receipt and committed event are not bound to the same transaction", err)
			}
			for member, staged := range stagedFiles {
				path := filepath.Join(target, filepath.FromSlash(member))
				if v2TxnFixtureIdentity(t, path) != staged.NativeIdentity || v2TxnFixtureDigest(mustReadFile(t, path)) != staged.File.SHA256 {
					t.Fatalf("cold recovery changed the moved native successor member %s", member)
				}
			}
			if err := v2TxnFixtureAbsent(journalPath); err != nil {
				t.Fatal(err)
			}
			eventBytes := mustReadFile(t, eventPath)
			durableEvents := 0
			for _, line := range bytes.Split(eventBytes, []byte{'\n'}) {
				if len(bytes.TrimSpace(line)) == 0 {
					continue
				}
				var event api.SupervisorEvent
				if err := json.Unmarshal(line, &event); err != nil {
					t.Fatal(err)
				}
				if event.Event != "windows-product-committed" {
					continue
				}
				durableEvents++
				if event.Body["transaction_id"] != txnID || event.Body["receipt_sha256"] != v2TxnFixtureDigest(receipt) {
					t.Fatal("durable V2 event is not bound to the settled receipt")
				}
			}
			if durableEvents != 1 {
				t.Fatalf("expected one durable committed event, got %d", durableEvents)
			}
			req.ExpectedJournalSHA256 = ""
			repeat, err := reconcileWindowsProductRecovery(req)
			if err != nil || repeat != nil || reopens != 1 || cold.discards != 1 || len(events) != 1 ||
				!bytes.Equal(receipt, mustReadFile(t, receiptPath)) || !bytes.Equal(eventBytes, mustReadFile(t, eventPath)) {
				t.Fatal("repeat after V2 settlement produced another effect", err)
			}
			v2TxnAssertFixtureRoot(t, target, dst)
			t.Log("one durable V2 decision received by fresh cold owner without an inverse or repeated effects")
		})
	}
}

// This fixture enters only the original file-move owner. No runtime recovery,
// task restore, or supervisor invocation is part of the injected failure.
func TestWindowsV2ProductTxnSelectivePlan(t *testing.T) {
	input, seed, _ := v2TxnFixtureInput(t)
	for _, tc := range []struct {
		name            string
		changedIdentity bool
		changedProduct  bool
		changedAttrs    bool
	}{
		{name: "all_equal"},
		{name: "identity_bytes_changed", changedIdentity: true},
		{name: "product_and_identity_changed", changedProduct: true},
		{name: "identity_attributes_changed", changedAttrs: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := t.Context()
			target := apitest.HardenedTempDir(t)
			dst, err := binaryadmission.CanonicalEntryTargetsV2(filepath.Join(target, binaryadmission.WindowsCanonicalEntryRelativePathV1))
			if err != nil {
				t.Fatal(err)
			}
			priorIDs := v2TxnSeedPrior(t, input, dst, seed)
			oldIdentity := seed.IdentityBytes
			oldProduct := mustReadFile(t, dst.ProductPath)
			if tc.changedProduct {
				oldProduct = append(oldProduct, []byte("fixture original Product")...)
				if err := os.WriteFile(dst.ProductPath, oldProduct, 0600); err != nil {
					t.Fatal(err)
				}
				identity, err := binaryadmission.ParseWindowsProductIdentityV2(oldIdentity)
				if err != nil {
					t.Fatal(err)
				}
				identity.SHA256 = v2TxnFixtureDigest(oldProduct)
				oldIdentity, err = json.Marshal(identity)
				if err != nil {
					t.Fatal(err)
				}
			}
			if tc.changedIdentity {
				oldIdentity = append(append([]byte(nil), oldIdentity...), '\n')
			}
			if tc.changedProduct || tc.changedIdentity {
				if err := os.WriteFile(dst.IdentityPath, oldIdentity, 0600); err != nil {
					t.Fatal(err)
				}
			}
			if tc.changedAttrs {
				p, err := windows.UTF16PtrFromString(dst.IdentityPath)
				if err != nil {
					t.Fatal(err)
				}
				if err := windows.SetFileAttributes(p, windows.FILE_ATTRIBUTE_HIDDEN); err != nil {
					t.Fatal(err)
				}
			}
			initial, err := os.ReadDir(target)
			if err != nil {
				t.Fatal(err)
			}
			source, err := binaryadmission.AcquireWindowsCanonicalEntrySetV2(ctx, input, seed)
			if source != nil {
				t.Cleanup(func() {
					if err := source.Close(); err != nil {
						t.Error(err)
					}
				})
			}
			if err != nil {
				t.Fatal(err)
			}
			carrier, err := stageRetainingCurrentEntryBundle(ctx, source, dst.CanonicalEntryPath)
			if carrier != nil {
				t.Cleanup(func() {
					if err := carrier.Close(); err != nil {
						t.Error(err)
					}
				})
			}
			if err != nil {
				t.Fatal(err)
			}
			if err := source.Close(); err != nil {
				t.Fatal(err)
			}
			members, err := carrier.original.Members()
			if err != nil {
				t.Fatal(err)
			}
			physical, err := os.ReadDir(target)
			if err != nil {
				t.Fatal(err)
			}
			wantCount, wantDisposition := 0, windowsProductTargetIdentical
			switch {
			case tc.changedProduct:
				wantCount, wantDisposition = 2, windowsProductTargetReplace
			case tc.changedIdentity || tc.changedAttrs:
				wantCount, wantDisposition = 1, windowsProductTargetReplace
			}
			if len(members) != wantCount || len(physical)-len(initial) != wantCount {
				t.Fatal("native Stage created files beyond the actual changed bytes")
			}
			opts := WindowsProductTxnOpts{Context: ctx, EntryBundle: carrier, Mode: WindowsProductPairModeUpgrade,
				CanonicalEntryPath: dst.CanonicalEntryPath, ProductPath: dst.ProductPath, IdentityPath: dst.IdentityPath,
				LegacyWindowlessPath: filepath.Join(target, "mcphub-windowless.exe")}
			if tc.changedProduct {
				opts.StagedProduct, err = currentStageProductPath(carrier.original)
				if err != nil {
					t.Fatal(err)
				}
			}
			preflight, err := inspectCurrentEntryPreflight(opts)
			if err != nil || preflight.Disposition.CLI != wantDisposition || !preflight.PriorCLI.Present {
				t.Fatal("selective preflight did not classify actual changed bytes and exact prior", err)
			}
			plan, arm, err := prepareCanonicalEntryBundle(opts, strings.Repeat("a", 32))
			if err != nil {
				t.Fatal(err)
			}
			witness, err := carrier.original.RetainedMembersWitness(ctx)
			if err != nil || arm.Bundle.Retained == nil || arm.Bundle.Retained.MembersSHA256 != witness.MembersSHA256 ||
				!reflect.DeepEqual(arm.Bundle.Retained.Singletons, witness.Singletons) {
				t.Fatal("selective journal plan lost its original native witness", err)
			}
			if len(arm.Bundle.Members) != wantCount || len(plan.stagedLocations) != wantCount || len(plan.priorLocations) != wantCount ||
				arm.StagedEntry != (windowsProductRecoveryStagedV3{}) ||
				(currentEntryMember(arm, arm.Expected.ProductRelativePath) != nil) != tc.changedProduct ||
				currentEntryMember(arm, arm.Expected.CanonicalEntryRelativePath) != nil ||
				!arm.CanonicalEntry.PriorPresent || arm.CanonicalEntry.PriorSHA256 != seed.CanonicalEntry.SHA256 ||
				arm.PriorRuntime == nil || !arm.PriorRuntime.Present || arm.PriorRuntime.Path != dst.ProductPath {
				t.Fatal("selective plan fabricated a retained core file stage or lost the exact prior")
			}
			for _, issued := range members {
				row := currentEntryMember(arm, issued.Member)
				if row == nil || row.Staged == nil || *row.Staged != issued || row.Prior == nil ||
					row.PriorAsideRelative == nil || row.SuccessorAsideRelative == nil {
					t.Fatal("changed row is not the exact issued stage and prior")
				}
				switch issued.Member {
				case seed.IdentityRelativePath:
					if row.Staged.File.SHA256 != seed.Identity.SHA256 || row.Prior.File.SHA256 != v2TxnFixtureDigest(oldIdentity) ||
						tc.changedAttrs && (row.Staged.File.SHA256 != row.Prior.File.SHA256 || row.Staged.Attributes == row.Prior.Attributes) {
						t.Fatal("identity byte or attribute change did not preserve native evidence")
					}
				case seed.ProductRelativePath:
					if !tc.changedProduct || row.Staged.File.SHA256 != seed.Product.SHA256 ||
						row.Prior.File.SHA256 != v2TxnFixtureDigest(oldProduct) ||
						!sameWindowsProductPath(opts.StagedProduct, filepath.Join(target, filepath.FromSlash(row.Staged.File.Path))) {
						t.Fatal("changed Product did not carry the exact issued staged path and bytes")
					}
				default:
					t.Fatal("unexpected runtime or Launcher stage")
				}
			}
			var tasks v2TxnFixtureTasks
			txn := &WindowsProductTxn{Opts: opts}
			txn.Opts.Tasks = &tasks
			if err := txn.Prepare(); !errors.Is(err, errCurrentEntryRuntimeRetentionUnavailable) || txn.prepared != nil || tasks.exports != 0 {
				t.Fatal("unintegrated selective plan crossed the mutation boundary", err)
			}
			if err := carrier.Close(); err != nil {
				t.Fatal(err)
			}
			closed, err := os.ReadDir(target)
			if err != nil || len(closed) != len(initial) {
				t.Fatal("ordinary cleanup did not settle the selective stage", err)
			}
			for name, id := range priorIDs {
				if v2TxnFixtureIdentity(t, name) != id {
					t.Fatal("selective close changed a retained original")
				}
			}
			if !bytes.Equal(mustReadFile(t, dst.IdentityPath), oldIdentity) || !bytes.Equal(mustReadFile(t, dst.ProductPath), oldProduct) {
				t.Fatal("preflight or cleanup changed unstaged original bytes")
			}
		})
	}
}

func v2TxnAssertRetainedRuntimeRequiresNativeWitness(t *testing.T, ctx context.Context, txn *WindowsProductTxn, journalPath string) {
	t.Helper()
	txn.Opts.Mode = WindowsProductPairModeUpgrade
	txn.Opts.FrozenPorts = []windowsProductIntentPortV1{}
	txn.Opts.PreSettlement = &windowsProductQuiescenceObservationV1{
		GUIOwner:   windowsProductGUIOwnerConfirmedAbsent,
		Supervisor: windowsProductSupervisorSettlementV1{State: windowsProductSupervisorKnownAbsent},
		Ports:      []windowsProductPortSettlementV1{}, SupervisorLockReleased: true,
	}
	if err := txn.Prepare(); err != nil {
		t.Fatal(err)
	}
	before := mustReadFile(t, journalPath)
	old, err := decodeWindowsProductRecoveryJournalV3(before)
	if err != nil || old.Phase != WindowsProductRecoveryPrepared || old.Quiescence != nil || old.EntryBundle.Bundle.Retained != nil {
		t.Fatal("existing exact BUNDLE prepared record did not round trip", err)
	}
	if encoded, err := encodeWindowsProductRecoveryJournalV3(old); err != nil || !bytes.Equal(encoded, before) {
		t.Fatal("existing exact-member wire bytes changed", err)
	}
	makeSparse := func(t *testing.T) windowsProductRecoveryJournalV3 {
		t.Helper()
		j, err := decodeWindowsProductRecoveryJournalV3(before)
		if err != nil {
			t.Fatal(err)
		}
		e := j.EntryBundle
		e.Bundle.Retained = &windowsCanonicalEntryRetainedRuntimeV2{
			MembersSHA256: strings.Repeat("a", 64),
			Singletons:    []string{e.Expected.RuntimePayload.Path},
		}
		files, _, err := currentEntryRetainedRuntime(e)
		if err != nil || len(files) != 9 {
			t.Fatal("exact fixture must select eight component files and one index", err)
		}
		changed := e.Bundle.Members[:0]
		for _, row := range e.Bundle.Members {
			if !files[row.Member] {
				changed = append(changed, row)
			}
		}
		e.Bundle.Members = changed
		for _, key := range []string{e.Expected.CanonicalEntryRelativePath, e.Expected.ProductRelativePath, e.Expected.IdentityRelativePath} {
			if row := currentEntryMember(e, key); row == nil || row.Prior == nil || row.Staged == nil {
				t.Fatal("a required changed core file lost its original rows")
			}
		}
		return j
	}
	sparse := makeSparse(t)
	raw, err := json.Marshal(sparse)
	if err != nil {
		t.Fatal(err)
	}
	var typed windowsProductRecoveryJournalV3
	if err := decodeStrictCanonicalProductRecord(raw, &typed); err != nil {
		t.Fatal("strict V3 retained wire did not receive typed evidence", err)
	}
	if err := validateCanonicalEntryBundleJournal(typed); err != nil {
		t.Fatal("valid selective V3 structure was not recognized", err)
	}
	if _, err := decodeWindowsProductRecoveryJournalV3(raw); !errors.Is(err, errCurrentEntryRuntimeRetentionUnavailable) {
		t.Fatal("selective prepared V3 did not refuse without native recovery", err)
	}
	projection, err := projectCurrentRecoveryJournal(typed, binaryadmission.WindowsCanonicalRecoveryRollbackV2)
	if !errors.Is(err, errCurrentEntryRuntimeRetentionUnavailable) || projection.Members != nil {
		t.Fatal("selective recovery projected a witness-free native bank", err)
	}
	writes, reads := 0, 0
	err = persistWindowsProductRecoveryJournalV3(ctx, txn.Opts.JournalAllowance, journalPath, typed,
		func(string, []byte) error { writes++; return nil },
		func(string) ([]byte, error) { reads++; return nil, nil })
	if !errors.Is(err, errCurrentEntryRuntimeRetentionUnavailable) || writes != 0 || reads != 0 {
		t.Fatal("selective warm writer reached effects without native retention", err)
	}
	for _, tc := range []struct {
		name    string
		corrupt func(*windowsCanonicalEntrySetJournalV2)
	}{
		{"missing witness", func(e *windowsCanonicalEntrySetJournalV2) { e.Bundle.Retained = nil }},
		{"invalid digest", func(e *windowsCanonicalEntrySetJournalV2) { e.Bundle.Retained.MembersSHA256 = "" }},
		{"foreign singleton", func(e *windowsCanonicalEntrySetJournalV2) {
			e.Bundle.Retained.Singletons = []string{path.Join(e.Bundle.RelativePath, "fixture-foreign")}
		}},
		{"changed old index", func(e *windowsCanonicalEntrySetJournalV2) {
			for i := range e.Bundle.BeforeInventory.Objects {
				if e.Bundle.BeforeInventory.Objects[i].Path == path.Base(e.Expected.RuntimePayload.Path) {
					e.Bundle.BeforeInventory.Objects[i].SHA256 = strings.Repeat("0", 64)
				}
			}
		}},
		{"foreign component leaf", func(e *windowsCanonicalEntrySetJournalV2) {
			payload, err := binaryadmission.ParseRuntimePayload(e.Expected.RuntimePayloadBytes)
			if err != nil {
				t.Fatal(err)
			}
			n := int64(1)
			original := &e.Bundle.BeforeInventory.Objects
			*original = append(*original, binaryadmission.RuntimeObjectV1{Path: path.Join(payload.Components[0].Root, "fixture-foreign"), Kind: "file", Size: &n, SHA256: strings.Repeat("b", 64)})
			sort.Slice(*original, func(i, j int) bool { return (*original)[i].Path < (*original)[j].Path })
		}},
		{"retained directory scheduled for disposal", func(e *windowsCanonicalEntrySetJournalV2) {
			for i := range e.Bundle.Directories {
				if e.Bundle.Directories[i].Original.RelativePath == e.Bundle.RelativePath {
					e.Bundle.Directories[i].State = "dispose_intent"
				}
			}
		}},
		{"retained file selected for move", func(e *windowsCanonicalEntrySetJournalV2) {
			e.Moves = append(e.Moves, windowsCanonicalEntryMoveRecordV2{Member: e.Expected.RuntimePayload.Path, Prior: true,
				SourceRelative: e.Expected.RuntimePayload.Path, DestinationRelative: "fixture-aside.stage"})
		}},
		{"retained file selected for cleanup", func(e *windowsCanonicalEntrySetJournalV2) {
			e.Cleanup = append(e.Cleanup, windowsCanonicalEntryCleanupV2{Member: e.Expected.RuntimePayload.Path, TransactionID: e.TransactionID,
				Side: "prior", ExpectedSHA256: e.Expected.RuntimePayload.SHA256, SourceRelative: "fixture-aside.stage",
				State: "discard_intent", Purpose: binaryadmission.WindowsCanonicalRecoveryCommittedForwardV2})
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			j := makeSparse(t)
			tc.corrupt(j.EntryBundle)
			if err := validateCanonicalEntryBundleJournal(j); err == nil {
				t.Fatal("corrupted selective V3 was accepted")
			}
		})
	}
	for _, tc := range []struct {
		name                        string
		launcher, product, identity bool
	}{
		{name: "retained Launcher", launcher: true},
		{name: "retained Product", product: true},
		{name: "all equal core files", launcher: true, product: true, identity: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			j := makeSparse(t)
			e := j.EntryBundle
			if tc.launcher {
				e.Bundle.Retained.Singletons = append(e.Bundle.Retained.Singletons, e.Expected.CanonicalEntryRelativePath)
				e.StagedEntry = windowsProductRecoveryStagedV3{}
			}
			if tc.product {
				e.Bundle.Retained.Singletons = append(e.Bundle.Retained.Singletons, e.Expected.ProductRelativePath)
				j.Staged = windowsProductRecoveryStagedV3{}
			}
			if tc.identity {
				e.Bundle.Retained.Singletons = append(e.Bundle.Retained.Singletons, e.Expected.IdentityRelativePath)
			}
			sort.Strings(e.Bundle.Retained.Singletons)
			e.Bundle.Members = slices.DeleteFunc(e.Bundle.Members, func(row windowsCanonicalEntryMemberJournalV2) bool {
				return e.retainsSingleton(row.Member)
			})
			if err := validateCanonicalEntryBundleJournal(j); err != nil {
				t.Fatal("retained core originals did not validate without stage rows", err)
			}
			raw, err := json.Marshal(j)
			if err != nil {
				t.Fatal(err)
			}
			var wire struct {
				Staged   json.RawMessage `json:"staged"`
				EntrySet struct {
					StagedEntry json.RawMessage `json:"staged_entry"`
				} `json:"entry_set"`
			}
			if err := json.Unmarshal(raw, &wire); err != nil {
				t.Fatal(err)
			}
			if (string(wire.Staged) == "null") != tc.product || (string(wire.EntrySet.StagedEntry) == "null") != tc.launcher {
				t.Fatal("retained core file did not record its exact absent stage")
			}
			var received windowsProductRecoveryJournalV3
			if err := decodeStrictCanonicalProductRecord(raw, &received); err != nil {
				t.Fatal("selective core wire did not receive its absent stages", err)
			}
			if err := validateCanonicalEntryBundleJournal(received); err != nil {
				t.Fatal("selective core wire lost its exact original proof", err)
			}
			if _, err := decodeWindowsProductRecoveryJournalV3(raw); !errors.Is(err, errCurrentEntryRuntimeRetentionUnavailable) {
				t.Fatal("cold core retention failed to keep native recovery pending", err)
			}
			if _, err := encodeWindowsProductRecoveryJournalV3(received); !errors.Is(err, errCurrentEntryRuntimeRetentionUnavailable) {
				t.Fatal("warm core retention reached the writer without native authority", err)
			}

			if tc.launcher {
				received.EntryBundle.CanonicalEntry.PriorSHA256 = strings.Repeat("0", 64)
			} else {
				received.Product.PriorSHA256 = strings.Repeat("0", 64)
			}
			if err := validateCanonicalEntryBundleJournal(received); err == nil {
				t.Fatal("retained core accepted a different original digest")
			}

			if tc.product {
				j.Staged = old.Staged
			} else {
				e.StagedEntry = old.EntryBundle.StagedEntry
			}
			if _, err := json.Marshal(j); err == nil {
				t.Fatal("retained core accepted a fabricated staged path")
			}
		})
	}
	if !bytes.Equal(mustReadFile(t, journalPath), before) {
		t.Fatal("refused journal admission changed the exact prepared record")
	}
}

func v2TxnAssertInverseDiagnosticAfterPrefix(t *testing.T, ctx context.Context, txn *WindowsProductTxn, staged map[string]binaryadmission.WindowsCanonicalEntryMemberEvidenceV2) {
	t.Helper()
	if err := txn.Prepare(); err != nil {
		t.Fatal(err)
	}
	if err := txn.promoteCurrentEntryBundle(ctx); err != nil {
		t.Fatal(err)
	}
	if err := txn.Close(); err != nil {
		t.Fatal(err)
	}

	o := txn.Opts
	journalPath := filepath.Join(o.StateDir, WindowsProductRecoveryFileLeafV3)
	journal, err := v2TxnReadFixtureJournal(journalPath)
	if err != nil {
		t.Fatal(err)
	}
	req := windowsProductRecoveryRequest{Context: ctx, JournalAllowance: o.JournalAllowance,
		StateDir: o.StateDir, CanonicalEntryPath: o.CanonicalEntryPath, ProductPath: o.ProductPath, IdentityPath: o.IdentityPath}
	s, err := prepareCurrentCanonicalRecoverySource(req, boundWindowsProductRecoveryDefaults(req), journalPath, journal, canonicalRecoveryRollback)
	if err != nil {
		t.Fatal(err)
	}
	var prior, successor *binaryadmission.WindowsCanonicalEntryRecoverySetV2
	closeOwners := func() error {
		if !s.active {
			return nil
		}
		defer func() { s.active = false }()
		return errors.Join(s.closeCurrentMembers(prior, successor), s.closeCurrentDirectories())
	}
	t.Cleanup(func() {
		if err := closeOwners(); err != nil {
			t.Error(err)
		}
	})
	prior, err = s.acquireCurrentSide(ctx, binaryadmission.WindowsCanonicalRecoveryPriorV1)
	if err != nil {
		t.Fatal(err)
	}
	successor, err = s.acquireCurrentSide(ctx, binaryadmission.WindowsCanonicalRecoverySuccessorV1)
	if err != nil {
		t.Fatal(err)
	}

	first := journal.EntryBundle.Expected.CanonicalEntryRelativePath
	second := journal.EntryBundle.Expected.ProductRelativePath
	firstAside := *currentEntryMember(journal.EntryBundle, first).SuccessorAsideRelative
	secondAside := *currentEntryMember(journal.EntryBundle, second).SuccessorAsideRelative
	private := &os.PathError{Op: "PRIVATE_ARGV", Path: "PRIVATE_PATH", Err: os.ErrPermission}
	checks := 0
	s.beforeCurrentInverse = func(context.Context) error {
		checks++
		if checks == 2 {
			return &currentInverseCheckError{check: currentInverseCheckIntentLease, cause: private}
		}
		return nil
	}
	if err := s.moveCurrentRecoveryOriginal(ctx, successor, false, first, firstAside); err != nil {
		t.Fatal(err)
	}
	err = s.moveCurrentRecoveryOriginal(ctx, successor, false, second, secondAside)
	public := committedRecoveryCommandError("E_WINDOWS_PRODUCT_RECOVERY_PENDING", "recover pending prior settlement", err)
	want := "E_WINDOWS_PRODUCT_RECOVERY_PENDING: recover pending prior settlement; recovery_result=OWNER_ERROR; inverse_stage=VERIFY_AFTER_MOVE_INTENT; inverse_moves_received=1; inverse_check=INTENT_RESERVATION"
	if checks != 2 || !errors.Is(public, private) || !errors.Is(public, os.ErrPermission) || public.Error() != want {
		t.Fatalf("inverse cause, exact progress, or safe location was lost: %v", public)
	}
	durable, err := v2TxnReadFixtureJournal(journalPath)
	if err != nil {
		t.Fatal(err)
	}
	moves := durable.EntryBundle.Moves
	firstNew := len(journal.EntryBundle.Moves)
	if len(moves) != firstNew+2 {
		t.Fatalf("expected one received result and one pending intent, got %d extra rows", len(moves)-firstNew)
	}
	a, b := moves[firstNew], moves[firstNew+1]
	if a.Member != first || !a.Attempted || !a.NativeStatusKnown || !a.Applied || !a.Readback ||
		b.Member != second || b.Attempted || b.NativeStatusKnown || b.Applied || b.Readback {
		t.Fatal("durable journal did not retain the received prefix and the unattempted next move")
	}
	if err := closeOwners(); err != nil {
		t.Fatal(err)
	}
	root := filepath.Dir(o.CanonicalEntryPath)
	for _, moved := range []struct{ member, relative string }{{first, firstAside}, {second, second}} {
		path := filepath.Join(root, filepath.FromSlash(moved.relative))
		original := staged[moved.member]
		if v2TxnFixtureIdentity(t, path) != original.NativeIdentity || v2TxnFixtureDigest(mustReadFile(t, path)) != original.File.SHA256 {
			t.Fatal("completion changed an original identity or its bytes")
		}
	}
	for _, absent := range []string{first, secondAside} {
		if err := v2TxnFixtureAbsent(filepath.Join(root, filepath.FromSlash(absent))); err != nil {
			t.Fatal(err)
		}
	}
	t.Log("one native original inverse result received, next durable intent refused before effect, next file unchanged")
}

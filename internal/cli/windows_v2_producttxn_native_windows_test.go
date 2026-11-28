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
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"golang.org/x/sys/windows"

	"mcp-local-hub/internal/api"
	"mcp-local-hub/internal/api/apitest"
	"mcp-local-hub/internal/binaryadmission"
)

type v2TxnFixtureTasks struct {
	root                         string
	currentXML, priorXML        string
	exports, rewrites, restores int
	closes, discards             int
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

func TestWindowsV2ProductTxnNativeEntryBundle(t *testing.T) {
	input, seed, binding := v2TxnFixtureInput(t)
	deadline, finite := t.Deadline()
	if !finite {
		t.Fatal("native V2 transaction fixture requires a runner deadline")
	}
	for _, phase := range []string{"wrong_or_missing_admission_has_no_effects", "launcher_last_predecision_inverse", "commit_decided_cold_forward_and_repeat"} {
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

			var prior map[string]binaryadmission.WindowsCanonicalEntryFileIdentityV2
			if phase == "launcher_last_predecision_inverse" {
				prior = v2TxnSeedPrior(t, input, dst, seed)
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

			var txnID string
			cut := errors.New("stop private V2 transaction at selected boundary")
			var observed []WindowsProductStage
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
				case WindowsProductStageCommitDecided:
					if phase == "commit_decided_cold_forward_and_repeat" {
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
			txnID = prepared.TransactionID
			_, runErr := txn.Run(ctx)
			wantOrder := []WindowsProductStage{WindowsProductStageIdentityPromoted, WindowsProductStageProductPromoted, WindowsProductStageCanonicalEntryPromoted}
			if !reflect.DeepEqual(observed, wantOrder) || txnID == "" || tasks.exports != 1 || tasks.rewrites != 1 {
				t.Fatal("real V2 publication/readback or Task rewrite was not reached", observed, runErr)
			}
			if phase == "launcher_last_predecision_inverse" {
				if !errors.Is(runErr, cut) || errors.Is(runErr, ErrWindowsProductRecoveryPending) ||
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

			if !errors.Is(runErr, ErrWindowsProductCleanupPending) || !strings.Contains(runErr.Error(), cut.Error()) ||
				tasks.restores != 0 || tasks.discards != 0 || len(events) != 0 {
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
				if v2TxnFixtureIdentity(t, path) != staged.NativeIdentity {
					t.Fatalf("commit decision did not retain the moved native source %s", member)
				}
			}

			cold := &v2TxnFixtureTasks{root: tasks.root, currentXML: tasks.currentXML}
			reopens := 0
			req := windowsProductRecoveryRequest{
				Context: ctx, JournalAllowance: allowance, StateDir: state, CanonicalEntryPath: dst.CanonicalEntryPath,
				ProductPath: dst.ProductPath, IdentityPath: dst.IdentityPath, LegacyWindowlessPath: txn.Opts.LegacyWindowlessPath,
				Legacy: windowsProductPairRecoveryRequest{Context: ctx, JournalAllowance: allowance, StateDir: state},
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

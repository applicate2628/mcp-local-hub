//go:build windows

package binaryadmission

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/sys/windows"
)

type canonicalPayloadSourceFixture struct {
	snapshot         WindowsCanonicalEntryPayloadDirectorySnapshotV1
	identityReceived bool
	disposeIntent    bool
	validationError  error
}

func (s *canonicalPayloadSourceFixture) ValidateOriginalRecovery(ctx context.Context) error {
	return errors.Join(ctx.Err(), s.validationError)
}
func (s *canonicalPayloadSourceFixture) WithOriginalPayloadDirectoryRecoveryInput(ctx context.Context, receive func(WindowsCanonicalEntryPayloadDirectorySnapshotV1) error) error {
	if err := s.ValidateOriginalRecovery(ctx); err != nil {
		return err
	}
	if !s.identityReceived {
		return errors.New("fixture has no durable original directory identity")
	}
	return receive(s.snapshot)
}
func (s *canonicalPayloadSourceFixture) WithOriginalPayloadDirectoryAbsenceInput(ctx context.Context, receive func(WindowsCanonicalEntryPayloadDirectorySnapshotV1) error) error {
	if !s.disposeIntent {
		return errors.New("fixture has no durable authorized rollback disposal intent")
	}
	return s.WithOriginalPayloadDirectoryRecoveryInput(ctx, receive)
}

type canonicalPayloadFileSourceFixture struct {
	*canonicalRecoverySourceFixture
	directory *WindowsCanonicalEntryPayloadDirectoryV1
}

func (s *canonicalPayloadFileSourceFixture) OriginalPayloadDirectory(ctx context.Context) (*WindowsCanonicalEntryPayloadDirectoryV1, error) {
	if err := s.ValidateOriginalRecovery(ctx); err != nil {
		return nil, err
	}
	return s.directory, nil
}

func canonicalPayloadStageFixture(t *testing.T) (*StagedWindowsCanonicalEntrySetV1, string) {
	t.Helper()
	targets, binding := canonicalEntrySetFixture(t)
	source, err := AdmitWindowsCanonicalEntrySetV1(context.Background(), targets.CanonicalEntryPath, binding)
	closeCanonicalEntryFixtureOwner(t, source)
	if err != nil {
		t.Fatal(err)
	}
	entry := filepath.Join(t.TempDir(), WindowsCanonicalEntryRelativePathV1)
	staged, err := source.Stage(context.Background(), entry)
	if staged != nil {
		t.Cleanup(func() {
			if err := staged.Close(); err != nil {
				t.Error(err)
			}
		})
	}
	if err != nil {
		t.Fatal(err)
	}
	return staged, entry
}

func TestOriginalPayloadParentObservationRefusesAppearedDirectory(t *testing.T) {
	ctx := context.Background()
	stage, entry := canonicalPayloadStageFixture(t)
	prior, err := stage.AcquireOriginalPriorSet(ctx)
	if prior != nil {
		t.Cleanup(func() {
			if err := prior.Close(); err != nil {
				t.Error(err)
			}
		})
	}
	if err != nil {
		t.Fatal(err)
	}
	if absent, err := prior.ObservePayloadParentAbsent(ctx); err != nil || !absent {
		t.Fatalf("original missing payload parent: absent=%t error=%v", absent, err)
	}
	payload := filepath.Join(filepath.Dir(entry), canonicalPayloadDirectoryRelativeV1())
	if err = os.Mkdir(payload, 0o700); err != nil {
		t.Fatal(err)
	}
	foreign, err := os.Stat(payload)
	if err != nil {
		t.Fatal(err)
	}
	if absent, err := prior.ObservePayloadParentAbsent(ctx); err == nil || absent {
		t.Fatalf("appeared payload parent qualified: absent=%t error=%v", absent, err)
	}
	directory, err := stage.EnsureOriginalPayloadDirectory(ctx)
	if directory != nil {
		t.Cleanup(func() {
			if err := directory.Close(); err != nil {
				t.Error(err)
			}
		})
	}
	if directory == nil || !errors.Is(err, windows.STATUS_OBJECT_NAME_COLLISION) {
		t.Fatalf("original creation did not receive a native collision: %v", err)
	}
	if snapshot, err := directory.Snapshot(); err == nil || snapshot.Created {
		t.Fatalf("collision issued created custody: snapshot=%+v error=%v", snapshot, err)
	}
	if err = errors.Join(prior.Close(), stage.Close(), directory.Close()); err != nil {
		t.Fatal(err)
	}
	after, err := os.Stat(payload)
	if err != nil || !os.SameFile(foreign, after) {
		t.Fatalf("foreign payload directory was changed: %v", err)
	}
}

func TestOriginalPayloadParentObservationDistinguishesMissingProductLeaf(t *testing.T) {
	ctx := context.Background()
	stage, entry := canonicalPayloadStageFixture(t)
	payload := filepath.Join(filepath.Dir(entry), canonicalPayloadDirectoryRelativeV1())
	if err := os.Mkdir(payload, 0o700); err != nil {
		t.Fatal(err)
	}
	prior, err := stage.AcquireOriginalPriorSet(ctx)
	if prior != nil {
		t.Cleanup(func() {
			if err := prior.Close(); err != nil {
				t.Error(err)
			}
		})
	}
	if err != nil {
		t.Fatal(err)
	}
	if present, _, _, err := prior.ReadOriginal("product"); err != nil || present {
		t.Fatalf("missing Product leaf: present=%t error=%v", present, err)
	}
	if absent, err := prior.ObservePayloadParentAbsent(ctx); err != nil || absent {
		t.Fatalf("retained original payload parent: absent=%t error=%v", absent, err)
	}
	directory, err := stage.EnsureOriginalPayloadDirectory(ctx)
	if directory != nil {
		t.Cleanup(func() {
			if err := directory.Close(); err != nil {
				t.Error(err)
			}
		})
	}
	if err != nil {
		t.Fatal(err)
	}
	if snapshot, err := directory.Snapshot(); err != nil || snapshot.Created {
		t.Fatalf("existing payload parent was not preserved: snapshot=%+v error=%v", snapshot, err)
	}
}

func TestOriginalPayloadDirectoryEmptyInverseAndBorrowCustody(t *testing.T) {
	stage, entry := canonicalPayloadStageFixture(t)
	old := []byte("original prior entry")
	if err := os.WriteFile(entry, old, 0o600); err != nil {
		t.Fatal(err)
	}
	// The prior root exists before Ensure. It must observe the SAME owner
	// through the staged reference when its discard scans the payload target.
	prior, err := stage.AcquireOriginalPriorSet(context.Background())
	if err != nil {
		if prior != nil {
			prior.Close()
		}
		t.Fatal(err)
	}
	directory, err := stage.EnsureOriginalPayloadDirectory(context.Background())
	if directory != nil {
		t.Cleanup(func() {
			if err := errors.Join(prior.Close(), stage.Close(), directory.Close()); err != nil {
				t.Error(err)
			}
		})
	}
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := directory.Snapshot()
	if err != nil || !snapshot.Created {
		t.Fatalf("created original directory snapshot=%+v error=%v", snapshot, err)
	}
	payload := filepath.Join(snapshot.Root, snapshot.RelativePath)
	if err = os.Rename(payload, payload+".foreign-swap"); err == nil {
		t.Fatal("live original directory name custody permitted foreign swap")
	}
	stages, err := stage.Targets()
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err = stage.TakeForTransaction(context.Background(), entry); err != nil {
		t.Fatal(err)
	}
	if result, err := prior.MoveOriginal(context.Background(), "launcher", ".prior-entry.aside"); err != nil || !result.Readback {
		t.Fatalf("prior displacement=%+v %v", result, err)
	}
	if result, err := prior.DiscardOriginal(context.Background(), "launcher", ".prior-entry.aside"); err != nil || !result.Readback {
		t.Fatalf("prior borrowed-directory scan/discard=%+v %v", result, err)
	}
	if result, err := stage.MoveOriginal(context.Background(), "product", WindowsCanonicalProductRelativePathV1); err != nil || !result.Readback {
		t.Fatalf("move through original RootDirectory=%+v %v", result, err)
	}
	if err = directory.Close(); err == nil {
		t.Fatal("directory Close ignored outstanding actual child borrowers")
	}
	if result, err := directory.DisposeOriginalIfEmpty(context.Background()); err == nil || result.Attempted || result.Applied {
		t.Fatalf("directory disposal ignored child borrowers: %+v %v", result, err)
	}
	if result, err := stage.MoveOriginal(context.Background(), "product", filepath.Base(stages.ProductPath)); err != nil || !result.Readback {
		t.Fatalf("withdraw original product=%+v %v", result, err)
	}
	for _, member := range []struct{ name, path string }{{"launcher", stages.CanonicalEntryPath}, {"product", stages.ProductPath}, {"identity", stages.IdentityPath}} {
		if result, err := stage.DiscardOriginal(context.Background(), member.name, filepath.Base(member.path)); err != nil || !result.Readback {
			t.Fatalf("settle original stage=%+v %v", result, err)
		}
	}
	if err = errors.Join(prior.Close(), stage.Close()); err != nil {
		t.Fatal(err)
	}
	result, err := directory.DisposeOriginalIfEmpty(context.Background())
	if err != nil || !result.Applied || !result.Readback || result.CloseError != nil {
		t.Fatalf("native empty-directory inverse=%+v error=%v", result, err)
	}
	if _, err = os.Stat(payload); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("disposed original directory still present: %v", err)
	}
	if err = directory.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestPayloadDirectoryColdReacquisitionPreservesForeignEmpty(t *testing.T) {
	stage, _ := canonicalPayloadStageFixture(t)
	directory, err := stage.EnsureOriginalPayloadDirectory(context.Background())
	if directory != nil {
		t.Cleanup(func() {
			if err := errors.Join(stage.Close(), directory.Close()); err != nil {
				t.Error(err)
			}
		})
	}
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := directory.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	if err = errors.Join(stage.Close(), directory.Close()); err != nil {
		t.Fatal(err)
	} // Close never deletes.
	payload := filepath.Join(snapshot.Root, snapshot.RelativePath)
	if err = os.Rename(payload, payload+".original-retained"); err != nil {
		t.Fatal(err)
	}
	if err = os.Mkdir(payload, 0o700); err != nil {
		t.Fatal(err)
	}
	foreign, err := os.Stat(payload)
	if err != nil {
		t.Fatal(err)
	}
	source := &canonicalPayloadSourceFixture{snapshot: snapshot, identityReceived: true, disposeIntent: true}
	recovered, err := AcquireWindowsCanonicalEntryPayloadDirectoryForRecoveryV1(context.Background(), source)
	if recovered == nil || err == nil {
		t.Fatalf("foreign empty directory became original-created ownership: %v %v", recovered, err)
	}
	if err = recovered.Close(); err != nil {
		t.Fatal(err)
	}
	after, err := os.Stat(payload)
	if err != nil || !os.SameFile(foreign, after) {
		t.Fatal("failed cold reacquisition changed foreign directory")
	}
	observation, err := ObserveWindowsCanonicalEntryPayloadDirectoryAbsenceV1(context.Background(), source)
	if err == nil || observation.Absent {
		t.Fatal("occupied foreign empty directory became absence proof")
	}
}

func TestPayloadDirectoryColdReacquiresPriorPresentWithoutDeleteAccess(t *testing.T) {
	ctx := context.Background()
	stage, entry := canonicalPayloadStageFixture(t)
	payload := filepath.Join(filepath.Dir(entry), canonicalPayloadDirectoryRelativeV1())
	if err := os.Mkdir(payload, 0o700); err != nil {
		t.Fatal(err)
	}
	before, err := os.Stat(payload)
	if err != nil {
		t.Fatal(err)
	}
	directory, err := stage.EnsureOriginalPayloadDirectory(ctx)
	if directory != nil {
		t.Cleanup(func() {
			if err := errors.Join(stage.Close(), directory.Close()); err != nil {
				t.Error(err)
			}
		})
	}
	if err != nil {
		t.Fatal(err)
	}
	expected, err := directory.Snapshot()
	if err != nil || expected.Created {
		t.Fatalf("prior-present snapshot=%+v error=%v", expected, err)
	}
	if err = errors.Join(stage.Close(), directory.Close()); err != nil {
		t.Fatal(err)
	}
	source := &canonicalPayloadSourceFixture{snapshot: expected, identityReceived: true}
	cold, err := AcquireWindowsCanonicalEntryPayloadDirectoryForRecoveryV1(ctx, source)
	if cold != nil {
		t.Cleanup(func() {
			if err := cold.Close(); err != nil {
				t.Error(err)
			}
		})
	}
	if err != nil {
		t.Fatal(err)
	}
	actual, err := cold.Snapshot()
	if err != nil || actual != expected {
		t.Fatalf("cold prior-present snapshot=%+v want=%+v error=%v", actual, expected, err)
	}
	// A probe that omits delete sharing cannot coexist with a DELETE owner.
	probe, err := canonicalEntryOpenNative(windows.Handle(cold.parent.file.Fd()), expected.RelativePath, true, windows.FILE_TRAVERSE, windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE, windows.FILE_OPEN)
	if err != nil {
		t.Fatalf("cold prior-present owner blocks an ordinary directory read: %v", err)
	}
	if err = windows.CloseHandle(probe); err != nil {
		t.Fatal(err)
	}
	if err = os.Rename(payload, payload+".swap"); err == nil {
		t.Fatal("cold prior-present owner lost directory name custody")
	}
	result, err := cold.DisposeOriginalIfEmpty(ctx)
	if err == nil || result.Attempted || result.Applied {
		t.Fatalf("cold prior-present owner attempted deletion: %+v %v", result, err)
	}
	if err = cold.Close(); err != nil {
		t.Fatal(err)
	}
	after, err := os.Stat(payload)
	if err != nil || !os.SameFile(before, after) {
		t.Fatalf("cold close failed to preserve the original directory: %v", err)
	}
}

func TestPayloadDirectoryPriorPresentAndNonemptyRemainPreserved(t *testing.T) {
	for _, mode := range []string{"prior-present", "created-nonempty"} {
		t.Run(mode, func(t *testing.T) {
			stage, entry := canonicalPayloadStageFixture(t)
			payload := filepath.Join(filepath.Dir(entry), canonicalPayloadDirectoryRelativeV1())
			if mode == "prior-present" {
				if err := os.Mkdir(payload, 0o700); err != nil {
					t.Fatal(err)
				}
			}
			directory, err := stage.EnsureOriginalPayloadDirectory(context.Background())
			if directory != nil {
				t.Cleanup(func() {
					if err := errors.Join(stage.Close(), directory.Close()); err != nil {
						t.Error(err)
					}
				})
			}
			if err != nil {
				t.Fatal(err)
			}
			child := filepath.Join(payload, "foreign-child.txt")
			body := []byte("preserve this child")
			if mode == "created-nonempty" {
				if err = os.WriteFile(child, body, 0o600); err != nil {
					t.Fatal(err)
				}
			}
			result, err := directory.DisposeOriginalIfEmpty(context.Background())
			if err == nil || result.Applied || result.Readback {
				t.Fatalf("unqualified directory disposed: %+v %v", result, err)
			}
			if mode == "prior-present" {
				if result.Attempted {
					t.Fatal("prior-present directory received native delete attempt")
				}
			} else {
				if !result.Attempted || !result.NativeStatusKnown || result.NativeStatus != uint32(windows.STATUS_DIRECTORY_NOT_EMPTY) {
					t.Fatalf("nonempty condition was not received from native enforcement: %+v", result)
				}
				actual, err := os.ReadFile(child)
				if err != nil || !bytes.Equal(actual, body) {
					t.Fatal("native nonempty refusal changed foreign child")
				}
			}
			if err = errors.Join(stage.Close(), directory.Close()); err != nil {
				t.Fatal(err)
			}
			if _, err = os.Stat(payload); err != nil {
				t.Fatal("Close alone removed preserved directory")
			}
		})
	}
}

func TestPayloadDirectoryAuthorizedAbsenceAfterDisposeCrash(t *testing.T) {
	stage, _ := canonicalPayloadStageFixture(t)
	directory, err := stage.EnsureOriginalPayloadDirectory(context.Background())
	if directory != nil {
		t.Cleanup(func() {
			if err := errors.Join(stage.Close(), directory.Close()); err != nil {
				t.Error(err)
			}
		})
	}
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := directory.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	if err = errors.Join(stage.Close(), directory.Close()); err != nil {
		t.Fatal(err)
	}
	// A cold owner must acquire the same durable object before the file owner.
	source := &canonicalPayloadSourceFixture{snapshot: snapshot, identityReceived: true, disposeIntent: true}
	cold, err := AcquireWindowsCanonicalEntryPayloadDirectoryForRecoveryV1(context.Background(), source)
	if cold != nil {
		t.Cleanup(func() {
			if err := cold.Close(); err != nil {
				t.Error(err)
			}
		})
	}
	if err != nil {
		t.Fatal(err)
	}
	aside := ".recovered-successor.aside"
	body := []byte("actual original successor object")
	if err = os.WriteFile(filepath.Join(snapshot.Root, aside), body, 0o600); err != nil {
		t.Fatal(err)
	}
	locations := WindowsCanonicalRecoveryLocationsV1{Product: WindowsCanonicalRecoveryMemberV1{true, aside, canonicalEntrySHA256(body)}}
	fileSource := &canonicalPayloadFileSourceFixture{canonicalRecoveryFixture(snapshot.Root, WindowsCanonicalRecoveryLocationsV1{}, locations), cold}
	files, err := AcquireWindowsCanonicalEntryForRecoveryV1(context.Background(), fileSource, WindowsCanonicalRecoverySuccessorV1)
	if files != nil {
		t.Cleanup(func() {
			if err := errors.Join(files.Close(), cold.Close()); err != nil {
				t.Error(err)
			}
		})
	}
	if err != nil {
		t.Fatal(err)
	}
	if result, err := files.MoveOriginal(context.Background(), "product", WindowsCanonicalProductRelativePathV1); err != nil || !result.Readback {
		t.Fatalf("cold original directory loan=%+v %v", result, err)
	}
	if result, err := files.MoveOriginal(context.Background(), "product", aside); err != nil || !result.Readback {
		t.Fatalf("cold original file inverse=%+v %v", result, err)
	}
	if result, err := files.DiscardOriginal(context.Background(), "product", aside); err != nil || !result.Readback {
		t.Fatalf("cold original file settlement=%+v %v", result, err)
	}
	if err = files.Close(); err != nil {
		t.Fatal(err)
	}
	if result, err := cold.DisposeOriginalIfEmpty(context.Background()); err != nil || !result.Readback {
		t.Fatalf("genuine original disposal before crash=%+v %v", result, err)
	}
	noIdentity := &canonicalPayloadSourceFixture{snapshot: snapshot, disposeIntent: true}
	if observation, err := ObserveWindowsCanonicalEntryPayloadDirectoryAbsenceV1(context.Background(), noIdentity); err == nil || observation.Absent {
		t.Fatal("missing durable identity became qualified absence")
	}
	noIntent := &canonicalPayloadSourceFixture{snapshot: snapshot, identityReceived: true}
	if observation, err := ObserveWindowsCanonicalEntryPayloadDirectoryAbsenceV1(context.Background(), noIntent); err == nil || observation.Absent {
		t.Fatal("missing durable disposal intent became qualified absence")
	}
	observation, err := ObserveWindowsCanonicalEntryPayloadDirectoryAbsenceV1(context.Background(), source)
	if err != nil || !observation.Absent || observation.ParentIdentity != snapshot.ParentIdentity {
		t.Fatalf("qualified readonly post-crash absence=%+v error=%v", observation, err)
	}
	// The strict native-owner constructor never turns absence into an owner.
	partial, err := AcquireWindowsCanonicalEntryPayloadDirectoryForRecoveryV1(context.Background(), source)
	if partial == nil || err == nil {
		t.Fatal("missing directory was recreated as a current native owner")
	}
	if err = partial.Close(); err != nil {
		t.Fatal(err)
	}
}

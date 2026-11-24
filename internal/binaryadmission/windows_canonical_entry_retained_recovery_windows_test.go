//go:build windows

package binaryadmission

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

type retainedRecoveryFixture struct {
	input         WindowsCanonicalEntryMembersRecoveryInputV2
	lenders       []*WindowsCanonicalEntryPayloadDirectoryV1
	validationErr error
}

func (f *retainedRecoveryFixture) ValidateOriginalRecovery(ctx context.Context) error {
	return errors.Join(ctx.Err(), f.validationErr)
}

func (f *retainedRecoveryFixture) WithOriginalMemberRecoveryInput(ctx context.Context, _ WindowsCanonicalRecoverySideV1, receive func(WindowsCanonicalEntryMembersRecoveryInputV2) error) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	return receive(f.input)
}

func (f *retainedRecoveryFixture) OriginalMemberDirectories(ctx context.Context) ([]*WindowsCanonicalEntryPayloadDirectoryV1, error) {
	return f.lenders, ctx.Err()
}

func TestColdRecoveryReceivesRetainedOriginalsReadOnly(t *testing.T) {
	ctx := t.Context()
	entry, binding, _ := v2TxnDonorEntryFixture(t)
	candidate, err := CanonicalEntryTargetsV2(entry)
	if err != nil {
		t.Fatal(err)
	}
	// Make only the inert sidecar bytes differ. Both authenticated runtime
	// components, Launcher and Product remain exact retained originals.
	identity := readRetainedFixtureFile(t, candidate.IdentityPath)
	if err := os.WriteFile(candidate.IdentityPath, append(identity, '\n'), 0600); err != nil {
		t.Fatal(err)
	}
	destination, _, _ := v2TxnDonorEntryFixture(t)
	targets, err := CanonicalEntryTargetsV2(destination)
	if err != nil {
		t.Fatal(err)
	}
	owner, err := AdmitWindowsCanonicalEntrySetV2(ctx, entry, binding)
	if owner != nil {
		t.Cleanup(func() {
			if err := owner.Close(); err != nil {
				t.Error(err)
			}
		})
	}
	if err != nil {
		t.Fatal(err)
	}

	selective, _, err := owner.StageRetainingEqualRuntime(ctx, destination)
	if selective != nil {
		t.Cleanup(func() {
			if err := selective.Close(); err != nil {
				t.Error(err)
			}
		})
	}
	if err != nil {
		t.Fatal(err)
	}
	witness, err := selective.RetainedMembersWitness(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := selective.Close(); err != nil {
		t.Fatal(err)
	}
	input := seedRetainedRecoveryFixture(t, ctx, owner, targets, witness)
	f := &retainedRecoveryFixture{input: input}
	var prior, successor *WindowsCanonicalEntryRecoverySetV2
	t.Cleanup(func() {
		if err := closeRetainedRecoveryFixture(prior, successor); err != nil {
			t.Error(err)
		}
	})
	prior, err = AcquireWindowsCanonicalEntryForRecoveryV2(ctx, f, WindowsCanonicalRecoveryPriorV1)
	if err != nil {
		t.Fatal(err)
	}
	f.lenders = prior.Directories()
	successor, err = AcquireWindowsCanonicalEntryForRecoveryV2(ctx, f, WindowsCanonicalRecoverySuccessorV1)
	if err != nil {
		t.Fatal("both live banks should be able to hold the same read-only originals", err)
	}
	for _, bank := range []*WindowsCanonicalEntryRecoverySetV2{prior, successor} {
		view, err := bank.RuntimeProduct(ctx)
		if err != nil || view == nil || view.Artifact().Path != targets.ProductPath {
			t.Fatal("retained original Product did not lend the runtime view", err)
		}
		paths, err := bank.IssuedImagePaths(ctx)
		if err != nil || !slices.Contains(paths, targets.ProductPath) || !slices.Contains(paths, targets.CanonicalEntryPath) {
			t.Fatal("retained canonical images were lost from quiescence", err)
		}
		for _, key := range witness.Singletons {
			moved, moveErr := bank.MoveOriginal(ctx, key, "refused-retained-aside.stage")
			discarded, discardErr := bank.DiscardOriginal(ctx, key, key)
			if moveErr == nil || discardErr == nil || moved.Attempted || discarded.Attempted {
				t.Fatal("read-only retained key entered a native effect", errors.Join(moveErr, discardErr))
			}
		}
	}

	// The same bank must reject a changed witness input even when the source
	// reuses its original singleton slice.
	original := f.input.Retained.Singletons[0]
	f.input.Retained.Singletons[0] = "foreign.exe"
	_, err = prior.IssuedImagePaths(ctx)
	f.input.Retained.Singletons[0] = original
	if err == nil {
		t.Fatal("changed retained input survived the stable original binding")
	}

	if err := closeRetainedRecoveryFixture(prior, successor); err != nil {
		t.Fatal(err)
	}
	prior, successor, f.lenders = nil, nil, nil

	t.Run("archive_rollback_cleanup_original", func(t *testing.T) {
		verifyRollbackCleanupArchiveCopy(t, input)
	})

	// Leave the original parked in the private fixture and insert an
	// ordinary, same-byte replacement with a different native identity.
	body := readRetainedFixtureFile(t, targets.ProductPath)
	old := filepath.Join(filepath.Dir(destination), "fixture-original-product.stage")
	if err := os.Rename(targets.ProductPath, old); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(targets.ProductPath, body, 0600); err != nil {
		t.Fatal(err)
	}
	prior, err = AcquireWindowsCanonicalEntryForRecoveryV2(ctx, f, WindowsCanonicalRecoveryPriorV1)
	if err == nil || !strings.Contains(err.Error(), "retained original identity witness changed") {
		t.Fatal("same-byte replacement did not fail the native witness", err)
	}
	result, err := prior.MoveOriginal(ctx, input.Members[0].Member, input.Members[0].Member)
	if err == nil || result.Attempted {
		t.Fatal("failed retained receiving granted a changed native effect", err)
	}
	if err := closeRetainedRecoveryFixture(prior); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(readRetainedFixtureFile(t, targets.ProductPath), body) ||
		!bytes.Equal(readRetainedFixtureFile(t, old), body) {
		t.Fatal("failed retained receiving changed fixture bytes")
	}
}

type rollbackArchiveTestWriter func([]byte) (int, error)

func (w rollbackArchiveTestWriter) Write(p []byte) (int, error) { return w(p) }

func verifyRollbackCleanupArchiveCopy(t *testing.T, input WindowsCanonicalEntryMembersRecoveryInputV2) {
	t.Helper()
	ctx := t.Context()
	input.Purpose = WindowsCanonicalRecoveryRollbackCleanupV2
	f := &retainedRecoveryFixture{input: input}
	plan := input.Members[0]
	expected := *plan.Staged
	originalPath := filepath.Join(filepath.Dir(input.Targets.CanonicalEntryPath), expected.File.Path)
	originalBytes := readRetainedFixtureFile(t, originalPath)
	run := func(side WindowsCanonicalRecoverySideV1, wantAcquire bool, check func(*WindowsCanonicalEntryRecoverySetV2)) {
		t.Helper()
		owner, err := AcquireWindowsCanonicalEntryForRecoveryV2(ctx, f, side)
		defer func() {
			if err := closeRetainedRecoveryFixture(owner); err != nil {
				t.Error(err)
			}
		}()
		if (err == nil) != wantAcquire {
			t.Fatalf("unexpected acquisition outcome: %v", err)
		}
		check(owner)
	}
	var buffer bytes.Buffer
	refused := func(owner *WindowsCanonicalEntryRecoverySetV2, want WindowsCanonicalEntryMemberEvidenceV2) {
		t.Helper()
		got, present, err := owner.CopyRollbackCleanupOriginal(ctx, want, &buffer)
		if err == nil || present || got != (WindowsCanonicalEntryMemberEvidenceV2{}) || buffer.Len() != 0 {
			t.Fatalf("archive refusal wrote or credited data: present=%v bytes=%d error=%v", present, buffer.Len(), err)
		}
	}
	run(WindowsCanonicalRecoveryPriorV1, true, func(owner *WindowsCanonicalEntryRecoverySetV2) {
		refused(owner, expected)
	})
	run(WindowsCanonicalRecoverySuccessorV1, true, func(owner *WindowsCanonicalEntryRecoverySetV2) {
		wrong := expected
		wrong.NativeIdentity = plan.Prior.NativeIdentity
		refused(owner, wrong)
		root := owner.original.original.root
		finals := root.memberFinalPaths
		root.memberFinalPaths = append(slices.Clone(finals), strings.ToUpper(expected.File.Path))
		refused(owner, expected)
		root.memberFinalPaths = finals
		got, present, err := owner.CopyRollbackCleanupOriginal(ctx, expected, &buffer)
		if err != nil || !present || got != expected || !bytes.Equal(buffer.Bytes(), originalBytes) {
			t.Fatalf("exact held original was not copied: present=%v bytes=%d error=%v", present, buffer.Len(), err)
		}
		buffer.Reset()
		got, present, err = owner.CopyRollbackCleanupOriginal(ctx, expected, rollbackArchiveTestWriter(func(p []byte) (int, error) {
			return len(p) - 1, nil
		}))
		if !errors.Is(err, io.ErrShortWrite) || present || got != (WindowsCanonicalEntryMemberEvidenceV2{}) {
			t.Fatalf("partial archive write was credited: present=%v error=%v", present, err)
		}
		marker := errors.New("fixture closing journal validation")
		got, present, err = owner.CopyRollbackCleanupOriginal(ctx, expected, rollbackArchiveTestWriter(func(p []byte) (int, error) {
			f.validationErr = marker
			return buffer.Write(p)
		}))
		f.validationErr = nil
		if !errors.Is(err, marker) || present || got != (WindowsCanonicalEntryMemberEvidenceV2{}) || !bytes.Equal(buffer.Bytes(), originalBytes) {
			t.Fatalf("archive copy with unconfirmed final validation was credited: present=%v error=%v", present, err)
		}
		buffer.Reset()
	})
	if !bytes.Equal(readRetainedFixtureFile(t, originalPath), originalBytes) {
		t.Fatal("archive transfer changed the original")
	}

	// An exact recorded native absence is received without writing any bytes.
	f.input.Cleanup = []WindowsCanonicalEntryMemberCleanupRecordV2{{
		TransactionID: input.TransactionID, Side: "unused-successor", Member: expected.Member,
		SourceRelative: expected.File.Path, ExpectedSHA256: expected.File.SHA256,
		State: "discard_intent", Purpose: input.Purpose,
	}}
	parked := filepath.Join(t.TempDir(), "archive-fixture-original")
	if err := os.Rename(originalPath, parked); err != nil {
		t.Fatal(err)
	}
	run(WindowsCanonicalRecoverySuccessorV1, true, func(owner *WindowsCanonicalEntryRecoverySetV2) {
		got, present, err := owner.CopyRollbackCleanupOriginal(ctx, expected, &buffer)
		if err != nil || present || got != (WindowsCanonicalEntryMemberEvidenceV2{}) || buffer.Len() != 0 {
			t.Fatalf("confirmed absence wrote or credited data: present=%v bytes=%d error=%v", present, buffer.Len(), err)
		}
	})
	if err := os.WriteFile(originalPath, originalBytes, 0600); err != nil {
		t.Fatal(err)
	}
	run(WindowsCanonicalRecoverySuccessorV1, false, func(owner *WindowsCanonicalEntryRecoverySetV2) {
		refused(owner, expected)
	})
	if err := os.Remove(originalPath); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(parked, originalPath); err != nil {
		t.Fatal(err)
	}
}

func readRetainedFixtureFile(t *testing.T, path string) []byte {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func seedRetainedRecoveryFixture(t *testing.T, ctx context.Context, owner *WindowsCanonicalEntrySetV2, targets WindowsCanonicalEntrySetTargetsV2, witness WindowsCanonicalEntryRetainedMembersV2) WindowsCanonicalEntryMembersRecoveryInputV2 {
	t.Helper()
	// The deployed full Stage is convenient solely for issuing real original
	// file evidence in this inert fixture. The recovery input records only
	// the one actual changed pair, without a selective transfer bypass.
	stage, err := owner.Stage(ctx, targets.CanonicalEntryPath)
	var prior *WindowsCanonicalEntryPriorSetV2
	closed := false
	closeStage := func() error {
		if closed {
			return nil
		}
		closed = true
		owners := stage.Directories()
		err := errors.Join(prior.Close(), stage.Close())
		for _, d := range owners {
			err = errors.Join(err, d.Close())
		}
		return err
	}
	if stage != nil {
		t.Cleanup(func() {
			if err := closeStage(); err != nil {
				t.Error(err)
			}
		})
	}
	if err != nil {
		t.Fatal(err)
	}
	prior, err = stage.AcquireOriginalPriorSet(ctx)
	if err != nil {
		t.Fatal(err)
	}
	before, err := prior.Observation(ctx)
	if err != nil {
		t.Fatal(err)
	}
	expected, err := stage.TakeForTransaction(ctx, targets.CanonicalEntryPath)
	if err != nil {
		t.Fatal(err)
	}
	member := expected.IdentityRelativePath
	staged, err := stage.ObserveOriginalMember(ctx, member)
	if err != nil {
		t.Fatal(err)
	}
	previous, err := prior.ObserveOriginalMember(ctx, member)
	if err != nil {
		t.Fatal(err)
	}
	oldAside, newAside := ".fixture-prior-identity.stage", ".fixture-successor-identity.stage"
	input := WindowsCanonicalEntryMembersRecoveryInputV2{
		Targets: targets, BeforeKind: before.BeforeKind, Expected: expected,
		TransactionID: strings.Repeat("1", 32), Purpose: WindowsCanonicalRecoveryRollbackV2, Retained: &witness,
		Members: []WindowsCanonicalEntryMemberRecoveryPlanV2{{Member: member, Prior: &previous, Staged: &staged, PriorAsideRelative: &oldAside, SuccessorAsideRelative: &newAside}},
	}
	for _, directory := range before.Directories {
		input.Directories = append(input.Directories, WindowsCanonicalEntryMemberDirectoryRecordV2{Original: directory, State: "retained"})
	}
	if err := closeStage(); err != nil {
		t.Fatal(err)
	}
	return input
}

func closeRetainedRecoveryFixture(banks ...*WindowsCanonicalEntryRecoverySetV2) error {
	var errs []error
	seen := make(map[*WindowsCanonicalEntryPayloadDirectoryV1]bool)
	var directories []*WindowsCanonicalEntryPayloadDirectoryV1
	for _, bank := range banks {
		for _, directory := range bank.Directories() {
			if !seen[directory] {
				seen[directory] = true
				directories = append(directories, directory)
			}
		}
		errs = append(errs, bank.Close())
	}
	for _, directory := range directories {
		errs = append(errs, directory.Close())
	}
	return errors.Join(errs...)
}

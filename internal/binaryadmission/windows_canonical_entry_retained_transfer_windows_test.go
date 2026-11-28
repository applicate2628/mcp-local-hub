//go:build windows

package binaryadmission

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestTakeRetainingForTransactionAllowsOnlyChangedOriginals(t *testing.T) {
	ctx := t.Context()
	entry, binding, _ := v2TxnDonorEntryFixture(t)
	candidate, err := CanonicalEntryTargetsV2(entry)
	if err != nil {
		t.Fatal(err)
	}
	destination, _, _ := v2TxnDonorEntryFixture(t)
	targets, err := CanonicalEntryTargetsV2(destination)
	if err != nil {
		t.Fatal(err)
	}
	// Only the inert sidecar bytes change. The Product, Launcher and runtime
	// originals genuinely stay at their canonical names.
	identity := append(readRetainedFixtureFile(t, candidate.IdentityPath), '\n')
	if err := os.WriteFile(candidate.IdentityPath, identity, 0600); err != nil {
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
	stage, prior, err := owner.StageRetainingEqualRuntime(ctx, destination)
	if stage != nil {
		t.Cleanup(func() {
			if err := stage.Close(); err != nil {
				t.Error(err)
			}
		})
	}
	if err != nil {
		t.Fatal(err)
	}
	witness, err := stage.RetainedMembersWitness(ctx)
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := stage.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	changed := snapshot.IdentityRelativePath
	if moved, err := prior.MoveOriginal(ctx, changed, "before-transfer.stage"); err == nil || moved.Attempted {
		t.Fatal("selective prior granted a native effect before custody transfer", err)
	}
	for _, tc := range []struct {
		target  string
		witness WindowsCanonicalEntryRetainedMembersV2
	}{
		{entry, witness},
		{destination, WindowsCanonicalEntryRetainedMembersV2{MembersSHA256: strings.Repeat("0", 64), Singletons: witness.Singletons}},
		{destination, WindowsCanonicalEntryRetainedMembersV2{MembersSHA256: witness.MembersSHA256, Singletons: slices.Clone(witness.Singletons[1:])}},
	} {
		if _, err := stage.TakeRetainingForTransaction(ctx, tc.target, tc.witness); err == nil || stage.Disposition().Transferred {
			t.Fatal("wrong target or witness transferred selective custody", err)
		}
	}

	got, err := stage.TakeRetainingForTransaction(ctx, destination, witness)
	if err != nil || got.Identity.SHA256 != snapshot.Identity.SHA256 || !stage.Disposition().Transferred {
		t.Fatal("exact held selective custody did not transfer", err)
	}
	if _, err := stage.TakeRetainingForTransaction(ctx, destination, witness); err == nil {
		t.Fatal("same selective custody transferred twice")
	}
	for _, key := range append(slices.Clone(witness.Singletons), "unissued.exe") {
		priorMove, me := prior.MoveOriginal(ctx, key, "refused-aside.stage")
		priorDrop, de := prior.DiscardOriginal(ctx, key, key)
		stageMove, se := stage.MoveOriginal(ctx, key, "refused-aside.stage")
		stageDrop, ce := stage.DiscardOriginal(ctx, key, key)
		if me == nil || de == nil || se == nil || ce == nil || priorMove.Attempted ||
			priorDrop.Attempted || stageMove.Attempted || stageDrop.Attempted {
			t.Fatal("retained or unissued key entered an effect", errors.Join(me, de, se, ce))
		}
	}

	// This inert native fixture exercises the original effect APIs. Actual
	// Product Prepare still owns V3 write/readback before any such call.
	aside := ".fixture-selective-prior.stage"
	old, err := prior.MoveOriginal(ctx, changed, aside)
	if err != nil || !old.Applied || !old.Readback {
		t.Fatal("changed prior was not moved through its existing owner", err)
	}
	next, err := stage.MoveOriginal(ctx, changed, changed)
	if err != nil || !next.Applied || !next.Readback {
		t.Fatal("changed stage was not moved through its existing owner", err)
	}
	discarded, err := prior.DiscardOriginal(ctx, changed, aside)
	if err != nil || !discarded.Applied || !discarded.Readback {
		t.Fatal("changed original could not use standard cleanup", err)
	}
	if err := stage.Close(); err != nil {
		t.Fatal(err)
	}
	if actual := readRetainedFixtureFile(t, targets.IdentityPath); !bytes.Equal(actual, identity) {
		t.Fatal("selected identity did not reach its exact canonical target")
	}
	if _, err := os.Lstat(filepath.Join(filepath.Dir(destination), aside)); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("standard changed-original cleanup left the aside", err)
	}
}

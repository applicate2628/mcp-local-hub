//go:build windows

package binaryadmission

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestStageForUpgradeSettlesMembersBeforeDirectoryDisposal(t *testing.T) {
	for _, transferred := range []bool{true, false} {
		name := "transferred"
		if !transferred {
			name = "before_transfer"
		}
		t.Run(name, func(t *testing.T) {
			ctx := t.Context()
			entry, binding, _ := v2TxnDonorEntryFixture(t)
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
			root := t.TempDir()
			destination := filepath.Join(root, WindowsCanonicalEntryRelativePathV1)
			if transferred {
				targets, err := CanonicalEntryTargetsV2(destination)
				if err != nil {
					t.Fatal(err)
				}
				if err := os.Mkdir(targets.BundlePath, 0700); err != nil {
					t.Fatal(err)
				}
			}
			before := retainedStageRootEntries(t, root)
			stage, prior, err := owner.StageForUpgrade(ctx, destination)
			if stage != nil {
				t.Cleanup(func() {
					if err := stage.Close(); err != nil {
						t.Error(err)
					}
				})
			}
			if err != nil || prior == nil || stage.RetainsRuntime() {
				t.Fatal("empty original target did not select full staging", err)
			}
			if transferred {
				if _, err := stage.EnsureOriginalMemberDirectories(ctx); err != nil {
					t.Fatal(err)
				}
				if _, err := stage.TakeForTransaction(ctx, destination); err != nil {
					t.Fatal(err)
				}
				// In the confirmed-unpublished case the existing coordinator
				// discards the original stage files before settling the banks.
				members, err := stage.Members()
				if err != nil {
					t.Fatal(err)
				}
				for _, member := range members {
					result, err := stage.DiscardOriginal(ctx, member.Member, member.File.Path)
					if err != nil || !result.Applied || !result.Readback {
						t.Fatal("original stage file cleanup failed", err)
					}
				}
				directories := stage.Directories()
				if len(directories) == 0 {
					t.Fatal("fixture did not issue any original directory owners")
				}
				for range 2 {
					if err := stage.CloseMembers(); err != nil {
						t.Fatal(err)
					}
				}
				if !stage.Disposition().CloseEntered || !stage.Disposition().OriginalOwnerPending ||
					!prior.Disposition().CloseEntered {
					t.Fatal("the two file banks did not settle with live directory owners")
				}
				created, retained := 0, 0
				for _, directory := range directories {
					original, err := directory.SnapshotForEntryMembersV2()
					if err != nil {
						t.Fatal("original directory was not live after member settlement", err)
					}
					if !original.Created {
						// Leave one live, prior-present lender for final Close.
						retained++
						continue
					}
					created++
					result, err := directory.DisposeOriginalIfEmpty(ctx)
					if err != nil || !result.Attempted || !result.NativeStatusKnown ||
						!result.Applied || !result.Readback || result.OriginalOwnerPending {
						t.Fatal("eligible original directory did not settle through existing disposal", err)
					}
					path := filepath.Join(root, filepath.FromSlash(original.RelativePath))
					if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
						t.Fatal("original directory absence not received", err)
					}
				}
				if created == 0 || retained == 0 || !stage.Disposition().OriginalOwnerPending {
					t.Fatal("fixture must leave a prior-present lender for final Close after original disposal")
				}
			} else if _, err := stage.TakeForTransaction(ctx, filepath.Join(root, "wrong.exe")); err == nil || stage.Disposition().Transferred {
				t.Fatal("invalid pre-transfer candidate received custody", err)
			}
			for range 2 {
				if err := stage.Close(); err != nil {
					t.Fatal(err)
				}
			}
			if !stage.Disposition().CloseEntered || stage.Disposition().OriginalOwnerPending ||
				!prior.Disposition().CloseEntered || prior.Disposition().OriginalOwnerPending ||
				!reflect.DeepEqual(before, retainedStageRootEntries(t, root)) {
				t.Fatal("final ordered close left original resources or fixture files pending")
			}
		})
	}
}

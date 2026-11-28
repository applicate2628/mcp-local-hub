//go:build windows

package binaryadmission

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestStageForUpgradeSelectsAdmittedPriorLayout(t *testing.T) {
	for _, tc := range []struct {
		name string
		kind WindowsCanonicalEntryBeforeKindV2
	}{
		{"absent", WindowsCanonicalEntryBeforeAbsentV2},
		{"root_product", WindowsCanonicalEntryBeforeRootProductV2},
		{"legacy", WindowsCanonicalEntryBeforeLegacyV2},
		{"unknown", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
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

			destination := filepath.Join(t.TempDir(), WindowsCanonicalEntryRelativePathV1)
			switch tc.kind {
			case WindowsCanonicalEntryBeforeRootProductV2:
				legacy, _ := portableCanonicalEntryBuildFixture(t)
				if err := os.WriteFile(destination, readRetainedFixtureFile(t, legacy.ProductPath), 0600); err != nil {
					t.Fatal(err)
				}
			case WindowsCanonicalEntryBeforeLegacyV2:
				legacy, _ := portableCanonicalEntryBuildFixture(t)
				destination = legacy.CanonicalEntryPath
			case "":
				if err := os.WriteFile(destination, []byte("unknown original"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			root := filepath.Dir(destination)
			before := retainedStageRootEntries(t, root)
			stage, prior, err := owner.StageForUpgrade(ctx, destination)
			if stage != nil {
				t.Cleanup(func() {
					if err := stage.Close(); err != nil {
						t.Error(err)
					}
				})
			}
			if stage == nil || prior == nil {
				t.Fatal("selector lost the originally received pair", err)
			}
			if tc.kind == "" {
				if err == nil || len(stage.original.keys) != 0 || !prior.readOnly ||
					!reflect.DeepEqual(before, retainedStageRootEntries(t, root)) {
					t.Fatal("unknown installed state entered the full staging loop", err)
				}
			} else {
				if err != nil || stage.RetainsRuntime() || prior.readOnly {
					t.Fatal("admitted non-Bundle prior did not select ordinary full custody", err)
				}
				again, err := stage.AcquireOriginalPriorSet(ctx)
				if err != nil || again != prior {
					t.Fatal("full stage replaced the initially admitted prior", err)
				}
				observed, err := prior.Observation(ctx)
				if err != nil || observed.BeforeKind != tc.kind {
					t.Fatal("wrong original prior layout", observed.BeforeKind, err)
				}
				want, _, err := canonicalMemberPlanV2(stage.original.snapshot)
				if err != nil || !reflect.DeepEqual(stage.original.keys, want) {
					t.Fatal("full stage omitted an original candidate member", err)
				}
			}
			if err := stage.Close(); err != nil {
				t.Fatal(err)
			}
			if !prior.Disposition().CloseEntered || prior.Disposition().OriginalOwnerPending ||
				!stage.Disposition().CloseEntered || stage.Disposition().OriginalOwnerPending ||
				!reflect.DeepEqual(before, retainedStageRootEntries(t, root)) {
				t.Fatal("selector did not settle its exact pair and original target entries")
			}
		})
	}
}

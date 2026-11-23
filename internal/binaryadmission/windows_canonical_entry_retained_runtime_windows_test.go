//go:build windows

package binaryadmission

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestStageRetainingEqualRuntime(t *testing.T) {
	for _, name := range []string{"equal", "different_runtime", "dirty_runtime"} {
		t.Run(name, func(t *testing.T) {
			ctx := t.Context()
			entry, binding, identity := v2TxnDonorEntryFixture(t)
			targets, err := CanonicalEntryTargetsV2(entry)
			if err != nil {
				t.Fatal(err)
			}
			// Change the inert Product bytes and rebuild their sidecar, while
			// keeping the Launcher and both authenticated runtime components.
			product, err := os.ReadFile(targets.ProductPath)
			if err != nil {
				t.Fatal(err)
			}
			product = append(product, []byte("fixture successor")...)
			if err := os.WriteFile(targets.ProductPath, product, 0600); err != nil {
				t.Fatal(err)
			}
			identity.SHA256 = RuntimeDigest(product)
			writeV2TxnDonorIdentity(t, targets.BundlePath, identity)

			directories := []string(nil)
			if name == "different_runtime" {
				directories = []string{"distinct"}
			}
			destination, _, _ := v2TxnDonorEntryFixture(t, directories...)
			installed, err := CanonicalEntryTargetsV2(destination)
			if err != nil {
				t.Fatal(err)
			}
			root := filepath.Dir(destination)
			indexBytes, err := os.ReadFile(filepath.Join(installed.BundlePath, identity.RuntimePayloadPath))
			if err != nil {
				t.Fatal(err)
			}
			payload, err := ParseRuntimePayload(indexBytes)
			if err != nil {
				t.Fatal(err)
			}
			retained := filepath.Join(installed.BundlePath, filepath.FromSlash(payload.Components[0].Root), "tool.exe")
			beforeRuntime, err := os.Stat(retained)
			if err != nil {
				t.Fatal(err)
			}
			runtimeBytes, err := os.ReadFile(retained)
			if err != nil {
				t.Fatal(err)
			}
			foreign := filepath.Join(filepath.Dir(retained), "foreign.txt")
			if name == "dirty_runtime" {
				if err := os.WriteFile(foreign, []byte("keep"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			priorNames := retainedStageRootEntries(t, root)

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
			stage, prior, stageErr := owner.StageRetainingEqualRuntime(ctx, destination)
			if stage != nil {
				t.Cleanup(func() {
					if err := stage.Close(); err != nil {
						t.Error(err)
					}
				})
			}
			if stage == nil || prior == nil {
				t.Fatal("the original received pair was lost", stageErr)
			}
			var wantError string
			switch name {
			case "different_runtime":
				wantError = "installed runtime components differ"
			case "dirty_runtime":
				wantError = "RUNTIME_UNACCOUNTED_OBJECT"
			}
			if wantError != "" {
				if stageErr == nil || !strings.Contains(stageErr.Error(), wantError) {
					t.Fatal("wrong stage refusal", stageErr)
				}
			} else {
				if stageErr != nil {
					t.Fatal(stageErr)
				}
				members, err := stage.Members()
				if err != nil {
					t.Fatal(err)
				}
				want := []string{stage.original.snapshot.ProductRelativePath, stage.original.snapshot.IdentityRelativePath}
				if len(members) != len(want) || members[0].Member != want[0] || members[1].Member != want[1] {
					t.Fatalf("physical stage should contain only Product and identity, got %d members", len(members))
				}
				if !reflect.DeepEqual(stage.original.root.memberFinalPaths, prior.original.keys) {
					t.Fatal("selective stage shortened the protected final namespace")
				}
				if _, err := stage.TakeForTransaction(ctx, destination); err == nil {
					t.Fatal("full-member transaction accepted the selective stage")
				}
				if _, err := stage.AcquireOriginalPriorSet(ctx); err == nil {
					t.Fatal("full-member preparation accepted the selective prior")
				}
				runtimeKey := filepath.ToSlash(filepath.Join(stage.original.snapshot.BundleRelativePath, payload.Components[0].Root, "tool.exe"))
				if result, err := prior.MoveOriginal(ctx, runtimeKey, "unissued.stage"); err == nil || result.Attempted {
					t.Fatal("retained prior lent native move authority", err)
				}
			}
			if stageErr != nil && (len(stage.original.keys) != 0 || !reflect.DeepEqual(priorNames, retainedStageRootEntries(t, root))) {
				t.Fatal("a refused original or runtime changed the destination")
			}
			if err := stage.Close(); err != nil {
				t.Fatal(err)
			}
			if !prior.Disposition().CloseEntered || prior.Disposition().OriginalOwnerPending ||
				!stage.Disposition().CloseEntered || stage.Disposition().OriginalOwnerPending {
				t.Fatal("prior, stage, or shared directory owner failed to settle")
			}
			after, err := os.Stat(retained)
			afterBytes, readErr := os.ReadFile(retained)
			if err != nil || readErr != nil || !os.SameFile(beforeRuntime, after) || !bytes.Equal(runtimeBytes, afterBytes) ||
				!reflect.DeepEqual(priorNames, retainedStageRootEntries(t, root)) {
				t.Fatal("a retained original or destination entry changed", errors.Join(err, readErr))
			}
			if name == "dirty_runtime" {
				if got, err := os.ReadFile(foreign); err != nil || string(got) != "keep" {
					t.Fatal("foreign runtime file was changed", err)
				}
			}
		})
	}
}

func retainedStageRootEntries(t *testing.T, root string) []string {
	t.Helper()
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, entry := range entries {
		names = append(names, entry.Name())
	}
	return names
}

//go:build windows

package cli

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/sys/windows"

	"mcp-local-hub/internal/binaryadmission"
)

func newOriginalPreflightFixture(t *testing.T, built *windowsCanonicalEntryFixture, priorKind string) *WindowsProductTxn {
	t.Helper()
	root := t.TempDir()
	targets, err := binaryadmission.CanonicalEntryTargetsV1(filepath.Join(root, binaryadmission.WindowsCanonicalEntryRelativePathV1))
	if err != nil {
		t.Fatal(err)
	}
	var copies [][2]string
	switch priorKind {
	case "absent":
	case "legacy-root":
		copies = append(copies, [2]string{built.Targets.ProductPath, targets.CanonicalEntryPath})
	case "complete":
		copies = append(copies, [2]string{built.Targets.CanonicalEntryPath, targets.CanonicalEntryPath}, [2]string{built.Targets.ProductPath, targets.ProductPath}, [2]string{built.Targets.IdentityPath, targets.IdentityPath})
	default:
		t.Fatalf("unknown prior fixture %q", priorKind)
	}
	for _, pair := range copies {
		body, err := os.ReadFile(pair[0])
		if err != nil {
			t.Fatal(err)
		}
		if err := os.MkdirAll(filepath.Dir(pair[1]), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(pair[1], body, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	stage, err := built.Original.Stage(t.Context(), targets.CanonicalEntryPath)
	source := &stagedWindowsCanonicalEntrySetV1{original: stage}
	if stage != nil {
		t.Cleanup(func() {
			if source.prior != nil {
				if err := source.prior.Close(); err != nil {
					t.Error(err)
				}
			}
			if err := stage.Close(); err != nil {
				t.Error(err)
			}
		})
	}
	if err != nil || stage == nil {
		t.Fatalf("stage qualified original set: %v", err)
	}
	paths, err := stage.Targets()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := source.acquirePrior(t.Context()); err != nil {
		t.Fatal(err)
	}
	return &WindowsProductTxn{Opts: WindowsProductTxnOpts{
		Context: t.Context(), Mode: WindowsProductPairModeUpgrade, EntrySet: source,
		CanonicalEntryPath: targets.CanonicalEntryPath, ProductPath: targets.ProductPath, IdentityPath: targets.IdentityPath,
		StagedProduct: paths.ProductPath, LegacyWindowlessPath: filepath.Join(root, "mcphub-windowless.exe"),
	}}
}

func TestWindowsProductPreflightOriginalEntrySet(t *testing.T) {
	built := loadWindowsCanonicalEntryFixture(t, "MCPHUB_CANONICAL_ENTRY_FIXTURE_REQUIRED")
	for _, tc := range []struct {
		prior       string
		disposition windowsProductTargetDispositionV1
	}{
		{"absent", windowsProductTargetCreate},
		{"legacy-root", windowsProductTargetReplace},
		{"complete", windowsProductTargetIdentical},
	} {
		t.Run(tc.prior, func(t *testing.T) {
			txn := newOriginalPreflightFixture(t, built, tc.prior)
			coldCalls := 0
			txn.Opts.Deps.AdmitProduct = func(binaryadmission.WindowsArtifact) (binaryadmission.WindowsArtifact, error) {
				coldCalls++
				return binaryadmission.WindowsArtifact{}, errors.New("cold staged admission must not replace the original owner")
			}
			// These are real retained DELETE owners: ordinary pathname readers
			// cannot acquire the same objects while this transaction owns them.
			guarded := []string{txn.Opts.StagedProduct}
			if tc.prior != "absent" {
				guarded = append(guarded, txn.Opts.CanonicalEntryPath)
			}
			for _, path := range guarded {
				file, openErr := os.Open(path)
				if file != nil {
					if err := file.Close(); err != nil {
						t.Error(err)
					}
				}
				if !errors.Is(openErr, windows.ERROR_SHARING_VIOLATION) {
					t.Fatalf("fixture did not retain its exclusive original %q: %v", path, openErr)
				}
			}
			want := windowsProductPairPreflightV1{Disposition: windowsProductPairDispositionV1{CLI: tc.disposition, Windowless: windowsProductTargetIdentical}}
			if tc.prior != "absent" {
				want.PriorCLI = windowsProductPreimageV1{Present: true, SHA256: built.Input.ProductSHA256}
			}
			for attempt := 0; attempt < 2; attempt++ {
				got, err := inspectWindowsProductPreflight(txn)
				if err != nil || got != want || coldCalls != 0 {
					t.Fatalf("original preflight attempt %d: got=%+v want=%+v cold=%d err=%v", attempt, got, want, coldCalls, err)
				}
				if err := txn.Opts.EntrySet.original.VerifyUnchanged(); err != nil {
					t.Fatalf("preflight released or changed the original stage: %v", err)
				}
			}
		})
	}
	t.Run("refuses-unbound-or-unavailable-originals", func(t *testing.T) {
		txn := newOriginalPreflightFixture(t, built, "complete")
		coldCalls := 0
		txn.Opts.Deps.AdmitProduct = func(binaryadmission.WindowsArtifact) (binaryadmission.WindowsArtifact, error) {
			coldCalls++
			return binaryadmission.WindowsArtifact{}, errors.New("cold admission fallback is forbidden")
		}
		cancelled, cancel := context.WithCancel(t.Context())
		cancel()
		for _, tc := range []struct {
			name   string
			change func(*WindowsProductTxnOpts)
		}{
			{"nil-context", func(o *WindowsProductTxnOpts) { o.Context = nil }},
			{"cancelled-context", func(o *WindowsProductTxnOpts) { o.Context = cancelled }},
			{"missing-stage", func(o *WindowsProductTxnOpts) { o.EntrySet = &stagedWindowsCanonicalEntrySetV1{} }},
			{"foreign-stage", func(o *WindowsProductTxnOpts) { o.StagedProduct += ".foreign" }},
			{"foreign-product", func(o *WindowsProductTxnOpts) { o.ProductPath += ".foreign" }},
			{"foreign-entry", func(o *WindowsProductTxnOpts) { o.CanonicalEntryPath += ".foreign" }},
			{"foreign-identity", func(o *WindowsProductTxnOpts) { o.IdentityPath += ".foreign" }},
		} {
			t.Run(tc.name, func(t *testing.T) {
				changed := &WindowsProductTxn{Opts: txn.Opts}
				tc.change(&changed.Opts)
				if _, err := inspectWindowsProductPreflight(changed); err == nil || coldCalls != 0 {
					t.Fatalf("unbound original accepted or cold fallback used: cold=%d err=%v", coldCalls, err)
				}
			})
		}
		if _, err := inspectWindowsProductPreflight(txn); err != nil {
			t.Fatalf("failed inspections changed original ownership: %v", err)
		}
		if err := txn.Opts.EntrySet.prior.Close(); err != nil {
			t.Fatal(err)
		}
		if _, err := inspectWindowsProductPreflight(txn); err == nil || coldCalls != 0 {
			t.Fatalf("closed prior owner accepted or reopened: cold=%d err=%v", coldCalls, err)
		}
		if err := txn.Opts.EntrySet.original.Close(); err != nil {
			t.Fatal(err)
		}
		if _, err := inspectWindowsProductPreflight(txn); err == nil || coldCalls != 0 {
			t.Fatalf("closed staged owner accepted or reopened: cold=%d err=%v", coldCalls, err)
		}
	})
}

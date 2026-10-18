package binaryadmission

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

type publisherPreparationFixture struct {
	request        PublisherSourcePreparationRequest
	base, index    []byte
	beforeObserve  func(int)
	observations   int
	observedCommit string
}

func newPublisherPreparationFixture(t *testing.T) *publisherPreparationFixture {
	t.Helper()
	parent := t.TempDir()
	checkout := filepath.Join(parent, "checkout")
	if err := os.Mkdir(checkout, 0700); err != nil {
		t.Fatal(err)
	}
	raw := []byte("module example.invalid/source\r\n")
	if err := os.WriteFile(filepath.Join(checkout, "go.mod"), raw, 0600); err != nil {
		t.Fatal(err)
	}
	oid := publisherGitBlob(raw)
	return &publisherPreparationFixture{
		request: PublisherSourcePreparationRequest{Checkout: checkout, ExpectedCommit: strings.Repeat("a", 40), Export: filepath.Join(parent, "export"), Snapshot: filepath.Join(parent, "snapshot.json")},
		base:    []byte("100644 blob " + oid + "\tgo.mod\x00"),
		index:   []byte("100644 " + oid + " 0\tgo.mod\x00"),
	}
}

func (f *publisherPreparationFixture) git(_ context.Context, _ string, args ...string) ([]byte, error) {
	switch strings.Join(args, " ") {
	case "rev-parse --show-toplevel":
		f.observations++
		if f.beforeObserve != nil {
			f.beforeObserve(f.observations)
		}
		return []byte(f.request.Checkout + "\n"), nil
	case "rev-parse --verify HEAD":
		commit := f.request.ExpectedCommit
		if f.observedCommit != "" {
			commit = f.observedCommit
		}
		return []byte(commit + "\n"), nil
	case "rev-parse --verify " + f.request.ExpectedCommit + "^{tree}", "rev-parse --verify " + f.observedCommit + "^{tree}":
		return []byte(strings.Repeat("b", 40) + "\n"), nil
	case "ls-tree -r -z --full-tree " + strings.Repeat("b", 40):
		return f.base, nil
	case "ls-files --stage -z --cached":
		return f.index, nil
	default:
		return nil, fmt.Errorf("unexpected Git operation %q", args)
	}
}

func TestPreparePublisherSourceCannotEmitMixedCommitTreeWhenHeadSwitchesWithinBothObservations(t *testing.T) {
	f := newPublisherPreparationFixture(t)
	commitA, commitB := f.request.ExpectedCommit, strings.Repeat("c", 40)
	treeA, treeB := strings.Repeat("b", 40), strings.Repeat("d", 40)
	head := commitA
	immutableTreeReads := 0
	run := func(ctx context.Context, checkout string, args ...string) ([]byte, error) {
		switch strings.Join(args, " ") {
		case "rev-parse --show-toplevel":
			// Repeat the same A -> B schedule in both acquisition cycles, so
			// comparing two equally mixed tuples cannot detect this defect.
			head = commitA
		case "rev-parse --verify HEAD":
			observed := head
			head = commitB
			return []byte(observed + "\n"), nil
		case "rev-parse --verify HEAD^{tree}":
			if head != commitB {
				t.Fatal("HEAD-switch control did not run")
			}
			return []byte(treeB + "\n"), nil
		case "rev-parse --verify " + commitA + "^{tree}":
			if head != commitB {
				t.Fatal("HEAD did not move between commit and tree reads")
			}
			immutableTreeReads++
			return []byte(treeA + "\n"), nil
		case "ls-tree -r -z --full-tree " + treeB:
			// Stable selected/index bytes are compatible with either observed
			// base relation; they do not prove commit -> tree ancestry.
			return f.base, nil
		}
		return f.git(ctx, checkout, args...)
	}
	var output bytes.Buffer
	if err := preparePublisherSource(context.Background(), f.request, &output, run); err != nil {
		t.Fatal(err)
	}
	var row publisherPreparationResult
	if err := json.Unmarshal(output.Bytes(), &row); err != nil {
		t.Fatal(err)
	}
	if immutableTreeReads != 2 || f.observations != 2 || row.GitCommit != commitA || row.GitTree != treeA {
		t.Fatalf("mixed commit/tree admitted: immutable_reads=%d observations=%d row=%s", immutableTreeReads, f.observations, output.Bytes())
	}
}

func TestPreparePublisherSourceUsesExistingIssuerAndActualGeneratedBytes(t *testing.T) {
	for _, dirty := range []bool{false, true} {
		t.Run(fmt.Sprint(dirty), func(t *testing.T) {
			f := newPublisherPreparationFixture(t)
			want := []byte("module example.invalid/source\r\n")
			if dirty {
				want = []byte("module example.invalid/changed\r\n")
				if err := os.WriteFile(filepath.Join(f.request.Checkout, "go.mod"), want, 0600); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(f.request.Checkout, "generated.json"), []byte("{}\r\n"), 0600); err != nil {
					t.Fatal(err)
				}
				f.request.GeneratedSource = []string{"go.mod", "generated.json"}
			}
			var output bytes.Buffer
			if err := preparePublisherSource(context.Background(), f.request, &output, f.git); err != nil {
				t.Fatal(err)
			}
			var row publisherPreparationResult
			if err := json.Unmarshal(output.Bytes(), &row); err != nil {
				t.Fatal(err)
			}
			if row.Schema != "windows-publisher-source-preparation-v1" || row.GitCommit != f.request.ExpectedCommit || row.GitTree != strings.Repeat("b", 40) || row.DirtySelectedSource != dirty || row.SourceInput.Schema != "windows-publisher-source-input-v1" {
				t.Fatalf("unexpected row: %s", output.Bytes())
			}
			snapshot, err := os.ReadFile(f.request.Snapshot)
			if err != nil {
				t.Fatal(err)
			}
			verified, err := VerifyPublisherSourceSnapshot(context.Background(), f.request.Export, snapshot)
			if err != nil {
				t.Fatal(err)
			}
			if verified != row.SourceInput.PublisherSourceInput {
				t.Fatal("preparation and existing issuer differ")
			}
			actual, err := os.ReadFile(filepath.Join(f.request.Export, "go.mod"))
			if err != nil || !bytes.Equal(actual, want) {
				t.Fatalf("exported bytes=%q err=%v", actual, err)
			}
			var fields map[string]json.RawMessage
			if err := json.Unmarshal(output.Bytes(), &fields); err != nil {
				t.Fatal(err)
			}
			if len(fields) != 5 || fields["dirty_selected_source"] == nil || output.Bytes()[output.Len()-1] != '\n' {
				t.Fatalf("wire shape: %s", output.Bytes())
			}
		})
	}
}

func TestPreparePublisherSourceRefusesInvalidInputsWithoutSuccess(t *testing.T) {
	for _, scenario := range []string{"commit", "commit mismatch", "duplicate", "case", "directory", "traversal", "empty", "glob", "overlap", "snapshot overlap", "existing", "unmerged", "link", "git drift", "head drift", "source drift", "cancel"} {
		t.Run(scenario, func(t *testing.T) {
			f := newPublisherPreparationFixture(t)
			ctx := context.Background()
			switch scenario {
			case "commit":
				f.request.ExpectedCommit = "bad"
			case "commit mismatch":
				f.observedCommit = strings.Repeat("c", 40)
			case "duplicate":
				f.request.GeneratedSource = []string{"go.mod", "go.mod"}
			case "case":
				f.request.GeneratedSource = []string{"GO.MOD"}
			case "directory":
				f.request.GeneratedSource = []string{"dir"}
				if err := os.Mkdir(filepath.Join(f.request.Checkout, "dir"), 0700); err != nil {
					t.Fatal(err)
				}
			case "traversal":
				f.request.GeneratedSource = []string{"../outside"}
			case "empty":
				f.request.GeneratedSource = []string{""}
			case "glob":
				f.request.GeneratedSource = []string{"*.go"}
			case "overlap":
				f.request.Export = filepath.Join(f.request.Checkout, "export")
			case "snapshot overlap":
				f.request.Snapshot = filepath.Join(f.request.Export, "snapshot.json")
			case "existing":
				if err := os.WriteFile(f.request.Snapshot, []byte("preserve"), 0600); err != nil {
					t.Fatal(err)
				}
			case "unmerged":
				f.index = bytes.ReplaceAll(f.index, []byte(" 0\t"), []byte(" 2\t"))
			case "link":
				linked := filepath.Join(f.request.Checkout, "linked")
				if err := os.Symlink("go.mod", linked); err != nil {
					t.Skipf("symlink unavailable: %v", err)
				}
				f.request.GeneratedSource = []string{"linked"}
			case "git drift":
				f.beforeObserve = func(n int) {
					if n == 2 {
						f.index = append(f.index, []byte("100644 "+strings.Repeat("c", 40)+" 0\tadded\x00")...)
					}
				}
			case "head drift":
				f.beforeObserve = func(n int) {
					if n == 2 {
						f.observedCommit = strings.Repeat("c", 40)
					}
				}
			case "source drift":
				f.beforeObserve = func(n int) {
					if n == 2 {
						if err := os.WriteFile(filepath.Join(f.request.Checkout, "go.mod"), []byte("changed"), 0600); err != nil {
							t.Fatal(err)
						}
					}
				}
			case "cancel":
				var cancel context.CancelFunc
				ctx, cancel = context.WithCancel(ctx)
				cancel()
			}
			var output bytes.Buffer
			if err := preparePublisherSource(ctx, f.request, &output, f.git); err == nil {
				t.Fatal("invalid preparation succeeded")
			}
			if output.Len() != 0 {
				t.Fatalf("unexpected success bytes: %s", output.Bytes())
			}
			if _, err := os.Lstat(f.request.Export); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("owned partial export remains: %v", err)
			}
			if scenario == "existing" {
				raw, err := os.ReadFile(f.request.Snapshot)
				if err != nil || string(raw) != "preserve" {
					t.Fatalf("prior target changed: %q %v", raw, err)
				}
			} else if _, err := os.Lstat(f.request.Snapshot); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("owned partial snapshot remains: %v", err)
			}
		})
	}
}

type publisherFailWriter struct{ err error }

func (w publisherFailWriter) Write([]byte) (int, error) { return 0, w.err }

type publisherShortWriter struct{}

func (publisherShortWriter) Write([]byte) (int, error) { return 0, nil }

func TestPreparePublisherSourceOutputFailureRollsBackOwnedLeaves(t *testing.T) {
	for _, writer := range []io.Writer{publisherFailWriter{errors.New("output failed")}, publisherShortWriter{}} {
		f := newPublisherPreparationFixture(t)
		if err := os.MkdirAll(filepath.Join(f.request.Checkout, "generated", "nested"), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(f.request.Checkout, "generated", "nested", "asset"), []byte("asset"), 0600); err != nil {
			t.Fatal(err)
		}
		f.request.GeneratedSource = []string{"generated/nested/asset"}
		if err := preparePublisherSource(context.Background(), f.request, writer, f.git); err == nil {
			t.Fatal("output failure succeeded")
		}
		for _, target := range []string{f.request.Export, f.request.Snapshot} {
			if _, err := os.Lstat(target); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("owned output remains: %s: %v", target, err)
			}
		}
	}
}

func TestPreparePublisherSourcePreservesExecutableMode(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows has no executable permission bits")
	}
	f := newPublisherPreparationFixture(t)
	if err := os.Chmod(filepath.Join(f.request.Checkout, "go.mod"), 0700); err != nil {
		t.Fatal(err)
	}
	f.base = bytes.ReplaceAll(f.base, []byte("100644"), []byte("100755"))
	f.index = bytes.ReplaceAll(f.index, []byte("100644"), []byte("100755"))
	var output bytes.Buffer
	if err := preparePublisherSource(context.Background(), f.request, &output, f.git); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(filepath.Join(f.request.Export, "go.mod"))
	if err != nil || info.Mode().Perm()&0111 == 0 {
		t.Fatalf("executable mode lost: %v", err)
	}
}

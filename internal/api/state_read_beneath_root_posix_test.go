//go:build !windows

package api

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestReadStateFileBeneathRootNoFollowPOSIXReadsVerifiedFinalHandle(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "pins")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	want := []byte(`{"mcpServers":{"serena":{"url":"http://127.0.0.1:9137/serena/mcp"}}}`)
	if err := os.WriteFile(filepath.Join(dir, "baseline.json"), want, 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := ReadStateFileBeneathRootNoFollow(context.Background(), root, []string{"pins", "baseline.json"}, stateReadDigest(want))
	if err != nil {
		t.Fatalf("read verified pin: %v", err)
	}
	if string(got) != string(want) {
		t.Fatalf("bytes=%q, want %q", got, want)
	}
}

func TestReadStateFileBeneathRootNoFollowPOSIXSecurityMatrix(t *testing.T) {
	t.Run("root-symlink", func(t *testing.T) {
		outside := t.TempDir()
		if err := os.Mkdir(filepath.Join(outside, "pins"), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(outside, "pins", "baseline.json"), []byte(`{}`), 0o600); err != nil {
			t.Fatal(err)
		}
		root := filepath.Join(t.TempDir(), "root-link")
		if err := os.Symlink(outside, root); err != nil {
			t.Fatal(err)
		}
		_, err := ReadStateFileBeneathRootNoFollow(context.Background(), root, []string{"pins", "baseline.json"}, stateReadDigest([]byte(`{}`)))
		requireStateReadCategory(t, err, StateFileReadErrorUnsafeObjectOrIO)
	})

	t.Run("intermediate-symlink", func(t *testing.T) {
		root := t.TempDir()
		outside := t.TempDir()
		if err := os.WriteFile(filepath.Join(outside, "baseline.json"), []byte(`{}`), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(outside, filepath.Join(root, "pins")); err != nil {
			t.Fatal(err)
		}
		_, err := ReadStateFileBeneathRootNoFollow(context.Background(), root, []string{"pins", "baseline.json"}, stateReadDigest([]byte(`{}`)))
		requireStateReadCategory(t, err, StateFileReadErrorUnsafeObjectOrIO)
	})

	t.Run("final-symlink", func(t *testing.T) {
		root := t.TempDir()
		outside := filepath.Join(t.TempDir(), "outside.json")
		if err := os.WriteFile(outside, []byte(`{}`), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(outside, filepath.Join(root, "baseline.json")); err != nil {
			t.Fatal(err)
		}
		_, err := ReadStateFileBeneathRootNoFollow(context.Background(), root, []string{"baseline.json"}, stateReadDigest([]byte(`{}`)))
		requireStateReadCategory(t, err, StateFileReadErrorUnsafeObjectOrIO)
	})

	t.Run("directory-as-final", func(t *testing.T) {
		root := t.TempDir()
		if err := os.Mkdir(filepath.Join(root, "directory-final"), 0o700); err != nil {
			t.Fatal(err)
		}
		_, err := ReadStateFileBeneathRootNoFollow(context.Background(), root, []string{"directory-final"}, stateReadDigest(nil))
		requireStateReadCategory(t, err, StateFileReadErrorUnsafeObjectOrIO)
	})

	t.Run("cancellation", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		_, err := ReadStateFileBeneathRootNoFollow(ctx, t.TempDir(), []string{"missing.json"}, stateReadDigest(nil))
		requireStateReadCategory(t, err, StateFileReadErrorCanceled)
	})

	t.Run("mid-read-cancellation", func(t *testing.T) {
		root := t.TempDir()
		payload := bytes.Repeat([]byte("x"), stateReadChunkSize+1)
		if err := os.WriteFile(filepath.Join(root, "baseline.json"), payload, 0o600); err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		triggered := false
		_, err := readStateFileBeneathRootNoFollow(ctx, root, []string{"baseline.json"}, stateReadDigest(payload), func(event stateReadBeneathRootStep) error {
			if event.Event == stateReadBeneathRootBeforeRead && !triggered {
				triggered = true
				cancel()
			}
			return nil
		})
		if !triggered {
			t.Fatal("mid-read cancellation seam did not run")
		}
		requireStateReadCategory(t, err, StateFileReadErrorCanceled)
	})

	t.Run("injected-step-preserves-cause", func(t *testing.T) {
		root := t.TempDir()
		payload := []byte(`{}`)
		if err := os.WriteFile(filepath.Join(root, "baseline.json"), payload, 0o600); err != nil {
			t.Fatal(err)
		}
		sentinel := errors.New("injected reader step failure")
		_, err := readStateFileBeneathRootNoFollow(context.Background(), root, []string{"baseline.json"}, stateReadDigest(payload), func(stateReadBeneathRootStep) error {
			return sentinel
		})
		requireStateReadCategory(t, err, StateFileReadErrorUnsafeObjectOrIO)
		if !errors.Is(err, sentinel) {
			t.Fatalf("injected failure lost its cause: %v", err)
		}
	})

	t.Run("checksum-mismatch", func(t *testing.T) {
		root := t.TempDir()
		if err := os.WriteFile(filepath.Join(root, "baseline.json"), []byte(`{"actual":true}`), 0o600); err != nil {
			t.Fatal(err)
		}
		_, err := ReadStateFileBeneathRootNoFollow(context.Background(), root, []string{"baseline.json"}, stateReadDigest([]byte(`{"expected":true}`)))
		requireStateReadCategory(t, err, StateFileReadErrorChecksumMismatch)
	})

	t.Run("oversize", func(t *testing.T) {
		root := t.TempDir()
		payload := bytes.Repeat([]byte("x"), maxStateFileBytes+1)
		if err := os.WriteFile(filepath.Join(root, "baseline.json"), payload, 0o600); err != nil {
			t.Fatal(err)
		}
		_, err := ReadStateFileBeneathRootNoFollow(context.Background(), root, []string{"baseline.json"}, stateReadDigest(payload))
		requireStateReadCategory(t, err, StateFileReadErrorTooLarge)
	})

	t.Run("component-swap", func(t *testing.T) {
		root := t.TempDir()
		pins := filepath.Join(root, "pins")
		if err := os.Mkdir(pins, 0o700); err != nil {
			t.Fatal(err)
		}
		outside := t.TempDir()
		if err := os.WriteFile(filepath.Join(outside, "baseline.json"), []byte(`{}`), 0o600); err != nil {
			t.Fatal(err)
		}
		swapped := false
		_, err := readStateFileBeneathRootNoFollow(context.Background(), root, []string{"pins", "baseline.json"}, stateReadDigest([]byte(`{}`)), func(event stateReadBeneathRootStep) error {
			if event.Event != stateReadBeneathRootBeforeComponentOpen || event.Component != "pins" || swapped {
				return nil
			}
			swapped = true
			if err := os.Rename(pins, filepath.Join(root, "pins-old")); err != nil {
				return err
			}
			return os.Symlink(outside, pins)
		})
		if !swapped {
			t.Fatal("component-swap hook did not run")
		}
		requireStateReadCategory(t, err, StateFileReadErrorUnsafeObjectOrIO)
	})

	t.Run("growth-at-cap-uses-one-byte-sentinel", func(t *testing.T) {
		root := t.TempDir()
		path := filepath.Join(root, "baseline.json")
		payload := bytes.Repeat([]byte("x"), maxStateFileBytes)
		if err := os.WriteFile(path, payload, 0o600); err != nil {
			t.Fatal(err)
		}
		grew := false
		sawSentinel := false
		var requests []int
		_, err := readStateFileBeneathRootNoFollow(context.Background(), root, []string{"baseline.json"}, stateReadDigest(payload), func(event stateReadBeneathRootStep) error {
			if event.Event != stateReadBeneathRootBeforeRead {
				return nil
			}
			requests = append(requests, event.Requested)
			if event.Requested == 1 {
				sawSentinel = true
			}
			if grew {
				return nil
			}
			grew = true
			return os.WriteFile(path, append(payload, 'y'), 0o600)
		})
		requireStateReadCategory(t, err, StateFileReadErrorTooLarge)
		if !grew || !sawSentinel {
			t.Fatalf("growth=%v sentinel=%v, want a bounded one-byte over-cap read", grew, sawSentinel)
		}
		for _, requested := range requests {
			if requested <= 0 || requested > stateReadChunkSize {
				t.Fatalf("read request=%d, want 1..%d", requested, stateReadChunkSize)
			}
		}
	})
}

func TestReadStateFileBeneathRootNoFollowPOSIXClosesDescriptorsOnEveryReturn(t *testing.T) {
	for _, test := range []struct {
		name     string
		expected []byte
	}{
		{name: "success", expected: []byte(`{"ok":true}`)},
		{name: "checksum-failure", expected: []byte(`{"other":true}`)},
	} {
		t.Run(test.name, func(t *testing.T) {
			root := t.TempDir()
			path := filepath.Join(root, "baseline.json")
			payload := []byte(`{"ok":true}`)
			if err := os.WriteFile(path, payload, 0o600); err != nil {
				t.Fatal(err)
			}
			_, _ = ReadStateFileBeneathRootNoFollow(context.Background(), root, []string{"baseline.json"}, stateReadDigest(test.expected))
			renamed := filepath.Join(root, "baseline-renamed.json")
			if err := os.Rename(path, renamed); err != nil {
				t.Fatalf("reader retained a final descriptor after %s: %v", test.name, err)
			}
			if err := os.Remove(renamed); err != nil {
				t.Fatalf("reader retained a final descriptor after %s: %v", test.name, err)
			}
		})
	}
}

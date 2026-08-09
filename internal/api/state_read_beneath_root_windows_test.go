//go:build windows

package api

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestReadStateFileBeneathRootNoFollowWindowsReadsVerifiedFinalHandle(t *testing.T) {
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
	if got == nil {
		t.Fatal("verified empty/non-empty result must always be non-nil")
	}
}

func TestReadStateFileBeneathRootNoFollowWindowsSecurityMatrix(t *testing.T) {
	t.Run("root-reparse", func(t *testing.T) {
		outside := t.TempDir()
		if err := os.Mkdir(filepath.Join(outside, "pins"), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(outside, "pins", "baseline.json"), []byte(`{}`), 0o600); err != nil {
			t.Fatal(err)
		}
		root := filepath.Join(t.TempDir(), "root-link")
		windowsStateReadSymlink(t, outside, root)
		_, err := ReadStateFileBeneathRootNoFollow(context.Background(), root, []string{"pins", "baseline.json"}, stateReadDigest([]byte(`{}`)))
		requireStateReadCategory(t, err, StateFileReadErrorUnsafeObjectOrIO)
	})

	t.Run("intermediate-reparse", func(t *testing.T) {
		root := t.TempDir()
		outside := t.TempDir()
		if err := os.WriteFile(filepath.Join(outside, "baseline.json"), []byte(`{}`), 0o600); err != nil {
			t.Fatal(err)
		}
		windowsStateReadSymlink(t, outside, filepath.Join(root, "pins"))
		_, err := ReadStateFileBeneathRootNoFollow(context.Background(), root, []string{"pins", "baseline.json"}, stateReadDigest([]byte(`{}`)))
		requireStateReadCategory(t, err, StateFileReadErrorUnsafeObjectOrIO)
	})

	t.Run("final-reparse", func(t *testing.T) {
		root := t.TempDir()
		outside := filepath.Join(t.TempDir(), "outside.json")
		if err := os.WriteFile(outside, []byte(`{}`), 0o600); err != nil {
			t.Fatal(err)
		}
		windowsStateReadSymlink(t, outside, filepath.Join(root, "baseline.json"))
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
		probe := filepath.Join(root, "symlink-capability-probe")
		windowsStateReadSymlink(t, outside, probe)
		if err := os.Remove(probe); err != nil {
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

func TestReadStateFileBeneathRootNoFollowWindowsClosesHandlesOnEveryReturn(t *testing.T) {
	for _, test := range []struct {
		name string
		run  func(t *testing.T, root, path string, payload []byte) error
	}{
		{
			name: "success",
			run: func(_ *testing.T, root, _ string, payload []byte) error {
				_, err := ReadStateFileBeneathRootNoFollow(context.Background(), root, []string{"baseline.json"}, stateReadDigest(payload))
				return err
			},
		},
		{
			name: "checksum-failure",
			run: func(_ *testing.T, root, _ string, _ []byte) error {
				_, err := ReadStateFileBeneathRootNoFollow(context.Background(), root, []string{"baseline.json"}, stateReadDigest([]byte(`{"other":true}`)))
				return err
			},
		},
		{
			name: "initial-oversize",
			run: func(t *testing.T, root, path string, _ []byte) error {
				payload := bytes.Repeat([]byte("x"), maxStateFileBytes+1)
				if err := os.WriteFile(path, payload, 0o600); err != nil {
					t.Fatal(err)
				}
				_, err := ReadStateFileBeneathRootNoFollow(context.Background(), root, []string{"baseline.json"}, stateReadDigest(payload))
				return err
			},
		},
		{
			name: "mid-read-cancellation",
			run: func(t *testing.T, root, _ string, payload []byte) error {
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
				return err
			},
		},
		{
			name: "injected-step-failure",
			run: func(_ *testing.T, root, _ string, payload []byte) error {
				sentinel := errors.New("injected cleanup reader step failure")
				_, err := readStateFileBeneathRootNoFollow(context.Background(), root, []string{"baseline.json"}, stateReadDigest(payload), func(event stateReadBeneathRootStep) error {
					if event.Event == stateReadBeneathRootBeforeRead {
						return sentinel
					}
					return nil
				})
				return err
			},
		},
		{
			name: "growth-at-cap",
			run: func(t *testing.T, root, path string, _ []byte) error {
				payload := bytes.Repeat([]byte("x"), maxStateFileBytes)
				if err := os.WriteFile(path, payload, 0o600); err != nil {
					t.Fatal(err)
				}
				grew := false
				_, err := readStateFileBeneathRootNoFollow(context.Background(), root, []string{"baseline.json"}, stateReadDigest(payload), func(event stateReadBeneathRootStep) error {
					if event.Event == stateReadBeneathRootBeforeRead && !grew {
						grew = true
						return os.WriteFile(path, append(payload, 'y'), 0o600)
					}
					return nil
				})
				if !grew {
					t.Fatal("growth seam did not run")
				}
				return err
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			root := t.TempDir()
			path := filepath.Join(root, "baseline.json")
			payload := []byte(`{"ok":true}`)
			if test.name == "mid-read-cancellation" {
				payload = bytes.Repeat([]byte("x"), stateReadChunkSize+1)
			}
			if err := os.WriteFile(path, payload, 0o600); err != nil {
				t.Fatal(err)
			}
			if err := test.run(t, root, path, payload); err == nil && test.name != "success" {
				t.Fatalf("%s unexpectedly succeeded", test.name)
			}
			renamed := filepath.Join(root, "baseline-renamed.json")
			if err := os.Rename(path, renamed); err != nil {
				t.Fatalf("reader retained a final handle after %s: %v", test.name, err)
			}
			if err := os.Remove(renamed); err != nil {
				t.Fatalf("reader retained a final handle after %s: %v", test.name, err)
			}
		})
	}
}

func TestReadClientConfigBackupBeneathRootNoFollowWindowsRejectsGrowthAfterStat(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "backup.json")
	payload := bytes.Repeat([]byte("x"), MaxClientConfigBackupBytes)
	if err := os.WriteFile(path, payload, 0o600); err != nil {
		t.Fatal(err)
	}
	grew := false
	sawSentinel := false
	_, err := readStateFileBeneathRootNoFollowWithPolicy(context.Background(), root, []string{"backup.json"}, stateReadBeneathRootPolicy{
		maxBytes: MaxClientConfigBackupBytes, allowEmptyExpectedSHA256: true,
	}, func(event stateReadBeneathRootStep) error {
		if event.Event != stateReadBeneathRootBeforeRead {
			return nil
		}
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
}

func windowsStateReadSymlink(t *testing.T, target, link string) {
	t.Helper()
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("creating Windows test reparse point is unavailable: %v", err)
	}
}

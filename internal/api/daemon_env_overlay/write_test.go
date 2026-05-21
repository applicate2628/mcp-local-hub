// write_test.go — unit tests for WriteOverlay (Task 2.3 of the v0.5.x
// Servers matrix revamp). Three cases pin the flock-protected read-
// modify-write contract documented in write.go:
//
//  1. Atomic create from missing path: mutator adds a row; Load reads
//     it back.
//  2. Mutator-error rollback: existing file's on-disk content is
//     unchanged when the mutator returns a sentinel error, even
//     though the mutator mutated the in-memory overlay first.
//  3. Concurrent serialization: 5 goroutines each appending a
//     distinct env key to the same daemon row complete without losing
//     any update — flock serializes the load+marshal+write triple so
//     all 5 keys are observed by a final Load.
//
// Spec: docs/superpowers/specs/2026-05-19-servers-matrix-lsp-and-env-revamp-design.md
// §"Apply env edit from GUI" + I-V4-4.

package daemon_env_overlay_test

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"mcp-local-hub/internal/api/apitest"
	"mcp-local-hub/internal/api/daemon_env_overlay"
)

// TestWriteOverlayAtomicCreate verifies that WriteOverlay on a missing
// path creates the parent directory (if needed), seeds an empty
// overlay, hands the empty overlay to the mutator, and persists the
// mutator's edits. A subsequent Load returns the freshly-written row.
func TestWriteOverlayAtomicCreate(t *testing.T) {
	dir := apitest.HardenedTempDir(t)
	path := filepath.Join(dir, "overlay.yaml")

	const key = "\\mcp-local-hub-memory-default"

	err := daemon_env_overlay.WriteOverlay(path, func(ov *daemon_env_overlay.Overlay) error {
		if ov == nil {
			return errors.New("mutator: got nil overlay")
		}
		if ov.Daemons == nil {
			return errors.New("mutator: got nil Daemons map")
		}
		ov.Daemons[key] = daemon_env_overlay.DaemonRow{
			Env:    map[string]string{"FOO": "bar"},
			Source: "operator",
		}
		return nil
	})
	if err != nil {
		t.Fatalf("WriteOverlay: %v", err)
	}

	got, err := daemon_env_overlay.Load(path)
	if err != nil {
		t.Fatalf("Load after write: %v", err)
	}
	row, ok := got.Daemons[key]
	if !ok {
		t.Fatalf("Load: row %q missing; daemons=%v", key, got.Daemons)
	}
	if row.Env["FOO"] != "bar" {
		t.Errorf("Env[FOO] = %q, want %q", row.Env["FOO"], "bar")
	}
	if row.Source != "operator" {
		t.Errorf("Source = %q, want %q", row.Source, "operator")
	}
}

// TestWriteOverlayMutatorErrorRollsBack verifies the documented
// rollback contract: when the mutator returns a non-nil error, the
// on-disk file is NOT modified — even when the mutator already
// mutated the in-memory overlay before returning. The disk image must
// match the pre-WriteOverlay seed exactly.
func TestWriteOverlayMutatorErrorRollsBack(t *testing.T) {
	dir := apitest.HardenedTempDir(t)
	path := filepath.Join(dir, "overlay.yaml")

	// Seed an initial overlay via WriteOverlay (a successful run) so
	// the on-disk content is the canonical marshalled shape — using
	// os.WriteFile with a hand-rolled body would let an unrelated
	// marshalling drift fail the test. We capture the exact bytes
	// after the seed so the assertion below compares against the
	// real persisted form.
	const key = "\\mcp-local-hub-memory-default"
	if err := daemon_env_overlay.WriteOverlay(path, func(ov *daemon_env_overlay.Overlay) error {
		ov.Daemons[key] = daemon_env_overlay.DaemonRow{
			Env: map[string]string{"ORIGINAL": "yes"},
		}
		return nil
	}); err != nil {
		t.Fatalf("seed WriteOverlay: %v", err)
	}
	seeded, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read seed bytes: %v", err)
	}

	sentinel := errors.New("mutator refused")
	err = daemon_env_overlay.WriteOverlay(path, func(ov *daemon_env_overlay.Overlay) error {
		// Mutate in-memory: a row's value AND add a new row. These
		// changes MUST NOT reach disk.
		row := ov.Daemons[key]
		row.Env["ORIGINAL"] = "MUTATED"
		row.Env["NEW"] = "extra"
		ov.Daemons[key] = row
		ov.Daemons["\\mcp-local-hub-other"] = daemon_env_overlay.DaemonRow{
			Env: map[string]string{"X": "y"},
		}
		return sentinel
	})
	if !errors.Is(err, sentinel) {
		t.Fatalf("WriteOverlay error = %v, want sentinel %v", err, sentinel)
	}

	// On-disk file must match the pre-mutator seed byte-for-byte.
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read post-error: %v", err)
	}
	if string(got) != string(seeded) {
		t.Errorf("on-disk bytes changed after mutator error\nbefore:\n%s\nafter:\n%s", seeded, got)
	}

	// And a Load returns the original row exactly — no new row, no
	// mutated value, no NEW key.
	ov, err := daemon_env_overlay.Load(path)
	if err != nil {
		t.Fatalf("Load post-error: %v", err)
	}
	if len(ov.Daemons) != 1 {
		t.Errorf("len(Daemons) = %d, want 1", len(ov.Daemons))
	}
	row, ok := ov.Daemons[key]
	if !ok {
		t.Fatalf("row %q missing after error: daemons=%v", key, ov.Daemons)
	}
	if row.Env["ORIGINAL"] != "yes" {
		t.Errorf("Env[ORIGINAL] = %q, want %q (mutator's in-memory change leaked to disk)", row.Env["ORIGINAL"], "yes")
	}
	if _, ok := row.Env["NEW"]; ok {
		t.Errorf("Env[NEW] = %q present; mutator's in-memory add leaked to disk", row.Env["NEW"])
	}
}

// TestWriteOverlayConcurrentSerializes verifies that 5 goroutines each
// appending a distinct env key to the same row via WriteOverlay
// complete without losing updates. The flock at <path>.lock must
// serialize the load+marshal+write triple so every goroutine reads
// the previous goroutine's persisted state before adding its own key.
//
// Without the flock, the classic read-then-write race would let two
// goroutines load the same overlay snapshot, each add its own key,
// and the second writer would overwrite the first writer's key.
func TestWriteOverlayConcurrentSerializes(t *testing.T) {
	dir := apitest.HardenedTempDir(t)
	path := filepath.Join(dir, "overlay.yaml")

	const key = "\\mcp-local-hub-shared-row"
	const goroutines = 5

	// Seed the shared row once so each goroutine appends rather than
	// races to create the same row from missing. (Two goroutines both
	// "creating" the same row map is not a meaningful concurrency
	// test for serialized appends — it would pass even without a
	// flock if both happen to write identical map values.)
	if err := daemon_env_overlay.WriteOverlay(path, func(ov *daemon_env_overlay.Overlay) error {
		ov.Daemons[key] = daemon_env_overlay.DaemonRow{
			Env: map[string]string{},
		}
		return nil
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}

	var wg sync.WaitGroup
	errs := make(chan error, goroutines)
	for i := 0; i < goroutines; i++ {
		i := i
		wg.Add(1)
		go func() {
			defer wg.Done()
			envKey := fmt.Sprintf("KEY%d", i)
			envVal := "val"
			err := daemon_env_overlay.WriteOverlay(path, func(ov *daemon_env_overlay.Overlay) error {
				row, ok := ov.Daemons[key]
				if !ok {
					return fmt.Errorf("goroutine %d: row %q missing under flock; lost update", i, key)
				}
				if row.Env == nil {
					row.Env = map[string]string{}
				}
				row.Env[envKey] = envVal
				ov.Daemons[key] = row
				return nil
			})
			if err != nil {
				errs <- err
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Errorf("goroutine: %v", err)
	}

	got, err := daemon_env_overlay.Load(path)
	if err != nil {
		t.Fatalf("Load after concurrent writes: %v", err)
	}
	row, ok := got.Daemons[key]
	if !ok {
		t.Fatalf("row %q missing after concurrent writes; daemons=%v", key, got.Daemons)
	}
	for i := 0; i < goroutines; i++ {
		envKey := fmt.Sprintf("KEY%d", i)
		if got := row.Env[envKey]; got != "val" {
			t.Errorf("Env[%s] = %q, want %q (lost update under concurrent writes)", envKey, got, "val")
		}
	}
}

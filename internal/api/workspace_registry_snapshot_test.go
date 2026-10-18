package api

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

func BenchmarkRegistrySnapshotObserver(b *testing.B) {
	for _, fixture := range []struct {
		name  string
		bytes int
	}{{"representative-7KiB", 7 * 1024}, {"near-cap-14MiB", 14 * 1024 * 1024}} {
		b.Run(fixture.name, func(b *testing.B) {
			path := filepath.Join(b.TempDir(), "workspaces.yaml")
			var content bytes.Buffer
			content.WriteString("version: 1\nworkspaces:\n")
			for i := 0; content.Len() < fixture.bytes; i++ {
				fmt.Fprintf(&content, "  - workspace_key: ws-%06d\n    workspace_path: /project/%06d\n    language: go\n    backend: gopls-mcp\n    port: 9200\n", i, i)
			}
			if err := WriteStateFileBytesLockHeld(path, content.Bytes()); err != nil {
				b.Fatal(err)
			}
			info, err := os.Stat(path)
			if err != nil {
				b.Fatal(err)
			}
			// Metadata-only is a conservative steady-state reference for the old
			// resolver staleness path, not a correctness-equivalent alternative.
			b.Run("metadata-reference", func(b *testing.B) {
				b.ReportAllocs()
				b.ResetTimer()
				for i := 0; i < b.N; i++ {
					current, err := os.Stat(path)
					if err != nil || !current.ModTime().Equal(info.ModTime()) {
						b.Fatalf("metadata lookup failed: %v", err)
					}
				}
			})
			o := NewRegistrySnapshotObserver(NewRegistry(path), path, true)
			if _, _, err := o.ObserveView(); err != nil {
				b.Fatal(err)
			}
			b.Run("observer", func(b *testing.B) {
				b.ReportAllocs()
				b.ResetTimer()
				for i := 0; i < b.N; i++ {
					if _, _, err := o.ObserveView(); err != nil {
						b.Fatal(err)
					}
				}
			})
		})
	}
}

func saveSnapshotRow(t *testing.T, path string, port int) {
	t.Helper()
	r := NewRegistry(path)
	if err := r.PutSerena(WorkspaceEntry{WorkspaceKey: "workspace", Language: SerenaLanguageSentinel, Port: port}); err != nil {
		t.Fatal(err)
	}
	if err := r.Save(); err != nil {
		t.Fatal(err)
	}
}

func observeRowsForTest(o *RegistrySnapshotObserver) ([]WorkspaceEntry, string, error) {
	view, state, err := o.ObserveView()
	rows := make([]WorkspaceEntry, 0, view.SerenaCount())
	for i := 0; i < view.SerenaCount(); i++ {
		candidate, _ := view.SerenaCandidate(i)
		row, _ := view.Materialize(candidate)
		rows = append(rows, row)
	}
	return rows, state, err
}

func TestRegistrySnapshotObserver_Unchanged64LookupsCompareBytesTwiceWithoutRedecode(t *testing.T) {
	path := filepath.Join(t.TempDir(), "workspaces.yaml")
	saveSnapshotRow(t, path, 9301)
	o := NewRegistrySnapshotObserver(NewRegistry(path), path, true)
	seed, _, err := o.ObserveView()
	if err != nil || seed.SerenaCount() != 1 {
		t.Fatalf("seed view=%+v err=%v", seed, err)
	}
	reads := 0
	o.readFnForTest = func(path string, sink func(string, string, map[string]any) error) ([]byte, error) {
		reads++
		return ReadStateFileInodeAnchoredWithAuditSink(path, sink)
	}
	for i := 0; i < 64; i++ {
		view, state, err := o.ObserveView()
		if err != nil || state != "" || view.generation != seed.generation {
			t.Fatalf("lookup %d changed generation/state: %q %v", i+1, state, err)
		}
	}
	if reads != 2 {
		t.Fatalf("64 unchanged lookups made %d content reads, want exactly 2", reads)
	}
}

func TestRegistrySnapshotView_RetainedGenerationAndSelectedNestedFieldsAreImmutable(t *testing.T) {
	path := filepath.Join(t.TempDir(), "workspaces.yaml")
	save := func(port int) {
		t.Helper()
		reg := NewRegistry(path)
		if err := reg.PutSerena(WorkspaceEntry{
			WorkspaceKey: "workspace", Language: SerenaLanguageSentinel, Port: port,
			ClientEntries: map[string]string{"codex": "original"}, Languages: []string{"go"},
		}); err != nil {
			t.Fatal(err)
		}
		if err := reg.Save(); err != nil {
			t.Fatal(err)
		}
	}
	save(9301)
	o := NewRegistrySnapshotObserver(NewRegistry(path), path, true)
	old, _, err := o.ObserveView()
	if err != nil {
		t.Fatal(err)
	}
	oldCandidate, ok := old.SerenaCandidate(0)
	if !ok {
		t.Fatal("old candidate missing")
	}
	save(9302)
	current, _, err := o.ObserveView()
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := current.Materialize(oldCandidate); ok {
		t.Fatal("candidate from old generation materialized against new generation")
	}
	row, ok := old.Materialize(oldCandidate)
	if !ok || row.Port != 9301 {
		t.Fatalf("retained row = %+v, present %v", row, ok)
	}
	row.ClientEntries["codex"] = "mutated"
	row.Languages[0] = "rust"
	again, _ := old.Materialize(oldCandidate)
	if again.Port != 9301 || again.ClientEntries["codex"] != "original" || again.Languages[0] != "go" {
		t.Fatalf("retained generation mutated: %+v", again)
	}
	currentCandidate, _ := current.SerenaCandidate(0)
	newRow, _ := current.Materialize(currentCandidate)
	if newRow.Port != 9302 {
		t.Fatalf("new generation port = %d, want 9302", newRow.Port)
	}
}

func TestRegistrySnapshotObserver_FailureAndMissingSemantics(t *testing.T) {
	for _, readOnly := range []bool{true, false} {
		t.Run(map[bool]string{true: "route", false: "gui"}[readOnly], func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "workspaces.yaml")
			saveSnapshotRow(t, path, 9301)
			reg := NewRegistry(path)
			o := NewRegistrySnapshotObserver(reg, path, readOnly)
			if rows, _, err := observeRowsForTest(o); err != nil || len(rows) != 1 || rows[0].Port != 9301 {
				t.Fatalf("seed rows=%+v err=%v", rows, err)
			}
			o.readFnForTest = func(string, func(string, string, map[string]any) error) ([]byte, error) {
				return nil, errors.New("synthetic read failure")
			}
			o.lookups = registrySnapshotCompareEvery - 1
			rows, state, err := observeRowsForTest(o)
			if err == nil || state != "registry_snapshot_read_failed" || len(rows) != 1 || rows[0].Port != 9301 {
				t.Fatalf("read failure rows=%+v state=%q err=%v", rows, state, err)
			}
			o.readFnForTest = nil
			if err := os.WriteFile(path, []byte("workspaces: [not-valid"), 0o600); err != nil {
				t.Fatal(err)
			}
			rows, state, err = observeRowsForTest(o)
			if err == nil || state != "registry_snapshot_parse_failed" || len(rows) != 1 || rows[0].Port != 9301 {
				t.Fatalf("parse failure rows=%+v state=%q err=%v", rows, state, err)
			}
			if err := os.Remove(path); err != nil {
				t.Fatal(err)
			}
			rows, state, err = observeRowsForTest(o)
			if err != nil || state != "registry_snapshot_absent" || len(rows) != 0 {
				t.Fatalf("missing rows=%+v state=%q err=%v", rows, state, err)
			}
			saveSnapshotRow(t, path, 9302)
			rows, state, err = observeRowsForTest(o)
			if err != nil || state != "" || len(rows) != 1 || rows[0].Port != 9302 {
				t.Fatalf("recreated rows=%+v state=%q err=%v", rows, state, err)
			}
			if readOnly {
				if _, err := os.Stat(path + ".lock"); !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("route created registry lock: %v", err)
				}
			}
		})
	}
}

func TestRegistrySnapshotObserver_GUILockFailurePreservesLastGood(t *testing.T) {
	path := filepath.Join(t.TempDir(), "workspaces.yaml")
	saveSnapshotRow(t, path, 9301)
	reg := NewRegistry(path)
	o := NewRegistrySnapshotObserver(reg, path, false)
	if _, _, err := observeRowsForTest(o); err != nil {
		t.Fatal(err)
	}
	saveSnapshotRow(t, path, 9302)
	reg.lockFn = func(string) (func() error, error) { return nil, errors.New("synthetic lock failure") }
	rows, state, err := observeRowsForTest(o)
	if err == nil || state != "registry_snapshot_lock_failed" || len(rows) != 1 || rows[0].Port != 9301 {
		t.Fatalf("lock failure rows=%+v state=%q err=%v", rows, state, err)
	}
}

func TestRegistrySnapshotObserver_GUIUnlockFailureKeepsParsedSnapshotAndReportsFailure(t *testing.T) {
	path := filepath.Join(t.TempDir(), "workspaces.yaml")
	saveSnapshotRow(t, path, 9301)
	reg := NewRegistry(path)
	reg.lockFn = func(string) (func() error, error) {
		return func() error { return errors.New("synthetic unlock failure") }, nil
	}
	o := NewRegistrySnapshotObserver(reg, path, false)
	rows, state, err := observeRowsForTest(o)
	if err == nil || state != "registry_snapshot_unlock_failed" || len(rows) != 1 || rows[0].Port != 9301 {
		t.Fatalf("unlock failure rows=%+v state=%q err=%v", rows, state, err)
	}
}

func TestRegistrySnapshotObserver_PathSwapDuringReadRetries(t *testing.T) {
	path := filepath.Join(t.TempDir(), "workspaces.yaml")
	saveSnapshotRow(t, path, 9301)
	o := NewRegistrySnapshotObserver(NewRegistry(path), path, true)
	var once sync.Once
	o.readFnForTest = func(path string, sink func(string, string, map[string]any) error) ([]byte, error) {
		data, err := ReadStateFileInodeAnchoredWithAuditSink(path, sink)
		once.Do(func() { saveSnapshotRow(t, path, 9302) })
		return data, err
	}
	rows, state, err := observeRowsForTest(o)
	if err != nil || state != "" || len(rows) != 1 || rows[0].Port != 9302 {
		t.Fatalf("swap rows=%+v state=%q err=%v", rows, state, err)
	}
}

package api

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"mcp-local-hub/internal/scheduler"
)

// Both public upgrade branches must refuse an existing registration before
// any mutation. Recovery support and a usable snapshot do not grant authority
// to delete a generation by its reusable name.
func TestSchedulerUpgradeExistingTaskRefusal(t *testing.T) {
	prior := []byte("<Task>original-Привет</Task>")
	createCause := errors.New("unreached create failure")
	for _, kind := range []string{"server", "lazy_proxy"} {
		t.Run(kind, func(t *testing.T) {
			t.Cleanup(SetDaemonStateRootForTest(hardenedTempDir(t)))
			name := `\mcp-local-hub-upgradesnapshot-alpha`
			if kind == "lazy_proxy" {
				name = `\mcp-local-hub-lsp-snapshot-python`
			}
			f := &upgradeSnapshotScheduler{
				t: t, name: name, snapshot: prior, createErr: createCause,
				xml:     map[string][]byte{name: bytes.Clone(prior)},
				deletes: map[string]int{}, creates: map[string]int{}, imports: map[string]int{},
			}
			canonical := filepath.Join(t.TempDir(), mcphubShortName)
			var result SchedulerUpgradeResult
			if kind == "server" {
				result = runServerUpgradeSnapshotFixture(t, f, canonical)
			} else {
				bare := strings.TrimPrefix(name, `\`)
				result = *upgradeLazyProxyTask(f, name, bare, canonical, map[string]WorkspaceEntry{
					bare: {WorkspacePath: filepath.Join(t.TempDir(), "workspace"), Language: "python", Port: 19321},
				})
			}
			path := assertUpgradeRecoverySnapshot(t, name, prior)
			if result.NewCmd != "" || !strings.Contains(result.Err, ErrSchedulerReplacementUnowned.Error()) ||
				strings.Contains(result.Err, createCause.Error()) || path == "" || !strings.Contains(result.Err, strconv.Quote(path)) {
				t.Fatalf("refusal lost the retained definition or reported a mutation outcome: %+v", result)
			}
			if !bytes.Equal(f.xml[name], prior) || f.deletes[name] != 0 || f.creates[name] != 0 || f.restores != 0 || f.imports[name] != 0 {
				t.Fatalf("refusal changed the task: current=%q delete/create/restore/import=%d/%d/%d/%d", f.xml[name], f.deletes[name], f.creates[name], f.restores, f.imports[name])
			}
		})
	}
}

func TestSchedulerUpgradeReplacementAdmission(t *testing.T) {
	for _, mode := range []string{"missing_capability", "snapshot_storage_failure", "absent"} {
		t.Run(mode, func(t *testing.T) {
			root := hardenedTempDir(t)
			if mode == "snapshot_storage_failure" {
				root = filepath.Join(root, "occupied")
				if err := os.WriteFile(root, []byte("not a directory"), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			t.Cleanup(SetDaemonStateRootForTest(root))
			name := "mcp-local-hub-lsp-admission-python"
			prior := []byte("<Task>original</Task>")
			f := &upgradeSnapshotScheduler{t: t, name: name, snapshot: prior,
				xml: map[string][]byte{name: prior}, deletes: map[string]int{}, creates: map[string]int{}, imports: map[string]int{}}
			var sch scheduler.Scheduler = f
			if mode != "snapshot_storage_failure" {
				sch = struct{ scheduler.Scheduler }{f}
			}
			if mode == "absent" {
				f.snapshot, f.exportErr, f.xml = nil, scheduler.ErrTaskNotFound, map[string][]byte{}
			}
			result := upgradeLazyProxyTask(sch, name, name, filepath.Join(t.TempDir(), mcphubShortName), map[string]WorkspaceEntry{
				name: {WorkspacePath: filepath.Join(t.TempDir(), "workspace"), Language: "python", Port: 19321},
			})
			if mode == "absent" {
				if result.Err != "" || result.NewCmd == "" || f.creates[name] != 1 || f.deletes[name] != 0 || f.restores != 0 {
					t.Errorf("clean absence result=%+v, want one create-only registration", result)
				}
				return
			}
			if !strings.Contains(result.Err, ErrSchedulerReplacementUnowned.Error()) || result.NewCmd != "" ||
				!bytes.Equal(f.xml[name], prior) || f.creates[name] != 0 || f.deletes[name] != 0 || f.restores != 0 || f.imports[name] != 0 {
				t.Fatalf("replacement admission did not preserve the original task: %+v", result)
			}
			if mode == "missing_capability" && strings.Contains(result.Err, scheduler.ErrNotImplemented.Error()) {
				t.Fatalf("refusal incorrectly depended on restoration capability: %+v", result)
			}
			if mode == "snapshot_storage_failure" && (strings.Contains(result.Err, "snapshot retained") || !strings.Contains(result.Err, "retain observed definition")) {
				t.Fatalf("snapshot failure was lost or reported as retained: %+v", result)
			}
		})
	}
}

func TestSchedulerUpgradeFreshCreateFailureObservation(t *testing.T) {
	createCause := errors.New("fixture create result unavailable")
	observeCause := errors.New("fixture observation unavailable")
	for _, tc := range []struct {
		name       string
		current    []byte
		observeErr error
	}{
		{name: "confirmed-absence"},
		{name: "create-applied-reply-lost", current: []byte("<Task>candidate</Task>")},
		{name: "competing-occupant", current: []byte("<Task>foreign</Task>")},
		{name: "observation-failed", observeErr: observeCause},
	} {
		t.Run(tc.name, func(t *testing.T) {
			name := "mcp-local-hub-lsp-fresh-python"
			f := &upgradeSnapshotScheduler{t: t, name: name, exportErr: scheduler.ErrTaskNotFound,
				xml: map[string][]byte{}, deletes: map[string]int{}, creates: map[string]int{}, imports: map[string]int{},
				createErr: createCause, createWrites: tc.current,
			}
			if tc.observeErr != nil {
				f.reexport = func() ([]byte, error) { return nil, tc.observeErr }
			}
			err := replaceTaskForSchedulerUpgrade(f, scheduler.TaskSpec{Name: name}, nil)
			if !errors.Is(err, createCause) || (tc.observeErr != nil && !errors.Is(err, tc.observeErr)) {
				t.Fatalf("fresh creation lost a cause: %v", err)
			}
			var incomplete *InstallSchedulerRollbackIncompleteError
			wantIncomplete := tc.current != nil || tc.observeErr != nil
			if errors.As(err, &incomplete) != wantIncomplete {
				t.Fatalf("fresh creation uncertainty = %v, want incomplete=%t", err, wantIncomplete)
			}
			if f.creates[name] != 1 || f.deletes[name] != 0 || f.restores != 0 || f.imports[name] != 0 || !bytes.Equal(f.xml[name], tc.current) {
				t.Fatalf("fresh creation observation mutated or retried: current=%q create/delete/restore/import=%d/%d/%d/%d", f.xml[name], f.creates[name], f.deletes[name], f.restores, f.imports[name])
			}
		})
	}
}

func assertUpgradeRecoverySnapshot(t *testing.T, name string, prior []byte) string {
	t.Helper()
	root, err := DaemonStateDir()
	if err != nil {
		t.Fatal(err)
	}
	paths, err := filepath.Glob(filepath.Join(root, "scheduler-recovery-*.json"))
	if err != nil {
		t.Fatal(err)
	}
	var matched []string
	for _, path := range paths {
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		var snapshot installTaskRecoverySnapshot
		if err := json.Unmarshal(raw, &snapshot); err != nil {
			t.Fatal(err)
		}
		if snapshot.TaskName != name {
			continue
		}
		sum := sha256.Sum256(prior)
		if snapshot.Version != 1 || !bytes.Equal(snapshot.XML, prior) || snapshot.XMLSHA256 != hex.EncodeToString(sum[:]) {
			t.Error("RECOVERY-SNAPSHOT: original task bytes or identity were not retained")
		}
		matched = append(matched, path)
	}
	if len(matched) != 1 {
		t.Errorf("RECOVERY-SNAPSHOT: retained %d snapshots for %s, want one", len(matched), name)
		return ""
	}
	return matched[0]
}

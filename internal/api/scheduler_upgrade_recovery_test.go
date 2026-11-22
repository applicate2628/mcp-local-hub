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

// Both public upgrade branches must retain the original definition before
// deleting it and must preserve any occupant after a failed Create. The fake
// exposes applied-but-failed replies as well as a competitor at restoration.
func TestSchedulerUpgradeRecovery(t *testing.T) {
	prior := []byte("<Task>original-Привет</Task>")
	foreign := []byte("<Task>foreign</Task>")
	candidate := []byte("<Task>candidate</Task>")
	createErr := errors.New("fixture: create failed")
	restoreErr := errors.New("fixture: restore reply unavailable")
	for _, kind := range []string{"server", "lazy_proxy"} {
		t.Run(kind, func(t *testing.T) {
			for _, tc := range []struct {
				name                                           string
				createWrites, restoreWrites, occupant, wantXML []byte
				restoreErr, wantErr                            error
			}{
				{name: "restored", wantXML: prior},
				{name: "restore_failed", restoreErr: restoreErr, wantErr: restoreErr},
				{name: "create_applied_reply_lost", createWrites: candidate, wantXML: candidate, wantErr: scheduler.ErrTaskAlreadyExists},
				{name: "concurrent_occupant", occupant: foreign, wantXML: foreign, wantErr: scheduler.ErrTaskAlreadyExists},
				{name: "restore_applied_reply_lost", restoreWrites: prior, restoreErr: restoreErr, wantXML: prior, wantErr: restoreErr},
			} {
				t.Run(tc.name, func(t *testing.T) {
					t.Cleanup(SetDaemonStateRootForTest(hardenedTempDir(t)))
					name := `\mcp-local-hub-upgradesnapshot-alpha`
					if kind == "lazy_proxy" {
						name = `\mcp-local-hub-lsp-snapshot-python`
					}
					f := &upgradeSnapshotScheduler{
						t: t, name: name, snapshot: prior, createErr: createErr,
						xml:     map[string][]byte{name: bytes.Clone(prior)},
						deletes: map[string]int{}, creates: map[string]int{}, imports: map[string]int{},
						createWrites: tc.createWrites, restoreWrites: tc.restoreWrites, restoreErr: tc.restoreErr,
					}
					f.beforeDelete = func(task string) {
						if task == name {
							assertUpgradeRecoverySnapshot(t, name, prior)
						}
					}
					f.beforeRestore = func(task string) {
						if tc.occupant != nil {
							f.xml[task] = bytes.Clone(tc.occupant)
						}
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
					if result.NewCmd != "" || !strings.Contains(result.Err, createErr.Error()) ||
						(tc.wantErr != nil && !strings.Contains(result.Err, tc.wantErr.Error())) ||
						path == "" || !strings.Contains(result.Err, strconv.Quote(path)) {
						t.Errorf("RECOVERY-RESULT: missing original/recovery error or retained snapshot, or false success: %+v", result)
					}
					if tc.wantErr != nil && strings.Contains(result.Err, "prior definition registered again") {
						t.Error("RECOVERY-RESULT: uncertain restoration reported as acknowledged")
					}
					if !bytes.Equal(f.xml[name], tc.wantXML) || f.deletes[name] != 1 || f.creates[name] != 1 || f.restores != 1 || f.imports[name] != 0 {
						t.Errorf("RECOVERY-STATE: task=%q want=%q delete/create/restore/import=%d/%d/%d/%d", f.xml[name], tc.wantXML, f.deletes[name], f.creates[name], f.restores, f.imports[name])
					}
				})
			}
		})
	}
}

// Admission failures must precede any task mutation. These exercise the shared
// replacement owner through the lazy-proxy caller, including an absent task
// that needs neither a snapshot nor restoration capability.
func TestSchedulerUpgradeRecoveryAdmission(t *testing.T) {
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
					t.Errorf("RECOVERY-ABSENCE: unexpected refusal or mutation: %+v", result)
				}
				return
			}
			if result.Err == "" || result.NewCmd != "" || !bytes.Equal(f.xml[name], prior) || f.creates[name] != 0 || f.deletes[name] != 0 || f.restores != 0 || f.imports[name] != 0 {
				t.Errorf("RECOVERY-ADMISSION: unavailable recovery did not preserve original task: %+v", result)
			}
			if mode == "missing_capability" && !strings.Contains(result.Err, scheduler.ErrNotImplemented.Error()) {
				t.Errorf("RECOVERY-ADMISSION: lost capability error: %+v", result)
			}
		})
	}
}

func TestSchedulerUpgradeRevalidation(t *testing.T) {
	for _, mode := range []string{"changed", "unreadable", "empty"} {
		t.Run(mode, func(t *testing.T) {
			t.Cleanup(SetDaemonStateRootForTest(hardenedTempDir(t)))
			name := "mcp-local-hub-lsp-revalidate-python"
			prior := []byte("<Task>original</Task>")
			foreign := []byte("<Task>competing definition</Task>")
			f := &upgradeSnapshotScheduler{t: t, name: name, snapshot: prior,
				xml: map[string][]byte{name: prior}, deletes: map[string]int{}, creates: map[string]int{}, imports: map[string]int{}}
			want := prior
			f.reexport = func() ([]byte, error) {
				if mode == "unreadable" {
					return nil, scheduler.ErrPermissionDenied
				}
				if mode == "empty" {
					return nil, nil
				}
				f.xml[name] = bytes.Clone(foreign)
				return foreign, nil
			}
			if mode == "changed" {
				want = foreign
			}
			result := upgradeLazyProxyTask(f, name, name, filepath.Join(t.TempDir(), mcphubShortName), map[string]WorkspaceEntry{
				name: {WorkspacePath: filepath.Join(t.TempDir(), "workspace"), Language: "python", Port: 19321},
			})
			path := assertUpgradeRecoverySnapshot(t, name, prior)
			if !strings.Contains(result.Err, "revalidate task") || result.NewCmd != "" ||
				!strings.Contains(result.Err, strconv.Quote(path)) || !bytes.Equal(f.xml[name], want) ||
				f.deletes[name] != 0 || f.creates[name] != 0 || f.restores != 0 || f.imports[name] != 0 {
				t.Errorf("RECOVERY-REVALIDATE: stale or unreadable task reached mutation: result=%+v current=%q", result, f.xml[name])
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

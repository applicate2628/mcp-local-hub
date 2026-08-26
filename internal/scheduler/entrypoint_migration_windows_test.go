//go:build windows

package scheduler

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"sort"
	"testing"
)

func TestEntrypointTaskTxnPreservesRawXMLOutsideCommand(t *testing.T) {
	t.Parallel()

	const priorCLI = "C:\\Program Files\\mcp-local-hub\\mcphub.exe"
	const runtime = "C:\\Program Files\\mcp-local-hub\\mcphub-runtime.exe"
	before := map[string][]byte{
		"\\mcp-local-hub-autostart": taskXML("\\mcp-local-hub-autostart", "test-user", priorCLI, "gui", "autostart keeps this "+priorCLI),
		"\\mcp-local-hub-liveness":  taskXML("\\mcp-local-hub-liveness", "test-user", priorCLI, "supervise --ensure-alive", "liveness keeps this "+priorCLI),
	}
	backend := newEntrypointTaskBackendFake(before, "test-user")

	txn, err := beginOwnedEntrypointTaskTxnWith(context.Background(), t.TempDir()+"\\entrypoint.lock", priorCLI, runtime, backend, "test-user")
	if err != nil {
		t.Fatalf("begin transaction: %v", err)
	}
	t.Cleanup(func() {
		if err := txn.Close(); err != nil {
			t.Errorf("close transaction: %v", err)
		}
	})
	if err := txn.InventoryExport(); err != nil {
		t.Fatalf("inventory export: %v", err)
	}
	if err := txn.RewriteCommand(); err != nil {
		t.Fatalf("rewrite command: %v", err)
	}
	if err := txn.VerifyRuntime(); err != nil {
		t.Fatalf("verify runtime: %v", err)
	}

	for _, name := range sortedTaskNames(before) {
		got, err := backend.ExportXML(name)
		if err != nil {
			t.Fatalf("export %s: %v", name, err)
		}
		want := bytes.Replace(before[name], []byte("<Command>"+priorCLI+"</Command>"), []byte("<Command>"+runtime+"</Command>"), 1)
		if !bytes.Equal(got, want) {
			t.Fatalf("task %s rewrite changed bytes outside <Command>\n got: %s\nwant: %s", name, got, want)
		}
	}

	if err := txn.RestoreImport(); err != nil {
		t.Fatalf("restore import: %v", err)
	}
	for _, name := range sortedTaskNames(before) {
		got, err := backend.ExportXML(name)
		if err != nil {
			t.Fatalf("export restored %s: %v", name, err)
		}
		if !bytes.Equal(got, before[name]) {
			t.Fatalf("task %s restore is not byte-exact\n got: %s\nwant: %s", name, got, before[name])
		}
	}
}

func TestEntrypointTaskTxnRewriteFailureLeavesCompensationToCoordinator(t *testing.T) {
	t.Parallel()

	const priorCLI = "C:\\mcp\\mcphub.exe"
	const runtime = "C:\\mcp\\mcphub-runtime.exe"
	before := map[string][]byte{
		"\\mcp-local-hub-autostart": taskXML("\\mcp-local-hub-autostart", "test-user", priorCLI, "gui", "autostart"),
		"\\mcp-local-hub-liveness":  taskXML("\\mcp-local-hub-liveness", "test-user", priorCLI, "supervise --ensure-alive", "liveness"),
	}
	backend := newEntrypointTaskBackendFake(before, "test-user")
	backend.failImportFor["\\mcp-local-hub-liveness"] = errors.New("injected import failure")

	txn, err := beginOwnedEntrypointTaskTxnWith(context.Background(), t.TempDir()+"\\entrypoint.lock", priorCLI, runtime, backend, "test-user")
	if err != nil {
		t.Fatalf("begin transaction: %v", err)
	}
	t.Cleanup(func() {
		if err := txn.Close(); err != nil {
			t.Errorf("close transaction: %v", err)
		}
	})
	if err := txn.InventoryExport(); err != nil {
		t.Fatalf("inventory export: %v", err)
	}
	if err := txn.RewriteCommand(); err == nil {
		t.Fatal("rewrite command returned nil error after injected import failure")
	}
	got, err := backend.ExportXML("\\mcp-local-hub-autostart")
	if err != nil {
		t.Fatalf("export partially rewritten task: %v", err)
	}
	if bytes.Equal(got, before["\\mcp-local-hub-autostart"]) {
		t.Fatal("scheduler self-restored a partial rewrite; only the bundle coordinator may compensate")
	}
}

func TestEntrypointTaskTxnReopenRestoresRetainedXMLSnapshots(t *testing.T) {
	t.Parallel()

	const priorCLI = "C:\\mcp\\mcphub.exe"
	const runtime = "C:\\mcp\\mcphub-runtime.exe"
	before := map[string][]byte{
		"\\mcp-local-hub-autostart": taskXML("\\mcp-local-hub-autostart", "test-user", priorCLI, "gui", "autostart"),
		"\\mcp-local-hub-liveness":  taskXML("\\mcp-local-hub-liveness", "test-user", priorCLI, "supervise --ensure-alive", "liveness"),
	}
	backend := newEntrypointTaskBackendFake(before, "test-user")
	store := &retainedTaskXMLStoreFake{bodies: map[string][]byte{}}
	lockPath := t.TempDir() + "\\entrypoint.lock"

	txn, err := beginOwnedEntrypointTaskTxnWithStore(context.Background(), lockPath, priorCLI, runtime, backend, "test-user", store)
	if err != nil {
		t.Fatalf("begin transaction: %v", err)
	}
	if err := txn.InventoryExport(); err != nil {
		t.Fatalf("inventory export: %v", err)
	}
	backups := txn.Backups()
	if len(backups) != len(before) {
		t.Fatalf("backup count = %d, want %d", len(backups), len(before))
	}
	for _, backup := range backups {
		if backup.Ref == "" || backup.SHA256 == "" {
			t.Fatalf("backup lacks opaque ref or hash: %+v", backup)
		}
	}
	if err := txn.RewriteCommand(); err != nil {
		t.Fatalf("rewrite command: %v", err)
	}
	if err := txn.Close(); err != nil {
		t.Fatalf("close crashed transaction: %v", err)
	}

	reopened, err := reopenOwnedEntrypointTaskTxnWithStore(context.Background(), lockPath, priorCLI, runtime, backend, "test-user", store, backups)
	if err != nil {
		t.Fatalf("reopen transaction: %v", err)
	}
	t.Cleanup(func() {
		if err := reopened.Close(); err != nil {
			t.Errorf("close reopened transaction: %v", err)
		}
	})
	if err := reopened.RestoreImport(); err != nil {
		t.Fatalf("restore retained snapshots: %v", err)
	}
	for _, name := range sortedTaskNames(before) {
		got, err := backend.ExportXML(name)
		if err != nil {
			t.Fatalf("export restored %s: %v", name, err)
		}
		if !bytes.Equal(got, before[name]) {
			t.Fatalf("task %s was not byte-exactly restored after reopen", name)
		}
	}
}

func TestEntrypointTaskTxnRejectsForeignOrAmbiguousTaskBeforeMutation(t *testing.T) {
	t.Parallel()

	const priorCLI = "C:\\mcp\\mcphub.exe"
	const runtime = "C:\\mcp\\mcphub-runtime.exe"
	tests := []struct {
		name    string
		owner   string
		xml     []byte
		wantErr string
	}{
		{
			name:    "foreign owner",
			owner:   "other-user",
			xml:     taskXML("\\mcp-local-hub-autostart", "other-user", priorCLI, "gui", "autostart"),
			wantErr: "foreign owner",
		},
		{
			name:    "multiple actions",
			owner:   "test-user",
			xml:     []byte("<?xml version=\"1.0\" encoding=\"UTF-16\"?><Task><RegistrationInfo><URI>\\mcp-local-hub-autostart</URI></RegistrationInfo><Principals><Principal><UserId>test-user</UserId></Principal></Principals><Actions><Exec><Command>" + priorCLI + "</Command></Exec><ComHandler><ClassId>x</ClassId></ComHandler></Actions></Task>"),
			wantErr: "action count",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			backend := newEntrypointTaskBackendFake(map[string][]byte{"\\mcp-local-hub-autostart": tc.xml}, tc.owner)
			txn, err := beginOwnedEntrypointTaskTxnWith(context.Background(), t.TempDir()+"\\entrypoint.lock", priorCLI, runtime, backend, "test-user")
			if err != nil {
				t.Fatalf("begin transaction: %v", err)
			}
			t.Cleanup(func() { _ = txn.Close() })
			err = txn.InventoryExport()
			if err == nil || !bytes.Contains([]byte(err.Error()), []byte(tc.wantErr)) {
				t.Fatalf("inventory error = %v, want %q", err, tc.wantErr)
			}
			if backend.importCalls != 0 {
				t.Fatalf("inventory mutated scheduler: import calls = %d", backend.importCalls)
			}
		})
	}
}

type entrypointTaskBackendFake struct {
	tasks         map[string][]byte
	owners        map[string]string
	failImportFor map[string]error
	importCalls   int
}

type retainedTaskXMLStoreFake struct {
	bodies map[string][]byte
	next   int
}

func (s *retainedTaskXMLStoreFake) Retain(_ context.Context, body []byte) (string, error) {
	s.next++
	ref := fmt.Sprintf("opaque-%d", s.next)
	s.bodies[ref] = append([]byte(nil), body...)
	return ref, nil
}

func (s *retainedTaskXMLStoreFake) Load(_ context.Context, ref string) ([]byte, error) {
	body, ok := s.bodies[ref]
	if !ok {
		return nil, errors.New("missing retained body")
	}
	return append([]byte(nil), body...), nil
}

func newEntrypointTaskBackendFake(tasks map[string][]byte, owner string) *entrypointTaskBackendFake {
	copyTasks := make(map[string][]byte, len(tasks))
	owners := make(map[string]string, len(tasks))
	for name, body := range tasks {
		copyTasks[name] = append([]byte(nil), body...)
		owners[name] = owner
	}
	return &entrypointTaskBackendFake{tasks: copyTasks, owners: owners, failImportFor: map[string]error{}}
}

func (f *entrypointTaskBackendFake) List(prefix string) ([]TaskStatus, error) {
	var statuses []TaskStatus
	for name := range f.tasks {
		if len(name) > 0 && name[0] == '\\' {
			if len(name) < 2 || name[1:] == "" || name[1:len(prefix)+1] != prefix {
				continue
			}
		}
		statuses = append(statuses, TaskStatus{Name: name, Owner: f.owners[name]})
	}
	return statuses, nil
}

func (f *entrypointTaskBackendFake) ExportXML(name string) ([]byte, error) {
	body, ok := f.tasks[name]
	if !ok {
		return nil, ErrTaskNotFound
	}
	return append([]byte(nil), body...), nil
}

func (f *entrypointTaskBackendFake) ImportXML(name string, body []byte) error {
	f.importCalls++
	if err := f.failImportFor[name]; err != nil {
		delete(f.failImportFor, name)
		return err
	}
	f.tasks[name] = append([]byte(nil), body...)
	return nil
}

func sortedTaskNames(tasks map[string][]byte) []string {
	names := make([]string, 0, len(tasks))
	for name := range tasks {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

func taskXML(name, owner, command, arguments, description string) []byte {
	return []byte(fmt.Sprintf("<?xml version=\"1.0\" encoding=\"UTF-16\"?><Task version=\"1.4\"><RegistrationInfo><URI>%s</URI><Description>%s</Description></RegistrationInfo><Principals><Principal><UserId>%s</UserId></Principal></Principals><Triggers><LogonTrigger><Enabled>true</Enabled></LogonTrigger></Triggers><Settings><Hidden>false</Hidden></Settings><Actions Context=\"Author\"><Exec><Command>%s</Command><Arguments>%s</Arguments></Exec></Actions></Task>", name, description, owner, command, arguments))
}

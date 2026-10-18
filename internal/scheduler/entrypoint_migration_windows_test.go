//go:build windows

package scheduler

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"sort"
	"strings"
	"testing"
	"time"
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

func TestEntrypointTaskTxnDigestUsesSortedCanonicalNamesAndExactLiveXML(t *testing.T) {
	const (
		prior   = `C:\mcp\mcphub-windowless.exe`
		product = `C:\mcp\mcphub.exe`
	)
	before := map[string][]byte{
		`\mcp-local-hub-zeta`:  taskXML(`\mcp-local-hub-zeta`, "test-user", prior, "gui", "zeta"),
		`\mcp-local-hub-alpha`: taskXML(`\mcp-local-hub-alpha`, "test-user", prior, "supervise", "alpha"),
	}
	backend := newEntrypointTaskBackendFake(before, "test-user")
	txn, err := beginOwnedEntrypointTaskTxnWith(context.Background(), t.TempDir()+`\entrypoint.lock`, prior, product, backend, "test-user")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = txn.Close() })
	if err := txn.InventoryExport(); err != nil {
		t.Fatal(err)
	}
	if err := txn.RewriteCommand(); err != nil {
		t.Fatal(err)
	}
	got, err := txn.VerifyRuntimeSHA256()
	if err != nil {
		t.Fatal(err)
	}
	alpha, _ := backend.ExportXML(`\mcp-local-hub-alpha`)
	zeta, _ := backend.ExportXML(`\mcp-local-hub-zeta`)
	alphaSHA, zetaSHA := sha256.Sum256(alpha), sha256.Sum256(zeta)
	canonicalJSON := fmt.Sprintf(
		`[{"task_name":"\\mcp-local-hub-alpha","xml_sha256":"%s"},{"task_name":"\\mcp-local-hub-zeta","xml_sha256":"%s"}]`,
		hex.EncodeToString(alphaSHA[:]), hex.EncodeToString(zetaSHA[:]),
	)
	wantSHA := sha256.Sum256([]byte(canonicalJSON))
	if want := hex.EncodeToString(wantSHA[:]); got != want {
		t.Fatalf("task-set digest=%s want=%s input=%s", got, want, canonicalJSON)
	}
}

func TestEntrypointTaskTxnKeepsExistingRuntimeAndIgnoresOperatorCommand(t *testing.T) {
	t.Parallel()

	const priorCLI = "C:\\mcp\\mcphub.exe"
	const runtime = "C:\\mcp\\mcphub-windowless.exe"
	const operator = "C:\\operator\\custom-launcher.exe"
	before := map[string][]byte{
		"\\mcp-local-hub-autostart": taskXML("\\mcp-local-hub-autostart", "test-user", runtime, "gui", "already migrated"),
		"\\mcp-local-hub-liveness":  taskXML("\\mcp-local-hub-liveness", "test-user", priorCLI, "supervise --ensure-alive", "legacy"),
		"\\mcp-local-hub-operator":  taskXML("\\mcp-local-hub-operator", "test-user", operator, "custom", "operator-owned command"),
	}
	backend := newEntrypointTaskBackendFake(before, "test-user")
	txn, err := beginOwnedEntrypointTaskTxnWith(context.Background(), t.TempDir()+"\\entrypoint.lock", priorCLI, runtime, backend, "test-user")
	if err != nil {
		t.Fatalf("begin transaction: %v", err)
	}
	t.Cleanup(func() { _ = txn.Close() })
	if err := txn.InventoryExport(); err != nil {
		t.Fatalf("inventory export: %v", err)
	}
	if err := txn.RewriteCommand(); err != nil {
		t.Fatalf("rewrite command: %v", err)
	}
	if err := txn.VerifyRuntime(); err != nil {
		t.Fatalf("verify runtime: %v", err)
	}
	for _, name := range []string{"\\mcp-local-hub-autostart", "\\mcp-local-hub-operator"} {
		got, err := backend.ExportXML(name)
		if err != nil || !bytes.Equal(got, before[name]) {
			t.Fatalf("task %s changed: err=%v\n got=%s\nwant=%s", name, err, got, before[name])
		}
	}
	legacy, err := backend.ExportXML("\\mcp-local-hub-liveness")
	if err != nil || !bytes.Contains(legacy, []byte("<Command>"+runtime+"</Command>")) {
		t.Fatalf("legacy task was not migrated: err=%v xml=%s", err, legacy)
	}
}

func TestEntrypointTaskTxnMigratesCaseVariantPriorPathButKeepsDifferentTarget(t *testing.T) {
	const priorCLI = `C:\Program Files\mcp-local-hub\mcphub.exe`
	const runtime = `C:\Program Files\mcp-local-hub\mcphub-windowless.exe`
	caseVariantPrior := `c:\PROGRAM FILES\MCP-LOCAL-HUB\MCPHUB.EXE`
	caseVariantRuntime := `c:\program files\mcp-local-hub\mcphub-windowless.exe`
	const differentTarget = `C:\Program Files\mcp-local-hub\mcphub-other.exe`
	before := map[string][]byte{
		`\mcp-local-hub-legacy`:  taskXML(`\mcp-local-hub-legacy`, "test-user", caseVariantPrior, "gui", "legacy"),
		`\mcp-local-hub-runtime`: taskXML(`\mcp-local-hub-runtime`, "test-user", caseVariantRuntime, "gui", "already migrated"),
		`\mcp-local-hub-other`:   taskXML(`\mcp-local-hub-other`, "test-user", differentTarget, "gui", "operator target"),
	}
	backend := newEntrypointTaskBackendFake(before, "test-user")
	txn, err := beginOwnedEntrypointTaskTxnWith(context.Background(), t.TempDir()+`\entrypoint.lock`, priorCLI, runtime, backend, "test-user")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = txn.Close() })
	if err := txn.InventoryExport(); err != nil {
		t.Fatalf("inventory case variant: %v", err)
	}
	if err := txn.RewriteCommand(); err != nil {
		t.Fatalf("rewrite case variant: %v", err)
	}
	if err := txn.VerifyRuntime(); err != nil {
		t.Fatalf("verify case variant: %v", err)
	}
	legacy, err := backend.ExportXML(`\mcp-local-hub-legacy`)
	if err != nil || !bytes.Contains(legacy, []byte(`<Command>`+runtime+`</Command>`)) {
		t.Fatalf("case-variant legacy task was not migrated: err=%v xml=%s", err, legacy)
	}
	for _, name := range []string{`\mcp-local-hub-runtime`, `\mcp-local-hub-other`} {
		got, err := backend.ExportXML(name)
		if err != nil || !bytes.Equal(got, before[name]) {
			t.Fatalf("non-prior task %q changed: err=%v got=%s want=%s", name, err, got, before[name])
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
	importHook    func(string, []byte) error
	exportHook    func(string) error
}

type retainedTaskXMLStoreFake struct {
	bodies map[string][]byte
	next   int
	retain func(context.Context, []byte) (string, error)
	load   func(context.Context, string) ([]byte, error)
}

func (s *retainedTaskXMLStoreFake) Retain(ctx context.Context, body []byte) (string, error) {
	if s.retain != nil {
		return s.retain(ctx, body)
	}
	s.next++
	ref := fmt.Sprintf("opaque-%d", s.next)
	s.bodies[ref] = append([]byte(nil), body...)
	return ref, nil
}

func (s *retainedTaskXMLStoreFake) Load(ctx context.Context, ref string) ([]byte, error) {
	if s.load != nil {
		return s.load(ctx, ref)
	}
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
	if f.exportHook != nil {
		if err := f.exportHook(name); err != nil {
			return nil, err
		}
	}
	body, ok := f.tasks[name]
	if !ok {
		return nil, ErrTaskNotFound
	}
	return append([]byte(nil), body...), nil
}

func (f *entrypointTaskBackendFake) ImportXML(name string, body []byte) error {
	f.importCalls++
	if f.importHook != nil {
		if err := f.importHook(name, body); err != nil {
			return err
		}
	}
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

func TestEntrypointTaskTxnRetainFailurePreservesCause(t *testing.T) {
	t.Parallel()
	cause := &taskRetentionProbeError{operation: "retain"}
	for _, ref := range []string{"", "nonempty-ref-with-error"} {
		t.Run(fmt.Sprintf("ref-present=%t", ref != ""), func(t *testing.T) {
			prior := t.TempDir() + "\\prior.exe"
			runtime := t.TempDir() + "\\runtime.exe"
			name := "\\mcp-local-hub-autostart"
			body := taskXML(name, "test-user", prior, "gui", "synthetic")
			backend := newEntrypointTaskBackendFake(map[string][]byte{name: body}, "test-user")
			store := &retainedTaskXMLStoreFake{retain: func(context.Context, []byte) (string, error) {
				return ref, cause
			}}
			txn, err := beginOwnedEntrypointTaskTxnWithStore(context.Background(), t.TempDir()+"\\entrypoint.lock", prior, runtime, backend, "test-user", store)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				if err := txn.Close(); err != nil {
					t.Error(err)
				}
			})
			err = txn.InventoryExport()
			var got *taskRetentionProbeError
			if !errors.Is(err, cause) || !errors.As(err, &got) || got != cause {
				t.Fatalf("retention cause not preserved: %v", err)
			}
			if len(txn.Backups()) != 0 || backend.importCalls != 0 {
				t.Fatal("failed retention published backup or mutated a task")
			}
		})
	}
}

func TestEntrypointTaskTxnRejectsEmptyRetainedReference(t *testing.T) {
	t.Parallel()
	for _, ref := range []string{"", " \t"} {
		t.Run(fmt.Sprintf("bytes=%d", len(ref)), func(t *testing.T) {
			prior := t.TempDir() + "\\prior.exe"
			runtime := t.TempDir() + "\\runtime.exe"
			name := "\\mcp-local-hub-autostart"
			backend := newEntrypointTaskBackendFake(map[string][]byte{name: taskXML(name, "test-user", prior, "gui", "synthetic")}, "test-user")
			store := &retainedTaskXMLStoreFake{retain: func(context.Context, []byte) (string, error) {
				return ref, nil
			}}
			txn, err := beginOwnedEntrypointTaskTxnWithStore(context.Background(), t.TempDir()+"\\entrypoint.lock", prior, runtime, backend, "test-user", store)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				if err := txn.Close(); err != nil {
					t.Error(err)
				}
			})
			err = txn.InventoryExport()
			if err == nil || !bytes.Contains([]byte(err.Error()), []byte("retained XML unavailable")) {
				t.Fatalf("malformed empty reference was accepted: %v", err)
			}
			if len(txn.Backups()) != 0 || backend.importCalls != 0 {
				t.Fatal("empty reference published backup or mutated a task")
			}
		})
	}
}

func TestEntrypointTaskTxnReopenPreservesLoadCause(t *testing.T) {
	t.Parallel()
	prior := t.TempDir() + "\\prior.exe"
	runtime := t.TempDir() + "\\runtime.exe"
	name := "\\mcp-local-hub-autostart"
	body := taskXML(name, "test-user", prior, "gui", "synthetic")
	backend := newEntrypointTaskBackendFake(map[string][]byte{name: body}, "test-user")
	cause := &taskRetentionProbeError{operation: "load"}
	store := &retainedTaskXMLStoreFake{load: func(context.Context, string) ([]byte, error) {
		return nil, cause
	}}
	backup := OwnedEntrypointTaskBackup{TaskName: name, Ref: "opaque-1", SHA256: sha256Hex(body)}
	lockPath := t.TempDir() + "\\entrypoint.lock"
	txn, err := reopenOwnedEntrypointTaskTxnWithStore(context.Background(), lockPath, prior, runtime, backend, "test-user", store, []OwnedEntrypointTaskBackup{backup})
	if txn != nil {
		_ = txn.Close()
		t.Fatal("failed load admitted recovered transaction")
	}
	var got *taskRetentionProbeError
	if !errors.Is(err, cause) || !errors.As(err, &got) || got != cause || backend.importCalls != 0 {
		t.Fatalf("load cause lost or task mutated: %v", err)
	}
	store.load = func(context.Context, string) ([]byte, error) { return body, nil }
	recovered, err := reopenOwnedEntrypointTaskTxnWithStore(context.Background(), lockPath, prior, runtime, backend, "test-user", store, []OwnedEntrypointTaskBackup{backup})
	if err != nil {
		t.Fatalf("failed reopen retained its transaction lock: %v", err)
	}
	if err := recovered.Close(); err != nil {
		t.Fatal(err)
	}
}

type taskRetentionProbeError struct{ operation string }

func (e *taskRetentionProbeError) Error() string {
	return "synthetic task retention " + e.operation + " failure"
}

func TestEntrypointTaskExcludedRestoreBarrier(t *testing.T) {
	txn, backend, _, original := excludedTaskFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := txn.DisableCapturedVerified(ctx, EntrypointTaskApply{Apply: true}); err != nil {
		t.Fatal(err)
	}
	for name, raw := range backend.tasks {
		var err error
		backend.tasks[name], err = rewriteOwnedEntrypointTaskCommand(raw, txn.prior, txn.runtime)
		if err != nil {
			t.Fatal(err)
		}
	}
	snapshot, err := txn.RestoreCapturedWhileDisabled(ctx, EntrypointTaskApply{Apply: true})
	if err != nil {
		t.Fatal(err)
	}
	entered, release, done := make(chan struct{}), make(chan struct{}), make(chan error, 1)
	go func() {
		_, err := txn.RestoreOriginalEnabledAfterReady(ctx, excludedEnableRequest(txn, EntrypointTaskCapturedPrior), snapshot, func(ctx context.Context, s EntrypointTaskExcludedSnapshot) (EntrypointTaskReadiness, error) {
			close(entered)
			select {
			case <-release:
				return excludedReadiness(txn, s.Target), nil
			case <-ctx.Done():
				return EntrypointTaskReadiness{}, ctx.Err()
			}
		})
		done <- err
	}()
	select {
	case <-entered:
	case <-ctx.Done():
		t.Fatal("readiness barrier was not reached")
	}
	for name, raw := range original {
		want := bytes.Replace(raw, []byte(" true\t"), []byte(" false\t"), 1)
		if !bytes.Equal(backend.tasks[name], want) {
			cancel()
			<-done
			t.Fatal("captured restoration enabled the task before readiness or changed protected bytes")
		}
	}
	close(release)
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-ctx.Done():
		t.Fatal("readiness finalization did not finish")
	}
	for name, raw := range original {
		if !bytes.Equal(backend.tasks[name], raw) {
			t.Fatal("prior finalization did not restore exact original true/false")
		}
	}
}

func excludedTaskXML(name, enabled string) []byte {
	return []byte("\xef\xbb\xbf" + fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8"?>
<s:Task xmlns:s="http://schemas.microsoft.com/windows/2004/02/mit/task" version="1.4">
 <s:RegistrationInfo><s:URI>%s</s:URI></s:RegistrationInfo>
 <s:Triggers><s:LogonTrigger><s:Enabled>true</s:Enabled><s:Delay>PT1M</s:Delay></s:LogonTrigger></s:Triggers>
 <s:Principals><s:Principal id="Author"><s:UserId>test-user</s:UserId></s:Principal></s:Principals>
 <s:Settings><s:Enabled> %s	</s:Enabled><s:Hidden>false</s:Hidden></s:Settings>
 <s:Actions><s:Exec><s:Command>C:\synthetic\prior.exe</s:Command><s:Arguments>supervise --ensure-alive</s:Arguments><s:WorkingDirectory>C:\synthetic</s:WorkingDirectory></s:Exec></s:Actions>
</s:Task>
`, name, enabled))
}

func excludedTaskFixture(t *testing.T) (*OwnedEntrypointTaskTxn, *entrypointTaskBackendFake, *retainedTaskXMLStoreFake, map[string][]byte) {
	t.Helper()
	original := map[string][]byte{`\mcp-local-hub-alpha`: excludedTaskXML(`\mcp-local-hub-alpha`, "true"), `\mcp-local-hub-zeta`: excludedTaskXML(`\mcp-local-hub-zeta`, "false")}
	backend := newEntrypointTaskBackendFake(original, "test-user")
	store := &retainedTaskXMLStoreFake{bodies: map[string][]byte{}}
	txn, err := beginOwnedEntrypointTaskTxnWithStore(context.Background(), t.TempDir()+`\entrypoint.lock`, `C:\synthetic\prior.exe`, `C:\synthetic\runtime.exe`, backend, "test-user", store)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := txn.Close(); err != nil {
			t.Error(err)
		}
	})
	if err := txn.InventoryExport(); err != nil {
		t.Fatal(err)
	}
	return txn, backend, store, original
}

func excludedEnableRequest(txn *OwnedEntrypointTaskTxn, target EntrypointTaskTarget) EntrypointTaskEnableRequest {
	path := txn.prior
	if target == EntrypointTaskAdmittedRuntime {
		path = txn.runtime
	}
	return EntrypointTaskEnableRequest{Apply: true, Target: target, ExpectedImagePath: path, ExpectedImageSHA256: strings.Repeat("2", 64), ExpectedOwnerPID: 4242, ExpectedOwnerStartedAt: "2026-10-07T09:00:00Z"}
}

func excludedReadiness(txn *OwnedEntrypointTaskTxn, target EntrypointTaskTarget) EntrypointTaskReadiness {
	r := excludedEnableRequest(txn, target)
	return EntrypointTaskReadiness{Target: target, ImagePath: r.ExpectedImagePath, ImageSHA256: r.ExpectedImageSHA256, OwnerPID: r.ExpectedOwnerPID, OwnerStartedAt: r.ExpectedOwnerStartedAt, EvidenceSHA256: strings.Repeat("9", 64)}
}

func requireExcludedRefusal(t *testing.T, err error, backend *entrypointTaskBackendFake, before int) {
	t.Helper()
	var refusal *EntrypointTaskTransitionError
	if err == nil || !errors.As(err, &refusal) || backend.importCalls != before {
		t.Fatalf("typed zero-write refusal missing: err=%v writes=%d before=%d", err, backend.importCalls, before)
	}
}

func TestEntrypointTaskExcludedCandidateFinalPreservesDisabledOriginal(t *testing.T) {
	txn, backend, _, original := excludedTaskFixture(t)
	if _, err := txn.DisableCapturedVerified(context.Background(), EntrypointTaskApply{Apply: true}); err != nil {
		t.Fatal(err)
	}
	for name, raw := range backend.tasks {
		rewritten, err := rewriteOwnedEntrypointTaskCommand(raw, txn.prior, txn.runtime)
		if err != nil {
			t.Fatal(err)
		}
		backend.tasks[name] = rewritten
	}
	snapshot, err := txn.VerifyExcludedSHA256(EntrypointTaskAdmittedRuntime)
	if err != nil {
		t.Fatal(err)
	}
	digest, err := txn.RestoreOriginalEnabledAfterReady(context.Background(), excludedEnableRequest(txn, snapshot.Target), snapshot, func(_ context.Context, s EntrypointTaskExcludedSnapshot) (EntrypointTaskReadiness, error) {
		return excludedReadiness(txn, s.Target), nil
	})
	if err != nil || !validEntrypointDigest(digest) {
		t.Fatalf("candidate final: %s %v", digest, err)
	}
	for name, raw := range original {
		want, err := rewriteOwnedEntrypointTaskCommand(raw, txn.prior, txn.runtime)
		if err != nil || !bytes.Equal(backend.tasks[name], want) {
			t.Fatalf("candidate changed protected XML or original disabled disposition: %s", name)
		}
	}
}

func TestEntrypointTaskExcludedAdmissionGuards(t *testing.T) {
	for _, tc := range []string{"apply", "nil-context", "cancelled-context", "nil-inventory", "empty-inventory", "store", "backup", "backup-body", "equal-images", "closed", "nil-owner-context"} {
		t.Run(tc, func(t *testing.T) {
			txn, backend, store, _ := excludedTaskFixture(t)
			ctx := context.Background()
			request := EntrypointTaskApply{Apply: true}
			switch tc {
			case "apply":
				request.Apply = false
			case "nil-context":
				ctx = nil
			case "cancelled-context":
				var cancel context.CancelFunc
				ctx, cancel = context.WithCancel(ctx)
				cancel()
			case "nil-inventory":
				txn.inventory = nil
			case "empty-inventory":
				txn.inventory = map[string]ownedEntrypointTaskSnapshot{}
			case "store":
				txn.store = nil
			case "backup":
				s := txn.inventory[`\mcp-local-hub-alpha`]
				s.backup.Ref = ""
				txn.inventory[`\mcp-local-hub-alpha`] = s
			case "backup-body":
				store.bodies[txn.inventory[`\mcp-local-hub-alpha`].backup.Ref] = []byte("changed")
			case "equal-images":
				txn.runtime = txn.prior
			case "closed":
				if err := txn.Close(); err != nil {
					t.Fatal(err)
				}
			case "nil-owner-context":
				txn.ctx = nil
			}
			_, err := txn.DisableCapturedVerified(ctx, request)
			requireExcludedRefusal(t, err, backend, 0)
			_, err = txn.RestoreCapturedWhileDisabled(ctx, request)
			requireExcludedRefusal(t, err, backend, 0)
			_, err = txn.RestoreOriginalEnabledAfterReady(ctx, EntrypointTaskEnableRequest{Apply: request.Apply}, EntrypointTaskExcludedSnapshot{}, nil)
			requireExcludedRefusal(t, err, backend, 0)
		})
	}
}

func TestEntrypointTaskExcludedEnabledShapeGuards(t *testing.T) {
	for _, tc := range []string{"missing", "duplicate", "nested", "namespace", "settings-namespace", "boolean", "empty", "entity", "duplicate-settings"} {
		t.Run(tc, func(t *testing.T) {
			txn, backend, store, _ := excludedTaskFixture(t)
			name := `\mcp-local-hub-alpha`
			raw := backend.tasks[name]
			node := []byte("<s:Enabled> true\t</s:Enabled>")
			replacement := []byte{}
			switch tc {
			case "duplicate":
				replacement = append(append([]byte(nil), node...), node...)
			case "nested":
				replacement = []byte("<s:Enabled><s:Value>true</s:Value></s:Enabled>")
			case "namespace":
				replacement = []byte("<x:Enabled xmlns:x=\"wrong\">true</x:Enabled>")
			case "settings-namespace":
				raw = bytes.Replace(raw, []byte("<s:Settings>"), []byte("<s:Settings xmlns:s=\"wrong\">"), 1)
				replacement = node
			case "boolean":
				replacement = []byte("<s:Enabled>1</s:Enabled>")
			case "empty":
				replacement = []byte("<s:Enabled></s:Enabled>")
			case "entity":
				replacement = []byte("<s:Enabled>tr&#117;e</s:Enabled>")
			case "duplicate-settings":
				raw = bytes.Replace(raw, []byte("</s:Settings>"), []byte("</s:Settings><s:Settings><s:Enabled>false</s:Enabled></s:Settings>"), 1)
				replacement = node
			}
			raw = bytes.Replace(raw, node, replacement, 1)
			backend.tasks[name] = raw
			s := txn.inventory[name]
			s.xml = append([]byte(nil), raw...)
			s.backup.SHA256 = sha256Hex(raw)
			txn.inventory[name] = s
			store.bodies[s.backup.Ref] = append([]byte(nil), raw...)
			_, err := txn.DisableCapturedVerified(context.Background(), EntrypointTaskApply{Apply: true})
			requireExcludedRefusal(t, err, backend, 0)
		})
	}
}

func TestEntrypointTaskExcludedSetAndSourceDriftGuards(t *testing.T) {
	for _, tc := range []string{"extra", "missing", "duplicate", "owner", "source", "command-drift"} {
		t.Run(tc, func(t *testing.T) {
			txn, backend, _, _ := excludedTaskFixture(t)
			switch tc {
			case "extra":
				n := `\mcp-local-hub-extra`
				backend.tasks[n] = excludedTaskXML(n, "true")
				backend.owners[n] = "test-user"
			case "missing":
				delete(backend.tasks, `\mcp-local-hub-alpha`)
			case "duplicate":
				backend.tasks["mcp-local-hub-alpha"] = backend.tasks[`\mcp-local-hub-alpha`]
				backend.owners["mcp-local-hub-alpha"] = "test-user"
			case "owner":
				backend.owners[`\mcp-local-hub-alpha`] = "foreign-user"
			case "source":
				backend.tasks[`\mcp-local-hub-alpha`] = append(backend.tasks[`\mcp-local-hub-alpha`], []byte(" ")...)
			case "command-drift":
				backend.tasks[`\mcp-local-hub-alpha`] = bytes.Replace(backend.tasks[`\mcp-local-hub-alpha`], []byte(txn.prior), []byte(`C:\foreign\other.exe`), 1)
			}
			_, err := txn.DisableCapturedVerified(context.Background(), EntrypointTaskApply{Apply: true})
			requireExcludedRefusal(t, err, backend, 0)
		})
	}
}

func TestEntrypointTaskExcludedSnapshotBindingAndCopy(t *testing.T) {
	txn, backend, _, _ := excludedTaskFixture(t)
	s, err := txn.DisableCapturedVerified(context.Background(), EntrypointTaskApply{Apply: true})
	if err != nil {
		t.Fatal(err)
	}
	before := backend.importCalls
	for _, tc := range []string{"forged", "map", "digest", "target", "extra", "missing"} {
		t.Run(tc, func(t *testing.T) {
			bad := copyEntrypointExcludedSnapshot(s)
			switch tc {
			case "forged":
				bad.owner = nil
			case "map":
				bad.DefinitionSHA256[`\mcp-local-hub-alpha`] = strings.Repeat("7", 64)
			case "digest":
				bad.TaskSetSHA256 = strings.Repeat("8", 64)
			case "target":
				bad.Target = "unknown"
			case "extra":
				bad.DefinitionSHA256[`\mcp-local-hub-extra`] = strings.Repeat("7", 64)
			case "missing":
				delete(bad.DefinitionSHA256, `\mcp-local-hub-alpha`)
			}
			_, err := txn.RestoreOriginalEnabledAfterReady(context.Background(), excludedEnableRequest(txn, EntrypointTaskCapturedPrior), bad, func(context.Context, EntrypointTaskExcludedSnapshot) (EntrypointTaskReadiness, error) {
				t.Fatal("invalid snapshot reached readiness")
				return EntrypointTaskReadiness{}, nil
			})
			requireExcludedRefusal(t, err, backend, before)
		})
	}
	s.DefinitionSHA256[`\mcp-local-hub-alpha`] = "changed caller copy"
	fresh, err := txn.VerifyExcludedSHA256(EntrypointTaskCapturedPrior)
	if err != nil || !validEntrypointDigest(fresh.DefinitionSHA256[`\mcp-local-hub-alpha`]) {
		t.Fatal("caller map changed retained authority")
	}
	_, err = txn.RestoreOriginalEnabledAfterReady(context.Background(), excludedEnableRequest(txn, fresh.Target), fresh, func(_ context.Context, callback EntrypointTaskExcludedSnapshot) (EntrypointTaskReadiness, error) {
		callback.DefinitionSHA256[`\mcp-local-hub-alpha`] = "callback copy"
		return excludedReadiness(txn, callback.Target), nil
	})
	if err != nil || fresh.DefinitionSHA256[`\mcp-local-hub-alpha`] == "callback copy" {
		t.Fatalf("callback alias changed caller/internal snapshot: %v", err)
	}
}

func TestEntrypointTaskExcludedReadinessGuards(t *testing.T) {
	for _, tc := range []string{"request-apply", "target", "path", "hash", "zero-hash", "pid", "started", "zero-started", "fractional-started", "nil-check", "callback-fail", "callback-cancel", "proof-target", "proof-image", "proof-hash", "proof-pid", "proof-started", "proof-evidence", "empty-proof", "post-callback-drift", "post-callback-extra", "post-callback-missing"} {
		t.Run(tc, func(t *testing.T) {
			txn, backend, store, _ := excludedTaskFixture(t)
			s, err := txn.DisableCapturedVerified(context.Background(), EntrypointTaskApply{Apply: true})
			if err != nil {
				t.Fatal(err)
			}
			before := backend.importCalls
			request := excludedEnableRequest(txn, s.Target)
			ready := excludedReadiness(txn, s.Target)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			cause := errors.New("synthetic readiness failure")
			check := EntrypointTaskReadinessCheck(func(context.Context, EntrypointTaskExcludedSnapshot) (EntrypointTaskReadiness, error) {
				switch tc {
				case "callback-fail":
					return EntrypointTaskReadiness{}, cause
				case "callback-cancel":
					cancel()
				case "post-callback-drift":
					backend.tasks[`\mcp-local-hub-alpha`] = append(backend.tasks[`\mcp-local-hub-alpha`], []byte("foreign drift")...)
				case "post-callback-extra":
					name := `\mcp-local-hub-extra`
					backend.tasks[name] = excludedTaskXML(name, "true")
					backend.owners[name] = "test-user"
				case "post-callback-missing":
					delete(backend.tasks, `\mcp-local-hub-alpha`)
				}
				return ready, nil
			})
			switch tc {
			case "request-apply":
				request.Apply = false
			case "target":
				request.Target = "unknown"
			case "path":
				request.ExpectedImagePath = ""
			case "hash":
				request.ExpectedImageSHA256 = ""
			case "zero-hash":
				request.ExpectedImageSHA256 = strings.Repeat("0", 64)
			case "pid":
				request.ExpectedOwnerPID = 0
			case "started":
				request.ExpectedOwnerStartedAt = ""
			case "zero-started":
				request.ExpectedOwnerStartedAt = "0001-01-01T00:00:00Z"
				ready.OwnerStartedAt = request.ExpectedOwnerStartedAt
			case "fractional-started":
				request.ExpectedOwnerStartedAt = "2026-10-07T09:00:00.1Z"
			case "nil-check":
				check = nil
			case "proof-target":
				ready.Target = "unknown"
			case "proof-image":
				ready.ImagePath = txn.runtime
			case "proof-hash":
				ready.ImageSHA256 = strings.Repeat("3", 64)
			case "proof-pid":
				ready.OwnerPID++
			case "proof-started":
				ready.OwnerStartedAt = "2026-10-07T09:00:01Z"
			case "proof-evidence":
				ready.EvidenceSHA256 = ""
			case "empty-proof":
				ready = EntrypointTaskReadiness{}
			}
			_, err = txn.RestoreOriginalEnabledAfterReady(ctx, request, s, check)
			requireExcludedRefusal(t, err, backend, before)
			if tc == "callback-fail" && !errors.Is(err, cause) {
				t.Fatal("readiness cause lost")
			}
			if len(store.bodies) != 2 {
				t.Fatal("failed readiness discarded custody")
			}
		})
	}
}

func TestEntrypointTaskExcludedRestoreContinuesAndRetainsCause(t *testing.T) {
	txn, backend, store, _ := excludedTaskFixture(t)
	if _, err := txn.DisableCapturedVerified(context.Background(), EntrypointTaskApply{Apply: true}); err != nil {
		t.Fatal(err)
	}
	for n, raw := range backend.tasks {
		rewritten, err := rewriteOwnedEntrypointTaskCommand(raw, txn.prior, txn.runtime)
		if err != nil {
			t.Fatal(err)
		}
		backend.tasks[n] = rewritten
	}
	cause := errors.New("synthetic excluded restore import failure")
	backend.failImportFor[`\mcp-local-hub-alpha`] = cause
	_, err := txn.RestoreCapturedWhileDisabled(context.Background(), EntrypointTaskApply{Apply: true})
	var partial *EntrypointTaskPartialProgressError
	if !errors.Is(err, cause) || !errors.As(err, &partial) || len(store.bodies) != 2 {
		t.Fatalf("restore failure lost cause/custody: %v", err)
	}
	shape, err := parseOwnedEntrypointTaskXML(backend.tasks[`\mcp-local-hub-zeta`])
	if err != nil || shape.enabled || shape.command != txn.prior {
		t.Fatal("later restore row was abandoned")
	}
}

func TestEntrypointTaskExcludedDurableLoadCauseAndReadOnlyTarget(t *testing.T) {
	txn, backend, store, _ := excludedTaskFixture(t)
	cause := errors.New("synthetic durable load error")
	store.load = func(context.Context, string) ([]byte, error) { return nil, cause }
	_, err := txn.DisableCapturedVerified(context.Background(), EntrypointTaskApply{Apply: true})
	requireExcludedRefusal(t, err, backend, 0)
	if !errors.Is(err, cause) {
		t.Fatal("durable load cause lost")
	}
	store.load = nil
	_, err = txn.VerifyExcludedSHA256("unknown")
	requireExcludedRefusal(t, err, backend, 0)
}

func TestEntrypointTaskExcludedFinalFailureCompensatesKnownRows(t *testing.T) {
	for _, tc := range []string{"import", "readback", "foreign-drift", "compensation-fail"} {
		t.Run(tc, func(t *testing.T) {
			txn, backend, store, original := excludedTaskFixture(t)
			s, err := txn.DisableCapturedVerified(context.Background(), EntrypointTaskApply{Apply: true})
			if err != nil {
				t.Fatal(err)
			}
			disabled := map[string][]byte{}
			for n, b := range backend.tasks {
				disabled[n] = append([]byte(nil), b...)
			}
			cause := errors.New("synthetic final failure")
			cleanup := errors.New("synthetic compensation failure")
			finalStarted := false
			unknown := []byte("foreign definition")
			backend.importHook = func(name string, body []byte) error {
				if bytes.Equal(body, original[name]) && name == `\mcp-local-hub-alpha` {
					finalStarted = true
				}
				if tc == "compensation-fail" && finalStarted && bytes.Equal(body, disabled[name]) && name == `\mcp-local-hub-alpha` {
					return cleanup
				}
				if name == `\mcp-local-hub-zeta` && finalStarted && tc != "readback" {
					if tc == "foreign-drift" {
						backend.tasks[`\mcp-local-hub-alpha`] = append([]byte(nil), unknown...)
					}
					backend.importHook = nil
					if tc == "compensation-fail" {
						backend.importHook = func(n string, b []byte) error {
							if n == `\mcp-local-hub-alpha` && bytes.Equal(b, disabled[n]) {
								return cleanup
							}
							return nil
						}
					}
					return cause
				}
				return nil
			}
			if tc == "readback" {
				backend.exportHook = func(string) error {
					if finalStarted {
						backend.exportHook = nil
						return cause
					}
					return nil
				}
			}
			_, err = txn.RestoreOriginalEnabledAfterReady(context.Background(), excludedEnableRequest(txn, s.Target), s, func(context.Context, EntrypointTaskExcludedSnapshot) (EntrypointTaskReadiness, error) {
				return excludedReadiness(txn, s.Target), nil
			})
			var partial *EntrypointTaskPartialProgressError
			if !errors.Is(err, cause) || !errors.As(err, &partial) || len(store.bodies) != 2 {
				t.Fatalf("final failure missing cause/partial/custody: %v", err)
			}
			if tc == "compensation-fail" {
				if !errors.Is(err, cleanup) {
					t.Fatal("compensation cause lost")
				}
				return
			}
			for n, b := range disabled {
				if tc == "foreign-drift" && n == `\mcp-local-hub-alpha` {
					b = unknown
				}
				if !bytes.Equal(backend.tasks[n], b) {
					t.Fatalf("compensation overwrote foreign data or failed to redisable %s", n)
				}
			}
		})
	}
}

func TestEntrypointTaskExcludedCreatorContextCancellation(t *testing.T) {
	for _, stage := range []string{"callback", "post-readback", "between-imports", "final-readback"} {
		t.Run(stage, func(t *testing.T) {
			creator, cancelCreator := context.WithCancel(context.Background())
			defer cancelCreator()
			method, cancelMethod := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancelMethod()
			original := map[string][]byte{
				`\mcp-local-hub-alpha`: excludedTaskXML(`\mcp-local-hub-alpha`, "true"),
				`\mcp-local-hub-zeta`:  excludedTaskXML(`\mcp-local-hub-zeta`, "false"),
			}
			backend := newEntrypointTaskBackendFake(original, "test-user")
			store := &retainedTaskXMLStoreFake{bodies: map[string][]byte{}}
			txn, err := beginOwnedEntrypointTaskTxnWithStore(creator, t.TempDir()+`\entrypoint.lock`, `C:\synthetic\prior.exe`, `C:\synthetic\runtime.exe`, backend, "test-user", store)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				if err := txn.Close(); err != nil {
					t.Error(err)
				}
			})
			if err := txn.InventoryExport(); err != nil {
				t.Fatal(err)
			}
			snapshot, err := txn.DisableCapturedVerified(method, EntrypointTaskApply{Apply: true})
			if err != nil {
				t.Fatal(err)
			}
			disabled := make(map[string][]byte, len(original))
			for name, body := range backend.tasks {
				disabled[name] = append([]byte(nil), body...)
			}
			before, enabledImports, exports := backend.importCalls, 0, 0
			backend.importHook = func(name string, body []byte) error {
				shape, err := parseOwnedEntrypointTaskXML(body)
				if err != nil {
					return err
				}
				if shape.enabled {
					enabledImports++
				}
				if stage == "between-imports" && bytes.Equal(body, original[name]) && name == `\mcp-local-hub-alpha` {
					cancelCreator()
				}
				return nil
			}
			_, err = txn.RestoreOriginalEnabledAfterReady(method, excludedEnableRequest(txn, snapshot.Target), snapshot, func(_ context.Context, s EntrypointTaskExcludedSnapshot) (EntrypointTaskReadiness, error) {
				if stage == "callback" {
					cancelCreator()
				}
				backend.exportHook = func(string) error {
					exports++
					if stage == "post-readback" && exports == 2 || stage == "final-readback" && exports == 4 {
						cancelCreator()
					}
					return nil
				}
				return excludedReadiness(txn, s.Target), nil
			})
			var refusal *EntrypointTaskTransitionError
			if !errors.Is(err, context.Canceled) || !errors.As(err, &refusal) || method.Err() != nil || creator.Err() != context.Canceled {
				t.Fatalf("creator cancellation allowed finalization: err=%v creator=%v method=%v finalEnabledImports=%d", err, creator.Err(), method.Err(), enabledImports)
			}
			if stage == "callback" || stage == "post-readback" {
				if enabledImports != 0 || backend.importCalls != before {
					t.Fatal("cancelled creator reached final imports")
				}
			} else {
				var partial *EntrypointTaskPartialProgressError
				if enabledImports != 1 || !errors.As(err, &partial) {
					t.Fatal("known partial progress was not compensated after creator cancellation")
				}
			}
			for name, body := range disabled {
				if !bytes.Equal(backend.tasks[name], body) {
					t.Fatalf("creator cancellation left task %s enabled or changed its protected XML", name)
				}
			}
			if len(store.bodies) != 2 || len(txn.Backups()) != 2 {
				t.Fatal("creator cancellation discarded retained custody")
			}
		})
	}
}

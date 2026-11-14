//go:build windows

package scheduler

import (
	"bytes"
	"context"
	"testing"
)

func TestCanonicalEntrySetTaskProjectionPreservesUnrelatedXMLAndRestoresBothPriors(t *testing.T) {
	const canonical = `C:\Hub\mcphub.exe`
	const legacy = `C:\Hub\mcphub-windowless.exe`
	const product = `C:\Hub\mcphub-product\mcphub.exe`
	const foreign = `C:\Hub\mcphub.exe.operator.exe`
	before := map[string][]byte{
		`\mcp-local-hub-canonical`: taskXML(`\mcp-local-hub-canonical`, "test-user", canonical, "gui --port 31", "preserve "+canonical),
		`\mcp-local-hub-legacy`:    taskXML(`\mcp-local-hub-legacy`, "test-user", legacy, "supervise --ensure-alive", "preserve "+legacy),
		`\mcp-local-hub-current`:   taskXML(`\mcp-local-hub-current`, "test-user", product, "gui", "already current"),
		`\mcp-local-hub-foreign`:   taskXML(`\mcp-local-hub-foreign`, "test-user", foreign, "gui", "exact path boundary"),
	}
	backend := newEntrypointTaskBackendFake(before, "test-user")
	store := &retainedTaskXMLStoreFake{bodies: map[string][]byte{}}
	txn, err := beginOwnedEntrypointTaskTxnWithStore(context.Background(), t.TempDir()+`\entrypoint.lock`, canonical, product, backend, "test-user", store)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := txn.Close(); err != nil {
			t.Error(err)
		}
	})
	txn.additionalPrior = legacy // exact same explicit projection as the public constructor
	if err := txn.InventoryExport(); err != nil {
		t.Fatal(err)
	}
	if len(txn.Backups()) != 3 {
		t.Fatal("inventory did not select exactly the two priors and current Product")
	}
	if err := txn.RewriteCommand(); err != nil {
		t.Fatal(err)
	}
	if err := txn.VerifyRuntime(); err != nil {
		t.Fatal(err)
	}
	for name, raw := range before {
		shape, err := parseOwnedEntrypointTaskXML(raw)
		if err != nil {
			t.Fatal(err)
		}
		want := raw
		if shape.command == canonical || shape.command == legacy {
			want, err = rewriteOwnedEntrypointTaskCommand(raw, shape.command, product)
			if err != nil {
				t.Fatal(err)
			}
		}
		got, err := backend.ExportXML(name)
		if err != nil || !bytes.Equal(got, want) {
			t.Fatalf("task %q changed outside its exact Command: %v", name, err)
		}
	}
	backups := txn.Backups()
	if err := txn.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := beginOwnedEntrypointTaskTxnWithStore(context.Background(), t.TempDir()+`\entrypoint.lock`, canonical, product, backend, "test-user", store)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := reopened.Close(); err != nil {
			t.Error(err)
		}
	})
	reopened.additionalPrior = legacy
	if _, err := receiveOwnedEntrypointTaskBackups(reopened, backups); err != nil {
		t.Fatal(err)
	}
	if err := reopened.RestoreImport(); err != nil {
		t.Fatal(err)
	}
	for name, raw := range before {
		got, err := backend.ExportXML(name)
		if err != nil || !bytes.Equal(got, raw) {
			t.Fatalf("task %q prior XML was not restored byte-for-byte: %v", name, err)
		}
	}
}

func TestCanonicalEntrySetTaskProjectionRejectsUnqualifiedLegacyBeforeBackend(t *testing.T) {
	_, err := BeginOwnedEntrypointTaskTxnForCanonicalEntrySetWithStore(context.Background(), "unused", `C:\Hub\mcphub.exe`, `C:\Other\mcphub-windowless.exe`, `C:\Hub\mcphub-product\mcphub.exe`, nil)
	if err == nil {
		t.Fatal("foreign helper projection reached scheduler backend")
	}
}

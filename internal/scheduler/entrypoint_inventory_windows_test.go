//go:build windows

package scheduler

import (
	"context"
	"reflect"
	"testing"
)

func TestScanOwnedEntrypointTaskReferencesKeepsForeignTasksOutOfManagedInventory(t *testing.T) {
	backend := newEntrypointTaskBackendFake(map[string][]byte{
		`\mcp-local-hub-daemon`: taskXML(`\mcp-local-hub-daemon`, "test-user", `C:\bin\mcphub-runtime.exe`, "daemon", "hub"),
		`\mcp-local-hub-custom`: taskXML(`\mcp-local-hub-custom`, "test-user", `C:\tools\operator.exe`, "serve", "operator"),
	}, "test-user")
	refs, err := scanOwnedEntrypointTaskReferences(context.Background(), backend, "test-user", `C:\bin\mcphub.exe`, `C:\bin\mcphub-runtime.exe`)
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	if want := []OwnedEntrypointTaskReference{{TaskName: `\mcp-local-hub-daemon`, Command: `C:\bin\mcphub-runtime.exe`}}; !reflect.DeepEqual(refs, want) {
		t.Fatalf("refs = %#v, want %#v", refs, want)
	}
}

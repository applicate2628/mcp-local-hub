package api

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// This seeds durable provenance and private files; it never performs adopt or
// launches a daemon. The consumer must forward the verified raw snapshot.
func TestExecuteDeAdoptCodexAliasPinnedProofRestoresSelectedBytes(t *testing.T) {
	const name = "deadopt-proof"
	const alias = "deadopt-proof-mcphub"
	const selected = "[mcp_servers.deadopt-proof] # selected source\ncommand = 'private-tool' # source transport\nargs = ['--stdio']\nenabled = true\nstartup_timeout_sec = 30.0 # startup\ntool_timeout_sec = 75 # tool timeout\nfuture = { mode = 'keep' } # future policy\n"
	original := []byte("title = 'private fixture'\n\n" + selected + "\n[mcp_servers.sibling]\ncommand = 'before'\n")
	legacy := "title = 'private fixture'\n\n[mcp_servers]\n[mcp_servers.deadopt-proof-mcphub]\nstartup_timeout_sec = 10.0\ntool_timeout_sec = 75.0\nurl = 'http://127.0.0.1:7/mcp'\n\n[mcp_servers.sibling]\ncommand = 'before'\n"
	path, manifestRoot, stateRoot, rec := setupDeAdoptPlannerFixture(t, name, deAdoptPlannerFixture{
		state: AdoptOperationStateAdopted, originalState: AdoptOriginalStatePresent,
		liveConfig: legacy, snapshotBytes: original, writeSnapshot: true,
		manifestPresent: true, manifestBytes: deAdoptManagedManifest(name, 75),
	})
	rec.Clients[0].TargetEntryName = alias
	rec.Clients[0].ToolTimeoutSec = 75
	if err := writeAdoptedEntries(&AdoptedEntries{Version: adoptedEntriesSchemaVersion, Records: []AdoptProvenanceRecord{rec}}); err != nil {
		t.Fatal(err)
	}
	seedDeAdoptSupervisorIntent(t, stateRoot, rec)
	var encoded map[string]any
	if err := json.Unmarshal(mustReadFileForAdoptTest(t, filepath.Join(stateRoot, adoptedEntriesFileLeaf)), &encoded); err != nil {
		t.Fatal(err)
	}
	clientJSON := encoded["records"].([]any)[0].(map[string]any)["clients"].([]any)[0].(map[string]any)
	wantHash := fmt.Sprintf("%x", sha256.Sum256(original))
	if clientJSON["snapshot_sha256"] != wantHash || clientJSON["original_state"] != "present" {
		t.Fatalf("actual provenance wire shape = %#v", clientJSON)
	}
	rec.Clients[0].SnapshotSHA256 = strings.Repeat("0", 64)
	if err := writeAdoptedEntries(&AdoptedEntries{Version: adoptedEntriesSchemaVersion, Records: []AdoptProvenanceRecord{rec}}); err != nil {
		t.Fatal(err)
	}
	api := NewAPI()
	report, err := api.ExecuteDeAdopt(name, nil)
	if err != nil || report == nil || len(report.Failed) != 1 || len(report.Restored) != 0 {
		t.Fatalf("tampered provenance hash report/error = %#v, %v", report, err)
	}
	if !bytes.Equal(mustReadFileForAdoptTest(t, path), []byte(legacy)) {
		t.Fatal("tampered proof mutated client config")
	}
	fresh, found, err := ReadAdoptProvenance(name)
	if err != nil || !found {
		t.Fatalf("recovery provenance missing: %v", err)
	}
	fresh.Clients[0].SnapshotSHA256 = wantHash
	if err := writeAdoptedEntries(&AdoptedEntries{Version: adoptedEntriesSchemaVersion, Records: []AdoptProvenanceRecord{*fresh}}); err != nil {
		t.Fatal(err)
	}
	foreign := []byte(strings.Replace(legacy, "url = 'http://127.0.0.1:7/mcp'", "url = 'http://127.0.0.1:7/mcp' # foreign selected", 1))
	if err := os.WriteFile(path, foreign, 0o600); err != nil {
		t.Fatal(err)
	}
	report, err = api.ExecuteDeAdopt(name, nil)
	if err != nil || report == nil || len(report.Failed) != 1 || len(report.Restored) != 0 || !bytes.Equal(mustReadFileForAdoptTest(t, path), foreign) {
		t.Fatalf("foreign selected-comment report/error = %#v, %v", report, err)
	}
	later := []byte(strings.Replace(legacy, "command = 'before'", "command = 'after' # later sibling", 1))
	if err := os.WriteFile(path, later, 0o600); err != nil {
		t.Fatal(err)
	}
	report, err = api.ExecuteDeAdopt(name, nil)
	if err != nil || report == nil || !reflect.DeepEqual(report.Restored, []string{"codex-cli"}) || len(report.Failed) != 0 {
		t.Fatalf("valid pinned alias restore report/error = %#v, %v", report, err)
	}
	got := mustReadFileForAdoptTest(t, path)
	if !bytes.Contains(got, []byte(selected)) || !bytes.HasSuffix(got, []byte("\n[mcp_servers.sibling]\ncommand = 'after' # later sibling\n")) || bytes.Contains(got, []byte(alias)) {
		t.Fatalf("consumer lost exact selected bytes or sibling:\n%s", got)
	}
	assertDeAdoptClosed(t, manifestRoot, stateRoot, rec)
}

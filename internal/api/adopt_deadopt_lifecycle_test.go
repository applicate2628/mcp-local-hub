package api

import (
	"bytes"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"mcp-local-hub/internal/clients"
)

// TestAdoptDeAdoptLifecycleRoundTrip is the end-to-end release-qualification test the consilium
// (fable + Sol, 2026-07-16) flagged as the missing cross-surface coverage: no test ran a REAL
// ExecuteAdopt AND a REAL ExecuteDeAdopt against the same server (the de-adopt suite seeds an
// already-adopted state; the Playwright suite mocks de-adopt eligibility). This exercises the
// full v0.7 adoption lifecycle under an isolated state root: a native stdio entry is adopted
// (manifest + supervisor-intent + provenance row + snapshot created, client repointed to the hub
// URL), then de-adopted, and the client config MUST be restored to its EXACT pre-adopt native
// entry with the manifest removed and the provenance row closed.
func TestAdoptDeAdoptLifecycleRoundTrip(t *testing.T) {
	entry := "lifecycleroundtrip"
	nativeBody := `[profile.default]
model = "gpt-5"

[mcp_servers.keep]
url = "http://example.invalid/mcp"

[mcp_servers.lifecycleroundtrip]
command = "go"
args = ["version"]
`
	codexPath, manifestRoot, _ := setupAdoptTestEnv(t, entry, nativeBody)
	// setupAdoptTestEnv redirects HOME/USERPROFILE/LOCALAPPDATA/XDG_* but NOT APPDATA, so on
	// Windows a client whose config lives under %APPDATA% (vscode) would resolve to the real
	// developer config — and de-adopt's GLOBAL gate-ON scan would trip on the developer's own
	// gate-ON state for an unrelated client. Redirect APPDATA to a fresh temp so every client
	// config is isolated and absent (this test owns only the codex-cli entry).
	t.Setenv("APPDATA", t.TempDir())

	mutator, ok := clients.AsCASEntryMutator(clients.AllClients()["codex-cli"])
	if !ok {
		t.Fatal("codex-cli does not expose CASEntryMutator")
	}
	// Capture the pre-adopt native entry subtree for the exact-restoration assertion.
	preBytes := mustReadFileForAdoptTest(t, codexPath)
	preSubtree, prePresent, err := mutator.EntryRawSubtree(preBytes, entry)
	if err != nil || !prePresent {
		t.Fatalf("pre-adopt native entry not present: present=%v err=%v", prePresent, err)
	}

	// 1. ADOPT.
	plan, err := NewAPI().BuildAdoptPlan(AdoptOpts{EntryName: entry, Client: "codex-cli", ManifestName: entry, Port: 9308})
	if err != nil {
		t.Fatalf("BuildAdoptPlan: %v", err)
	}
	var out bytes.Buffer
	if err := NewAPI().ExecuteAdopt(plan, &out); err != nil {
		t.Fatalf("ExecuteAdopt: %v\n%s", err, out.String())
	}

	// Provenance captured: adopted row + snapshot dir on disk.
	rec, found, err := ReadAdoptProvenance(entry)
	if err != nil || !found {
		t.Fatalf("provenance row missing after adopt: found=%v err=%v", found, err)
	}
	if rec.OperationState != AdoptOperationStateAdopted {
		t.Errorf("provenance state = %s after adopt, want adopted", rec.OperationState)
	}
	snapDir, _ := adoptSnapshotDir(entry)
	if _, statErr := os.Stat(snapDir); statErr != nil {
		t.Fatalf("snapshot dir missing after adopt: %v", statErr)
	}
	// Client entry repointed to the hub (no longer native).
	adoptedBytes := mustReadFileForAdoptTest(t, codexPath)
	adoptedSubtree, _, _ := mutator.EntryRawSubtree(adoptedBytes, entry)
	if reflect.DeepEqual(adoptedSubtree, preSubtree) {
		t.Fatal("adopt did not change the client entry (still native)")
	}

	// 2. DE-ADOPT.
	var out2 bytes.Buffer
	report, err := NewAPI().ExecuteDeAdopt(entry, &out2)
	if err != nil {
		t.Fatalf("ExecuteDeAdopt: %v\n%s", err, out2.String())
	}
	if len(report.Restored) != 1 || report.Restored[0] != "codex-cli" {
		t.Fatalf("de-adopt report = %+v, want codex-cli restored", report)
	}

	// EXACT restoration: the restored entry subtree matches the pre-adopt native entry.
	restoredBytes := mustReadFileForAdoptTest(t, codexPath)
	restoredSubtree, restoredPresent, err := mutator.EntryRawSubtree(restoredBytes, entry)
	if err != nil || !restoredPresent {
		t.Fatalf("restored entry not present after de-adopt: present=%v err=%v", restoredPresent, err)
	}
	if !reflect.DeepEqual(restoredSubtree, preSubtree) {
		t.Errorf("de-adopt did not restore the exact pre-adopt native entry\n got: %#v\nwant: %#v", restoredSubtree, preSubtree)
	}

	// Manifest removed by de-adopt.
	if _, statErr := os.Stat(filepath.Join(manifestRoot, entry, "manifest.yaml")); !os.IsNotExist(statErr) {
		t.Errorf("manifest survived de-adopt (stat err = %v)", statErr)
	}
	// Provenance row closed (or gone).
	if rec2, found2, _ := ReadAdoptProvenance(entry); found2 && rec2.OperationState != AdoptOperationStateClosed {
		t.Errorf("provenance row state = %s after de-adopt, want closed (or row absent)", rec2.OperationState)
	}
}

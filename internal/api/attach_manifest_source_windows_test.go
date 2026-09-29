//go:build windows

package api

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestRestore_SourceAndLeasePreview_CodexSnapshotCandidate(t *testing.T) {
	root := hardenedTempDir(t)
	stateDir := filepath.Join(root, "state")
	if err := os.Mkdir(stateDir, 0700); err != nil {
		t.Fatal(err)
	}
	withDaemonStateRootOverride(t, stateDir)
	t.Setenv("MCPHUB_STATE_DIR_OVERRIDE", stateDir)
	manifestDir := filepath.Join(root, "manifests")
	t.Setenv("MCPHUB_MANIFEST_DIR_OVERRIDE", manifestDir)
	name := "restore-case"
	snapshot := []byte("[mcp_servers.restore-case]\ncommand = \"node\"\nargs = [\"server.js\"]\n")
	ref, sha, err := writeAdoptClientSnapshot(name, "codex-cli", snapshot)
	if err != nil {
		t.Fatal(err)
	}
	rec := AdoptProvenanceRecord{ManifestName: name, SourceClient: "codex-cli", SourceEntryName: name,
		Port: 19237, AdoptClients: []string{"codex-cli"}, OperationState: AdoptOperationStateAdopted,
		Clients: []AdoptClientProvenance{{Client: "codex-cli", OriginalState: AdoptOriginalStatePresent,
			SnapshotRef: ref, SnapshotSHA256: sha}}}
	binding, err := expectedAdoptBindingsFromProvenance(&rec)
	if err != nil {
		t.Fatal(err)
	}
	raw := renderStdioBridgeManifestYAMLWithMCPProtocolCompatibilityProfile(name, "node", []string{"server.js"}, nil, rec.Port, binding, "")
	rec.AdoptManifestHash, rec.ExpectedManifestHash = ManifestHashContent([]byte(raw)), ManifestHashContent([]byte(raw))
	if err := writeAdoptedEntries(&AdoptedEntries{Records: []AdoptProvenanceRecord{rec}}); err != nil {
		t.Fatal(err)
	}
	intent := &SupervisorIntentFile{Version: 1, IntentGeneration: 2, Daemons: []SupervisorDaemon{{
		TaskName: `\mcp-local-hub-restore-case-default`, Server: name, Daemon: "default", Command: filepath.Join(root, "mcphub.exe"),
		Args: []string{"daemon", "--server", name, "--daemon", "default"}, Port: rec.Port, ManifestHash: rec.ExpectedManifestHash,
	}}}
	if err := WriteSupervisorIntent(filepath.Join(stateDir, supervisorIntentFileLeaf), intent); err != nil {
		t.Fatal(err)
	}
	a := NewAPI()
	candidate, err := a.resolveMissingAttachManifest(AttachClientRequest{Server: name, Daemon: "default", Client: "mimocode"})
	if err != nil || candidate == nil || candidate.raw != raw || candidate.hash != rec.ExpectedManifestHash || candidate.provenance != "adopted-codex-snapshot" {
		t.Fatalf("conditional candidate refused: candidate_present=%t err=%v", candidate != nil, err)
	}
	if _, err := os.Lstat(filepath.Join(manifestDir, name, "manifest.yaml")); !os.IsNotExist(err) {
		t.Fatalf("candidate preview wrote canonical manifest: %v", err)
	}
	originalIdentity := candidate.sourceIdentity
	rec.UpdatedAt = time.Date(2026, 9, 28, 0, 0, 0, 0, time.UTC)
	if err := writeAdoptedEntries(&AdoptedEntries{Records: []AdoptProvenanceRecord{rec}}); err != nil {
		t.Fatal(err)
	}
	updated, err := a.resolveMissingAttachManifest(AttachClientRequest{Server: name, Daemon: "default", Client: "mimocode"})
	if err != nil || updated == nil || updated.sourceIdentity == originalIdentity {
		t.Fatalf("adopt provenance generation change was not frozen: candidate_present=%t err=%v", updated != nil, err)
	}
	intent.Daemons[0].ManifestHash = ManifestHashContent([]byte(raw + "\n"))
	if err := WriteSupervisorIntent(filepath.Join(stateDir, supervisorIntentFileLeaf), intent); err != nil {
		t.Fatal(err)
	}
	if candidate, err := a.resolveMissingAttachManifest(AttachClientRequest{Server: name, Daemon: "default", Client: "mimocode"}); candidate != nil || err == nil {
		t.Fatalf("mismatched frozen intent accepted: candidate_present=%t err=%v", candidate != nil, err)
	}
	assertCode := func(source, want string) {
		t.Helper()
		candidate, err := a.resolveMissingAttachManifest(AttachClientRequest{Server: name, Daemon: "default", Client: "mimocode", ManifestSource: source})
		var refusal *AttachPlanError
		if candidate != nil || !errors.As(err, &refusal) || refusal.Code != want {
			t.Fatalf("candidate_present=%t err=%v want=%s", candidate != nil, err, want)
		}
	}
	intent.Daemons[0].ManifestHash = rec.ExpectedManifestHash
	if err := WriteSupervisorIntent(filepath.Join(stateDir, supervisorIntentFileLeaf), intent); err != nil {
		t.Fatal(err)
	}
	mixedSource := []byte("[mcp_servers.restore-case]\ncommand = \"node\"\nargs = [\"server.js\"]\nurl = \"http://127.0.0.1:19377/mcp\"\n")
	_, mixedSHA, err := writeAdoptClientSnapshot(name, "codex-cli", mixedSource)
	if err != nil {
		t.Fatal(err)
	}
	rec.Clients[0].SnapshotSHA256 = mixedSHA
	if err := writeAdoptedEntries(&AdoptedEntries{Records: []AdoptProvenanceRecord{rec}}); err != nil {
		t.Fatal(err)
	}
	assertCode("", "manifest-source-drift")
	explicit := filepath.Join(stateDir, "exact-source.yaml")
	if err := WriteStateFileBytesAtomic(explicit, []byte(raw)); err != nil {
		t.Fatal(err)
	}
	if err := WriteStateFileBytesAtomic(filepath.Join(stateDir, filepath.FromSlash(ref)), []byte("corrupt pre-adopt snapshot")); err != nil {
		t.Fatal(err)
	}
	assertCode(explicit, "manifest-source-drift")
	if err := writeAdoptedEntries(&AdoptedEntries{}); err != nil {
		t.Fatal(err)
	}
	assertCode("", "manifest-source-not-retained")
	candidate, err = a.resolveMissingAttachManifest(AttachClientRequest{Server: name, Daemon: "default", Client: "mimocode", ManifestSource: explicit})
	if err != nil || candidate == nil || candidate.hash != rec.ExpectedManifestHash || candidate.provenance != "explicit-file" {
		t.Fatalf("ordinary intent did not accept exact explicit bytes: candidate_present=%t err=%v", candidate != nil, err)
	}
	if err := writeAdoptedEntries(&AdoptedEntries{ProtocolProfileUpdates: []AdoptProfileUpdateJournal{{ManifestName: name}}}); err != nil {
		t.Fatal(err)
	}
	assertCode(explicit, "profile-update-recovery-required")
}

// The literal manifest is a 0.4.36-format oracle, not an expectation rendered
// by the current implementation. Release commit 1038305527c2 has the same
// ordered YAML fields and ClientBinding wire shape in scan.go/adopt.go.
func TestRestore_SourceInventory_036GoldenAndRefusals(t *testing.T) {
	const goldenRaw = "name: qa-golden\nkind: global\ntransport: stdio-bridge\ncommand: node\nbase_args:\n    - server.js\ndaemons:\n    - name: default\n      port: 19481\nclient_bindings:\n    - client: codex-cli\n      daemon: default\n      url_path: /mcp\nweekly_refresh: false\n"
	const snapshot = "[mcp_servers.qa-golden]\ncommand = \"node\"\nargs = [\"server.js\"]\n"
	type fixture struct {
		a           *API
		name        string
		port        int
		raw         string
		stateDir    string
		manifestDir string
		snapshot    string
		rec         AdoptProvenanceRecord
		intent      *SupervisorIntentFile
	}
	makeFixture := func(t *testing.T, name string, port int, raw string) fixture {
		t.Helper()
		root := hardenedTempDir(t)
		stateDir := filepath.Join(root, "state")
		if err := os.Mkdir(stateDir, 0700); err != nil {
			t.Fatal(err)
		}
		withDaemonStateRootOverride(t, stateDir)
		t.Setenv("MCPHUB_STATE_DIR_OVERRIDE", stateDir)
		manifestDir := filepath.Join(root, "manifests")
		t.Setenv("MCPHUB_MANIFEST_DIR_OVERRIDE", manifestDir)
		source := strings.ReplaceAll(snapshot, "qa-golden", name)
		ref, sha, err := writeAdoptClientSnapshot(name, "codex-cli", []byte(source))
		if err != nil {
			t.Fatal(err)
		}
		hash := ManifestHashContent([]byte(raw))
		rec := AdoptProvenanceRecord{ManifestName: name, SourceClient: "codex-cli", SourceEntryName: name,
			Port: port, AdoptClients: []string{"codex-cli"}, OperationState: AdoptOperationStateAdopted,
			AdoptManifestHash: hash, ExpectedManifestHash: hash,
			Clients: []AdoptClientProvenance{{Client: "codex-cli", OriginalState: AdoptOriginalStatePresent,
				SnapshotRef: ref, SnapshotSHA256: sha}}}
		if err := writeAdoptedEntries(&AdoptedEntries{Records: []AdoptProvenanceRecord{rec}}); err != nil {
			t.Fatal(err)
		}
		intent := &SupervisorIntentFile{Version: 1, IntentGeneration: 2, Daemons: []SupervisorDaemon{{
			TaskName: `\mcp-local-hub-` + name + `-default`, Server: name, Daemon: "default", Command: filepath.Join(root, "mcphub.exe"),
			Args: []string{"daemon", "--server", name, "--daemon", "default"}, Port: port, ManifestHash: hash,
		}}}
		if err := WriteSupervisorIntent(filepath.Join(stateDir, supervisorIntentFileLeaf), intent); err != nil {
			t.Fatal(err)
		}
		return fixture{a: NewAPI(), name: name, port: port, raw: raw, stateDir: stateDir,
			manifestDir: manifestDir, snapshot: filepath.Join(stateDir, filepath.FromSlash(ref)), rec: rec, intent: intent}
	}
	assertCode := func(t *testing.T, f fixture, source, want string) {
		t.Helper()
		beforeIntent, _ := os.ReadFile(filepath.Join(f.stateDir, supervisorIntentFileLeaf))
		beforeAdopt, _ := os.ReadFile(filepath.Join(f.stateDir, adoptedEntriesFileLeaf))
		beforeSnapshot, _ := os.ReadFile(f.snapshot)
		candidate, err := f.a.resolveMissingAttachManifest(AttachClientRequest{Server: f.name, Daemon: "default", Client: "mimocode", ManifestSource: source})
		var refusal *AttachPlanError
		if candidate != nil || !errors.As(err, &refusal) || refusal.Code != want {
			t.Fatalf("candidate_present=%t err=%v want=%s", candidate != nil, err, want)
		}
		afterIntent, _ := os.ReadFile(filepath.Join(f.stateDir, supervisorIntentFileLeaf))
		afterAdopt, _ := os.ReadFile(filepath.Join(f.stateDir, adoptedEntriesFileLeaf))
		afterSnapshot, _ := os.ReadFile(f.snapshot)
		if string(afterIntent) != string(beforeIntent) || string(afterAdopt) != string(beforeAdopt) || string(afterSnapshot) != string(beforeSnapshot) {
			t.Fatal("source preview changed frozen intent, adopt provenance or saved snapshot")
		}
		if _, err := os.Lstat(filepath.Join(f.manifestDir, f.name, "manifest.yaml")); !os.IsNotExist(err) {
			t.Fatalf("source preview published canonical manifest: %v", err)
		}
	}
	for _, tc := range []struct {
		name string
		port int
		raw  string
	}{
		{"qa-golden", 19481, goldenRaw},
		{"arbitrary-zen", 19482, strings.ReplaceAll(strings.ReplaceAll(goldenRaw, "qa-golden", "arbitrary-zen"), "19481", "19482")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := makeFixture(t, tc.name, tc.port, tc.raw)
			candidate, err := f.a.resolveMissingAttachManifest(AttachClientRequest{Server: tc.name, Daemon: "default", Client: "mimocode"})
			if err != nil || candidate == nil || candidate.raw != tc.raw || candidate.hash != ManifestHashContent([]byte(tc.raw)) || candidate.provenance != "adopted-codex-snapshot" {
				t.Fatalf("0.4.36-format golden refused or changed: candidate_present=%t err=%v", candidate != nil, err)
			}
			if _, err := os.Lstat(filepath.Join(f.manifestDir, tc.name, "manifest.yaml")); !os.IsNotExist(err) {
				t.Fatalf("automatic preview published canonical manifest: %v", err)
			}
			explicit := filepath.Join(f.stateDir, "same-exact-source.yaml")
			if err := WriteStateFileBytesAtomic(explicit, []byte(tc.raw)); err != nil {
				t.Fatal(err)
			}
			withSameExplicit, err := f.a.resolveMissingAttachManifest(AttachClientRequest{Server: tc.name, Daemon: "default", Client: "mimocode", ManifestSource: explicit})
			if err != nil || withSameExplicit == nil || withSameExplicit.raw != tc.raw || withSameExplicit.provenance != "adopted-codex-snapshot" {
				t.Fatalf("identical explicit bytes displaced retained product source: candidate_present=%t err=%v", withSameExplicit != nil, err)
			}
		})
	}
	for _, tc := range []struct {
		name, want string
		mutate     func(*testing.T, fixture) string
	}{
		{"missing-intent", "intent-unavailable", func(t *testing.T, f fixture) string {
			if err := os.Remove(filepath.Join(f.stateDir, supervisorIntentFileLeaf)); err != nil {
				t.Fatal(err)
			}
			return ""
		}},
		{"missing-snapshot", "manifest-source-not-retained", func(t *testing.T, f fixture) string {
			if err := os.Remove(f.snapshot); err != nil {
				t.Fatal(err)
			}
			return ""
		}},
		{"corrupt-snapshot", "manifest-source-drift", func(t *testing.T, f fixture) string {
			if err := WriteStateFileBytesAtomic(f.snapshot, []byte("corrupt saved source")); err != nil {
				t.Fatal(err)
			}
			return ""
		}},
		{"unsettled-journal", "profile-update-recovery-required", func(t *testing.T, f fixture) string {
			if err := writeAdoptedEntries(&AdoptedEntries{Records: []AdoptProvenanceRecord{f.rec}, ProtocolProfileUpdates: []AdoptProfileUpdateJournal{{ManifestName: f.name}}}); err != nil {
				t.Fatal(err)
			}
			return ""
		}},
		{"duplicate-intent", "intent-ambiguous", func(t *testing.T, f fixture) string {
			f.intent.Daemons = append(f.intent.Daemons, f.intent.Daemons[0])
			if err := WriteSupervisorIntent(filepath.Join(f.stateDir, supervisorIntentFileLeaf), f.intent); err != nil {
				t.Fatal(err)
			}
			return ""
		}},
		{"duplicate-adopt-record", "adopt-provenance-conflict", func(t *testing.T, f fixture) string {
			if err := writeAdoptedEntries(&AdoptedEntries{Records: []AdoptProvenanceRecord{f.rec, f.rec}}); err != nil {
				t.Fatal(err)
			}
			return ""
		}},
		{"renderer-drift-hash", "manifest-candidate-mismatch", func(t *testing.T, f fixture) string {
			f.rec.ExpectedManifestHash = ManifestHashContent([]byte(f.raw + "# frozen alternate renderer output\n"))
			f.intent.Daemons[0].ManifestHash = f.rec.ExpectedManifestHash
			if err := writeAdoptedEntries(&AdoptedEntries{Records: []AdoptProvenanceRecord{f.rec}}); err != nil {
				t.Fatal(err)
			}
			if err := WriteSupervisorIntent(filepath.Join(f.stateDir, supervisorIntentFileLeaf), f.intent); err != nil {
				t.Fatal(err)
			}
			return ""
		}},
		{"distinct-explicit-hash", "manifest-candidate-mismatch", func(t *testing.T, f fixture) string {
			path := filepath.Join(f.stateDir, "distinct.yaml")
			if err := WriteStateFileBytesAtomic(path, []byte(f.raw+"\n")); err != nil {
				t.Fatal(err)
			}
			return path
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := makeFixture(t, "qa-golden", 19481, goldenRaw)
			assertCode(t, f, tc.mutate(t, f), tc.want)
		})
	}
}

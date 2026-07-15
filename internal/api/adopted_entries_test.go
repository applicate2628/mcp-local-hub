package api

import (
	"bytes"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

// sampleAdoptRecord builds a fully-populated record (all slices non-nil, fixed
// UTC times) so round-trip comparisons are deterministic.
func sampleAdoptRecord() AdoptProvenanceRecord {
	return AdoptProvenanceRecord{
		ManifestName:         "context7",
		SourceClient:         "codex-cli",
		SourceEntryName:      "context7",
		Port:                 9137,
		AdoptClients:         []string{"codex-cli", "claude-code"},
		AdoptManifestHash:    "aaaa1111",
		ExpectedManifestHash: "aaaa1111",
		RoutedSecretKeys:     []string{"CONTEXT7_API_KEY"},
		OperationState:       AdoptOperationStateAdopted,
		CreatedAt:            time.Unix(1720008000, 0).UTC(),
		UpdatedAt:            time.Unix(1720008001, 0).UTC(),
		Clients: []AdoptClientProvenance{
			{
				Client:         "codex-cli",
				OriginalState:  AdoptOriginalStatePresent,
				RestoreMode:    AdoptRestoreModeFunctionalEquivalent,
				SnapshotRef:    "adopt-provenance/context7/codex-cli.snapshot",
				SnapshotSHA256: "bbbb2222",
			},
			{
				Client:        "claude-code",
				OriginalState: AdoptOriginalStateAbsent,
				RestoreMode:   AdoptRestoreModeNA,
			},
		},
	}
}

func assertAdoptRecordEqual(t *testing.T, got, want AdoptProvenanceRecord) {
	t.Helper()
	if got.ManifestName != want.ManifestName ||
		got.SourceClient != want.SourceClient ||
		got.SourceEntryName != want.SourceEntryName ||
		got.Port != want.Port ||
		got.AdoptManifestHash != want.AdoptManifestHash ||
		got.ExpectedManifestHash != want.ExpectedManifestHash ||
		got.OperationState != want.OperationState {
		t.Errorf("scalar mismatch:\n got=%+v\nwant=%+v", got, want)
	}
	if !reflect.DeepEqual(got.AdoptClients, want.AdoptClients) {
		t.Errorf("AdoptClients = %#v, want %#v", got.AdoptClients, want.AdoptClients)
	}
	if !reflect.DeepEqual(got.RoutedSecretKeys, want.RoutedSecretKeys) {
		t.Errorf("RoutedSecretKeys = %#v, want %#v", got.RoutedSecretKeys, want.RoutedSecretKeys)
	}
	if !got.CreatedAt.Equal(want.CreatedAt) || !got.UpdatedAt.Equal(want.UpdatedAt) {
		t.Errorf("times = (%v,%v), want (%v,%v)", got.CreatedAt, got.UpdatedAt, want.CreatedAt, want.UpdatedAt)
	}
	if !reflect.DeepEqual(got.Clients, want.Clients) {
		t.Errorf("Clients = %#v, want %#v", got.Clients, want.Clients)
	}
}

// A2 — round-trip a record through the store.
func TestAdoptedEntriesRoundTrip(t *testing.T) {
	isolateStateDir(t)
	want := sampleAdoptRecord()
	if err := writeAdoptedEntries(&AdoptedEntries{Version: adoptedEntriesSchemaVersion, Records: []AdoptProvenanceRecord{want}}); err != nil {
		t.Fatalf("writeAdoptedEntries: %v", err)
	}
	got, err := readAdoptedEntries()
	if err != nil {
		t.Fatalf("readAdoptedEntries: %v", err)
	}
	if got.Version != adoptedEntriesSchemaVersion {
		t.Errorf("version = %d, want %d", got.Version, adoptedEntriesSchemaVersion)
	}
	if len(got.Records) != 1 {
		t.Fatalf("records = %d, want 1", len(got.Records))
	}
	assertAdoptRecordEqual(t, got.Records[0], want)
}

// A3 — schema-version reject (version ∉ {0,1}) and version-0 normalization.
func TestAdoptedEntriesSchemaVersionReject(t *testing.T) {
	isolateStateDir(t)

	if err := writeHubMcpStateFile(adoptedEntriesFileLeaf, []byte(`{"version":2,"records":[]}`)); err != nil {
		t.Fatalf("seed version-2 store: %v", err)
	}
	if _, err := readAdoptedEntries(); err == nil {
		t.Fatal("readAdoptedEntries accepted an unknown schema version; want error")
	} else if !strings.Contains(err.Error(), "unknown schema version") {
		t.Errorf("error = %v, want an 'unknown schema version' message", err)
	}

	if err := writeHubMcpStateFile(adoptedEntriesFileLeaf, []byte(`{"version":0,"records":[]}`)); err != nil {
		t.Fatalf("seed version-0 store: %v", err)
	}
	m, err := readAdoptedEntries()
	if err != nil {
		t.Fatalf("version-0 must normalize, got error: %v", err)
	}
	if m.Version != adoptedEntriesSchemaVersion {
		t.Errorf("version-0 normalized to %d, want %d", m.Version, adoptedEntriesSchemaVersion)
	}
}

// A4 — missing file → empty store, not an error.
func TestAdoptedEntriesMissingFileEmpty(t *testing.T) {
	isolateStateDir(t)
	m, err := readAdoptedEntries()
	if err != nil {
		t.Fatalf("readAdoptedEntries on fresh state: %v", err)
	}
	if m.Version != adoptedEntriesSchemaVersion {
		t.Errorf("version = %d, want %d", m.Version, adoptedEntriesSchemaVersion)
	}
	if len(m.Records) != 0 {
		t.Errorf("records = %d, want 0 on missing file", len(m.Records))
	}
}

// A5 — snapshot is written to the non-prunable adopt-provenance location, is
// hardened owner-only, and its returned sha256 matches the on-disk bytes.
func TestWriteAdoptClientSnapshotHardenedAndHashed(t *testing.T) {
	// snapshotTrapStateDir broadens the snapshot's parent on Windows (inheritable
	// non-allowlisted Modify ACE) so a regression to the plain backup copy would
	// inherit it and fail the owner-only assertion; POSIX uses a hardened root.
	stateDir := snapshotTrapStateDir(t)
	configBytes := []byte("{\n  \"mcpServers\": {\n    \"context7\": {\"command\": \"npx\", \"env\": {\"API_KEY\": \"literal-secret-value\"}}\n  }\n}\n")

	ref, sha, err := writeAdoptClientSnapshot("provsnap", "claude-code", configBytes)
	if err != nil {
		t.Fatalf("writeAdoptClientSnapshot: %v", err)
	}
	if ref != "adopt-provenance/provsnap/claude-code.snapshot" {
		t.Errorf("ref = %q, want adopt-provenance/provsnap/claude-code.snapshot", ref)
	}
	full := filepath.Join(stateDir, filepath.FromSlash(ref))
	onDisk, err := os.ReadFile(full)
	if err != nil {
		t.Fatalf("read snapshot bytes: %v", err)
	}
	if !bytes.Equal(onDisk, configBytes) {
		t.Errorf("snapshot bytes differ from source config")
	}
	if sha != ManifestHashContent(configBytes) || sha != ManifestHashContent(onDisk) {
		t.Errorf("returned sha %q != whole-file sha of on-disk bytes %q", sha, ManifestHashContent(onDisk))
	}
	assertAdoptSnapshotOwnerOnly(t, full)
}

// A6 — removeAdoptSnapshots deletes the whole manifest dir (incl. .lock
// sidecars); the snapshot path proves the non-prunable location (no
// .bak-mcp-local-hub- prefix, under adopt-provenance/).
func TestRemoveAdoptSnapshots(t *testing.T) {
	stateDir := isolateStateDir(t)
	if _, _, err := writeAdoptClientSnapshot("provrm", "codex-cli", []byte("A")); err != nil {
		t.Fatalf("pin codex snapshot: %v", err)
	}
	if _, _, err := writeAdoptClientSnapshot("provrm", "cursor", []byte("B")); err != nil {
		t.Fatalf("pin cursor snapshot: %v", err)
	}
	dir := filepath.Join(stateDir, adoptProvenanceSnapshotSubdir, "provrm")
	if _, err := os.Stat(filepath.Join(dir, "codex-cli.snapshot")); err != nil {
		t.Fatalf("codex snapshot missing pre-remove: %v", err)
	}

	// Non-prunable-location proof (claim 3): not a client-config sibling backup.
	if strings.Contains(dir, ".bak-mcp-local-hub-") {
		t.Errorf("snapshot dir carries a backup prefix (prunable): %s", dir)
	}
	if !strings.Contains(filepath.ToSlash(dir), "/"+adoptProvenanceSnapshotSubdir+"/") {
		t.Errorf("snapshot dir not under %s/: %s", adoptProvenanceSnapshotSubdir, dir)
	}

	if err := removeAdoptSnapshots("provrm"); err != nil {
		t.Fatalf("removeAdoptSnapshots: %v", err)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Errorf("snapshot dir survived removeAdoptSnapshots: stat err = %v", err)
	}
	// Idempotent — removing a missing dir is a no-op success.
	if err := removeAdoptSnapshots("provrm"); err != nil {
		t.Errorf("removeAdoptSnapshots on missing dir: %v", err)
	}
}

// A7 — ReadAdoptProvenance: present/absent/error (fail-closed on corrupt store).
func TestReadAdoptProvenancePresentAbsentError(t *testing.T) {
	isolateStateDir(t)

	// Absent: no store file.
	if rec, found, err := ReadAdoptProvenance("nope"); err != nil || found || rec != nil {
		t.Fatalf("absent read = (%v,%v,%v), want (nil,false,nil)", rec, found, err)
	}

	// Present.
	want := sampleAdoptRecord()
	if err := writeAdoptedEntries(&AdoptedEntries{Version: 1, Records: []AdoptProvenanceRecord{want}}); err != nil {
		t.Fatalf("seed store: %v", err)
	}
	rec, found, err := ReadAdoptProvenance("context7")
	if err != nil || !found || rec == nil {
		t.Fatalf("present read = (%v,%v,%v), want a record", rec, found, err)
	}
	if rec.ManifestName != "context7" {
		t.Errorf("present read manifest = %q, want context7", rec.ManifestName)
	}

	// Error: corrupt store → fail-closed.
	if err := writeHubMcpStateFile(adoptedEntriesFileLeaf, []byte("this is not json")); err != nil {
		t.Fatalf("seed corrupt store: %v", err)
	}
	if _, _, err := ReadAdoptProvenance("context7"); err == nil {
		t.Fatal("corrupt store read returned nil error; want fail-closed error")
	}
}

// A8 — F7: Phase 6 implements the two whole-manifest de-adopt mutators while
// the subset-only hash updater remains comment-only. Authoritative parse-time
// check via go/ast.
func TestF7DeAdoptMutatorImplementationBoundary(t *testing.T) {
	const fname = "adopted_entries.go"
	src, err := os.ReadFile(fname)
	if err != nil {
		t.Fatalf("read %s: %v", fname, err)
	}
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, fname, src, parser.ParseComments)
	if err != nil {
		t.Fatalf("parse %s: %v", fname, err)
	}
	wantBodies := map[string]bool{
		"MarkAdoptProvenanceDeAdopting": false,
		"CloseAdoptProvenance":          false,
	}
	for _, decl := range f.Decls {
		fd, ok := decl.(*ast.FuncDecl)
		if !ok {
			continue
		}
		if _, ok := wantBodies[fd.Name.Name]; ok {
			wantBodies[fd.Name.Name] = true
		}
		if fd.Name.Name == "UpdateAdoptExpectedManifestHash" {
			t.Errorf("F7 violation: %s defines subset-only func %q with a Go body; it must remain comment-only", fname, fd.Name.Name)
		}
	}
	for name, found := range wantBodies {
		if !found {
			t.Errorf("Phase 6 mutator %q has no Go body in %s", name, fname)
		}
	}
	// The subset updater MUST still appear as a comment so the follow-up contract
	// remains declared.
	for _, name := range []string{"UpdateAdoptExpectedManifestHash"} {
		if !bytes.Contains(src, []byte(name)) {
			t.Errorf("subset follow-up mutator %q must be declared as a comment in %s", name, fname)
		}
	}
}

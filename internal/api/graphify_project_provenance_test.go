package api

import (
	"bytes"
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// This is the real metadata/lease/publisher fixture. It deliberately supplies
// no native/profile capability and does not claim a qualified provider launch.
func TestProjectServiceStoppedDefinitionPublisherAndColdSubjectRead(t *testing.T) {
	isolateStateDir(t)
	t.Setenv("MCPHUB_MANIFEST_DIR_OVERRIDE", t.TempDir())
	ordinary := sampleAdoptRecord()
	if err := writeAdoptedEntries(&AdoptedEntries{Records: []AdoptProvenanceRecord{ordinary}}); err != nil {
		t.Fatal(err)
	}
	ordinaryBytes := graphifySiblingWireBytes(t, ordinary.ManifestName)
	record := sampleGraphifyServiceRecord(t)
	subject := projectServiceSubject{workspace: record.ProjectService.WorkspacePath, service: ProjectServiceGraphify}
	record.ManifestName = projectServiceSlot(subject)
	record.ProjectService.Schema = "project-service-v1"
	record.OperationState = AdoptOperationStateAdopting
	record.Port = adoptPortStart
	if record.Port == ordinary.Port {
		record.Port++
	}
	manifest := renderProviderStdioBridgeManifestYAML(record.ManifestName, "synthetic-provider", []string{"--graph", filepath.Join(subject.workspace, "fixture-graph.json")}, nil, nil, subject.workspace, record.Port, nil, "")
	record.AdoptManifestHash = ManifestHashContent([]byte(manifest))
	record.ExpectedManifestHash = record.AdoptManifestHash
	phase := "uncommitted"
	a := NewAPI()
	err := a.withProjectServiceLeases(subject, nil, &phase, func(current *AdoptProvenanceRecord, _ []byte, name string) error {
		if current != nil || name != record.ManifestName {
			t.Fatal("empty subject selected another manifest")
		}
		// Inventory DATA isolates this publisher test; no bindability or native
		// launch oracle is inferred from the synthetic free-port view.
		inventory := &adoptPortInventoryObservation{used: map[int]bool{}}
		if err := projectServicePublishRecord(nil, record, subject, inventory); err != nil {
			return err
		}
		phase = "pending"
		if err := a.manifestCreateWithAdoptLeaseHeld(name, manifest); err != nil {
			return err
		}
		observed, err := a.ManifestGetObserved(context.Background(), name)
		if err != nil {
			return err
		}
		if !bytes.Equal(observed.Bytes(), []byte(manifest)) {
			t.Fatal("actual stopped manifest readback changed")
		}
		defined := record
		defined.OperationState = AdoptOperationStateDefined
		if err := projectServicePublishRecord(&record, defined, subject, nil); err != nil {
			return err
		}
		record = defined
		phase = "committed_unverified"
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	cold, preimage, err := readProjectServiceAssociation(subject)
	if err != nil || cold == nil || cold.OperationState != AdoptOperationStateDefined || cold.ManifestName != record.ManifestName || cold.Port != record.Port {
		t.Fatalf("stopped definition did not survive cold metadata read: %v", err)
	}
	if !bytes.Equal(ordinaryBytes, graphifySiblingWireBytes(t, ordinary.ManifestName)) {
		t.Fatal("stopped publisher changed an unselected record")
	}
	duplicate := record
	duplicate.ManifestName = "other-project-service"
	duplicate.Port++
	if err := projectServicePublishRecord(nil, duplicate, subject, &adoptPortInventoryObservation{used: map[int]bool{}}); err == nil {
		t.Fatal("same subject reserved a second association")
	}
	_, after, err := readProjectServiceAssociation(subject)
	if err != nil || !bytes.Equal(preimage, after) {
		t.Fatal("failed competing reservation changed canonical bytes")
	}
}

func TestProjectServiceUnserializablePreimageNeverEqualsAbsence(t *testing.T) {
	invalid := sampleAdoptRecord()
	invalid.CreatedAt = time.Date(10000, time.January, 1, 0, 0, 0, 0, time.UTC)
	equal, err := sameProjectServiceRecord(nil, &invalid)
	if err == nil || equal {
		t.Fatal("failed record encoding weakened the preimage guard to absent equals absent")
	}
}

func sampleGraphifyServiceRecord(t *testing.T) AdoptProvenanceRecord {
	t.Helper()
	root, err := CanonicalWorkspacePath(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	rec := sampleAdoptRecord()
	rec.ManifestName = "graphify-" + WorkspaceKey(root)
	rec.SourceEntryName = "graphify"
	rec.AdoptManifestHash = strings.Repeat("a", 64)
	rec.ExpectedManifestHash = rec.AdoptManifestHash
	rec.Clients = []AdoptClientProvenance{}
	rec.AdoptClients = []string{}
	rec.RoutedSecretKeys = []string{}
	rec.OperationState = AdoptOperationStateDefined
	rec.ProjectService = &ProjectServiceProvenanceV1{Schema: "graphify-project-service-v1", Service: "graphify", WorkspacePath: root, WorkspaceKey: WorkspaceKey(root), ProfileSHA256: strings.Repeat("b", 64)}
	return rec
}

func seedAdoptProfileGraphifySibling(t *testing.T, state AdoptOperationState) (string, []byte) {
	t.Helper()
	service := sampleGraphifyServiceRecord(t)
	service.OperationState = state
	if err := withAdoptedEntriesLock(func() error {
		store, err := readCanonicalGraphifyAdoptedEntries()
		if err != nil {
			return err
		}
		store.selectedProjectService = service.ManifestName
		store.Records = append(store.Records, service)
		return writeAdoptedEntries(store)
	}); err != nil {
		t.Fatal(err)
	}
	return service.ManifestName, graphifySiblingWireBytes(t, service.ManifestName)
}

func graphifySiblingWireBytes(t *testing.T, name string) []byte {
	t.Helper()
	raw, err := readHubMcpStateFile(adoptedEntriesFileLeaf)
	if err != nil {
		t.Fatal(err)
	}
	var wire struct {
		Records []json.RawMessage `json:"records"`
	}
	if err := json.Unmarshal(raw, &wire); err != nil {
		t.Fatal(err)
	}
	for _, row := range wire.Records {
		var record struct {
			Name string `json:"manifest_name"`
		}
		if err := json.Unmarshal(row, &record); err != nil {
			t.Fatal(err)
		}
		if record.Name == name {
			return bytes.Clone(row)
		}
	}
	t.Fatal("protected Graphify sibling disappeared")
	return nil
}

func assertAdoptProfileGraphifySibling(t *testing.T, name string, before []byte) {
	t.Helper()
	if !bytes.Equal(before, graphifySiblingWireBytes(t, name)) {
		t.Fatal("ordinary journal changed protected Graphify bytes")
	}
}

func TestGraphifyPublisherPreservesJournalAndProtectedRowRefusals(t *testing.T) {
	_, ordinary, old := seedAdoptProfileUpdate(t)
	name, protected := seedAdoptProfileGraphifySibling(t, AdoptOperationStateDefined)
	journal := profileUpdateJournal(t, ordinary, old)
	for _, change := range []func(*AdoptedEntries){
		func(store *AdoptedEntries) {
			store.selectedProjectService = name
			store.ProtocolProfileUpdates = append(store.ProtocolProfileUpdates, *journal)
		},
		func(store *AdoptedEntries) {
			for i := range store.Records {
				if store.Records[i].ManifestName == name {
					store.Records[i].Port++
				}
			}
		},
	} {
		before, err := readHubMcpStateFile(adoptedEntriesFileLeaf)
		if err != nil {
			t.Fatal(err)
		}
		if err := withAdoptedEntriesLock(func() error {
			store, err := readCanonicalGraphifyAdoptedEntries()
			if err != nil {
				return err
			}
			change(store)
			return writeAdoptedEntries(store)
		}); err == nil {
			t.Fatal("selected journal/protected row mutation was admitted")
		}
		after, err := readHubMcpStateFile(adoptedEntriesFileLeaf)
		if err != nil || !bytes.Equal(before, after) {
			t.Fatal("refused publisher mutation changed persisted bytes")
		}
		assertAdoptProfileGraphifySibling(t, name, protected)
	}
}

func TestGraphifyCanonicalPublisherPreservesOrdinaryRecordAndColdRead(t *testing.T) {
	isolateStateDir(t)
	ordinary := sampleAdoptRecord()
	if err := writeAdoptedEntries(&AdoptedEntries{Records: []AdoptProvenanceRecord{ordinary}}); err != nil {
		t.Fatal(err)
	}
	before, err := readHubMcpStateFile(adoptedEntriesFileLeaf)
	if err != nil {
		t.Fatal(err)
	}
	store, err := readCanonicalGraphifyAdoptedEntries()
	if err != nil {
		t.Fatal(err)
	}
	service := sampleGraphifyServiceRecord(t)
	store.selectedProjectService = service.ManifestName
	store.Records = append(store.Records, service)
	if err := writeAdoptedEntries(store); err != nil {
		t.Fatal(err)
	}
	after, err := readHubMcpStateFile(adoptedEntriesFileLeaf)
	if err != nil {
		t.Fatal(err)
	}
	var oldWire, newWire struct {
		Records []json.RawMessage `json:"records"`
	}
	if json.Unmarshal(before, &oldWire) != nil || json.Unmarshal(after, &newWire) != nil || !bytes.Equal(oldWire.Records[0], newWire.Records[0]) {
		t.Fatal("unselected ordinary record bytes changed")
	}
	cold, err := readAdoptedEntries()
	if err != nil || cold.Version != adoptedEntriesSchemaV4 || len(cold.Records) != 2 || cold.Records[1].OperationState != AdoptOperationStateDefined {
		t.Fatal("defined association did not survive fresh read")
	}
	if _, err := decodeAdoptedEntriesWithMaxVersion(after, adoptedEntriesSchemaV3); err == nil {
		t.Fatal("older reader accepted schema4")
	}
}

func TestGraphifyPublisherRefusesForeignPreimageWithoutWrites(t *testing.T) {
	for _, change := range []func([]byte) []byte{
		func(raw []byte) []byte { return append(raw, '\n') },
		func(raw []byte) []byte {
			return bytes.Replace(raw, []byte(`"version": 1`), []byte(`"version": 1, "foreign": true`), 1)
		},
		func(raw []byte) []byte {
			return bytes.Replace(raw, []byte(`"version": 1`), []byte(`"version": 1, "version": 1`), 1)
		},
	} {
		t.Run("foreign", func(t *testing.T) {
			isolateStateDir(t)
			original, err := json.MarshalIndent(&AdoptedEntries{Version: 1, Records: []AdoptProvenanceRecord{sampleAdoptRecord()}}, "", "  ")
			if err != nil {
				t.Fatal(err)
			}
			foreign := change(original)
			if err := writeHubMcpStateFile(adoptedEntriesFileLeaf, foreign); err != nil {
				t.Fatal(err)
			}
			if _, err := readCanonicalGraphifyAdoptedEntries(); err == nil {
				t.Fatal("foreign representation admitted")
			}
			actual, err := readHubMcpStateFile(adoptedEntriesFileLeaf)
			if err != nil || !bytes.Equal(actual, foreign) {
				t.Fatal("foreign bytes mutated")
			}
		})
	}
}

func TestGraphifyRecoveryKeepsDefinedPendingAndClosedGeneration(t *testing.T) {
	isolateStateDir(t)
	service := sampleGraphifyServiceRecord(t)
	for _, state := range []AdoptOperationState{AdoptOperationStateAdopting, AdoptOperationStateDefined, AdoptOperationStateClosed} {
		service.OperationState = state
		service.UpdatedAt = time.Unix(1, 0).UTC()
		if verdict := classifyDeadAdoptingRow(service); verdict != adoptRowRecoveryKeep {
			t.Fatal("project association classified as disposable")
		}
		if err := abortAdoptProvenance(&service); err == nil {
			t.Fatal("legacy abort consumed project association")
		}
	}
	service.OperationState = AdoptOperationStateClosed
	if err := writeAdoptedEntries(&AdoptedEntries{Records: []AdoptProvenanceRecord{service}, selectedProjectService: service.ManifestName}); err != nil {
		t.Fatal(err)
	}
	before, err := readHubMcpStateFile(adoptedEntriesFileLeaf)
	if err != nil {
		t.Fatal(err)
	}
	if err := CloseAdoptProvenance(service.ManifestName); err == nil {
		t.Fatal("ordinary close consumed generation tombstone")
	}
	after, err := readHubMcpStateFile(adoptedEntriesFileLeaf)
	if err != nil || !bytes.Equal(before, after) {
		t.Fatal("closed tombstone removed")
	}
}

func TestProjectServiceCanonicalReadKeepsDistinctServicesAndWire(t *testing.T) {
	for _, schema := range []string{"graphify-project-service-v1", "project-service-v1"} {
		t.Run(schema, func(t *testing.T) {
			isolateStateDir(t)
			graphify := sampleGraphifyServiceRecord(t)
			graphify.ProjectService.Schema = schema
			graft := graphify
			graftService := *graphify.ProjectService
			graftService.Schema, graftService.Service = "project-service-v1", ProjectServiceGraft
			graft.ProjectService = &graftService
			graft.ManifestName = "graft-" + graftService.WorkspaceKey
			records := []AdoptProvenanceRecord{sampleAdoptRecord(), graphify, graft}
			version, err := adoptedEntriesVersionForRecords(records)
			if err != nil || version != adoptedEntriesSchemaV4 {
				t.Fatalf("shared version validation refused distinct subjects: %v", err)
			}
			raw, err := json.MarshalIndent(&AdoptedEntries{Version: version, Records: records}, "", "  ")
			if err != nil {
				t.Fatal(err)
			}
			if err := writeHubMcpStateFile(adoptedEntriesFileLeaf, raw); err != nil {
				t.Fatal(err)
			}
			store, err := readAdoptedEntries()
			if err != nil || store == nil || len(store.Records) != 3 || !store.present || !bytes.Equal(store.preimage, raw) {
				t.Fatalf("canonical full-subject read lost original model/preimage: %v", err)
			}
			if store.Records[1].ProjectService.Schema != schema || store.Records[1].ProjectService.Service != ProjectServiceGraphify || store.Records[2].ProjectService.Schema != "project-service-v1" || store.Records[2].ProjectService.Service != ProjectServiceGraft || store.Records[1].ProjectService.WorkspacePath != store.Records[2].ProjectService.WorkspacePath {
				t.Fatal("schema/service/workspace meaning changed")
			}
			selected, preimage, err := readProjectServiceAssociation(projectServiceSubject{workspace: graftService.WorkspacePath, service: ProjectServiceGraft})
			if err != nil || selected == nil || selected.ManifestName != graft.ManifestName || !bytes.Equal(preimage, raw) {
				t.Fatalf("full-subject selection confused the other service or lost its preimage: %v", err)
			}
			encoded, err := json.MarshalIndent(store, "", "  ")
			if err != nil || !bytes.Equal(encoded, raw) {
				t.Fatal("canonical decode changed original wire fields or values")
			}
			actual, err := readHubMcpStateFile(adoptedEntriesFileLeaf)
			if err != nil || !bytes.Equal(actual, raw) {
				t.Fatal("read normalized or rewrote original bytes")
			}
		})
	}
}

func TestProjectServiceCanonicalReadRefusesDuplicateFullSubject(t *testing.T) {
	for _, service := range []ProjectServiceKind{ProjectServiceGraphify, ProjectServiceGraft} {
		t.Run(string(service), func(t *testing.T) {
			isolateStateDir(t)
			first := sampleGraphifyServiceRecord(t)
			if service == ProjectServiceGraft {
				first.ProjectService.Schema, first.ProjectService.Service = "project-service-v1", service
			}
			first.OperationState = AdoptOperationStateClosed
			second := first
			secondService := *first.ProjectService
			secondService.Schema = "project-service-v1"
			second.ProjectService = &secondService
			second.ManifestName = "other-" + secondService.WorkspaceKey
			second.OperationState = AdoptOperationStateDefined
			raw, err := json.MarshalIndent(&AdoptedEntries{Version: adoptedEntriesSchemaV4, Records: []AdoptProvenanceRecord{first, second}}, "", "  ")
			if err != nil {
				t.Fatal(err)
			}
			if err := writeHubMcpStateFile(adoptedEntriesFileLeaf, raw); err != nil {
				t.Fatal(err)
			}
			if _, err := readAdoptedEntries(); err == nil || err.Error() != "GRAPHIFY_PROVENANCE_DUPLICATE_SUBJECT" {
				t.Fatalf("duplicate full subject was not refused by its canonical owner: %v", err)
			}
			actual, err := readHubMcpStateFile(adoptedEntriesFileLeaf)
			if err != nil || !bytes.Equal(actual, raw) {
				t.Fatal("duplicate refusal changed original bytes")
			}
		})
	}
}

func TestProjectServiceCodecRefusesUnknownAndMixedDiscriminators(t *testing.T) {
	for _, row := range []struct {
		name, schema string
		service      ProjectServiceKind
	}{
		{"legacy-schema-graft", "graphify-project-service-v1", ProjectServiceGraft},
		{"unknown-schema", "unknown-project-service-v1", ProjectServiceGraphify},
		{"unknown-service", "project-service-v1", ProjectServiceKind("unknown")},
		{"omitted-service", "project-service-v1", ""},
	} {
		t.Run(row.name, func(t *testing.T) {
			isolateStateDir(t)
			record := sampleGraphifyServiceRecord(t)
			record.ProjectService.Schema, record.ProjectService.Service = row.schema, row.service
			if _, err := adoptedEntriesVersionForRecords([]AdoptProvenanceRecord{record}); err == nil {
				t.Fatal("version-validation caller admitted unknown/mixed subject")
			}
			raw, err := json.MarshalIndent(&AdoptedEntries{Version: adoptedEntriesSchemaV4, Records: []AdoptProvenanceRecord{record}}, "", "  ")
			if err != nil {
				t.Fatal(err)
			}
			if err := writeHubMcpStateFile(adoptedEntriesFileLeaf, raw); err != nil {
				t.Fatal(err)
			}
			if _, err := readAdoptedEntries(); err == nil || err.Error() != "GRAPHIFY_PROJECT_SERVICE_SUBJECT_INVALID" {
				t.Fatalf("record decoder admitted unknown/mixed subject: %v", err)
			}
			actual, err := readHubMcpStateFile(adoptedEntriesFileLeaf)
			if err != nil || !bytes.Equal(actual, raw) {
				t.Fatal("invalid subject refusal changed original bytes")
			}
		})
	}
}

package api

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"path/filepath"
	"slices"
)

// The discriminator and full subject are durable metadata. None of these
// exported wire fields creates a qualified recipe or source authority.
type ProjectServiceKind string

const (
	ProjectServiceGraphify ProjectServiceKind = "graphify"
	ProjectServiceGraft    ProjectServiceKind = "graft"
)

type ProjectServiceProvenanceV1 struct {
	Schema        string             `json:"schema"`
	Service       ProjectServiceKind `json:"service"`
	WorkspacePath string             `json:"workspace_path"`
	WorkspaceKey  string             `json:"workspace_key"`
	ProfileSHA256 string             `json:"profile_sha256"`
}

type GraphifyTargetAttachmentState string

const (
	GraphifyTargetPending  GraphifyTargetAttachmentState = "pending"
	GraphifyTargetAttached GraphifyTargetAttachmentState = "attached"
	GraphifyTargetDetached GraphifyTargetAttachmentState = "detached"
)

var errGraphifyServiceOperationRequired = errors.New("GRAPHIFY_PROJECT_SERVICE_OPERATION_REQUIRED")

func validateProjectServiceRecord(rec AdoptProvenanceRecord) error {
	p := rec.ProjectService
	validSubjectKind := p != nil && (p.Schema == "graphify-project-service-v1" && p.Service == ProjectServiceGraphify ||
		p.Schema == "project-service-v1" && (p.Service == ProjectServiceGraphify || p.Service == ProjectServiceGraft))
	if !validSubjectKind || !filepath.IsAbs(p.WorkspacePath) || filepath.Clean(p.WorkspacePath) != p.WorkspacePath || p.WorkspaceKey != WorkspaceKey(p.WorkspacePath) {
		return errors.New("GRAPHIFY_PROJECT_SERVICE_SUBJECT_INVALID")
	}
	if _, ok := decodeLowerProjectAdoptSHA256(p.ProfileSHA256); !ok {
		return errors.New("GRAPHIFY_PROJECT_SERVICE_PROFILE_INVALID")
	}
	if checkManifestName(rec.ManifestName) != nil || !slices.Contains(adoptSupportedClients, rec.SourceClient) || rec.SourceEntryName == "" || rec.Port < 1 || rec.Port > 65535 || rec.CreatedAt.IsZero() || rec.UpdatedAt.IsZero() {
		return errors.New("GRAPHIFY_PROJECT_SERVICE_PROVENANCE_INVALID")
	}
	if _, ok := decodeLowerProjectAdoptSHA256(rec.AdoptManifestHash); !ok {
		return errors.New("GRAPHIFY_PROJECT_SERVICE_ORIGIN_INVALID")
	}
	if _, ok := decodeLowerProjectAdoptSHA256(rec.ExpectedManifestHash); !ok {
		return errors.New("GRAPHIFY_PROJECT_SERVICE_GENERATION_INVALID")
	}
	switch rec.OperationState {
	case AdoptOperationStateAdopting, AdoptOperationStateDefined, AdoptOperationStateAdopted, AdoptOperationStateClosed:
	default:
		return errors.New("GRAPHIFY_PROJECT_SERVICE_STATE_INVALID")
	}
	seen := make(map[string]bool, len(rec.Clients))
	for _, target := range rec.Clients {
		id, err := graphifyProjectTargetID(rec, target)
		if err != nil || target.TargetID != id || seen[id] {
			return errors.New("GRAPHIFY_PROJECT_SERVICE_TARGET_INVALID")
		}
		seen[id] = true
		switch target.AttachmentState {
		case GraphifyTargetPending, GraphifyTargetAttached, GraphifyTargetDetached:
		default:
			return errors.New("GRAPHIFY_PROJECT_SERVICE_ATTACHMENT_INVALID")
		}
	}
	return nil
}

func graphifyProjectTargetID(rec AdoptProvenanceRecord, target AdoptClientProvenance) (string, error) {
	if target.Client == "" || target.TargetEntryName == "" || target.ConfigTarget == nil {
		return "", errors.New("GRAPHIFY_TARGET_FULL_IDENTITY_REQUIRED")
	}
	if err := validateAdoptClientConfigTarget(rec, target); err != nil {
		return "", err
	}
	material, err := json.Marshal(struct {
		Domain string                     `json:"domain"`
		Client string                     `json:"client"`
		Target *AdoptClientConfigTargetV1 `json:"config_target"`
		Entry  string                     `json:"entry"`
	}{"graphify-project-target-v1", target.Client, target.ConfigTarget, target.TargetEntryName})
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256(material)
	return hex.EncodeToString(digest[:]), nil
}

type projectServiceSubject struct {
	workspace string
	service   ProjectServiceKind
}

// Selection uses the full canonical subject, independently of the invoking
// client, entry alias and profile. The existing short store owner serializes
// this read with every publisher; callers retain only their private snapshot.
// A snapshot grants no association or execution authority. Plan/apply callers
// acquire the subject slot before this selection and then the actual manifest
// lease before qualifying or publishing the selected generation.
func readProjectServiceAssociation(subject projectServiceSubject) (record *AdoptProvenanceRecord, preimage []byte, resultErr error) {
	if subject.workspace == "" || (subject.service != ProjectServiceGraphify && subject.service != ProjectServiceGraft) {
		return nil, nil, errors.New("PROJECT_SERVICE_SUBJECT_INVALID")
	}
	canonical, err := CanonicalWorkspacePath(subject.workspace)
	if err != nil || canonical != subject.workspace {
		return nil, nil, errors.Join(err, errors.New("PROJECT_SERVICE_SUBJECT_INVALID"))
	}
	resultErr = withAdoptedEntriesLock(func() error {
		store, err := readCanonicalGraphifyAdoptedEntries()
		if err != nil {
			return err
		}
		preimage = bytes.Clone(store.preimage)
		for _, candidate := range store.Records {
			p := candidate.ProjectService
			if p == nil {
				continue
			}
			if p.WorkspaceKey == WorkspaceKey(subject.workspace) && p.WorkspacePath != subject.workspace {
				return errors.New("PROJECT_SERVICE_SUBJECT_LOCATOR_COLLISION")
			}
			if p.WorkspacePath != subject.workspace || p.Service != subject.service {
				continue
			}
			if record != nil {
				return errors.New("PROJECT_SERVICE_DUPLICATE_SUBJECT")
			}
			selected := candidate
			record = &selected
		}
		return nil
	})
	return record, preimage, resultErr
}

func requireCanonicalAdoptedEntries(raw []byte, m *AdoptedEntries) error {
	canonical, err := json.MarshalIndent(m, "", "  ")
	if err != nil || !bytes.Equal(raw, canonical) {
		return errors.New("GRAPHIFY_PROVENANCE_CANONICAL_PREIMAGE_REQUIRED")
	}
	seenNames := make(map[string]bool, len(m.Records))
	seenSubjects := make(map[projectServiceSubject]bool)
	seenSlots := make(map[string]string)
	for _, rec := range m.Records {
		if rec.ManifestName == "" || seenNames[rec.ManifestName] {
			return errors.New("GRAPHIFY_PROVENANCE_DUPLICATE_MANIFEST")
		}
		seenNames[rec.ManifestName] = true
		if p := rec.ProjectService; p != nil {
			subject := projectServiceSubject{workspace: p.WorkspacePath, service: p.Service}
			if seenSubjects[subject] {
				return errors.New("GRAPHIFY_PROVENANCE_DUPLICATE_SUBJECT")
			}
			seenSubjects[subject] = true
			if prior, exists := seenSlots[p.WorkspaceKey]; exists && prior != p.WorkspacePath {
				return errors.New("GRAPHIFY_PROVENANCE_SUBJECT_LOCATOR_COLLISION")
			}
			seenSlots[p.WorkspaceKey] = p.WorkspacePath
		}
	}
	return nil
}

func readCanonicalGraphifyAdoptedEntries() (*AdoptedEntries, error) {
	m, err := readAdoptedEntries()
	if err != nil {
		return nil, err
	}
	if m.present {
		if err := requireCanonicalAdoptedEntries(m.preimage, m); err != nil {
			return nil, err
		}
	}
	return m, nil
}

func adoptedEntriesPreimageHasProjectService(raw []byte) bool {
	var header struct {
		Version int `json:"version"`
	}
	return json.Unmarshal(raw, &header) == nil && header.Version == adoptedEntriesSchemaV4
}

// The existing publisher alone validates canonical source bytes, actual current
// preimage, unchanged record/root spans and independent persisted readback.
func validateAdoptedEntriesPublication(m *AdoptedEntries, postimage []byte) error {
	current, err := readHubMcpStateFile(adoptedEntriesFileLeaf)
	if err != nil && !(isHubMcpStateMissingErr(err) && !m.present) {
		return err
	}
	if m.present != (err == nil) || !bytes.Equal(current, m.preimage) {
		return errors.New("GRAPHIFY_PROVENANCE_PREIMAGE_CHANGED")
	}
	if m.present {
		before, err := decodeAdoptedEntriesWithMaxVersion(current, adoptedEntriesSchemaV4)
		if err != nil {
			return err
		}
		if err := requireCanonicalAdoptedEntries(current, before); err != nil {
			return err
		}
	}
	after, err := decodeAdoptedEntriesWithMaxVersion(postimage, adoptedEntriesSchemaV4)
	if err != nil {
		return err
	}
	if err := requireCanonicalAdoptedEntries(postimage, after); err != nil {
		return err
	}
	if !m.present {
		return nil
	}
	var oldWire, newWire struct {
		Records                []json.RawMessage `json:"records"`
		ProtocolProfileUpdates json.RawMessage   `json:"protocol_profile_updates"`
	}
	if json.Unmarshal(current, &oldWire) != nil || json.Unmarshal(postimage, &newWire) != nil {
		return errors.New("GRAPHIFY_PROVENANCE_WIRE_INVALID")
	}
	if m.selectedProjectService != "" && !bytes.Equal(oldWire.ProtocolProfileUpdates, newWire.ProtocolProfileUpdates) {
		return errors.New("GRAPHIFY_PROVENANCE_UNSELECTED_ROOT_CHANGED")
	}
	newByName := make(map[string]json.RawMessage, len(newWire.Records))
	newOrder := make([]string, 0, len(newWire.Records))
	for _, raw := range newWire.Records {
		var rec AdoptProvenanceRecord
		if json.Unmarshal(raw, &rec) != nil {
			return errors.New("GRAPHIFY_PROVENANCE_WIRE_INVALID")
		}
		newByName[rec.ManifestName] = raw
		if rec.ManifestName != m.selectedProjectService {
			newOrder = append(newOrder, rec.ManifestName)
		}
	}
	oldOrder := make([]string, 0, len(oldWire.Records))
	for _, raw := range oldWire.Records {
		var rec AdoptProvenanceRecord
		if json.Unmarshal(raw, &rec) != nil {
			return errors.New("GRAPHIFY_PROVENANCE_WIRE_INVALID")
		}
		if rec.ManifestName == m.selectedProjectService {
			continue
		}
		oldOrder = append(oldOrder, rec.ManifestName)
		if rec.ProjectService != nil || m.selectedProjectService != "" {
			if !bytes.Equal(raw, newByName[rec.ManifestName]) {
				return errors.New("GRAPHIFY_PROVENANCE_UNSELECTED_RECORD_CHANGED")
			}
		}
	}
	if m.selectedProjectService != "" {
		oldNames, _ := json.Marshal(oldOrder)
		newNames, _ := json.Marshal(newOrder)
		if !bytes.Equal(oldNames, newNames) {
			return errors.New("GRAPHIFY_PROVENANCE_RECORD_ORDER_CHANGED")
		}
	}
	return nil
}

package api

import (
	"encoding/json"
	"errors"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"slices"
	"sort"
	"strings"

	"mcp-local-hub/internal/config"
)

// attachManifestCandidate is private: neither the source path nor raw YAML
// crosses the CLI/JSON boundary. Every use is recomputed at apply time.
type attachManifestCandidate struct {
	raw, hash, provenance, sourceIdentity string
}

func sameAttachManifestCandidate(left, right *attachManifestCandidate) bool {
	return left != nil && right != nil && left.raw == right.raw && left.hash == right.hash &&
		left.provenance == right.provenance && left.sourceIdentity == right.sourceIdentity
}

const attachManifestSourceMaxBytes = 8 << 20

func (a *API) resolveMissingAttachManifest(req AttachClientRequest) (*attachManifestCandidate, error) {
	deny := func(code string) (*attachManifestCandidate, error) { return nil, &AttachPlanError{Code: code} }
	if runtime.GOOS != "windows" {
		return deny("manifest-restore-unsupported-fs")
	}
	if err := checkManifestName(req.Server); err != nil || req.Daemon == "" || req.Client != "mimocode" {
		return deny("unsupported-selection")
	}
	if isEmbeddedManifestName(req.Server) {
		return deny("manifest-source-ambiguous")
	}
	dir, err := adoptCommittedManifestDir()
	if err != nil {
		return deny("manifest-source-drift")
	}
	if _, err := os.Lstat(filepath.Join(dir, req.Server, "manifest.yaml")); err == nil {
		return deny("manifest-restore-conflict")
	} else if !errors.Is(err, os.ErrNotExist) {
		return deny("manifest-source-drift")
	}
	stateDir, err := DaemonStateDir()
	if err != nil {
		return deny("intent-unavailable")
	}
	intent, err := ReadSupervisorIntent(filepath.Join(stateDir, supervisorIntentFileLeaf))
	if err != nil {
		return deny("intent-unavailable")
	}
	row, code := selectAttachIntent(intent, req)
	if code != "" || row == nil || row.ManifestHash == "" || row.Port <= 0 {
		if code == "intent-duplicate" {
			return deny("intent-ambiguous")
		}
		return deny("intent-unavailable")
	}
	store, err := readAdoptedEntries()
	if err != nil {
		return deny("adopt-provenance-conflict")
	}
	for _, journal := range store.ProtocolProfileUpdates {
		if journal.ManifestName == req.Server {
			return deny("profile-update-recovery-required")
		}
	}
	var rec *AdoptProvenanceRecord
	for i := range store.Records {
		if store.Records[i].ManifestName != req.Server {
			continue
		}
		if rec != nil {
			return deny("adopt-provenance-conflict")
		}
		rec = &store.Records[i]
		if rec.ProjectService != nil {
			return deny("graphify-project-service-operation-required")
		}
	}
	if rec != nil && (rec.OperationState != AdoptOperationStateAdopted || rec.ExpectedManifestHash == "" || rec.AdoptManifestHash == "" ||
		rec.ExpectedManifestHash != row.ManifestHash || rec.Port != row.Port || rec.ManifestName != req.Server ||
		rec.SourceClient == "" || rec.SourceEntryName == "" || len(rec.AdoptClients) == 0) {
		return deny("adopt-provenance-conflict")
	}
	var provenanceIdentity string
	if rec != nil {
		encoded, err := json.Marshal(rec)
		if err != nil {
			return deny("adopt-provenance-conflict")
		}
		provenanceIdentity = ManifestHashContent(encoded)
	}
	var automatic *attachManifestCandidate
	if rec != nil && rec.SourceClient == "codex-cli" && rec.ProviderSource == nil {
		candidate, code := a.deriveCodexAdoptManifestCandidate(rec)
		switch code {
		case "":
			if candidate.hash != row.ManifestHash {
				code = "manifest-candidate-mismatch"
			} else {
				candidate.sourceIdentity = provenanceIdentity
				automatic = candidate
			}
		case "manifest-source-not-retained":
			// An exact caller-supplied file may still recover missing saved bytes.
		default:
			return deny(code)
		}
		if code == "manifest-candidate-mismatch" && req.ManifestSource == "" {
			return deny(code)
		}
	}
	var explicit *attachManifestCandidate
	if req.ManifestSource != "" {
		// The anchored reader opens the named regular file without following a
		// final reparse point, bounds bytes, and checks its opened identity.
		raw, err := readStateFileInodeAnchoredWithOptions(req.ManifestSource,
			operatorRequiresSingleUserHome, attachManifestSourceMaxBytes, false, false, nil, nil)
		if err != nil {
			return deny("manifest-source-drift")
		}
		explicit = &attachManifestCandidate{raw: string(raw), hash: ManifestHashContent(raw), provenance: "explicit-file", sourceIdentity: provenanceIdentity}
		if explicit.hash != row.ManifestHash {
			return deny("manifest-candidate-mismatch")
		}
	}
	if automatic != nil && explicit != nil && automatic.raw != explicit.raw {
		return deny("manifest-source-ambiguous")
	}
	candidate := automatic
	if candidate == nil {
		candidate = explicit
	}
	if candidate == nil {
		return deny("manifest-source-not-retained")
	}
	if len(candidate.raw) == 0 || len(candidate.raw) > attachManifestSourceMaxBytes {
		return deny("manifest-candidate-mismatch")
	}
	if rec != nil && candidate.hash != rec.ExpectedManifestHash {
		return deny("manifest-candidate-mismatch")
	}
	manifest, err := parseManifestForName(req.Server, []byte(candidate.raw))
	if err != nil || len(manifest.Daemons) != 1 || manifest.Kind != config.KindGlobal || manifest.Transport == config.TransportRemoteHTTP ||
		manifest.Daemons[0].Name != req.Daemon || manifest.Daemons[0].Port != row.Port ||
		len(a.validateManifestForStorageName(req.Server, candidate.raw)) != 0 {
		return deny("manifest-candidate-mismatch")
	}
	if _, err := a.ManifestValidateMode(candidate.raw, ValidateModeStrict); err != nil {
		return deny("manifest-candidate-mismatch")
	}
	return candidate, nil
}

// deriveCodexAdoptManifestCandidate uses the saved pre-adopt source and the
// ordinary adopt renderer exactly once. It does not consult current client
// config, the vault, ambient defaults, or alternate format variants.
func (a *API) deriveCodexAdoptManifestCandidate(rec *AdoptProvenanceRecord) (*attachManifestCandidate, string) {
	if rec == nil || rec.ProviderSource != nil || rec.SourceClient != "codex-cli" || rec.SourceEntryName == "" || rec.Port <= 0 || rec.ExpectedManifestHash == "" {
		return nil, "adopt-provenance-conflict"
	}
	if len(rec.AdoptClients) == 0 || len(rec.Clients) != len(rec.AdoptClients) {
		return nil, "adopt-provenance-conflict"
	}
	var source *AdoptClientProvenance
	for _, client := range rec.AdoptClients {
		matchCount := 0
		for i := range rec.Clients {
			if rec.Clients[i].Client == client {
				matchCount++
				if client == rec.SourceClient {
					source = &rec.Clients[i]
				}
			}
		}
		if matchCount != 1 {
			return nil, "adopt-provenance-conflict"
		}
	}
	if source == nil || source.ConfigTarget != nil || source.OriginalState != AdoptOriginalStatePresent {
		return nil, "manifest-source-not-retained"
	}
	if source.SnapshotSHA256 == "" || source.SnapshotRef == "" {
		return nil, "manifest-source-not-retained"
	}
	wantRef := path.Join(adoptProvenanceSnapshotSubdir, rec.ManifestName, source.Client+adoptSnapshotFileSuffix)
	if source.SnapshotRef != wantRef {
		return nil, "adopt-provenance-conflict"
	}
	dir, err := adoptSnapshotDir(rec.ManifestName)
	if err != nil {
		return nil, "adopt-provenance-conflict"
	}
	snapshot, err := readStateFileInodeAnchoredWithOptions(filepath.Join(dir, source.Client+adoptSnapshotFileSuffix),
		operatorRequiresSingleUserHome, attachManifestSourceMaxBytes, false, false, nil, nil)
	if errors.Is(err, os.ErrNotExist) {
		return nil, "manifest-source-not-retained"
	}
	if err != nil || ManifestHashContent(snapshot) != source.SnapshotSHA256 {
		return nil, "manifest-source-drift"
	}
	entry, err := extractCodexStdioEntryFromBytes(snapshot, rec.SourceEntryName, codexStdioEntryGlobal)
	if err != nil || entry.Disabled || entry.URLPresent || entry.Command == "" {
		return nil, "manifest-source-drift"
	}
	env := cloneStringMap(entry.Env)
	routed, _, err := rewriteAdoptSensitiveEnv(rec.ManifestName, env)
	if err != nil {
		return nil, "adopt-provenance-conflict"
	}
	wantKeys := append([]string(nil), rec.RoutedSecretKeys...)
	sort.Strings(wantKeys)
	sort.Strings(routed)
	if !slices.Equal(wantKeys, routed) {
		return nil, "adopt-provenance-conflict"
	}
	bindings, err := expectedAdoptBindingsFromProvenance(rec)
	if err != nil {
		return nil, "adopt-provenance-conflict"
	}
	raw := renderStdioBridgeManifestYAMLWithMCPProtocolCompatibilityProfile(rec.ManifestName, entry.Command, entry.Args, env, rec.Port, bindings, strings.TrimSpace(rec.MCPProtocolCompatibilityProfile))
	return &attachManifestCandidate{raw: raw, hash: ManifestHashContent([]byte(raw)), provenance: "adopted-codex-snapshot"}, ""
}

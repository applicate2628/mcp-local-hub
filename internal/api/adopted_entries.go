// adopted_entries.go — durable pre-adopt provenance for `mcphub adopt`.
//
// Background. Adopt (adopt.go) absorbs an unmanaged direct-stdio client entry
// into a hub-managed manifest: it routes secrets, creates the manifest, and
// installs (rewrites each selected client's config to a hub URL). The reverse
// operation (de-adopt, a SEPARATE work-item) must restore each client's ORIGINAL
// pre-adopt entry — including its original secret-literal spelling — and it must
// hash-gate the manifest delete so it never removes an externally-edited
// manifest. None of that is recoverable from the post-adopt on-disk state alone.
//
// This file captures a durable, adopt-scoped provenance RECORD per adopt-created
// manifest in <state-dir>/adopted-entries.json, plus a pinned, hardened,
// non-prunable whole-config-file SNAPSHOT per `present` client under
// <state-dir>/adopt-provenance/<manifest>/<client>.snapshot. The record is
// written in state `adopting` (with snapshots) BEFORE the first irreversible
// adopt mutation, flipped to `adopted` only after Install succeeds, and aborted
// (row + snapshots) inside the adopt failure-cleanup.
//
// Store shape (decision work-items/decisions/2026-07-10-adopt-provenance-store-shape.md):
// a NEW file, NOT an extension of managed-entries.json (which is a data-loss-
// critical demigrate marker with a different lifecycle). The storage mechanics
// (schema version + flock + hardened state-file read/write) are COPIED from
// managed_entries.go:84,99-167, not shared.
//
// Scope boundary (arch F7 / plan AC A8). THIS work-item owns the storage layer,
// the snapshot helpers, the adopt-side lifecycle (capture / promote / abort),
// and the read accessor ReadAdoptProvenance. The de-adopt work-item IMPLEMENTS
// MarkAdoptProvenanceDeAdopting and CloseAdoptProvenance here;
// UpdateAdoptExpectedManifestHash remains DECLARED as a comment for the subset
// follow-up. The shared schema declares the de_adopting/closed enum values.
//
// Design: work-items/active/2026-07-09-adopt-side-durable-pre-adopt-provenance/design.md.

package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"mcp-local-hub/internal/clients"
	"mcp-local-hub/internal/config"

	"github.com/gofrs/flock"
)

const (
	adoptedEntriesFileLeaf     = "adopted-entries.json"
	adoptedEntriesLockFileLeaf = "adopted-entries.lock"

	// adoptProvenanceSnapshotSubdir is the state-dir-relative parent of the
	// per-manifest pinned snapshot directories. It is NON-PRUNABLE by
	// construction (design claim 3): pruneOldTimestamped only scans siblings of
	// a live client-config path whose names carry the ".bak-mcp-local-hub-"
	// prefix (clients.go:1145-1191); our snapshots live in a different directory
	// and carry no backup prefix, so no BackupKeep pass can reach them.
	adoptProvenanceSnapshotSubdir = "adopt-provenance"
	adoptSnapshotFileSuffix       = ".snapshot"
	adoptLeaseNamespaceLockLeaf   = ".lease-namespace.lock"

	// adoptManifestLeaseSuffix names the per-manifest adopt LEASE file
	// (<state-dir>/adopt-provenance/<manifest>.lease), a SIBLING to the
	// <manifest>/ snapshot dir so removeAdoptSnapshots' RemoveAll of the dir never
	// touches it. The lease is the owner-liveness authority (design r2 Signal 1):
	// held (flock) capture->promote by ExecuteAdoptWithOpts; a reaper TryLocks it
	// before reaping (can't acquire => a live adopt owns it => skip/fail-closed).
	adoptManifestLeaseSuffix = ".lease"
)

// Adopted-entry schema v1 remains the ordinary-record format. Schema v2 is
// required only while a provider source record exists, so removing the last
// provider record restores safe v1 downgrade compatibility.
const (
	adoptedEntriesSchemaV1      = 1
	adoptedEntriesSchemaV2      = 2
	adoptedEntriesSchemaVersion = adoptedEntriesSchemaV1
)

type ProviderSourceProvenanceV1 struct {
	ProviderClient              string `json:"provider_client"`
	PluginRef                   string `json:"plugin_ref"`
	ServerName                  string `json:"server_name"`
	Scope                       string `json:"scope"`
	ReceiptFingerprint          string `json:"receipt_fingerprint"`
	ActivationFingerprint       string `json:"activation_fingerprint"`
	PolicyFingerprint           string `json:"policy_fingerprint"`
	PriorEnabledPresent         bool   `json:"prior_enabled_present"`
	PriorEnabled                bool   `json:"prior_enabled"`
	ExpectedDisabledFingerprint string `json:"expected_disabled_fingerprint"`
	DisablePhase                string `json:"disable_phase"`
	DeAdoptPhase                string `json:"de_adopt_phase,omitempty"`
}

// ---------------------------------------------------------------------------
// Schema types (design "API-contract sketch", design.md:426-459).
// ---------------------------------------------------------------------------

// AdoptOperationState is the record's lifecycle state.
type AdoptOperationState string

const (
	AdoptOperationStateAdopting AdoptOperationState = "adopting"
	AdoptOperationStateAdopted  AdoptOperationState = "adopted"

	// AdoptOperationStateDeAdopting and AdoptOperationStateClosed are the
	// de-adopt-owned states. DECLARED here so the shared schema supports them;
	// THIS item never writes them (arch F7). The de-adopt work-item drives
	// adopted -> de_adopting -> closed.
	AdoptOperationStateDeAdopting AdoptOperationState = "de_adopting"
	AdoptOperationStateClosed     AdoptOperationState = "closed"
)

// AdoptOriginalState records whether a same-name entry existed in a client's
// config BEFORE adopt ran.
type AdoptOriginalState string

const (
	AdoptOriginalStatePresent AdoptOriginalState = "present"
	AdoptOriginalStateAbsent  AdoptOriginalState = "absent"

	// AdoptOriginalStatePresentMergedLower marks a client whose entry IS present
	// (GetEntry non-nil) but whose hub write target (ConfigPath) does not exist —
	// the entry resolves from a LOWER read layer the hub never writes (e.g.
	// MiMoCode config.json below an absent mimocode.json). NO snapshot is pinned:
	// de-adopt restores by REMOVING the hub entry from the write target, which
	// re-exposes the untouched lower-layer original. Additive enum value (no schema
	// bump); de-adopt MUST handle it (codex bot PR #528 finding 5 / design r2
	// "MiMoCode layer-source rule").
	AdoptOriginalStatePresentMergedLower AdoptOriginalState = "present-merged-lower"
)

// AdoptRestoreMode is the honesty label for how faithfully de-adopt can restore
// the pre-adopt entry. v1 ships "functional-equivalent" for every present
// client (byte-equivalence is UNVERIFIED per adapter — design limit i).
type AdoptRestoreMode string

const (
	AdoptRestoreModeFunctionalEquivalent AdoptRestoreMode = "functional-equivalent"
	AdoptRestoreModeByteEquivalent       AdoptRestoreMode = "byte-equivalent"
	AdoptRestoreModeNA                   AdoptRestoreMode = "n/a"
)

// AdoptClientProvenance is the per-client pre-adopt state + pinned-snapshot
// pointer. SnapshotRef/SnapshotSHA256 are present-only (empty for `absent`).
type AdoptClientProvenance struct {
	Client         string `json:"client"`
	ToolTimeoutSec int    `json:"tool_timeout_sec,omitempty"`
	// TargetEntryName is the physical client-config key written by adopt. Older
	// records omit it; readers treat that omission as SourceEntryName.
	TargetEntryName string             `json:"target_entry_name,omitempty"`
	OriginalState   AdoptOriginalState `json:"original_state"`
	RestoreMode     AdoptRestoreMode   `json:"restore_mode"`
	// SnapshotRef is the state-dir-relative (forward-slashed) path to the pinned
	// whole-config-file snapshot; present-only.
	SnapshotRef string `json:"snapshot_ref"`
	// SnapshotSHA256 is the WHOLE-FILE sha256 (hex) of the pinned snapshot bytes
	// (design F5 — trips on unrelated sibling-entry edits too). It is a
	// FAIL-CLOSED restore gate de-adopt MUST recompute and refuse restore on
	// mismatch OR missing snapshot (design P2-1); present-only.
	SnapshotSHA256 string `json:"snapshot_sha256"`
}

// AdoptProvenanceRecord is one adopt-created manifest's durable provenance.
// (No expected_hub_shape — DROPPED per arch F3; de-adopt recomputes the expected
// hub shape via the existing liveEntryMatchesManifestBinding owner.)
type AdoptProvenanceRecord struct {
	ManifestName                    string   `json:"manifest_name"`
	SourceClient                    string   `json:"source_client"`
	SourceEntryName                 string   `json:"source_entry_name"`
	Port                            int      `json:"port"`
	MCPProtocolCompatibilityProfile string   `json:"mcp_protocol_compatibility_profile,omitempty"`
	AdoptClients                    []string `json:"adopt_clients"`
	// AdoptManifestHash is the immutable sha256 of the adopt-generated manifest
	// bytes (plan.ManifestYAML). ExpectedManifestHash starts equal to it; de-adopt
	// updates ExpectedManifestHash after a subset binding edit. BOTH are populated
	// AT CAPTURE (arch F1) so a committed-but-`adopting` row is never empty-hashed.
	AdoptManifestHash    string                      `json:"adopt_manifest_hash"`
	ExpectedManifestHash string                      `json:"expected_manifest_hash"`
	RoutedSecretKeys     []string                    "json:\"routed_secr\u0065t_keys\""
	OperationState       AdoptOperationState         `json:"operation_state"`
	CreatedAt            time.Time                   `json:"created_at"`
	UpdatedAt            time.Time                   `json:"updated_at"`
	Clients              []AdoptClientProvenance     `json:"clients"`
	ProviderSource       *ProviderSourceProvenanceV1 `json:"provider_source,omitempty"`
}

// AdoptedEntries is the <state-dir>/adopted-entries.json file root.
type AdoptedEntries struct {
	Version                int                         `json:"version"`
	Records                []AdoptProvenanceRecord     `json:"records"`
	ProtocolProfileUpdates []AdoptProfileUpdateJournal `json:"protocol_profile_updates,omitempty"`
}

// ---------------------------------------------------------------------------
// Storage (COPIED from managed_entries.go:84,99-167).
// ---------------------------------------------------------------------------

// adoptedEntriesMu serializes in-process read-modify-write cycles on the store.
// Cross-process serialization is the flock in withAdoptedEntriesLock.
var adoptedEntriesMu sync.Mutex

// withAdoptedEntriesLock holds the in-process mutex AND a cross-process flock on
// <state-dir>/adopted-entries.lock for the duration of fn. Lock ordering mirrors
// withManagedEntriesLock: in-process mutex FIRST, then the flock.
//
// Deadlock-freedom of the whole capture path: the per-snapshot flock that
// WriteStateFileBytesAtomic takes (<snapshot>.lock) is acquired STRICTLY INSIDE
// this lock (adopted-entries.lock -> <snapshot>.lock, never reversed).
func withAdoptedEntriesLock(fn func() error) error {
	adoptedEntriesMu.Lock()
	defer adoptedEntriesMu.Unlock()

	dir, err := DaemonStateDir()
	if err != nil {
		return fmt.Errorf("adopted-entries lock: resolve state dir: %w", err)
	}
	lockPath := filepath.Join(dir, adoptedEntriesLockFileLeaf)
	lk := flock.New(lockPath)
	if err := lk.Lock(); err != nil {
		return fmt.Errorf("adopted-entries flock %s: %w", lockPath, err)
	}
	defer func() { _ = lk.Unlock() }()

	return fn()
}

// readAdoptedEntries accepts ordinary v1 and provider-bearing v2 stores. The
// existing v1 reader contract is preserved by decodeAdoptedEntriesWithMaxVersion
// when a caller's maximum is v1.
func readAdoptedEntries() (*AdoptedEntries, error) {
	raw, err := readHubMcpStateFile(adoptedEntriesFileLeaf)
	if err != nil {
		if isHubMcpStateMissingErr(err) {
			return &AdoptedEntries{Version: adoptedEntriesSchemaVersion}, nil
		}
		return nil, err
	}
	return decodeAdoptedEntriesWithMaxVersion(raw, adoptedEntriesSchemaV2)
}

func decodeAdoptedEntriesWithMaxVersion(raw []byte, maxVersion int) (*AdoptedEntries, error) {
	var m AdoptedEntries
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, fmt.Errorf("parse adopted-entries.json: %w", err)
	}
	if m.Version == 0 {
		m.Version = adoptedEntriesSchemaV1
	}
	if m.Version != adoptedEntriesSchemaV1 && m.Version != adoptedEntriesSchemaV2 || m.Version > maxVersion {
		return nil, fmt.Errorf("adopted-entries.json: unknown schema version %d (this build expects at most %d)", m.Version, maxVersion)
	}
	hasProvider := false
	for i := range m.Records {
		provider := m.Records[i].ProviderSource
		if provider == nil {
			continue
		}
		hasProvider = true
		if err := validateProviderSourceProvenance(provider); err != nil {
			return nil, fmt.Errorf("adopted-entries.json: record %q provider source: %w", m.Records[i].ManifestName, err)
		}
	}
	if hasProvider && m.Version != adoptedEntriesSchemaV2 {
		return nil, errors.New("adopted-entries.json: provider provenance requires schema version 2")
	}
	if !hasProvider && m.Version != adoptedEntriesSchemaV1 {
		return nil, errors.New("adopted-entries.json: schema version 2 requires provider provenance")
	}
	return &m, nil
}

// writeAdoptedEntries serializes m and writes it via the hardened hub-mcp
// state-file pipeline (handle-relative, DACL-bound temp + atomic rename).
func writeAdoptedEntries(m *AdoptedEntries) error {
	version, err := adoptedEntriesVersionForRecords(m.Records)
	if err != nil {
		return err
	}
	m.Version = version
	raw, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal adopted-entries: %w", err)
	}
	return writeHubMcpStateFile(adoptedEntriesFileLeaf, raw)
}

func adoptedEntriesVersionForRecords(records []AdoptProvenanceRecord) (int, error) {
	hasProvider := false
	for i := range records {
		if records[i].ProviderSource == nil {
			continue
		}
		hasProvider = true
		if err := validateProviderSourceProvenance(records[i].ProviderSource); err != nil {
			return 0, fmt.Errorf("adopted-entries: record %q provider source: %w", records[i].ManifestName, err)
		}
	}
	if hasProvider {
		return adoptedEntriesSchemaV2, nil
	}
	return adoptedEntriesSchemaV1, nil
}

func validateProviderSourceProvenance(provider *ProviderSourceProvenanceV1) error {
	if provider == nil {
		return nil
	}
	for _, field := range []struct {
		name  string
		value string
	}{
		{"provider_client", provider.ProviderClient},
		{"plugin_ref", provider.PluginRef},
		{"server_name", provider.ServerName},
		{"scope", provider.Scope},
		{"receipt_fingerprint", provider.ReceiptFingerprint},
		{"activation_fingerprint", provider.ActivationFingerprint},
		{"policy_fingerprint", provider.PolicyFingerprint},
		{"expected_disabled_fingerprint", provider.ExpectedDisabledFingerprint},
	} {
		if strings.TrimSpace(field.value) == "" {
			return fmt.Errorf("%s is required", field.name)
		}
	}
	if provider.Scope != "user" {
		return fmt.Errorf("scope %q is unsupported", provider.Scope)
	}
	if !provider.PriorEnabledPresent && provider.PriorEnabled {
		return errors.New("prior_enabled requires prior_enabled_present")
	}
	if provider.DisablePhase != "disable_planned" && provider.DisablePhase != "disable_applied" {
		return fmt.Errorf("disable_phase %q is invalid", provider.DisablePhase)
	}
	switch provider.DeAdoptPhase {
	case "", "managed_stop_settled", "managed_removed", "restore_applied":
	default:
		return fmt.Errorf("de_adopt_phase %q is invalid", provider.DeAdoptPhase)
	}
	return nil
}

func markProviderDisableApplied(manifestName string, activationFingerprint string) error {
	if activationFingerprint == "" {
		return fmt.Errorf("provider disable applied: activation fingerprint is empty")
	}
	return withAdoptedEntriesLock(func() error {
		store, err := readAdoptedEntries()
		if err != nil {
			return err
		}
		for i := range store.Records {
			record := &store.Records[i]
			if record.ManifestName != manifestName || record.ProviderSource == nil {
				continue
			}
			provider := record.ProviderSource
			if provider.DisablePhase != "disable_planned" || provider.ExpectedDisabledFingerprint != activationFingerprint {
				return fmt.Errorf("provider disable applied: durable provider state changed")
			}
			provider.DisablePhase = "disable_applied"
			record.UpdatedAt = time.Now().UTC()
			return writeAdoptedEntries(store)
		}
		return fmt.Errorf("provider disable applied: provenance record missing")
	})
}

// ---------------------------------------------------------------------------
// Snapshot storage (hardened, non-prunable, secret-bearing).
// ---------------------------------------------------------------------------

// adoptSnapshotDir returns <state-dir>/adopt-provenance/<manifest>. The manifest
// name is re-validated as a safe single path component (defense-in-depth: it is
// already CheckManifestName'd upstream, but this helper composes a full path fed
// to WriteStateFileBytesAtomic, so a traversal here would escape the snapshot
// root).
func adoptSnapshotDir(manifestName string) (string, error) {
	if err := CheckManifestName(manifestName); err != nil {
		return "", fmt.Errorf("adopt snapshot dir: invalid manifest name %q: %w", manifestName, err)
	}
	// Reserve the ".lease" suffix adopt-provenance-locally (P3-1). CheckManifestName
	// ACCEPTS a dotted name like "foo.lease", but <state>/adopt-provenance/foo.lease
	// is BOTH manifest "foo.lease"'s snapshot DIR and manifest "foo"'s lease FILE
	// (adoptManifestLeasePath). Without this guard, removeAdoptSnapshots("foo.lease")
	// RemoveAll's that path and unlinks a concurrently-HELD "foo" lease → split-lease
	// → the dead-owner precondition of the reap classifier is defeated → a live
	// committed "foo" row can be reaped (P1 data loss). Both path owners carry the
	// SAME guard (arch C1) so they can never diverge; the refusal fail-closes at
	// tryAcquireAdoptManifestLease (adopt step 0b) with ZERO side effects.
	if strings.HasSuffix(manifestName, adoptManifestLeaseSuffix) {
		return "", fmt.Errorf("adopt provenance: manifest name %q ends in the reserved %q suffix", manifestName, adoptManifestLeaseSuffix)
	}
	dir, err := DaemonStateDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, adoptProvenanceSnapshotSubdir, manifestName), nil
}

// writeAdoptClientSnapshot pins client's whole live-config bytes as a hardened,
// owner-only snapshot under <state-dir>/adopt-provenance/<manifest>/<client>.snapshot
// and returns the state-dir-relative ref plus the whole-file sha256 (hex).
//
// The write goes through WriteStateFileBytesAtomic (owner-only handle-bound DACL
// + parent-gate posture + per-file flock + atomic temp+rename), NEVER the backup
// lane's plain 0600 copy — the config may hold literal secret env values (design
// claim 8 / "Snapshot is secret-bearing"). The sha256 is WHOLE-FILE (design F5).
func writeAdoptClientSnapshot(manifestName, client string, configBytes []byte) (ref, sha256Hex string, err error) {
	if err := validateAdoptSnapshotClientName(client); err != nil {
		return "", "", err
	}
	dir, err := adoptSnapshotDir(manifestName)
	if err != nil {
		return "", "", err
	}
	leaf := client + adoptSnapshotFileSuffix
	full := filepath.Join(dir, leaf)
	if err := WriteStateFileBytesAtomic(full, configBytes); err != nil {
		return "", "", fmt.Errorf("write adopt snapshot %s/%s: %w", manifestName, leaf, err)
	}
	// State-dir-relative ref, forward-slashed so the on-disk JSON is portable
	// across OSes; the de-adopt consumer FromSlash-joins it to the state dir.
	ref = path.Join(adoptProvenanceSnapshotSubdir, manifestName, leaf)
	sha256Hex = ManifestHashContent(configBytes)
	return ref, sha256Hex, nil
}

// removeAdoptSnapshots deletes the entire per-manifest snapshot directory
// (including any <client>.snapshot.lock sidecar WriteStateFileBytesAtomic left).
// Idempotent — RemoveAll on a missing dir returns nil.
func removeAdoptSnapshots(manifestName string) error {
	dir, err := adoptSnapshotDir(manifestName)
	if err != nil {
		return err
	}
	if err := os.RemoveAll(dir); err != nil {
		return fmt.Errorf("remove adopt snapshots %s: %w", manifestName, err)
	}
	return nil
}

// validateAdoptSnapshotClientName rejects a client id that would not be a safe
// single path component once suffixed with ".snapshot".
func validateAdoptSnapshotClientName(client string) error {
	if err := validateStateFileName(client + adoptSnapshotFileSuffix); err != nil {
		return fmt.Errorf("adopt snapshot: invalid client name %q: %w", client, err)
	}
	return nil
}

// ---------------------------------------------------------------------------
// Read surface consumed by de-adopt — IN SCOPE for THIS item (real body).
// ---------------------------------------------------------------------------

// ReadAdoptProvenance returns the provenance record for manifestName. found is
// false when no row exists; a read/parse error propagates (fail-closed). Pure
// read: takes only the in-process mutex and relies on the atomic-rename
// guarantee of the state-file pipeline (mirrors IsManagedEntry).
func ReadAdoptProvenance(manifestName string) (rec *AdoptProvenanceRecord, found bool, err error) {
	adoptedEntriesMu.Lock()
	defer adoptedEntriesMu.Unlock()

	m, err := readAdoptedEntries()
	if err != nil {
		return nil, false, err
	}
	for i := range m.Records {
		if m.Records[i].ManifestName == manifestName {
			cp := m.Records[i]
			return &cp, true, nil
		}
	}
	return nil, false, nil
}

// ListDeAdoptRecoverableManifestNames returns the redaction-safe manifest names
// whose durable provenance row is waiting for de-adopt roll-forward recovery.
// Snapshot references, routed secret keys, and all other provenance fields stay
// inside the API layer and are never exposed to callers.
func ListDeAdoptRecoverableManifestNames() ([]string, error) {
	adoptedEntriesMu.Lock()
	defer adoptedEntriesMu.Unlock()

	store, err := readAdoptedEntries()
	if err != nil {
		return nil, err
	}
	names := make([]string, 0)
	for _, rec := range store.Records {
		if rec.OperationState == AdoptOperationStateDeAdopting {
			names = append(names, rec.ManifestName)
		}
	}
	sort.Strings(names)
	return names, nil
}

// ---------------------------------------------------------------------------
// Crash-consistency + concurrency primitives (design r2 addendum): the
// per-manifest LEASE (Signal 1), the snapshot-dir lister (Signal 3 backstop),
// and the ONE classifier (Signal 2) used by BOTH capture-reap and the GC.
// ---------------------------------------------------------------------------

// adoptManifestLeasePath returns <state-dir>/adopt-provenance/<manifest>.lease —
// a SIBLING to the <manifest>/ snapshot dir (so removeAdoptSnapshots' RemoveAll of
// the dir never touches it). This is a pure derivation helper: namespace creation
// belongs exclusively to the handle-relative AdoptManifestLease owner.
func adoptManifestLeasePath(manifestName string) (string, error) {
	if err := CheckManifestName(manifestName); err != nil {
		return "", fmt.Errorf("adopt lease: invalid manifest name %q: %w", manifestName, err)
	}
	// SAME reserved-suffix guard as adoptSnapshotDir (arch C1 — the two path owners
	// MUST fail identically, or a ".lease"-suffixed manifest could still resolve a
	// lease path here while its snapshot dir is refused, re-opening the collision).
	// Placed BEFORE the MkdirAll below so a rejected name causes NO side effect.
	if strings.HasSuffix(manifestName, adoptManifestLeaseSuffix) {
		return "", fmt.Errorf("adopt provenance: manifest name %q ends in the reserved %q suffix", manifestName, adoptManifestLeaseSuffix)
	}
	dir, err := DaemonStateDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, adoptProvenanceSnapshotSubdir, manifestName+adoptManifestLeaseSuffix), nil
}

// listAdoptProvenanceSnapshotManifests returns the manifest names that have a
// snapshot DIRECTORY under <state-dir>/adopt-provenance/ (the <manifest>/ dirs,
// NOT the <manifest>.lease sibling files). Used by the GC's snapshot-dir backstop
// (design r2 Signal 3) to find rowless dirs. A missing provenance parent returns an
// empty list, no error (nothing pinned yet).
func listAdoptProvenanceSnapshotManifests() ([]string, error) {
	dir, err := DaemonStateDir()
	if err != nil {
		return nil, err
	}
	provDir := filepath.Join(dir, adoptProvenanceSnapshotSubdir)
	ents, err := os.ReadDir(provDir)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("adopt provenance dir scan %s: %w", provDir, err)
	}
	var out []string
	for _, e := range ents {
		if e.IsDir() {
			out = append(out, e.Name())
		}
	}
	return out, nil
}

// adoptRowVerdict is the classifier result.
type adoptRowVerdict int

const (
	adoptRowCommittedKeep adoptRowVerdict = iota // Install committed (a hub binding is live) — NEVER reap
	adoptRowCrashReap                            // pre-commit crash — safe to reap
	adoptRowRecoveryKeep                         // provider recovery requires explicit de-adopt
)

// adoptCommittedManifestDir resolves the on-disk directory that holds an
// adopt-created manifest — where adopt's ManifestCreate writes it (adopt.go:297 ->
// ManifestCreate -> ManifestCreateIn(defaultManifestDir(), …)). It honors the
// MCPHUB_MANIFEST_DIR_OVERRIDE test seam (manifestDirForTests) so a hermetic
// classifier/GC test can control the manifest-exists KEEP signal; in production the
// override is unset, so this is EXACTLY defaultManifestDir() — the same directory
// BuildAdoptPlan (adopt.go:163) and ManifestCreate (manifest.go:418) use.
func adoptCommittedManifestDir() string {
	if dir := manifestDirForTests(); dir != "" {
		return dir
	}
	return defaultManifestDir()
}

// adoptManifestExistsFn is the SINGLE owner of the "does this adopt-created manifest
// still exist on disk?" committed-KEEP signal, consumed by BOTH
// classifyDeadAdoptingRow (Signal 2b) and the GC's mutation-point guard so the two
// can never diverge (single owner, arch C1). It is a package var only so a test can
// force the fail-closed stat-error branch (=> KEEP). Production always stats
// adoptCommittedManifestDir().
var adoptManifestExistsFn = func(manifestName string) (bool, error) {
	return manifestExistsIn(adoptCommittedManifestDir(), manifestName)
}

// classifyDeadAdoptingRow is the SINGLE committed-vs-crash classifier for an
// `adopting` row whose owner is PROVABLY DEAD (precondition: the caller holds — or
// has just TryLock'd — the row's manifest lease, design r2 Signal 1). BOTH the
// capture-UPSERT reap and the cross-manifest GC route through it, so they can never
// diverge (design r2 claim 22).
//
// It classifies from the row's IMMUTABLE captured fields ONLY (codex bot PR #528 r3
// findings A+B). The committed signal is "Install wrote a live hub entry": for each
// adopt_client, reconstruct the EXPECTED hub binding from manifest_name + the row's
// CAPTURED port + the adopt-v1 binding constants (daemon "default", url_path
// "/mcp"), and ask the single recognition owner liveEntryMatchesManifestBinding
// (managed_entries.go:355, the demigrate.go:426 pattern) whether the live entry
// matches it. The manifest FILE is not read for its CONTENTS — an operator deleting
// or editing it (port change, binding removal) after a committed adopt must NOT let
// the committed row's provenance be reaped (finding A).
//
// Signal 2b (bug 2026-07-11 P1-2): the live hub entry is NOT the only committed
// signal. adopt's ManifestCreate (adopt.go:297) runs strictly BEFORE Install
// (adopt.go:310), and NO routine drift op deletes the manifest (gate-ON reconcile /
// port-edit+reinstall / uninstall / demigrate all leave it), so a committed adopt
// ALWAYS still has its manifest on disk even after the live hub entry has drifted
// away. The mere EXISTENCE of the manifest is therefore a drift-proof committed-KEEP
// signal (its contents are still not consulted).
//
// Uncertainty is ALWAYS KEEP (never reap on what we cannot disprove, finding B): a
// client that cannot be constructed, or whose GetEntry ERRORS, => KEEP; and a
// manifest that exists OR cannot be stat'd => KEEP (fail-closed — REAP demands
// positive absence, destructive-default polarity). Only when EVERY adopt_client is
// cleanly readable AND NONE holds the expected hub entry AND no manifest exists on
// disk is the row a true pre-install crash orphan => REAP. A provider receipt is
// retained for explicit activation recovery only when the durable install-phase
// marker proves Install never started; started/missing/corrupt phase is uncertainty
// and therefore KEEP.
func classifyDeadAdoptingRow(rec AdoptProvenanceRecord) adoptRowVerdict {
	// Synthetic manifest carrying only the row's IMMUTABLE name + captured port; the
	// recognition SHAPE stays single-owned in liveEntryMatchesManifestBinding — this
	// merely supplies its daemon-port input from the row instead of the mutable file.
	expected := &config.ServerManifest{
		Name:    rec.ManifestName,
		Daemons: []config.DaemonSpec{{Name: adoptDefaultDaemonName, Port: rec.Port}},
	}
	all := clients.AllClients()
	for _, c := range rec.AdoptClients {
		adapter, ok := all[c]
		if !ok {
			return adoptRowCommittedKeep // cannot construct this client => cannot DISPROVE => KEEP
		}
		live, gErr := adapter.GetEntry(rec.SourceEntryName)
		if gErr != nil {
			return adoptRowCommittedKeep // read error => cannot DISPROVE => KEEP (finding B)
		}
		if live == nil {
			continue // cleanly no entry here; check the other adopt_clients
		}
		binding := config.ClientBinding{Client: c, Daemon: adoptDefaultDaemonName, URLPath: adoptDefaultURLPath, ToolTimeoutSec: adoptClientToolTimeout(rec, c)}
		if matched, _ := liveEntryMatchesManifestBinding(live, rec.SourceEntryName, binding, expected); matched {
			return adoptRowCommittedKeep // Install committed a live hub binding
		}
	}
	// Signal 2b — the drift-proof committed signal (see the doc comment): a manifest
	// that still exists (or cannot be stat'd, fail-closed) is a committed adopt whose
	// live hub entry merely drifted away, NEVER a pre-install crash. Inert in the
	// capture-UPSERT lane, which classifies only with the manifest ABSENT
	// (BuildAdoptPlan refuses a pre-existing manifest, capture runs before
	// ManifestCreate), so it cannot spuriously refuse an operator re-adopt.
	if exists, err := adoptManifestExistsFn(rec.ManifestName); err != nil || exists {
		return adoptRowCommittedKeep
	}
	if rec.ProviderSource != nil && (rec.ProviderSource.DisablePhase == "disable_planned" || rec.ProviderSource.DisablePhase == "disable_applied") {
		if providerInstallPhaseIsNotStarted(rec.ManifestName) {
			return adoptRowRecoveryKeep
		}
		return adoptRowCommittedKeep
	}
	return adoptRowCrashReap // no live binding AND no manifest on disk => pre-install crash orphan
}

func adoptClientToolTimeout(rec AdoptProvenanceRecord, client string) int {
	for _, candidate := range rec.Clients {
		if candidate.Client == client {
			return candidate.ToolTimeoutSec
		}
	}
	return 0
}

// ---------------------------------------------------------------------------
// Adopt-side lifecycle — capture / promote / abort (THIS item; full bodies).
// ---------------------------------------------------------------------------

// captureAdoptProvenance writes the durable pre-adopt provenance for plan and
// returns the persisted `adopting` record. It is an UPSERT keyed by
// manifest_name (design "Orphan lifecycle + upsert"): a prior row + its snapshot
// dir for the same manifest are reaped FIRST, so at most one row per manifest
// ever exists and a pre-crash orphan is cleaned on the operator's natural retry.
//
// Fail-closed (design F4 / claims 1,2,14): a client GetEntry/config-read/parse
// error (other than a genuinely-missing config, fs.ErrNotExist) is a CAPTURE
// FAILURE — capture returns an error with ZERO durable side effects for the
// manifest (no row, no snapshot), NEVER a guessed `absent`. The caller
// (ExecuteAdoptWithOpts, Phase C) returns before persistAdoptRoutedSecrets, so a
// currently-successful adopt is not regressed.
//
// Both manifest hashes are populated AT CAPTURE from plan.ManifestYAML (design
// F1) — the verbatim bytes ManifestCreateIn later writes — so a committed-but-
// `adopting` row (Install succeeded, flip crashed) is never empty-hashed.
//
// Locking + ordering (design r2 Signal 3, ROW-FIRST): capture writes a MINIMAL
// `adopting` ANCHOR row (manifest + BOTH hashes + empty clients) under the store
// lock BEFORE any secret-bearing snapshot; then pins the snapshots OUTSIDE the
// store lock (each takes its own per-file flock); then finalizes the row with the
// client provenance under the store lock again. A crash at any point leaves a row
// a reaper can find (row->maybe-missing-snapshots is reclaimable), NEVER a snapshot
// dir with no row. Lock order: <manifest>.lease (held by the caller) ->
// adopted-entries.lock (per transaction) -> <snapshot>.lock (per file).
//
// PRECONDITION: the caller (ExecuteAdoptWithOpts) holds the per-manifest lease, so
// a prior `adopting` row for this manifest has a PROVABLY-DEAD owner and is
// classified (not blindly reaped) by the SINGLE classifyDeadAdoptingRow.
func (a *API) captureAdoptProvenance(plan *AdoptPlan) (*AdoptProvenanceRecord, error) {
	if plan == nil {
		return nil, fmt.Errorf("adopt provenance capture: nil plan")
	}

	hash := ManifestHashContent([]byte(plan.ManifestYAML))
	now := time.Now().UTC()

	// c1 — prior-row handling + write the MINIMAL `adopting` ANCHOR row.
	var (
		reapedPrior  bool
		reapedAgeSec float64
	)
	c1Err := withAdoptedEntriesLock(func() error {
		store, err := readAdoptedEntries()
		if err != nil {
			return fmt.Errorf("adopt provenance capture: read store: %w", err)
		}
		// A prior row for this manifest is either a COMMITTED adopt (never destroy —
		// FAIL CLOSED) or a dead-owner `adopting` orphan (the lease is held, so the
		// owner is provably dead — classify via the SINGLE classifier, do not blindly
		// reap: a committed-but-unflipped `adopting` row is COMMITTED_KEEP). A row the
		// classifier calls CRASH_REAP then passes the SAME positive-crash-evidence gate
		// the GC reap uses (adoptRowProvablyUnmutatedFn — ONE predicate across both reap
		// lanes, bug 2026-07-11 P1-2 case-5): a committed adopt whose manifest was deleted
		// and whose hub bindings drifted looks like a pre-install crash to the classifier,
		// so only a row whose write-target entry shapes prove Install committed nowhere is
		// reaped. Any unprovable client is REFUSED, never overwritten (which would strand
		// the de-adopt snapshots as rowless residue).
		for _, r := range store.Records {
			if r.ManifestName != plan.ManifestName {
				continue
			}
			if r.OperationState != AdoptOperationStateAdopting {
				return fmt.Errorf("adopt provenance capture: manifest %q already has committed adopt provenance (state %q); refusing to overwrite it", plan.ManifestName, r.OperationState)
			}
			switch classifyDeadAdoptingRow(r) {
			case adoptRowRecoveryKeep:
				return fmt.Errorf("E_PROVIDER_RECOVERY_REQUIRED: adopt provenance capture: manifest %q has an unresolved provider recovery receipt; use de-adopt --yes to resume recovery explicitly", plan.ManifestName)
			case adoptRowCommittedKeep:
				return fmt.Errorf("adopt provenance capture: manifest %q already has a committed (install-live) adopt still in `adopting` state; refusing to overwrite it", plan.ManifestName)
			}
			if !adoptRowProvablyUnmutatedFn(r) {
				return fmt.Errorf("adopt provenance capture: manifest %[1]q has a prior adopt whose client entry shapes do not prove a pre-Install crash "+
					"(its manifest was deleted and its hub bindings drifted, so it looks like a crash orphan — but a write-target hub relay or any unreadable/unverifiable entry means Install may have COMMITTED it); "+
					"refusing to overwrite it, which would destroy the pre-adopt snapshots. "+
					"WARNING: if ANY client entry for this server was rewritten to a hub URL, the prior adopt COMMITTED — do NOT delete adopt-provenance/%[1]s (it is the only copy of the original entries a future de-adopt (or manual) restore would need). "+
					"Only after confirming the prior adopt never completed Install (no client was hub-rewritten) is it safe to remove its adopted-entries.json row + adopt-provenance/%[1]s dir and re-adopt: %[2]w", plan.ManifestName, errAdoptPriorConfigMutated)
			}
			reapedPrior = true
			reapedAgeSec = time.Since(r.UpdatedAt).Seconds()
		}
		// Drop the prior CRASH_REAP row (if any) + its stale snapshot dir, then write
		// the minimal anchor.
		var kept []AdoptProvenanceRecord
		for _, r := range store.Records {
			if r.ManifestName == plan.ManifestName {
				continue
			}
			kept = append(kept, r)
		}
		if reapedPrior {
			if err := removeAdoptSnapshots(plan.ManifestName); err != nil {
				return fmt.Errorf("adopt provenance capture: reap stale snapshot dir: %w", err)
			}
		}
		kept = append(kept, AdoptProvenanceRecord{
			ManifestName:                    plan.ManifestName,
			SourceClient:                    plan.SourceClient,
			SourceEntryName:                 plan.EntryName,
			Port:                            plan.Port,
			MCPProtocolCompatibilityProfile: plan.MCPProtocolCompatibilityProfile,
			AdoptClients:                    append([]string(nil), plan.AdoptClients...),
			AdoptManifestHash:               hash,
			ExpectedManifestHash:            hash,
			RoutedSecretKeys:                append([]string(nil), plan.SecretRoutedKeys...),
			OperationState:                  AdoptOperationStateAdopting,
			CreatedAt:                       now,
			UpdatedAt:                       now,
			Clients:                         nil, // ANCHOR: no snapshots pinned yet (row-first)
			ProviderSource:                  plan.providerSource,
		})
		store.Records = kept
		return writeAdoptedEntries(store)
	})
	if c1Err != nil {
		return nil, c1Err
	}
	if reapedPrior {
		emitAdoptProvenanceOrphanReaped(plan.ManifestName, reapedAgeSec, adoptOrphanReapTriggerUpsert)
	}

	// c2 — pin snapshots for present clients (OUTSIDE the store lock). On failure,
	// abort (remove snapshots + drop the anchor row) and SURFACE the cleanup error
	// rather than swallow it (design r2 finding 4); the anchor row keeps the failure
	// GC-reclaimable regardless.
	clientsProv, capErr := captureAdoptClientsProvenance(plan)
	if capErr != nil {
		if abortErr := abortAdoptProvenance(&AdoptProvenanceRecord{ManifestName: plan.ManifestName}); abortErr != nil {
			return nil, fmt.Errorf("%w; additionally the pre-adopt provenance cleanup failed (the `adopting` row remains, so a later GC still reclaims it): %v", capErr, abortErr)
		}
		return nil, capErr
	}

	// c3 — finalize the anchor row with the client provenance.
	var rec *AdoptProvenanceRecord
	c3Err := withAdoptedEntriesLock(func() error {
		store, err := readAdoptedEntries()
		if err != nil {
			return fmt.Errorf("adopt provenance capture: read store (finalize): %w", err)
		}
		for i := range store.Records {
			if store.Records[i].ManifestName == plan.ManifestName && store.Records[i].OperationState == AdoptOperationStateAdopting {
				store.Records[i].Clients = clientsProv
				store.Records[i].UpdatedAt = time.Now().UTC()
				if wErr := writeAdoptedEntries(store); wErr != nil {
					return fmt.Errorf("adopt provenance capture: finalize write store: %w", wErr)
				}
				cp := store.Records[i]
				rec = &cp
				return nil
			}
		}
		return fmt.Errorf("adopt provenance capture: anchor row for manifest %q vanished before finalize", plan.ManifestName)
	})
	if c3Err != nil {
		if abortErr := abortAdoptProvenance(&AdoptProvenanceRecord{ManifestName: plan.ManifestName}); abortErr != nil {
			return nil, fmt.Errorf("%w; additionally the pre-adopt provenance cleanup failed (the `adopting` row remains, so a later GC still reclaims it): %v", c3Err, abortErr)
		}
		return nil, c3Err
	}

	emitAdoptProvenanceCaptured(rec)
	return rec, nil
}

// adoptCaptureBeforeSnapshotReadHook is a test-only seam fired between the present
// client's GetEntry and the snapshot os.ReadFile, so a test can simulate a
// concurrent config edit and exercise the finding-C TOCTOU guard. nil in production.
var adoptCaptureBeforeSnapshotReadHook func(client string)

// captureAdoptClientsProvenance classifies each selected client's pre-adopt
// state and pins a hardened whole-config snapshot for every `present` client.
//
// Classification (design "Fail-closed classification", clients.go:208-209):
//   - GetEntry returns a non-nil entry           -> present (pin snapshot)
//   - GetEntry returns (nil, nil)                 -> absent (clean parse, no entry)
//   - GetEntry returns fs.ErrNotExist             -> absent (config file genuinely
//     missing; a fanout target with no pre-adopt entry to preserve — mirrors
//     adoptExtractionErrorClass, which treats fs.ErrNotExist as a normal
//     not-a-candidate, NOT corruption)
//   - GetEntry returns any other error            -> CAPTURE FAILURE (arch F4;
//     never guess `absent` on a corrupted/unreadable config)
func captureAdoptClientsProvenance(plan *AdoptPlan) ([]AdoptClientProvenance, error) {
	all := clients.AllClients()
	presentAtBuild := make(map[string]bool, len(plan.presentAtBuild))
	for _, c := range plan.presentAtBuild {
		presentAtBuild[c] = true
	}
	out := make([]AdoptClientProvenance, 0, len(plan.AdoptClients))
	for _, name := range plan.AdoptClients {
		targetEntryName := plan.targetEntryName(name)
		adapter, ok := all[name]
		if !ok {
			return nil, fmt.Errorf("adopt provenance capture: client %q not constructible on this host", name)
		}
		entry, err := adapter.GetEntry(plan.EntryName)
		switch {
		case err != nil && errors.Is(err, fs.ErrNotExist):
			if presentAtBuild[name] {
				return nil, fmt.Errorf("adopt provenance capture: client %q had the %q entry at plan time but its config is missing at capture; refusing to record it absent (fail-closed — a guessed absent would let de-adopt delete the adopted entry)", name, plan.EntryName)
			}
			out = append(out, adoptClientProvenanceAbsent(name, targetEntryName, plan.toolTimeoutForClient(name)))
		case err != nil:
			return nil, fmt.Errorf("adopt provenance capture: read client %q config: %w", name, err)
		case entry != nil:
			if entry.SourceBelowWriteTarget {
				out = append(out, AdoptClientProvenance{
					Client:          name,
					TargetEntryName: targetEntryName,
					OriginalState:   AdoptOriginalStatePresentMergedLower,
					RestoreMode:     AdoptRestoreModeFunctionalEquivalent,
				})
				continue
			}
			cfgPath := adapter.ConfigPath()
			if adoptCaptureBeforeSnapshotReadHook != nil {
				adoptCaptureBeforeSnapshotReadHook(name)
			}
			configBytes, rErr := os.ReadFile(cfgPath)
			if rErr != nil {
				return nil, fmt.Errorf("adopt provenance capture: client %q config for entry %q disappeared during capture (%v); refusing to record present with no durable snapshot bytes (fail-closed)", name, plan.EntryName, rErr)
			}
			checker, ok := adapter.(clients.EntryBytesChecker)
			if !ok {
				return nil, fmt.Errorf("adopt provenance capture: client %q does not support snapshot-byte validation; refusing to pin an unvalidated snapshot (fail-closed)", name)
			}
			present, pErr := checker.EntryPresentInBytes(configBytes, plan.EntryName)
			if pErr != nil || !present {
				return nil, fmt.Errorf("adopt provenance capture: client %q snapshot bytes do not contain entry %q (present=%t err=%v); config changed during the snapshot read — refusing to pin a snapshot inconsistent with the recorded present state (fail-closed)", name, plan.EntryName, present, pErr)
			}
			ref, sha, wErr := writeAdoptClientSnapshot(plan.ManifestName, name, configBytes)
			if wErr != nil {
				return nil, wErr
			}
			out = append(out, AdoptClientProvenance{
				Client:          name,
				ToolTimeoutSec:  plan.toolTimeoutForClient(name),
				TargetEntryName: targetEntryName,
				OriginalState:   AdoptOriginalStatePresent,
				RestoreMode:     AdoptRestoreModeFunctionalEquivalent,
				SnapshotRef:     ref,
				SnapshotSHA256:  sha,
			})
		default:
			if presentAtBuild[name] {
				return nil, fmt.Errorf("adopt provenance capture: client %q had the %q entry at plan time but it is gone at capture; refusing to record it absent (fail-closed — a guessed absent would let de-adopt delete the adopted entry)", name, plan.EntryName)
			}
			out = append(out, adoptClientProvenanceAbsent(name, targetEntryName, plan.toolTimeoutForClient(name)))
		}
	}
	return out, nil
}

func adoptClientProvenanceAbsent(name, targetEntryName string, toolTimeoutSec int) AdoptClientProvenance {
	return AdoptClientProvenance{
		Client:          name,
		ToolTimeoutSec:  toolTimeoutSec,
		TargetEntryName: targetEntryName,
		OriginalState:   AdoptOriginalStateAbsent,
		RestoreMode:     AdoptRestoreModeNA,
	}
}

func adoptClientTargetEntryName(rec AdoptProvenanceRecord, client AdoptClientProvenance) string {
	if client.TargetEntryName != "" {
		return client.TargetEntryName
	}
	return rec.SourceEntryName
}

func promoteAdoptProvenanceToAdopted(manifestName string) error {
	var (
		flipped      bool
		manifestHash string
	)
	err := withAdoptedEntriesLock(func() error {
		store, err := readAdoptedEntries()
		if err != nil {
			return fmt.Errorf("adopt provenance promote: read store: %w", err)
		}
		for i := range store.Records {
			if store.Records[i].ManifestName != manifestName {
				continue
			}
			manifestHash = store.Records[i].AdoptManifestHash
			if store.Records[i].OperationState == AdoptOperationStateAdopted {
				return nil
			}
			store.Records[i].OperationState = AdoptOperationStateAdopted
			store.Records[i].UpdatedAt = time.Now().UTC()
			flipped = true
			return writeAdoptedEntries(store)
		}
		return fmt.Errorf("adopt provenance promote: no row for manifest %q", manifestName)
	})
	if err != nil {
		return err
	}
	if flipped {
		emitAdoptProvenanceCommitted(manifestName, manifestHash)
	}
	return nil
}

var writeAdoptedEntriesFn = writeAdoptedEntries

func abortAdoptProvenance(rec *AdoptProvenanceRecord) error {
	if rec == nil || rec.ManifestName == "" {
		return nil
	}
	manifestName := rec.ManifestName
	err := withAdoptedEntriesLock(func() error {
		store, err := readAdoptedEntries()
		if err != nil {
			return fmt.Errorf("adopt provenance abort: read store: %w", err)
		}
		var kept []AdoptProvenanceRecord
		for _, r := range store.Records {
			if r.ManifestName == manifestName {
				continue
			}
			kept = append(kept, r)
		}
		store.Records = kept
		if err := removeAdoptSnapshots(manifestName); err != nil {
			return fmt.Errorf("adopt provenance abort: remove snapshots: %w", err)
		}
		if err := writeAdoptedEntriesFn(store); err != nil {
			return fmt.Errorf("adopt provenance abort: write store: %w", err)
		}
		return nil
	})
	if err != nil {
		return err
	}
	emitAdoptProvenanceAbort(manifestName, "adopt failure cleanup")
	return nil
}

const adoptOrphanGCThreshold = 24 * time.Hour

func reapAdoptProvenanceRow(manifestName string, expectedState AdoptOperationState, expectedUpdatedAt time.Time) error {
	return withAdoptedEntriesLock(func() error {
		store, err := readAdoptedEntries()
		if err != nil {
			return fmt.Errorf("adopt provenance reap: read store: %w", err)
		}
		matched := false
		for _, r := range store.Records {
			if r.ManifestName == manifestName && r.OperationState == expectedState && r.UpdatedAt.Equal(expectedUpdatedAt) {
				matched = true
				break
			}
		}
		if !matched {
			return nil
		}
		if err := removeAdoptSnapshots(manifestName); err != nil {
			return fmt.Errorf("adopt provenance reap: remove snapshots: %w", err)
		}
		var kept []AdoptProvenanceRecord
		for _, r := range store.Records {
			if r.ManifestName == manifestName {
				continue
			}
			kept = append(kept, r)
		}
		store.Records = kept
		if err := writeAdoptedEntries(store); err != nil {
			return fmt.Errorf("adopt provenance reap: write store: %w", err)
		}
		return nil
	})
}

var adoptGCBeforePhase2Hook func()
var adoptGCBeforeReapHook func()
var errAdoptPriorConfigMutated = errors.New("prior adopt entry state does not prove a pre-Install crash; refusing to overwrite committed-looking provenance")
var adoptRowProvablyUnmutatedFn = adoptRowProvablyUnmutated
var reapAdoptProvenanceRowFn = reapAdoptProvenanceRow
var gcRemoveRowlessSnapshotsFn = removeAdoptSnapshots

func adoptRowProvablyUnmutated(rec AdoptProvenanceRecord) bool {
	all := clients.AllClients()
	for _, c := range rec.Clients {
		adapter, ok := all[c.Client]
		if !ok {
			return false
		}
		mutator, ok := clients.AsCASEntryMutator(adapter)
		if !ok {
			return false
		}

		var snapshotSubtree any
		switch c.OriginalState {
		case AdoptOriginalStatePresent:
			state, _, subtree, _ := readDeAdoptSnapshot(&rec, c, mutator)
			if state != deAdoptSnapshotAvailable {
				return false
			}
			snapshotSubtree = subtree
		case AdoptOriginalStateAbsent, AdoptOriginalStatePresentMergedLower:
		default:
			return false
		}

		verdict, err := mutator.ClassifyEntryUnderLock(
			rec.SourceEntryName,
			deAdoptLiveBindingMatcher(&rec, config.ClientBinding{
				Client: c.Client, Daemon: adoptDefaultDaemonName, URLPath: adoptDefaultURLPath,
				ToolTimeoutSec: adoptClientToolTimeout(rec, c.Client),
			}),
			snapshotSubtree,
		)
		if err != nil {
			return false
		}

		switch c.OriginalState {
		case AdoptOriginalStatePresent:
			if verdict != clients.ClassifyRestoreDone {
				return false
			}
		case AdoptOriginalStateAbsent, AdoptOriginalStatePresentMergedLower:
			if verdict != clients.ClassifyRestoreDone && verdict != clients.ClassifyGenuineConflict {
				return false
			}
		}
	}
	return true
}

func gcOrphanedAdoptingProvenance(olderThan time.Duration) (reaped int, err error) {
	type candidate struct {
		rec    AdoptProvenanceRecord
		ageSec float64
	}
	var candidates []candidate
	rowManifests := map[string]bool{}
	cutoff := time.Now().Add(-olderThan)
	if lockErr := withAdoptedEntriesLock(func() error {
		store, rErr := readAdoptedEntries()
		if rErr != nil {
			return fmt.Errorf("adopt provenance gc: read store: %w", rErr)
		}
		for _, r := range store.Records {
			rowManifests[r.ManifestName] = true
			if r.OperationState == AdoptOperationStateAdopting && r.UpdatedAt.Before(cutoff) {
				candidates = append(candidates, candidate{rec: r, ageSec: time.Since(r.UpdatedAt).Seconds()})
			}
		}
		return nil
	}); lockErr != nil {
		return 0, lockErr
	}

	if adoptGCBeforePhase2Hook != nil {
		adoptGCBeforePhase2Hook()
	}

	for _, c := range candidates {
		lk, ok, lErr := tryAcquireAdoptManifestLease(c.rec.ManifestName)
		if lErr != nil {
			emitAdoptProvenanceReapFailed(c.rec.ManifestName, adoptReapFailPhaseLeasePathError, lErr.Error())
			continue
		}
		if !ok {
			continue
		}
		cleanupErr := func() (cleanupErr error) {
			defer func() { cleanupErr = finishAdoptGCLease(c.rec.ManifestName, lk) }()
			var (
				live      AdoptProvenanceRecord
				stillOurs bool
			)
			_ = withAdoptedEntriesLock(func() error {
				store, rErr := readAdoptedEntries()
				if rErr != nil {
					return nil
				}
				for _, r := range store.Records {
					if r.ManifestName != c.rec.ManifestName {
						continue
					}
					if r.OperationState == AdoptOperationStateAdopting && r.UpdatedAt.Equal(c.rec.UpdatedAt) && r.UpdatedAt.Before(cutoff) {
						live = r
						stillOurs = true
					}
					break
				}
				return nil
			})
			if !stillOurs {
				return nil
			}
			if classifyDeadAdoptingRow(live) != adoptRowCrashReap {
				return nil
			}
			if adoptGCBeforeReapHook != nil {
				adoptGCBeforeReapHook()
			}
			if exists, mErr := adoptManifestExistsFn(live.ManifestName); mErr != nil || exists {
				emitAdoptProvenanceReapSkippedManifestPresent(live.ManifestName, c.ageSec)
				return nil
			}
			if !adoptRowProvablyUnmutatedFn(live) {
				return nil
			}
			if rErr := reapAdoptProvenanceRowFn(live.ManifestName, AdoptOperationStateAdopting, live.UpdatedAt); rErr == nil {
				emitAdoptProvenanceOrphanReaped(live.ManifestName, c.ageSec, adoptOrphanReapTriggerGC)
				reaped++
			} else {
				emitAdoptProvenanceReapFailed(live.ManifestName, adoptReapFailPhaseRow, rErr.Error())
			}
			return nil
		}()
		if cleanupErr != nil {
			return reaped, cleanupErr
		}
	}

	dirManifests, dErr := listAdoptProvenanceSnapshotManifests()
	if dErr != nil {
		return reaped, dErr
	}
	for _, m := range dirManifests {
		if rowManifests[m] {
			continue
		}
		lk, ok, lErr := tryAcquireAdoptManifestLease(m)
		if lErr != nil {
			emitAdoptProvenanceReapFailed(m, adoptReapFailPhaseLeasePathError, lErr.Error())
			continue
		}
		if !ok {
			continue
		}
		cleanupErr := func() (cleanupErr error) {
			defer func() { cleanupErr = finishAdoptGCLease(m, lk) }()
			hasRow := true
			_ = withAdoptedEntriesLock(func() error {
				store, rErr := readAdoptedEntries()
				if rErr != nil {
					return nil
				}
				hasRow = false
				for _, r := range store.Records {
					if r.ManifestName == m {
						hasRow = true
						break
					}
				}
				return nil
			})
			if !hasRow {
				if rmErr := gcRemoveRowlessSnapshotsFn(m); rmErr == nil {
					emitAdoptProvenanceOrphanReaped(m, 0, adoptOrphanReapTriggerGC)
					reaped++
				} else {
					emitAdoptProvenanceReapFailed(m, adoptReapFailPhaseRowlessDir, rmErr.Error())
				}
			}
			return nil
		}()
		if cleanupErr != nil {
			return reaped, cleanupErr
		}
	}
	return reaped, nil
}

func finishAdoptGCLease(manifestName string, lease *AdoptManifestLease) error {
	if err := lease.Unlock(); err != nil {
		if !hasLeaseFailureID(err, adoptLeaseFailureCleanup) {
			err = leaseCleanupFailure(err)
		}
		emitAdoptLeaseFailed(manifestName, err)
		return err
	}
	return nil
}

func MarkAdoptProvenanceDeAdopting(manifestName string) error {
	return withAdoptedEntriesLock(func() error {
		store, err := readAdoptedEntries()
		if err != nil {
			return fmt.Errorf("adopt provenance mark de-adopting: read store: %w", err)
		}
		for i := range store.Records {
			rec := &store.Records[i]
			if rec.ManifestName != manifestName {
				continue
			}
			switch rec.OperationState {
			case AdoptOperationStateDeAdopting:
				return nil
			case AdoptOperationStateAdopted:
			case AdoptOperationStateAdopting:
				if verdict := classifyDeadAdoptingRow(*rec); verdict != adoptRowCommittedKeep && verdict != adoptRowRecoveryKeep {
					return fmt.Errorf("adopt provenance mark de-adopting: manifest %q adopting row is not committed; refusing to take it from adopt orphan GC", manifestName)
				}
			case AdoptOperationStateClosed:
				return fmt.Errorf("adopt provenance mark de-adopting: manifest %q provenance is already closed", manifestName)
			default:
				return fmt.Errorf("adopt provenance mark de-adopting: manifest %q has unsupported state %q", manifestName, rec.OperationState)
			}
			rec.OperationState = AdoptOperationStateDeAdopting
			rec.UpdatedAt = time.Now().UTC()
			if err := writeAdoptedEntries(store); err != nil {
				return fmt.Errorf("adopt provenance mark de-adopting: write store: %w", err)
			}
			return nil
		}
		return fmt.Errorf("adopt provenance mark de-adopting: manifest %q has no provenance row", manifestName)
	})
}

func AdvanceProviderDeAdoptPhase(manifestName, expectedPhase, nextPhase string) (*AdoptProvenanceRecord, error) {
	validNext := map[string]string{
		"":                     "managed_stop_settled",
		"managed_stop_settled": "managed_removed",
		"managed_removed":      "restore_applied",
	}
	if validNext[expectedPhase] != nextPhase {
		return nil, fmt.Errorf("provider de-adopt phase transition %q -> %q is invalid", expectedPhase, nextPhase)
	}
	var updated *AdoptProvenanceRecord
	err := withAdoptedEntriesLock(func() error {
		store, err := readAdoptedEntries()
		if err != nil {
			return fmt.Errorf("provider de-adopt phase: read store: %w", err)
		}
		for i := range store.Records {
			rec := &store.Records[i]
			if rec.ManifestName != manifestName {
				continue
			}
			if rec.OperationState != AdoptOperationStateDeAdopting || rec.ProviderSource == nil {
				return fmt.Errorf("provider de-adopt phase: manifest %q is not provider de-adopting", manifestName)
			}
			if rec.ProviderSource.DeAdoptPhase != expectedPhase {
				return fmt.Errorf("provider de-adopt phase: manifest %q changed phase", manifestName)
			}
			rec.ProviderSource.DeAdoptPhase = nextPhase
			rec.UpdatedAt = time.Now().UTC()
			if err := writeAdoptedEntries(store); err != nil {
				return fmt.Errorf("provider de-adopt phase: write store: %w", err)
			}
			copy := *rec
			copy.ProviderSource = cloneProviderSourceProvenance(rec.ProviderSource)
			updated = &copy
			return nil
		}
		return fmt.Errorf("provider de-adopt phase: manifest %q has no provenance row", manifestName)
	})
	return updated, err
}

func CloseAdoptProvenance(manifestName string) error {
	var (
		found     bool
		updatedAt time.Time
	)
	if err := withAdoptedEntriesLock(func() error {
		store, err := readAdoptedEntries()
		if err != nil {
			return fmt.Errorf("adopt provenance close: read store: %w", err)
		}
		for _, rec := range store.Records {
			if rec.ManifestName != manifestName {
				continue
			}
			if rec.OperationState != AdoptOperationStateDeAdopting {
				return fmt.Errorf("adopt provenance close: manifest %q has state %q, want %q", manifestName, rec.OperationState, AdoptOperationStateDeAdopting)
			}
			found = true
			updatedAt = rec.UpdatedAt
			return nil
		}
		return nil
	}); err != nil {
		return err
	}
	if !found {
		return nil
	}
	if err := reapAdoptProvenanceRow(manifestName, AdoptOperationStateDeAdopting, updatedAt); err != nil {
		return fmt.Errorf("adopt provenance close: %w", err)
	}
	return nil
}

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
// and the read accessor ReadAdoptProvenance. The de-adopt-owned mutators
// (MarkAdoptProvenanceDeAdopting / UpdateAdoptExpectedManifestHash /
// CloseAdoptProvenance) and the de_adopting/closed states are DECLARED here (as
// comments + schema enum values) so the shared schema supports them, but MUST
// NOT ship as Go bodies in this item — the de-adopt work-item authors them.
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

	// adoptManifestLeaseSuffix names the per-manifest adopt LEASE file
	// (<state-dir>/adopt-provenance/<manifest>.lease), a SIBLING to the
	// <manifest>/ snapshot dir so removeAdoptSnapshots' RemoveAll of the dir never
	// touches it. The lease is the owner-liveness authority (design r2 Signal 1):
	// held (flock) capture->promote by ExecuteAdoptWithOpts; a reaper TryLocks it
	// before reaping (can't acquire => a live adopt owns it => skip/fail-closed).
	adoptManifestLeaseSuffix = ".lease"
)

// adoptedEntriesSchemaVersion is the on-disk format version. Bumping requires a
// migration step in readAdoptedEntries. Isolated from managed-entries.json's
// schema (separate file, separate version) per the store-shape decision.
const adoptedEntriesSchemaVersion = 1

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
	Client        string             `json:"client"`
	OriginalState AdoptOriginalState `json:"original_state"`
	RestoreMode   AdoptRestoreMode   `json:"restore_mode"`
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
	ManifestName    string   `json:"manifest_name"`
	SourceClient    string   `json:"source_client"`
	SourceEntryName string   `json:"source_entry_name"`
	Port            int      `json:"port"`
	AdoptClients    []string `json:"adopt_clients"`
	// AdoptManifestHash is the immutable sha256 of the adopt-generated manifest
	// bytes (plan.ManifestYAML). ExpectedManifestHash starts equal to it; de-adopt
	// updates ExpectedManifestHash after a subset binding edit. BOTH are populated
	// AT CAPTURE (arch F1) so a committed-but-`adopting` row is never empty-hashed.
	AdoptManifestHash    string                  `json:"adopt_manifest_hash"`
	ExpectedManifestHash string                  `json:"expected_manifest_hash"`
	RoutedSecretKeys     []string                `json:"routed_secret_keys"`
	OperationState       AdoptOperationState     `json:"operation_state"`
	CreatedAt            time.Time               `json:"created_at"`
	UpdatedAt            time.Time               `json:"updated_at"`
	Clients              []AdoptClientProvenance `json:"clients"`
}

// AdoptedEntries is the <state-dir>/adopted-entries.json file root.
type AdoptedEntries struct {
	Version int                     `json:"version"`
	Records []AdoptProvenanceRecord `json:"records"`
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

// readAdoptedEntries returns the parsed store, or an empty
// AdoptedEntries{Version: adoptedEntriesSchemaVersion} when the file does not yet
// exist. A version-0 file (pre-version write) is normalized to the current
// version; any other version is a hard error (fail-closed). Every other
// read/parse error propagates.
func readAdoptedEntries() (*AdoptedEntries, error) {
	raw, err := readHubMcpStateFile(adoptedEntriesFileLeaf)
	if err != nil {
		if isHubMcpStateMissingErr(err) {
			return &AdoptedEntries{Version: adoptedEntriesSchemaVersion}, nil
		}
		return nil, err
	}
	var m AdoptedEntries
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, fmt.Errorf("parse adopted-entries.json: %w", err)
	}
	if m.Version == 0 {
		m.Version = adoptedEntriesSchemaVersion
	}
	if m.Version != adoptedEntriesSchemaVersion {
		return nil, fmt.Errorf("adopted-entries.json: unknown schema version %d (this build expects %d)", m.Version, adoptedEntriesSchemaVersion)
	}
	return &m, nil
}

// writeAdoptedEntries serializes m and writes it via the hardened hub-mcp
// state-file pipeline (handle-relative, DACL-bound temp + atomic rename).
func writeAdoptedEntries(m *AdoptedEntries) error {
	m.Version = adoptedEntriesSchemaVersion
	raw, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal adopted-entries: %w", err)
	}
	return writeHubMcpStateFile(adoptedEntriesFileLeaf, raw)
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

// ---------------------------------------------------------------------------
// Crash-consistency + concurrency primitives (design r2 addendum): the
// per-manifest LEASE (Signal 1), the snapshot-dir lister (Signal 3 backstop),
// and the ONE classifier (Signal 2) used by BOTH capture-reap and the GC.
// ---------------------------------------------------------------------------

// adoptManifestLeasePath returns <state-dir>/adopt-provenance/<manifest>.lease —
// a SIBLING to the <manifest>/ snapshot dir (so removeAdoptSnapshots' RemoveAll of
// the dir never touches it). Ensures the owner-only provenance parent exists so
// flock can create the lease file; the lease holds no secret content (a pure lock).
func adoptManifestLeasePath(manifestName string) (string, error) {
	if err := CheckManifestName(manifestName); err != nil {
		return "", fmt.Errorf("adopt lease: invalid manifest name %q: %w", manifestName, err)
	}
	dir, err := DaemonStateDir()
	if err != nil {
		return "", err
	}
	provDir := filepath.Join(dir, adoptProvenanceSnapshotSubdir)
	if err := os.MkdirAll(provDir, 0o700); err != nil {
		return "", fmt.Errorf("adopt lease: mkdir %s: %w", provDir, err)
	}
	return filepath.Join(provDir, manifestName+adoptManifestLeaseSuffix), nil
}

// tryAcquireAdoptManifestLease non-blockingly TryLocks the per-manifest lease.
// Returns (lk, true, nil) when acquired — caller MUST defer lk.Unlock(); (nil,
// false, nil) when a LIVE same-manifest adopt already holds it; (nil, false, err)
// on a path/lock error. The OS auto-releases a dead holder's flock, so a
// successful acquire proves no live same-manifest adopt exists (design r2 Signal 1).
func tryAcquireAdoptManifestLease(manifestName string) (*flock.Flock, bool, error) {
	leasePath, err := adoptManifestLeasePath(manifestName)
	if err != nil {
		return nil, false, err
	}
	lk := flock.New(leasePath)
	locked, lockErr := lk.TryLock()
	if lockErr != nil {
		return nil, false, fmt.Errorf("adopt lease TryLock %s: %w", leasePath, lockErr)
	}
	if !locked {
		return nil, false, nil
	}
	return lk, true, nil
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
)

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
// matches it. The manifest FILE is NEVER read — an operator deleting or editing it
// (port change, binding removal) after a committed adopt must NOT let the committed
// row's provenance be reaped (finding A).
//
// Uncertainty is ALWAYS KEEP (never reap on what we cannot disprove, finding B): a
// client that cannot be constructed, or whose GetEntry ERRORS, => KEEP. Only when
// EVERY adopt_client is cleanly readable AND NONE holds the expected hub entry is
// the row a true pre-install crash orphan => REAP.
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
		binding := config.ClientBinding{Client: c, Daemon: adoptDefaultDaemonName, URLPath: adoptDefaultURLPath}
		if matched, _ := liveEntryMatchesManifestBinding(live, rec.SourceEntryName, binding, expected); matched {
			return adoptRowCommittedKeep // Install committed a live hub binding
		}
	}
	return adoptRowCrashReap // every adopt_client readable, NONE holds the expected hub entry
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
		// reap: a committed-but-unflipped `adopting` row is COMMITTED_KEEP).
		for _, r := range store.Records {
			if r.ManifestName != plan.ManifestName {
				continue
			}
			if r.OperationState != AdoptOperationStateAdopting {
				return fmt.Errorf("adopt provenance capture: manifest %q already has committed adopt provenance (state %q); refusing to overwrite it", plan.ManifestName, r.OperationState)
			}
			if classifyDeadAdoptingRow(r) == adoptRowCommittedKeep {
				return fmt.Errorf("adopt provenance capture: manifest %q already has a committed (install-live) adopt still in `adopting` state; refusing to overwrite it", plan.ManifestName)
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
			ManifestName:         plan.ManifestName,
			SourceClient:         plan.SourceClient,
			SourceEntryName:      plan.EntryName,
			Port:                 plan.Port,
			AdoptClients:         append([]string(nil), plan.AdoptClients...),
			AdoptManifestHash:    hash,
			ExpectedManifestHash: hash,
			RoutedSecretKeys:     append([]string(nil), plan.SecretRoutedKeys...),
			OperationState:       AdoptOperationStateAdopting,
			CreatedAt:            now,
			UpdatedAt:            now,
			Clients:              nil, // ANCHOR: no snapshots pinned yet (row-first)
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
	// presentAtBuild = clients whose same-name entry was PRESENT and adoptable at
	// BuildAdoptPlan time (always includes the source client). Such a client MUST
	// NOT be recorded `absent` if it reads no-entry at capture — that is a
	// Build->capture change, and since Install still writes the hub relay to it,
	// a guessed `absent` would let de-adopt delete the adopted entry with no
	// snapshot (security F4 — a vanished entry does not "parse cleanly and lack
	// the entry"; it is a fail-closed capture failure).
	presentAtBuild := make(map[string]bool, len(plan.presentAtBuild))
	for _, c := range plan.presentAtBuild {
		presentAtBuild[c] = true
	}
	out := make([]AdoptClientProvenance, 0, len(plan.AdoptClients))
	for _, name := range plan.AdoptClients {
		adapter, ok := all[name]
		if !ok {
			// A selected client whose adapter cannot be constructed on this host
			// is a capture failure — we cannot prove its pre-adopt state, so we
			// must not guess (fail closed, symmetric with F4).
			return nil, fmt.Errorf("adopt provenance capture: client %q not constructible on this host", name)
		}
		entry, err := adapter.GetEntry(plan.EntryName)
		switch {
		case err != nil && errors.Is(err, fs.ErrNotExist):
			// A genuinely-missing config file. Fail closed if the client was present
			// at Build (its config vanished in the Build->capture window); otherwise
			// it is a legitimate configless fanout target with no entry to preserve.
			if presentAtBuild[name] {
				return nil, fmt.Errorf("adopt provenance capture: client %q had the %q entry at plan time but its config is missing at capture; refusing to record it absent (fail-closed — a guessed absent would let de-adopt delete the adopted entry)", name, plan.EntryName)
			}
			out = append(out, adoptClientProvenanceAbsent(name))
		case err != nil:
			return nil, fmt.Errorf("adopt provenance capture: read client %q config: %w", name, err)
		case entry != nil:
			cfgPath := adapter.ConfigPath()
			if adoptCaptureBeforeSnapshotReadHook != nil {
				adoptCaptureBeforeSnapshotReadHook(name) // test-only: simulate a concurrent config edit
			}
			configBytes, rErr := os.ReadFile(cfgPath)
			if rErr != nil {
				if errors.Is(rErr, fs.ErrNotExist) {
					// MiMoCode merged-layer (design r2 finding 5): the entry resolves
					// from a LOWER read layer but the hub WRITE TARGET (ConfigPath) does
					// not exist yet. GetEntry non-nil PROVES the entry is resolvable, so
					// this is NOT a vanished entry (the P2 data-loss case is GetEntry
					// nil, handled by the present-at-Build branches above). Record
					// present-merged-lower with NO snapshot; de-adopt restores by
					// removing the hub entry from the write target, which re-exposes the
					// untouched lower-layer original. (No write-target bytes to
					// revalidate, so the finding-C guard below does not apply here.)
					out = append(out, AdoptClientProvenance{
						Client:        name,
						OriginalState: AdoptOriginalStatePresentMergedLower,
						RestoreMode:   AdoptRestoreModeFunctionalEquivalent,
					})
					continue
				}
				return nil, fmt.Errorf("adopt provenance capture: read client %q config file: %w", name, rErr)
			}
			// Capture TOCTOU guard (codex bot PR #528 r3 finding C): the entry was
			// present at the GetEntry above, but the config could have been edited
			// (entry deleted/renamed) BEFORE this ReadFile, so configBytes may LACK the
			// entry while we record `original_state: present` — a later de-adopt would
			// then restore that DELETION instead of the pre-adopt entry. Re-verify the
			// entry is STILL present in the live config after the snapshot read; if it
			// is gone, the config changed during capture => FAIL CLOSED (do not pin a
			// snapshot whose bytes are inconsistent with the recorded present state).
			if recheck, reErr := adapter.GetEntry(plan.EntryName); reErr != nil || recheck == nil {
				return nil, fmt.Errorf("adopt provenance capture: client %q config for entry %q changed during the snapshot read (entry no longer present); refusing to pin a snapshot inconsistent with the recorded present state", name, plan.EntryName)
			}
			ref, sha, wErr := writeAdoptClientSnapshot(plan.ManifestName, name, configBytes)
			if wErr != nil {
				return nil, wErr
			}
			out = append(out, AdoptClientProvenance{
				Client:         name,
				OriginalState:  AdoptOriginalStatePresent,
				RestoreMode:    AdoptRestoreModeFunctionalEquivalent,
				SnapshotRef:    ref,
				SnapshotSHA256: sha,
			})
		default:
			// entry == nil, err == nil: the config parsed cleanly but the same-name
			// entry is gone. Fail closed if the client was present at Build (the
			// entry was deleted/renamed/edited away in the Build->capture window —
			// the TOCTOU that Install would still write the hub relay over, so a
			// guessed `absent` is silent data loss on de-adopt, security F4).
			// Otherwise it is a legitimate entryless-fanout target.
			if presentAtBuild[name] {
				return nil, fmt.Errorf("adopt provenance capture: client %q had the %q entry at plan time but it is gone at capture; refusing to record it absent (fail-closed — a guessed absent would let de-adopt delete the adopted entry)", name, plan.EntryName)
			}
			out = append(out, adoptClientProvenanceAbsent(name))
		}
	}
	return out, nil
}

func adoptClientProvenanceAbsent(name string) AdoptClientProvenance {
	return AdoptClientProvenance{
		Client:        name,
		OriginalState: AdoptOriginalStateAbsent,
		RestoreMode:   AdoptRestoreModeNA,
	}
}

// promoteAdoptProvenanceToAdopted flips the manifest's row adopting -> adopted.
// It writes NO hashes (both are already on the row from capture, F1) and is
// idempotent (already-adopted -> no-op success). A missing row is an error (the
// row must exist — capture wrote it before Install). NON-FATAL by caller
// contract: a flip-write failure downgrades to a recoverable `adopting` state,
// never rolls back a committed adopt (design claim 10).
//
// NOTE (Phase B): UNWIRED — called by unit tests only until Phase C.
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
				return nil // idempotent no-op
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

// writeAdoptedEntriesFn is the abort-path store-write step, injected as a package
// var ONLY so a test can prove abort's crash-safe ordering (snapshots removed
// BEFORE the row write — codex bot PR #528 finding 3) by forcing the write to
// fail after the snapshot removal. Production always uses writeAdoptedEntries.
var writeAdoptedEntriesFn = writeAdoptedEntries

// abortAdoptProvenance deletes the manifest's row and RemoveAll's its snapshot
// dir during adopt failure cleanup. Idempotent + best-effort: a second call (or
// a call for a manifest with no row) is a no-op success; an abort error is
// RETURNED to the caller (which appends it to the operator message) and never
// masks the caller's original adopt error.
//
// Crash-safe ordering (codex bot PR #528 finding 3): the secret-bearing snapshot
// dir is removed FIRST, then the row is dropped. A crash between leaves a
// row->missing-snapshot (harmless; GC/UPSERT reclaims the row), never a
// snapshot->no-row (an unreclaimable secret leak the row-scanning GC could never
// reach) — the same ordering gcOrphanedAdoptingProvenance uses.
//
// NOTE (Phase B): UNWIRED — called by unit tests only until Phase C.
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

// adoptOrphanGCThreshold is the age past which an `adopting` provenance row is
// treated as a hard-crash orphan. A live in-flight adopt has a fresh updated_at
// and holds the provenance lock across each mutation, so only a process that died
// between capture and promote/abort leaves an aged `adopting` row. Default per
// design.md "Orphan lifecycle + upsert" (24h) — conservative on purpose so a
// genuinely-slow in-flight adopt is never reaped out from under itself.
const adoptOrphanGCThreshold = 24 * time.Hour

// reapAdoptProvenanceRow removes a manifest's snapshot dir (FIRST) then drops its
// row (crash-safe ordering, codex bot PR #528 finding 3). Caller MUST hold the
// manifest lease so the reap cannot race a concurrent adopt. No event emit — the
// GC caller emits orphan-reaped.
func reapAdoptProvenanceRow(manifestName string) error {
	return withAdoptedEntriesLock(func() error {
		store, err := readAdoptedEntries()
		if err != nil {
			return fmt.Errorf("adopt provenance reap: read store: %w", err)
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

// gcOrphanedAdoptingProvenance reaps stale CROSS-manifest orphans using the three
// design-r2 signals — the per-manifest LEASE (Signal 1), the hub-binding-live
// classifier (Signal 2), and the snapshot-dir backstop (Signal 3):
//
//   Phase 1 (store lock): snapshot the aged `adopting` candidates + the set of
//     manifests that have ANY store row; release the store lock.
//   Phase 2 (per candidate, OUTSIDE the store lock): TryLock its lease — a LIVE
//     adopt holds it => skip (claim 16). With the lease held the owner is provably
//     dead; classifyDeadAdoptingRow decides: COMMITTED_KEEP (a hub binding is live —
//     a promote-flip-failure recoverable row, finding 2 KEEP) is preserved;
//     CRASH_REAP (manifest absent, OR present with no live binding — finding 2 REAP)
//     removes snapshots-then-row under the held lease.
//   Phase 3 (backstop): reap ROWLESS snapshot dirs (a <manifest>/ dir with no store
//     row — findings 3/4 residue or any future ordering bug), gated on the lease +
//     no-store-row, NOT age-gated (a rowless dir has no updated_at).
//
// Lock order (acyclic): <manifest>.lease (TryLock, non-blocking) -> adopted-entries
// .lock. The store lock is NEVER held while acquiring a lease. Returns the count
// reaped; a GC error must not block a fresh adopt (callers run it best-effort).
func gcOrphanedAdoptingProvenance(olderThan time.Duration) (reaped int, err error) {
	type candidate struct {
		rec    AdoptProvenanceRecord
		ageSec float64
	}
	// Phase 1 — snapshot aged `adopting` candidates + the row-manifest set.
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

	// Phase 2 — reap true cross-manifest row-bearing orphans under each own lease.
	for _, c := range candidates {
		lk, ok, lErr := tryAcquireAdoptManifestLease(c.rec.ManifestName)
		if lErr != nil || !ok {
			continue // lease unavailable (live adopt) or path error => skip (fail-safe)
		}
		if classifyDeadAdoptingRow(c.rec) == adoptRowCrashReap {
			if rErr := reapAdoptProvenanceRow(c.rec.ManifestName); rErr == nil {
				emitAdoptProvenanceOrphanReaped(c.rec.ManifestName, c.ageSec, adoptOrphanReapTriggerGC)
				reaped++
			}
		}
		_ = lk.Unlock()
	}

	// Phase 3 — snapshot-dir backstop: reap ROWLESS <manifest>/ dirs under lease.
	dirManifests, dErr := listAdoptProvenanceSnapshotManifests()
	if dErr != nil {
		return reaped, dErr
	}
	for _, m := range dirManifests {
		if rowManifests[m] {
			continue // has (or had) a store row — handled by Phase 2 or kept intentionally
		}
		lk, ok, lErr := tryAcquireAdoptManifestLease(m)
		if lErr != nil || !ok {
			continue // live adopt (may be mid-anchor) or path error => skip
		}
		// Confirm still rowless UNDER the lease before removing (authoritative).
		hasRow := true
		_ = withAdoptedEntriesLock(func() error {
			store, rErr := readAdoptedEntries()
			if rErr != nil {
				return nil // fail-safe: leave hasRow=true, do not reap on read error
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
			if rmErr := removeAdoptSnapshots(m); rmErr == nil {
				emitAdoptProvenanceOrphanReaped(m, 0, adoptOrphanReapTriggerGC)
				reaped++
			}
		}
		_ = lk.Unlock()
	}
	return reaped, nil
}

// ---------------------------------------------------------------------------
// De-adopt-owned MUTATORS — DECLARED (comments) for schema/contract shape ONLY.
//
// THIS work-item MUST NOT land Go bodies (empty or otherwise) for these three
// (anti-layering, arch F7 / plan AC A8). The de-adopt work-item
// (2026-07-09-deadopt-hub-to-native) authors them against this schema; they
// drive the adopted -> de_adopting -> closed transitions this item does NOT own.
// Listed here so the schema (the de_adopting/closed AdoptOperationState values)
// supports them without a second store owner.
//
//	func MarkAdoptProvenanceDeAdopting(manifestName string) error         // adopted -> de_adopting
//	func UpdateAdoptExpectedManifestHash(manifestName, newHash string) error // subset binding edit
//	func CloseAdoptProvenance(manifestName string) error                  // de_adopting -> closed + delete snapshots
// ---------------------------------------------------------------------------

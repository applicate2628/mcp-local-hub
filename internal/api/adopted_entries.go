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
	Client string `json:"client"`
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
	RoutedSecretKeys     []string                "json:\"routed_secr\u0065t_keys\""
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
// disk is the row a true pre-install crash orphan => REAP.
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
	// Signal 2b — the drift-proof committed signal (see the doc comment): a manifest
	// that still exists (or cannot be stat'd, fail-closed) is a committed adopt whose
	// live hub entry merely drifted away, NEVER a pre-install crash. Inert in the
	// capture-UPSERT lane, which classifies only with the manifest ABSENT
	// (BuildAdoptPlan refuses a pre-existing manifest, capture runs before
	// ManifestCreate), so it cannot spuriously refuse an operator re-adopt.
	if exists, err := adoptManifestExistsFn(rec.ManifestName); err != nil || exists {
		return adoptRowCommittedKeep
	}
	return adoptRowCrashReap // no live binding AND no manifest on disk => pre-install crash orphan
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
			if classifyDeadAdoptingRow(r) == adoptRowCommittedKeep {
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
		targetEntryName := plan.targetEntryName(name)
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
			out = append(out, adoptClientProvenanceAbsent(name, targetEntryName))
		case err != nil:
			return nil, fmt.Errorf("adopt provenance capture: read client %q config: %w", name, err)
		case entry != nil:
			// Finding 1 (codex bot PR #528 r4): present-merged-lower is keyed on the
			// ADAPTER's authoritative SourceBelowWriteTarget signal, NOT on a missing
			// ConfigPath. SourceBelowWriteTarget==true means the entry resolves from a
			// LOWER read/import layer the hub never writes; the write target may EXIST
			// (holding other entries) yet not contain this entry. Record
			// present-merged-lower with NO snapshot — de-adopt restores by removing the
			// hub entry from the write target, which re-exposes the untouched lower-layer
			// original — and do NOT read or snapshot the write-target bytes.
			// (present-merged-lower <=> SourceBelowWriteTarget, exactly.)
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
				adoptCaptureBeforeSnapshotReadHook(name) // test-only: simulate a concurrent config edit
			}
			configBytes, rErr := os.ReadFile(cfgPath)
			if rErr != nil {
				// Finding 2 (r4): a SourceBelowWriteTarget==false entry lives IN the
				// write target (ConfigPath). If that file is now gone, the config
				// vanished in the GetEntry->ReadFile window — there are no durable bytes
				// to preserve. FAIL CLOSED. This is deliberately NOT present-merged-lower:
				// that state is keyed EXCLUSIVELY on SourceBelowWriteTarget above, so a
				// ConfigPath ENOENT here (fs.ErrNotExist included) is a capture failure,
				// never a merged-lower guess.
				return nil, fmt.Errorf("adopt provenance capture: client %q config for entry %q disappeared during capture (%v); refusing to record present with no durable snapshot bytes (fail-closed)", name, plan.EntryName, rErr)
			}
			// Finding 3 (r4): validate the EXACT snapshotted bytes physically contain the
			// entry, parsed via the adapter's own reader (NO second disk read). The prior
			// GetEntry re-verify left a double-TOCTOU open: the entry could be deleted
			// before ReadFile (so configBytes LACKS it) then re-created before the
			// re-verify (so GetEntry sees it again) — pinning a snapshot whose bytes a
			// later de-adopt would restore as a DELETION. Checking the captured bytes
			// themselves closes it.
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
				TargetEntryName: targetEntryName,
				OriginalState:   AdoptOriginalStatePresent,
				RestoreMode:     AdoptRestoreModeFunctionalEquivalent,
				SnapshotRef:     ref,
				SnapshotSHA256:  sha,
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
			out = append(out, adoptClientProvenanceAbsent(name, targetEntryName))
		}
	}
	return out, nil
}

func adoptClientProvenanceAbsent(name, targetEntryName string) AdoptClientProvenance {
	return AdoptClientProvenance{
		Client:          name,
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

// promoteAdoptProvenanceToAdopted flips the manifest's row adopting -> adopted.
// It writes NO hashes (both are already on the row from capture, F1) and is
// idempotent (already-adopted -> no-op success). A missing row is an error (the
// row must exist — capture wrote it before Install). A flip-write failure leaves
// a recoverable `adopting` state; the transaction owner reports that receipt
// failure and never converts it to success or unsafe rollback.
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
//
// Identity gate (bug 2026-07-11): the reap is a NO-OP unless the LIVE row still
// matches the caller's expected identity — same ManifestName AND OperationState ==
// expectedState AND UpdatedAt.Equal(expectedUpdatedAt). The held lease already
// excludes a concurrent same-manifest adopt, but the caller selected the row from a
// copy taken EARLIER (the GC's Phase-1 snapshot); this defense-in-depth re-check AT
// THE MUTATION POINT ensures a name-only match can never destroy a row that was
// REPLACED since selection (a fresh committed re-adopt + its secret snapshots).
// Mismatch or absent => return nil without touching snapshots or the store.
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
			return nil // live row changed/vanished since selection => not ours => no-op
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

// adoptGCBeforePhase2Hook is a test-only seam fired ONCE inside
// gcOrphanedAdoptingProvenance AFTER Phase 1 snapshots the candidates but BEFORE
// Phase 2 re-reads + classifies them. It lets a test deterministically simulate a
// concurrent same-manifest re-adopt committing inside the Phase-1->Phase-2 gap and
// exercise Phase 2's under-lease re-read guard (bug 2026-07-11). nil in production.
var adoptGCBeforePhase2Hook func()

// adoptGCBeforeReapHook is a test-only seam fired ONCE inside
// gcOrphanedAdoptingProvenance Phase 2 AFTER a candidate classifies CRASH_REAP but
// BEFORE the mutation-point manifest guard re-checks the manifest. It lets a test
// deterministically simulate a manifest re-created inside the classify->reap window
// and exercise the guard's refuse-and-emit path (bug 2026-07-11 P1-2 Part 3). nil in
// production.
var adoptGCBeforeReapHook func()

// errAdoptPriorConfigMutated flags the ONE capture-UPSERT refusal where a prior
// `adopting` row classifies CRASH_REAP but its per-client entry proof cannot establish
// that Install committed nowhere. The capture refuses to overwrite it rather than
// strand the de-adopt snapshots. It is wrapped into that refusal error so
// ExecuteAdoptWithOpts can errors.Is-recognize it and classify the audit event's reason
// distinctly from an ordinary capture I/O failure (path-free class).
var errAdoptPriorConfigMutated = errors.New("prior adopt entry state does not prove a pre-Install crash; refusing to overwrite committed-looking provenance")

// adoptRowProvablyUnmutatedFn is the GC-lane positive-crash-evidence gate (bug
// 2026-07-11 P1-2 case-5 / Option B), a package var only so a test can prove the gate
// is load-bearing (neutralize it => a case-5 row reaps => data loss). Production
// always uses adoptRowProvablyUnmutated.
var adoptRowProvablyUnmutatedFn = adoptRowProvablyUnmutated

// reapAdoptProvenanceRowFn is the GC Phase-2 row-reap step, a package var ONLY so a
// test can force the reap to fail and exercise the reap-failed audit path (P3-3).
// Production always uses reapAdoptProvenanceRow.
var reapAdoptProvenanceRowFn = reapAdoptProvenanceRow

// gcRemoveRowlessSnapshotsFn is the GC Phase-3 rowless-dir snapshot-removal step, a
// package var ONLY so a test can force the removal to fail and exercise the
// reap-failed audit path (P3-3). Production always uses removeAdoptSnapshots.
var gcRemoveRowlessSnapshotsFn = removeAdoptSnapshots

// adoptRowProvablyUnmutated reports POSITIVE crash-before-Install evidence for a
// dead-owner `adopting` row the caller has already classified CRASH_REAP. REAP
// destroys the row's secret snapshots, so uncertainty always KEEPS the row
// (destructive-default polarity); absence of a committed signal alone is never proof.
//
// SAFETY RESTS ON VALUE-AT-RISK, NOT MONOTONICITY. An earlier draft claimed no
// `adopting` path removes an installed write-target hub relay, so hub-relay presence
// was a monotonic Install-commit signal (ClassifyStillHub => Install committed => keep).
// That premise is FALSE: demigrate / uninstall / hub-mode reconcile revert a committed
// client's entry to native WITHOUT consulting provenance, so ClassifyStillHub can go
// absent on a row Install DID commit. The predicate is safe anyway because a reap never
// puts a secret/config value at risk. Every recorded client is classified through
// CASEntryMutator while its config read lock is held, against the physical write target
// rather than any merged view, and the per-state gate is:
//   - `present`: inode-anchored read + sha-gate the pinned snapshot, extract its raw
//     entry subtree, and require ClassifyRestoreDone — the live write-target entry
//     reflect.DeepEqual-equals the pinned native snapshot. The snapshot is therefore
//     deleted ONLY when its exact content already survives, identical, in the live
//     config: zero restore value at risk. A committed-then-reverted row reaps only when
//     the reverted native equals the pinned native (the original spelling is already
//     live) — the deleted snapshot was redundant. Unrelated whole-config churn is
//     intentionally irrelevant.
//   - `absent` / `present-merged-lower`: no native snapshot is pinned by construction,
//     so classify against a nil snapshot and require a readable non-hub verdict
//     (ClassifyRestoreDone or ClassifyGenuineConflict). A reap here deletes only the row
//   - empty snapshot dir — no secret at risk.
//
// The reap NEVER deletes the row's routed vault keys (owned by de-adopt's hash-gated
// --reclaim-crashed), so the worst case of a wrongly-reaped committed-but-reverted row
// is a BOOKKEEPING residual (orphan row + lingering owner-only vault keys), never a lost
// secret/config spelling. See work-items/bugs/
// 2026-07-12-adopt-reap-native-revert-deletes-committed-provenance.md.
//
// An unavailable/non-CAS adapter, unverifiable snapshot, unreadable classification,
// unknown original state, ClassifyStillHub, or any unexpected verdict fails safe to
// KEEP. A row with NO recorded clients remains vacuously reap-safe: capture writes the
// anchor before Install and has no committed snapshots to preserve.
func adoptRowProvablyUnmutated(rec AdoptProvenanceRecord) bool {
	all := clients.AllClients()
	for _, c := range rec.Clients {
		adapter, ok := all[c.Client]
		if !ok {
			return false // adapter not constructible on this host => cannot prove
		}
		mutator, ok := clients.AsCASEntryMutator(adapter)
		if !ok {
			return false // no locked write-target classifier => cannot prove
		}

		var snapshotSubtree any
		switch c.OriginalState {
		case AdoptOriginalStatePresent:
			state, _, subtree, _ := readDeAdoptSnapshot(&rec, c, mutator)
			if state != deAdoptSnapshotAvailable {
				return false // pinned native snapshot missing/unreadable/mismatched => KEEP
			}
			snapshotSubtree = subtree
		case AdoptOriginalStateAbsent, AdoptOriginalStatePresentMergedLower:
			// These states intentionally have no snapshot. Their proof is solely that
			// Install did not place the expected hub relay in the physical write target.
		default:
			return false // unknown persisted state => cannot prove
		}

		verdict, err := mutator.ClassifyEntryUnderLock(
			rec.SourceEntryName,
			deAdoptLiveBindingMatcher(&rec, c.Client),
			snapshotSubtree,
		)
		if err != nil {
			return false // read/parse/recognizer failure => cannot prove
		}

		switch c.OriginalState {
		case AdoptOriginalStatePresent:
			// ClassifyRestoreDone is reflect.DeepEqual(liveSubtree, snapshotSubtree)
			// over PARSED subtrees (cas_mutator.go:352), not byte equality — which is
			// exactly the right gate, because de-adopt would perform NO restore from a
			// snapshot in this state, so deleting it risks zero restore value:
			//   - De-adopt's OWN disposition consumes the SAME ClassifyEntryUnderLock
			//     verdict: ClassifyRestoreDone maps to DeAdoptClientRestoreDone
			//     ("client already in its de-adopted target state", deadopt.go:980-981)
			//     and the executor SKIPS the client's mutation entirely
			//     (deadopt.go:504-508). Same predicate on both sides, not two DeepEquals.
			//   - Backstop: de-adopt's E3 restore is CASRestoreEntryFromBytes with
			//     allowHubEntry=false (cas_mutator.go:258); casRestoreFromBytes requires
			//     the LIVE entry to still hub-recognizer-match before it touches the
			//     snapshot, so a native (RestoreDone) live entry fails the match =>
			//     ErrCASConflict, no write (cas_mutator.go:216-225). The snapshot is
			//     provably never consumed under RestoreDone.
			// So the byte-level formatting the parsed compare ignores (quote style,
			// comments, whitespace) is never a de-adopt restore product; its loss on
			// reap is immaterial, and the secret-literal VALUE round-trips through the
			// shared extractor anyway. The ONE byte-exact writer,
			// wholeFileRestoreIfWriteTargetGone, is gated on allowHubEntry=true so it is
			// unreachable from de-adopt (adopt-rollback lane only), and it fires only
			// when the live file is ABSENT — which a present client classifies
			// GenuineConflict (present=false + non-nil snapshot), never RestoreDone.
			if verdict != clients.ClassifyRestoreDone {
				return false // StillHub, conflict, unreadable, or unknown => KEEP
			}
		case AdoptOriginalStateAbsent, AdoptOriginalStatePresentMergedLower:
			if verdict != clients.ClassifyRestoreDone && verdict != clients.ClassifyGenuineConflict {
				return false // StillHub, unreadable, or unknown => KEEP
			}
		}
	}
	return true // Install committed on no recorded client
}

// gcOrphanedAdoptingProvenance reaps stale CROSS-manifest orphans using the three
// design-r2 signals — the per-manifest LEASE (Signal 1), the hub-binding-live
// classifier (Signal 2), and the snapshot-dir backstop (Signal 3):
//
//	Phase 1 (store lock): snapshot the aged `adopting` candidates + the set of
//	  manifests that have ANY store row; release the store lock.
//	Phase 2 (per candidate, OUTSIDE the store lock): TryLock its lease — a lease-PATH
//	  resolver ERROR (a legacy ".lease"-suffixed manifest now refused by the P3-1
//	  guard) is REPORTED as adopt-provenance-reap-failed{phase:gc-lease-path-error}
//	  then skipped (F1 — an unreachable orphan must not be silent); a lease HELD by a
//	  LIVE adopt is a legitimate silent skip (claim 16). With the lease held the owner
//	  is provably dead; then RE-READ the row under the store lock and require it is STILL the
//	  exact orphan Phase 1 selected (still `adopting`, UpdatedAt unchanged, still
//	  older than the cutoff) — a stale Phase-1 copy must never drive a reap after a
//	  concurrent re-adopt replaced the row (bug 2026-07-11). classifyDeadAdoptingRow
//	  decides on the LIVE bytes: COMMITTED_KEEP (a live hub binding OR a manifest on
//	  disk, Signal 2b) is preserved. A CRASH_REAP verdict then passes TWO more
//	  destructive-safety gates before the reap (bug 2026-07-11 P1-2): a mutation-point
//	  manifest re-check (Part 3 — refuse + emit adopt-provenance-reap-skipped-manifest
//	  -present if a manifest exists now), and a POSITIVE crash-evidence gate (Part 2 —
//	  adoptRowProvablyUnmutatedFn: reap only when every client's locked physical
//	  write-target entry shape proves Install committed nowhere; any uncertainty =>
//	  KEEP). Only a triply-cleared row removes snapshots-then-row under the held lease,
//	  identity-gated at the mutation point.
//	Phase 3 (backstop): reap ROWLESS snapshot dirs (a <manifest>/ dir with no store
//	  row — findings 3/4 residue or any future ordering bug), gated on the lease +
//	  no-store-row, NOT age-gated (a rowless dir has no updated_at). Same F1 lease-path
//	  -error report as Phase 2 for a rowless legacy ".lease"-named dir.
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

	// Test-only seam: simulate a concurrent same-manifest re-adopt committing inside
	// the Phase-1->Phase-2 gap. nil in production.
	if adoptGCBeforePhase2Hook != nil {
		adoptGCBeforePhase2Hook()
	}

	// Phase 2 — reap true cross-manifest row-bearing orphans under each own lease.
	for _, c := range candidates {
		lk, ok, lErr := tryAcquireAdoptManifestLease(c.rec.ManifestName)
		if lErr != nil {
			// A lease-PATH resolver error (notably a legacy ".lease"-suffixed manifest a
			// pre-P3-1 build allowed on disk — its lease path now fails the reserved-suffix
			// guard) makes this orphan permanently unreachable by the reaper. Do NOT silently
			// skip it (F1): REPORT it so an operator can remove adopt-provenance/<name>
			// manually. Best-effort emit; still skip the reap (nothing was mutated).
			emitAdoptProvenanceReapFailed(c.rec.ManifestName, adoptReapFailPhaseLeasePathError, lErr.Error())
			continue
		}
		if !ok {
			continue // lease HELD by a live adopt => legitimate silent skip (claim 16)
		}
		cleanupErr := func() (cleanupErr error) {
			defer func() { cleanupErr = finishAdoptGCLease(c.rec.ManifestName, lk) }()
			// RE-READ the row UNDER the held lease before classifying (bug 2026-07-11).
			// c.rec is the STALE Phase-1 copy taken before the lease was held: a concurrent
			// same-manifest re-adopt can UPSERT a FRESH committed row (new UpdatedAt /
			// `adopted` state / a new port) into the Phase-1->Phase-2 gap. Classifying the
			// stale copy would reconstruct the expected binding from the OLD port, miss the
			// new live binding, and reap the freshly-committed row + its secret snapshots.
			// Only reap when the LIVE row is STILL the exact dead-owner orphan Phase 1
			// selected: still `adopting`, UpdatedAt unchanged, still older than the cutoff
			// (mirrors Phase 3's under-lease re-confirm). Classify the LIVE bytes.
			var (
				live      AdoptProvenanceRecord
				stillOurs bool
			)
			_ = withAdoptedEntriesLock(func() error {
				store, rErr := readAdoptedEntries()
				if rErr != nil {
					return nil // fail-safe: leave stillOurs=false, do not reap on a read error
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
				return nil // row changed/vanished since Phase-1 selection => not our orphan => skip
			}
			if classifyDeadAdoptingRow(live) != adoptRowCrashReap {
				return nil // COMMITTED_KEEP (live hub binding OR manifest on disk, Signal 2b)
			}
			// Test-only seam: simulate a manifest re-created inside the classify->reap
			// window so the mutation-point guard below is exercised. nil in production.
			if adoptGCBeforeReapHook != nil {
				adoptGCBeforeReapHook()
			}
			// Part 3 — mutation-point manifest guard (defense-in-depth for Signal 2b).
			// Re-check the manifest UNDER the held lease, immediately before the destructive
			// reap. classifyDeadAdoptingRow already KEEPs a manifest-present row, so under
			// normal flow this never fires; it catches a classifier regression or a manifest
			// re-created between the classify and the reap, and records a DISTINCT audit event
			// so an over-reap-that-was-averted is operator-visible (NAMES/COUNTS only).
			if exists, mErr := adoptManifestExistsFn(live.ManifestName); mErr != nil || exists {
				emitAdoptProvenanceReapSkippedManifestPresent(live.ManifestName, c.ageSec)
				return nil
			}
			// Part 2 — positive crash-before-Install evidence gate (case-5 closure). REAP
			// only when every client's locked physical write-target entry shape proves
			// Install committed nowhere; anything unprovable fails safe toward KEEP. This
			// distinguishes a committed adopt after manifest/binding drift without treating
			// unrelated whole-config churn as evidence of Install.
			if !adoptRowProvablyUnmutatedFn(live) {
				return nil // cannot positively prove pre-install => preserve the row + snapshots
			}
			if rErr := reapAdoptProvenanceRowFn(live.ManifestName, AdoptOperationStateAdopting, live.UpdatedAt); rErr == nil {
				emitAdoptProvenanceOrphanReaped(live.ManifestName, c.ageSec, adoptOrphanReapTriggerGC)
				reaped++
				// The GC reaps the row + snapshot dir ONLY. It does NOT delete the row's
				// routed vault keys: a background GC must never autonomously drop secret
				// material a live adopt could still reference (bug
				// 2026-07-12-adopt-preinstall-crash-orphan-triple — a normalization
				// collision or corrupted-provenance row could share a key with a LIVE
				// committed adopt, so cross-manifest key deletion here is unsafe). Routed-key
				// cleanup is owned by de-adopt (hash-gated, operator-driven --reclaim-crashed).
				// Bounded residual: a reversed-preserve reap leaves the routed keys in the
				// owner-only vault until de-adopt (or the operator) removes them.
			} else {
				// Reap failed (store write / snapshot removal error): the secret-bearing
				// `adopting` orphan remains on disk. Surface it (P3-3) so an operator sees
				// the stuck orphan instead of it being silently retried next pass. The reason
				// is the returned error's path/class string (reapAdoptProvenanceRow errors are
				// path/class only, never a secret value). Best-effort — an audit miss must
				// never fail the best-effort GC.
				emitAdoptProvenanceReapFailed(live.ManifestName, adoptReapFailPhaseRow, rErr.Error())
			}
			return nil
		}()
		if cleanupErr != nil {
			return reaped, cleanupErr
		}
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
		if lErr != nil {
			// Same F1 legacy ".lease" case as Phase 2: a rowless snapshot dir whose name
			// fails the lease-path suffix guard is unreachable by the reaper — REPORT it
			// instead of silently skipping. Best-effort emit; still skip the removal.
			emitAdoptProvenanceReapFailed(m, adoptReapFailPhaseLeasePathError, lErr.Error())
			continue
		}
		if !ok {
			continue // live adopt (may be mid-anchor) => legitimate silent skip
		}
		cleanupErr := func() (cleanupErr error) {
			defer func() { cleanupErr = finishAdoptGCLease(m, lk) }()
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
				if rmErr := gcRemoveRowlessSnapshotsFn(m); rmErr == nil {
					emitAdoptProvenanceOrphanReaped(m, 0, adoptOrphanReapTriggerGC)
					reaped++
				} else {
					// Rowless-dir snapshot removal failed: the secret-bearing rowless dir
					// remains. Surface it (P3-3) instead of silently leaving it for the next
					// pass. Reason is the error's path/class string (removeAdoptSnapshots
					// errors are path/class only, never a secret value). Best-effort.
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

// ---------------------------------------------------------------------------
// De-adopt-owned MUTATORS. Phase 6 implements the whole-manifest Mark + Close
// operations here against the protected store. The subset hash update remains
// declared-only for its follow-up:
//
//	func UpdateAdoptExpectedManifestHash(manifestName, newHash string) error // subset binding edit
// ---------------------------------------------------------------------------

// MarkAdoptProvenanceDeAdopting transitions adopted (or a re-verified,
// committed adopting row) to de_adopting.
//
// PRECONDITION: the caller (the de-adopt executor, ExecuteDeAdoptWithOpts) holds
// the per-manifest lease across the E1..E6 flow; this mutator does NOT re-acquire
// it (a second same-process flock handle would fail-closed on Windows). The
// entries lock still protects the row read-classify-write transaction.
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
				// Ready to transition below.
			case AdoptOperationStateAdopting:
				if classifyDeadAdoptingRow(*rec) != adoptRowCommittedKeep {
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

// CloseAdoptProvenance removes a de_adopting row and its snapshots.
//
// PRECONDITION: the caller (the de-adopt executor, ExecuteDeAdoptWithOpts) holds
// the per-manifest lease across the E1..E6 flow; this mutator does NOT re-acquire
// it (a second same-process flock handle would fail-closed on Windows). The
// caller retains the lease while CloseAdoptProvenance delegates deletion to
// reapAdoptProvenanceRow, the store's single identity-gated snapshots-first
// deletion path.
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

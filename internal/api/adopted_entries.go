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
// Locking: the entire body runs under withAdoptedEntriesLock; the per-snapshot
// flock is nested strictly inside (a consistent, deadlock-free lock order).
//
// NOTE (Phase B): this is UNWIRED — ExecuteAdoptWithOpts does not call it yet
// (that is Phase C). It is exercised by unit tests only.
func (a *API) captureAdoptProvenance(plan *AdoptPlan) (*AdoptProvenanceRecord, error) {
	if plan == nil {
		return nil, fmt.Errorf("adopt provenance capture: nil plan")
	}

	var (
		rec          *AdoptProvenanceRecord
		reapedPrior  bool
		reapedAgeSec float64
	)
	lockErr := withAdoptedEntriesLock(func() error {
		store, err := readAdoptedEntries()
		if err != nil {
			return fmt.Errorf("adopt provenance capture: read store: %w", err)
		}

		// UPSERT step 1: drop any prior row for this manifest (in memory) and
		// reap its stale snapshot dir on disk before re-pinning.
		var kept []AdoptProvenanceRecord
		for _, r := range store.Records {
			if r.ManifestName == plan.ManifestName {
				reapedPrior = true
				reapedAgeSec = time.Since(r.UpdatedAt).Seconds()
				continue
			}
			kept = append(kept, r)
		}
		store.Records = kept
		if err := removeAdoptSnapshots(plan.ManifestName); err != nil {
			return fmt.Errorf("adopt provenance capture: reap stale snapshot dir: %w", err)
		}

		// UPSERT step 2: classify each selected client + pin present snapshots.
		clientsProv, capErr := captureAdoptClientsProvenance(plan)
		if capErr != nil {
			// Fail closed: drop any partial snapshots pinned this call, and if we
			// removed a prior row, persist that removal so no row points at the
			// now-deleted snapshot dir.
			_ = removeAdoptSnapshots(plan.ManifestName)
			if reapedPrior {
				if wErr := writeAdoptedEntries(store); wErr != nil {
					return fmt.Errorf("%w; additionally failed to persist provenance cleanup: %v", capErr, wErr)
				}
			}
			return capErr
		}

		hash := ManifestHashContent([]byte(plan.ManifestYAML))
		now := time.Now().UTC()
		built := AdoptProvenanceRecord{
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
			Clients:              clientsProv,
		}
		store.Records = append(store.Records, built)
		if err := writeAdoptedEntries(store); err != nil {
			_ = removeAdoptSnapshots(plan.ManifestName)
			return fmt.Errorf("adopt provenance capture: write store: %w", err)
		}
		rec = &built
		return nil
	})

	if reapedPrior {
		emitAdoptProvenanceOrphanReaped(plan.ManifestName, reapedAgeSec, adoptOrphanReapTriggerUpsert)
	}
	if lockErr != nil {
		return nil, lockErr
	}
	emitAdoptProvenanceCaptured(rec)
	return rec, nil
}

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
	presentAtBuild := make(map[string]bool, len(plan.PresentAtBuild))
	for _, c := range plan.PresentAtBuild {
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
			configBytes, rErr := os.ReadFile(cfgPath)
			if rErr != nil {
				return nil, fmt.Errorf("adopt provenance capture: read client %q config file: %w", name, rErr)
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

// abortAdoptProvenance deletes the manifest's row and RemoveAll's its snapshot
// dir during adopt failure cleanup. Idempotent + best-effort: a second call (or
// a call for a manifest with no row) is a no-op success; an abort error is
// RETURNED to the caller (which appends it to the operator message) and never
// masks the caller's original adopt error.
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
		if err := writeAdoptedEntries(store); err != nil {
			return fmt.Errorf("adopt provenance abort: write store: %w", err)
		}
		if err := removeAdoptSnapshots(manifestName); err != nil {
			return fmt.Errorf("adopt provenance abort: remove snapshots: %w", err)
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

// gcOrphanedAdoptingProvenance reaps stale CROSS-manifest `adopting` orphans: a
// row still `adopting` whose updated_at is older than olderThan (hard-crash
// debris), plus its owner-only, secret-bearing snapshot dir. It is the
// cross-manifest complement to the capture UPSERT (which reaps only a
// SAME-manifest orphan on the operator's next same-manifest adopt): a hard crash
// between captureAdoptProvenance and Install/abort otherwise leaves a snapshot
// under <state-dir>/adopt-provenance/<manifest>/ with no automatic reaper for any
// other manifest.
//
// Bounded + safe: only `adopting` rows (never adopted/de_adopting/closed), only
// older-than-threshold rows, all under withAdoptedEntriesLock so it cannot race a
// concurrent capture/promote/abort. Returns the count reaped. Callers run it
// best-effort — a GC error must not block a fresh adopt.
//
// Crash-safety / ordering: snapshot dirs are removed FIRST, then the row removal
// is persisted in one write. A crash between leaves the rows `adopting` (aged), so
// the next GC re-reaps them (removeAdoptSnapshots is idempotent on a missing dir)
// — it never leaves an orphaned dir with no row (which no GC could reach). A
// snapshot-removal error aborts before the store write, so nothing is half-reaped.
func gcOrphanedAdoptingProvenance(olderThan time.Duration) (reaped int, err error) {
	type reapedOrphan struct {
		manifest string
		ageSec   float64
	}
	var toEmit []reapedOrphan
	lockErr := withAdoptedEntriesLock(func() error {
		store, err := readAdoptedEntries()
		if err != nil {
			return fmt.Errorf("adopt provenance gc: read store: %w", err)
		}
		cutoff := time.Now().Add(-olderThan)
		var kept []AdoptProvenanceRecord
		var reapedManifests []string
		for _, r := range store.Records {
			if r.OperationState == AdoptOperationStateAdopting && r.UpdatedAt.Before(cutoff) {
				reapedManifests = append(reapedManifests, r.ManifestName)
				toEmit = append(toEmit, reapedOrphan{manifest: r.ManifestName, ageSec: time.Since(r.UpdatedAt).Seconds()})
				continue
			}
			kept = append(kept, r)
		}
		if len(reapedManifests) == 0 {
			return nil // nothing stale; no write, no snapshot removal
		}
		for _, m := range reapedManifests {
			if rmErr := removeAdoptSnapshots(m); rmErr != nil {
				return fmt.Errorf("adopt provenance gc: remove snapshots for %q: %w", m, rmErr)
			}
		}
		store.Records = kept
		if err := writeAdoptedEntries(store); err != nil {
			return fmt.Errorf("adopt provenance gc: write store: %w", err)
		}
		return nil
	})
	if lockErr != nil {
		return 0, lockErr
	}
	for _, o := range toEmit {
		emitAdoptProvenanceOrphanReaped(o.manifest, o.ageSec, adoptOrphanReapTriggerGC)
	}
	return len(toEmit), nil
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

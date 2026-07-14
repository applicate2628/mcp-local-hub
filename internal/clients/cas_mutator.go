package clients

import (
	"errors"
	"fmt"
)

// ErrCASConflict is the sentinel a CAS entry mutator returns when the live
// client-config entry no longer matches the hub entry the caller expected —
// the compare-and-swap "swap" is refused because the "compare" failed. A
// de-adopt caller treats it as a per-client CONFLICT (surfaced in the report),
// never a silent overwrite. Wrapped with %w at every refusal site so
// errors.Is(err, ErrCASConflict) holds.
var ErrCASConflict = errors.New("clients: CAS conflict — live client entry no longer matches the expected hub entry")

// CASEntryMutator is the read-check-mutate capability the de-adopt flow uses to
// restore or remove a client's hub entry ATOMICALLY under the config lock. It is
// a capability interface (mirroring EntryBytesChecker), NOT a Client method:
// never-adoptable adapters must not be compile-forced to implement a restore they
// can never run.
//
// The whole point is atomicity. withConfigLock wraps each adapter method
// individually, so a plan-time recognizer check followed by a later
// AddEntry/RemoveEntry is NOT one critical section — an operator hand-edit (or a
// demigrate) landing between plan and execute would let de-adopt restore a stale
// snapshot OVER the operator's fresh edit (silent data loss). Each CAS method
// instead does the whole re-read -> check -> mutate under ONE held lock.
//
// LOCK OWNERSHIP (design P3). The lockingClient FORWARDER holds
// withConfigLock(ConfigPath) across the whole call (type-assert-inside-lock,
// mirroring AddEntryWithConfigWriter, config_lock.go:229-239). The concrete
// method bodies below run UNDER that held lock and are themselves LOCK-FREE: the
// per-path mutex is non-reentrant (config_lock.go:24-30), so a concrete body
// calling withConfigLock would self-deadlock. They call only the LOCK-FREE
// concrete reads/writes (GetEntry, restoreEntryFromBytes, RemoveEntry).
//
// ADOPT-REACHABILITY ENFORCEMENT (Phase-3 constraint, fable-5 Phase-2 audit).
// These methods are defined on EACH adopt-reachable CONCRETE adapter, NEVER on
// the shared jsonMCPClient base. That is deliberate and load-bearing:
// windsurfClient (NOT adopt-reachable — it overrides RestoreEntryFromBackup* with
// serverUrl-aware bodies) EMBEDS *jsonMCPClient, so any method on the base is
// PROMOTED onto windsurf's method set. If CAS lived on the base, a bare
// client.(CASEntryMutator) assert would SUCCEED for windsurf via promotion, with
// base restore semantics that windsurf deliberately overrode. Keeping CAS off the
// base means windsurf (and any other non-adopt jsonMCPClient embedder) does NOT
// satisfy CASEntryMutator at all — the method set IS the allowlist. Callers MUST
// resolve the capability through AsCASEntryMutator (which inspects the CONCRETE
// method set), never a bare assert on the lockingClient wrapper (whose forwarders
// would satisfy the interface regardless of the wrapped concrete).
//
// Phase 4 GROWS this interface with the read-only ClassifyEntryUnderLock +
// EntryRawSubtree seam; Phase 3 ships the two mutating methods only.
type CASEntryMutator interface {
	// CASRestoreEntryFromBytes, under the held lock: re-read the named live
	// entry, then
	//   - live == nil            -> ErrCASConflict (restoring into an
	//     operator-emptied slot would resurrect an entry against intent).
	//   - match(live) == false   -> ErrCASConflict.
	//   - EntryPresentInBytes(snapshotBytes, name) == false -> ErrCASConflict,
	//     fail-closed: an impossible state for a `present` caller (adopt capture
	//     GUARANTEED + sha-pinned the entry), and removal is CASGuardedRemoveEntry's
	//     alone — restore NEVER silently deletes (design B5).
	//   - else restore by COMPOSING the shipped per-adapter restore core:
	//     restoreEntryFromBytes(snapshotBytes, name, allowHubEntry=false, nil) —
	//     the guarded polarity that refuses a hub-shaped snapshot entry (design
	//     B1/P3). One read, one write, one lock. No second restore/extraction owner.
	//
	// match is the injected hub recognizer (dependency inversion: clients defines
	// the callback shape; the api layer injects liveEntryMatchesManifestBinding, so
	// the equality owner stays single). A nil match fails closed (ErrCASConflict),
	// never panics. The body nil-guards `live` BEFORE calling match (the recognizer
	// derefs live.URL).
	CASRestoreEntryFromBytes(entryName string, match func(*MCPEntry) bool, snapshotBytes []byte) error

	// CASGuardedRemoveEntry, under the held lock: re-read the named live entry, then
	//   - live == nil            -> already-done idempotent SUCCESS (nothing to remove).
	//   - match(live) == false   -> ErrCASConflict.
	//   - else remove it.
	// Used to restore to ABSENCE (original_state absent) or to strip the hub
	// write-target entry so a lower/merged layer re-emerges (present-merged-lower).
	// nil match fails closed (ErrCASConflict), never panics.
	CASGuardedRemoveEntry(entryName string, match func(*MCPEntry) bool) error
}

// Compile-time proof that every adopt-reachable adapter carries the CAS method
// set in its OWN right (the 5 standalone adapters implement the methods directly;
// the 4 jsonMCPClient embedders compose the promoted restoreEntryFromBytes /
// EntryPresentInBytes / RemoveEntry core with their OWN GetEntry). This set is the
// allowlist mirror of adoptSupportedClients (api layer). There is DELIBERATELY no
// `_ CASEntryMutator = (*windsurfClient)(nil)` line: windsurf must NOT satisfy the
// capability (see the interface doc), and the absence would be a compile error if
// CAS were ever wrongly added to the shared base.
var (
	_ CASEntryMutator = (*claudeCode)(nil)
	_ CASEntryMutator = (*codexCLI)(nil)
	_ CASEntryMutator = (*cursorClient)(nil)
	_ CASEntryMutator = (*vscodeClient)(nil)
	_ CASEntryMutator = (*geminiCLI)(nil)
	_ CASEntryMutator = (*qwenCLI)(nil)
	_ CASEntryMutator = (*antigravityClient)(nil)
	_ CASEntryMutator = (*openCodeClient)(nil)
	_ CASEntryMutator = (*mimoCodeClient)(nil)
	_ CASEntryMutator = (*lockingClient)(nil)
)

// AsCASEntryMutator resolves the CAS capability for c, but ONLY when c's CONCRETE
// adapter carries the CAS method set in its own right. It returns (nil, false)
// for every other client — crucially including windsurfClient and any other
// non-adopt jsonMCPClient embedder.
//
// This is the EXPLICIT ALLOWLIST the de-adopt site must consult instead of a bare
// c.(CASEntryMutator) assert. The lockingClient wrapper itself satisfies
// CASEntryMutator through its forwarders, so a bare assert on the wrapper would
// SUCCEED even for a windsurf-wrapped client and only fail (fail-closed) at call
// time. Unwrapping to inspect the CONCRETE adapter's own method set turns that
// into a clean, up-front (nil, false) — and it never depends on client NAMES, so
// it cannot drift out of sync with a rename.
//
// On success it returns the ORIGINAL c (the lockingClient wrapper in production),
// so the returned mutator's forwarder still HOLDS withConfigLock. A bare concrete
// (test-only, never lockingClient-wrapped) is returned as-is and runs LOCK-FREE —
// correct only for a single-threaded unit test.
func AsCASEntryMutator(c Client) (CASEntryMutator, bool) {
	concrete := c
	if lc, ok := c.(*lockingClient); ok {
		concrete = lc.Client
	}
	// The gate: does the CONCRETE adapter (not the forwarding wrapper) carry the
	// CAS method set? windsurfClient does not, so it lands here as (nil, false).
	if _, ok := concrete.(CASEntryMutator); !ok {
		return nil, false
	}
	m, ok := c.(CASEntryMutator)
	return m, ok
}

// casRestoreFromBytes is the SINGLE owner of the CAS restore gate. All I/O flows
// through the injected adapter functions so the live read resolves the adapter's
// OWN GetEntry (relay / serverUrl / httpUrl overrides included — the same GetEntry
// the shipped classifyDeadAdoptingRow recognizer path uses) and the restore
// COMPOSES the adapter's OWN restoreEntryFromBytes core (design B1: no second
// extraction owner). Runs under the caller-held config lock; never locks itself.
func casRestoreFromBytes(
	entryName string,
	match func(*MCPEntry) bool,
	snapshotBytes []byte,
	getEntry func(string) (*MCPEntry, error),
	entryPresentInBytes func([]byte, string) (bool, error),
	restoreFromBytes func([]byte, string, bool, WriteConfigFileFunc) error,
) error {
	if match == nil {
		return fmt.Errorf("%w: nil recognizer for entry %q (fail-closed)", ErrCASConflict, entryName)
	}
	live, err := getEntry(entryName)
	if err != nil {
		return err
	}
	if live == nil {
		return fmt.Errorf("%w: live entry %q is absent — refusing to resurrect an operator-removed entry", ErrCASConflict, entryName)
	}
	if !match(live) {
		return fmt.Errorf("%w: live entry %q is no longer the hub entry — refusing to overwrite it", ErrCASConflict, entryName)
	}
	present, err := entryPresentInBytes(snapshotBytes, entryName)
	if err != nil {
		return fmt.Errorf("CAS restore %q: validate snapshot bytes: %w", entryName, err)
	}
	if !present {
		// Impossible for a `present` caller (capture guaranteed + sha-pinned the
		// entry). Fail closed — NEVER fall through to a remove (that is
		// CASGuardedRemoveEntry's job alone; design B5).
		return fmt.Errorf("%w: snapshot bytes do not contain entry %q — refusing (fail-closed; restore never removes)", ErrCASConflict, entryName)
	}
	// Compose the shipped restore core with the guarded (allowHubEntry=false)
	// polarity, and no scoped writer (default hardened write — single entry, one
	// client, one lock).
	return restoreFromBytes(snapshotBytes, entryName, false, nil)
}

// casGuardedRemove is the SINGLE owner of the CAS remove gate. Same lock/injection
// posture as casRestoreFromBytes.
func casGuardedRemove(
	entryName string,
	match func(*MCPEntry) bool,
	getEntry func(string) (*MCPEntry, error),
	removeEntry func(string) error,
) error {
	if match == nil {
		return fmt.Errorf("%w: nil recognizer for entry %q (fail-closed)", ErrCASConflict, entryName)
	}
	live, err := getEntry(entryName)
	if err != nil {
		return err
	}
	if live == nil {
		return nil // nothing to remove — already-done idempotent success
	}
	if !match(live) {
		return fmt.Errorf("%w: live entry %q is no longer the hub entry — refusing to remove it", ErrCASConflict, entryName)
	}
	return removeEntry(entryName)
}

// ---- per-adapter CAS methods (methods may live in any file of package clients;
// co-located here so the adopt-reachable set is auditable in one place) ----
//
// Each pair is a thin binding of the shared gate to the adapter's OWN methods, so
// the live read dispatches to the concrete GetEntry override and the restore
// composes the concrete (or promoted-base) restoreEntryFromBytes.

func (c *claudeCode) CASRestoreEntryFromBytes(name string, match func(*MCPEntry) bool, snapshotBytes []byte) error {
	return casRestoreFromBytes(name, match, snapshotBytes, c.GetEntry, c.EntryPresentInBytes, c.restoreEntryFromBytes)
}
func (c *claudeCode) CASGuardedRemoveEntry(name string, match func(*MCPEntry) bool) error {
	return casGuardedRemove(name, match, c.GetEntry, c.RemoveEntry)
}

func (c *codexCLI) CASRestoreEntryFromBytes(name string, match func(*MCPEntry) bool, snapshotBytes []byte) error {
	return casRestoreFromBytes(name, match, snapshotBytes, c.GetEntry, c.EntryPresentInBytes, c.restoreEntryFromBytes)
}
func (c *codexCLI) CASGuardedRemoveEntry(name string, match func(*MCPEntry) bool) error {
	return casGuardedRemove(name, match, c.GetEntry, c.RemoveEntry)
}

func (v *vscodeClient) CASRestoreEntryFromBytes(name string, match func(*MCPEntry) bool, snapshotBytes []byte) error {
	return casRestoreFromBytes(name, match, snapshotBytes, v.GetEntry, v.EntryPresentInBytes, v.restoreEntryFromBytes)
}
func (v *vscodeClient) CASGuardedRemoveEntry(name string, match func(*MCPEntry) bool) error {
	return casGuardedRemove(name, match, v.GetEntry, v.RemoveEntry)
}

func (o *openCodeClient) CASRestoreEntryFromBytes(name string, match func(*MCPEntry) bool, snapshotBytes []byte) error {
	return casRestoreFromBytes(name, match, snapshotBytes, o.GetEntry, o.EntryPresentInBytes, o.restoreEntryFromBytes)
}
func (o *openCodeClient) CASGuardedRemoveEntry(name string, match func(*MCPEntry) bool) error {
	return casGuardedRemove(name, match, o.GetEntry, o.RemoveEntry)
}

func (o *mimoCodeClient) CASRestoreEntryFromBytes(name string, match func(*MCPEntry) bool, snapshotBytes []byte) error {
	return casRestoreFromBytes(name, match, snapshotBytes, o.GetEntry, o.EntryPresentInBytes, o.restoreEntryFromBytes)
}
func (o *mimoCodeClient) CASGuardedRemoveEntry(name string, match func(*MCPEntry) bool) error {
	return casGuardedRemove(name, match, o.GetEntry, o.RemoveEntry)
}

// The jsonMCPClient embedders (cursor / gemini-cli / qwen-cli / antigravity) each
// compose the PROMOTED base restoreEntryFromBytes / EntryPresentInBytes /
// RemoveEntry with their OWN GetEntry — cursor promotes the base URL reader;
// gemini/qwen/antigravity override GetEntry for their "url" / "httpUrl" / relay
// shapes, and passing the concrete method value binds the override (Go has no
// virtual dispatch, so a CAS method on the base would bind the base GetEntry and
// silently mis-read relay/httpUrl entries — another reason CAS is per-concrete).

func (c *cursorClient) CASRestoreEntryFromBytes(name string, match func(*MCPEntry) bool, snapshotBytes []byte) error {
	return casRestoreFromBytes(name, match, snapshotBytes, c.GetEntry, c.EntryPresentInBytes, c.restoreEntryFromBytes)
}
func (c *cursorClient) CASGuardedRemoveEntry(name string, match func(*MCPEntry) bool) error {
	return casGuardedRemove(name, match, c.GetEntry, c.RemoveEntry)
}

func (g *geminiCLI) CASRestoreEntryFromBytes(name string, match func(*MCPEntry) bool, snapshotBytes []byte) error {
	return casRestoreFromBytes(name, match, snapshotBytes, g.GetEntry, g.EntryPresentInBytes, g.restoreEntryFromBytes)
}
func (g *geminiCLI) CASGuardedRemoveEntry(name string, match func(*MCPEntry) bool) error {
	return casGuardedRemove(name, match, g.GetEntry, g.RemoveEntry)
}

func (q *qwenCLI) CASRestoreEntryFromBytes(name string, match func(*MCPEntry) bool, snapshotBytes []byte) error {
	return casRestoreFromBytes(name, match, snapshotBytes, q.GetEntry, q.EntryPresentInBytes, q.restoreEntryFromBytes)
}
func (q *qwenCLI) CASGuardedRemoveEntry(name string, match func(*MCPEntry) bool) error {
	return casGuardedRemove(name, match, q.GetEntry, q.RemoveEntry)
}

func (a *antigravityClient) CASRestoreEntryFromBytes(name string, match func(*MCPEntry) bool, snapshotBytes []byte) error {
	return casRestoreFromBytes(name, match, snapshotBytes, a.GetEntry, a.EntryPresentInBytes, a.restoreEntryFromBytes)
}
func (a *antigravityClient) CASGuardedRemoveEntry(name string, match func(*MCPEntry) bool) error {
	return casGuardedRemove(name, match, a.GetEntry, a.RemoveEntry)
}

// ---- lockingClient forwarders: HOLD withConfigLock across the whole CAS call ----
//
// The forwarder owns the lock; the concrete body (dispatched via l.Client) runs
// under it and is lock-free. A wrapped client whose CONCRETE does not implement
// the capability (e.g. a windsurf-wrapped lockingClient reached by accident)
// fails closed with an explicit error — but the de-adopt site should never reach
// this because AsCASEntryMutator gates on the concrete first.

func (l *lockingClient) CASRestoreEntryFromBytes(name string, match func(*MCPEntry) bool, snapshotBytes []byte) error {
	return withConfigLock(l.Client.ConfigPath(), func() error {
		m, ok := l.Client.(CASEntryMutator)
		if !ok {
			return fmt.Errorf("client %s does not support CAS entry mutation", l.Client.Name())
		}
		return m.CASRestoreEntryFromBytes(name, match, snapshotBytes)
	})
}

func (l *lockingClient) CASGuardedRemoveEntry(name string, match func(*MCPEntry) bool) error {
	return withConfigLock(l.Client.ConfigPath(), func() error {
		m, ok := l.Client.(CASEntryMutator)
		if !ok {
			return fmt.Errorf("client %s does not support CAS entry mutation", l.Client.Name())
		}
		return m.CASGuardedRemoveEntry(name, match)
	})
}

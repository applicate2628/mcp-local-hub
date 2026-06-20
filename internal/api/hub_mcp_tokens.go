// hub_mcp_tokens.go — Phase 2 Task 2.4 (G4 unified hub MCP).
//
// Per-client token table. Each MCP-aware client (claude-code,
// codex-cli, cursor, vscode, gemini-cli, qwen-cli) gets its own
// 64-lower-hex token via crypto/rand 32-byte → hex.EncodeToString.
// The token is the `X-Mcphub-Hub-Token` HTTP header value the client
// sends on every request to `http://127.0.0.1:<port>/clients/<id>/mcp`;
// the Phase 4 auth gate uses ConstantTimeCompareToken to validate it
// (subtle.ConstantTimeCompare under a 64-byte shape gate).
//
// Lifecycle:
//
//   - EnsureHubTokens(clients) generates fresh tokens for clients NOT
//     yet present and returns the post-mutation snapshot. Existing
//     tokens are NEVER rotated by Ensure — that's RotateHubToken's
//     job. This is the "install a new client without invalidating
//     existing clients" path (Phase 5 install reconciler).
//   - RotateHubToken(client) generates a new token for `client` and
//     rewrites the file under hub-mcp.lock. Triggered by
//     `mcphub hub-mcp regenerate-token --client X`. The Phase 4
//     /internal/reload-tokens endpoint or the Phase 5 CLI's
//     publish-and-restart path makes the rotation visible to live
//     requests.
//   - ReloadHubTokens() re-reads <state-dir>/hub-mcp-tokens.json from
//     disk and publishes a new snapshot via atomic.Pointer. Phase 4's
//     /internal/reload-tokens HTTP handler calls this; the CLI side
//     does NOT need to (RotateHubToken already publishes).
//
// In-process publication via `atomic.Pointer[HubTokenTable]` so the
// auth gate (Phase 4) can load the live snapshot without taking a
// mutex on every request. Writers serialize on hub-mcp.lock.
//
// Spec: docs/superpowers/specs/2026-05-12-g4-unified-hub-mcp-design-v3.md
// §"Token + endpoint state hardening" (atomic-write block) +
// §"Token-table reload on rotation" + §"Control endpoint contract
// /internal/reload-tokens". Plan: Task 2.4.

package api

import (
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"sync/atomic"
)

// HubTokenTable is the on-disk shape of hub-mcp-tokens.json plus the
// in-process snapshot type. One entry per canonical client id (per
// `clients.SupportedClientNames()`); values are 64-lower-hex tokens.
//
// The JSON wire format keeps `Tokens` lowercase so future fields can
// be added without breaking forward compatibility (the load helper
// returns the parsed struct; unknown JSON fields are silently
// ignored).
type HubTokenTable struct {
	Tokens map[string]string `json:"tokens"`
}

// hubMcpGroupTokenOrphansFileLeaf records group scope keys whose groups.yaml
// delete committed but whose token row could not be pruned. It is the durable
// discriminator that survives stale resolver snapshots and cold starts.
const hubMcpGroupTokenOrphansFileLeaf = "hub-mcp-group-token-orphans.json"

type groupTokenOrphanTable struct {
	Orphans map[string]bool `json:"orphans"`
}

// liveTokenTable holds the published snapshot for the auth gate. The
// atomic.Pointer swap makes per-request reads lock-free while
// guaranteeing visibility of new tokens after every Ensure / Rotate /
// Reload. The pointer is `nil` until the first publish.
var liveTokenTable atomic.Pointer[HubTokenTable]

// EnsureHubTokens generates a fresh token for every named client that
// doesn't yet have one, persists the updated table, and publishes the
// new snapshot. Returns the post-mutation table.
//
// If every named client is already present, no write happens — the
// existing on-disk file is left intact and the returned snapshot is
// the loaded one. Calling Ensure with `clients=nil` is a valid
// "publish the current snapshot" no-op.
//
// Existing tokens are never rotated. Callers that need rotation use
// RotateHubToken; callers that need a full burn-down use
// RotateHubInstanceID (which makes EVERY client re-install).
func EnsureHubTokens(clients []string) (HubTokenTable, error) {
	lk, err := acquireHubMcpLock()
	if err != nil {
		return HubTokenTable{}, err
	}
	defer func() { _ = lk.Unlock() }()
	return ensureHubTokensLocked(clients)
}

// EnsureGroupTokens ensures a per-group hub-token row exists for each
// supplied group scope key ("g:<group>", from GroupScopeKeys) and returns
// the post-mutation snapshot. It is the §D auth seam wired loopback-only
// (operator decision 1): a group row gets the SAME generated-token
// treatment a client row gets — present in the table, keyed by the
// kind-namespaced scope string — so ConstantTimeCompareToken (already
// keyed on the opaque scope string) is kind-agnostic. Phase 4a builds NO
// real bearer-key auth; the row's presence IS the seam.
//
// Because group scope keys carry the "g:" prefix and client keys are
// bare, the two never collide in tbl.Tokens by construction — passing a
// group key here can never overwrite a client row and vice-versa.
//
// Existing rows are never rotated (same contract as EnsureHubTokens).
func EnsureGroupTokens(groupKeys []string) (HubTokenTable, error) {
	lk, err := acquireHubMcpLock()
	if err != nil {
		return HubTokenTable{}, err
	}
	defer func() { _ = lk.Unlock() }()
	return ensureHubTokensLocked(groupKeys)
}

// pruneHubTokensLocked removes the token row for each supplied scope key
// (e.g. the "g:<group>" key of a just-deleted group), rewrites the table,
// and republishes the live snapshot. Caller MUST already hold hub-mcp.lock
// (called from ReadModifyWriteGroups under its held lock). Kind-agnostic
// like ensureHubTokensLocked: it deletes whatever keys it is handed.
//
// A missing token file is treated as "nothing to prune" (no rows exist), not
// an error — the same additive-by-omission posture the ensure path takes. A
// key not present in the table is a no-op. Only a genuine load/write failure
// (corrupt file, DACL gate) surfaces.
func pruneHubTokensLocked(scopeKeys []string) error {
	tbl, err := loadHubTokensLocked()
	if err != nil {
		if isHubMcpStateMissingErr(err) {
			// No token file ⇒ no rows to prune.
			return nil
		}
		return err
	}
	if tbl.Tokens == nil {
		return nil
	}
	changed := false
	for _, k := range scopeKeys {
		if _, ok := tbl.Tokens[k]; ok {
			delete(tbl.Tokens, k)
			changed = true
		}
	}
	if !changed {
		return nil
	}
	if werr := writeHubTokensLocked(tbl); werr != nil {
		return werr
	}
	publishTokenTable(tbl)
	return nil
}

func loadGroupTokenOrphansLocked() (groupTokenOrphanTable, error) {
	raw, err := readHubMcpStateFile(hubMcpGroupTokenOrphansFileLeaf)
	if err != nil {
		if isHubMcpStateMissingErr(err) {
			return groupTokenOrphanTable{Orphans: map[string]bool{}}, nil
		}
		return groupTokenOrphanTable{}, err
	}
	var tbl groupTokenOrphanTable
	if uerr := json.Unmarshal(raw, &tbl); uerr != nil {
		return groupTokenOrphanTable{}, fmt.Errorf("hub-mcp group-token orphan tombstones corrupt: %w", uerr)
	}
	if tbl.Orphans == nil {
		tbl.Orphans = map[string]bool{}
	}
	return tbl, nil
}

func writeGroupTokenOrphansLocked(tbl groupTokenOrphanTable) error {
	if tbl.Orphans == nil {
		tbl.Orphans = map[string]bool{}
	}
	payload, err := json.Marshal(tbl)
	if err != nil {
		return fmt.Errorf("marshal group-token orphan tombstones: %w", err)
	}
	return writeHubMcpStateFile(hubMcpGroupTokenOrphansFileLeaf, payload)
}

func markGroupTokenOrphansLocked(scopeKeys []string) error {
	if len(scopeKeys) == 0 {
		return nil
	}
	tbl, err := loadGroupTokenOrphansLocked()
	if err != nil {
		return err
	}
	changed := false
	for _, key := range scopeKeys {
		if key == "" || tbl.Orphans[key] {
			continue
		}
		tbl.Orphans[key] = true
		changed = true
	}
	if !changed {
		return nil
	}
	return writeGroupTokenOrphansLocked(tbl)
}

func clearGroupTokenOrphansLocked(scopeKeys []string) error {
	if len(scopeKeys) == 0 {
		return nil
	}
	tbl, err := loadGroupTokenOrphansLocked()
	if err != nil {
		return err
	}
	changed := false
	for _, key := range scopeKeys {
		if tbl.Orphans[key] {
			delete(tbl.Orphans, key)
			changed = true
		}
	}
	if !changed {
		return nil
	}
	return writeGroupTokenOrphansLocked(tbl)
}

// rotateReusedGroupTokensLocked mints a FRESH token for every declared
// group scope key that has a pre-existing ORPHANED token row. Caller MUST
// already hold hub-mcp.lock (called from
// PublishGroupsSnapshotLocked under its held lock).
//
// Why this exists (the "stale token reused on group re-create" defect):
// ensureHubTokensLocked NEVER rotates an existing row (its contract is
// "install a new group/client without invalidating existing ones"). When
// a group delete hits ErrTokenPruneFailed, its "g:<name>" row is
// intentionally LEFT in the table (the prune failed; restart_required is
// forced). If the operator later RE-CREATES a group of the same name,
// the plain ensure path REUSES that stale (possibly leaked/compromised)
// row instead of minting a fresh secret. This helper closes that hole by
// rotating exactly the orphaned-row case.
//
// The durable discriminator is hub-mcp-group-token-orphans.json: a delete
// writes that tombstone if groups.yaml committed but pruning the token row
// failed. That marker wins over both a stale previous resolver snapshot and
// a nil first-publish snapshot. `activeKeys` is only the live-session safety
// set for the already-published process state; when rotateUntracked is true,
// a declared row that is absent from that known-live set is also rotated for
// the original in-process recreate-over-orphan case. A declared key with NO
// existing row is left to the subsequent ensureHubTokensLocked (which mints a
// fresh one anyway) and any stale tombstone for it is cleared.
//
// Returns true when at least one row was rotated (the table was written +
// republished). A load/write failure surfaces; a missing token file is
// "nothing to rotate" (the recreate path then mints fresh via ensure).
func rotateReusedGroupTokensLocked(declaredKeys []string, activeKeys map[string]bool, rotateUntracked bool) (bool, error) {
	if len(declaredKeys) == 0 {
		return false, nil
	}
	orphans, err := loadGroupTokenOrphansLocked()
	if err != nil {
		return false, err
	}
	tbl, err := loadHubTokensLocked()
	if err != nil {
		if isHubMcpStateMissingErr(err) {
			return false, nil
		}
		return false, err
	}
	if tbl.Tokens == nil {
		return false, nil
	}
	rotated := false
	clearOrphans := make([]string, 0)
	for _, k := range declaredKeys {
		tombstoned := orphans.Orphans[k]
		if _, present := tbl.Tokens[k]; !present {
			if tombstoned {
				clearOrphans = append(clearOrphans, k)
			}
			continue // no row yet — ensure mints a fresh one, nothing stale to rotate.
		}
		if !tombstoned && (!rotateUntracked || activeKeys[k]) {
			continue // still-active group — keep its token so live sessions survive.
		}
		// Orphaned row being (re)created over: mint a fresh secret.
		fresh, gerr := generateHexToken()
		if gerr != nil {
			return rotated, gerr
		}
		tbl.Tokens[k] = fresh
		rotated = true
		if tombstoned {
			clearOrphans = append(clearOrphans, k)
		}
	}
	if !rotated {
		if err := clearGroupTokenOrphansLocked(clearOrphans); err != nil {
			return false, fmt.Errorf("clear group-token orphan tombstones: %w", err)
		}
		return false, nil
	}
	if werr := writeHubTokensLocked(tbl); werr != nil {
		return false, werr
	}
	if err := clearGroupTokenOrphansLocked(clearOrphans); err != nil {
		return false, fmt.Errorf("clear group-token orphan tombstones: %w", err)
	}
	publishTokenTable(tbl)
	return true, nil
}

// ensureHubTokensLocked is the in-flock half. Caller MUST already
// hold hub-mcp.lock. The supplied list is a set of SCOPE KEYS — bare
// client ids from EnsureHubTokens, or kind-namespaced "g:<group>" keys
// from EnsureGroupTokens. The function is kind-agnostic: it generates a
// token for every key not already present and preserves existing rows.
func ensureHubTokensLocked(clients []string) (HubTokenTable, error) {
	tbl, err := loadHubTokensLocked()
	if err != nil && !isHubMcpStateMissingErr(err) {
		// Corrupt / DACL-bad file is NOT silently regenerated — same
		// rule as endpoint file (spec §"Bind ordering" step 4).
		return HubTokenTable{}, err
	}
	if tbl.Tokens == nil {
		tbl.Tokens = map[string]string{}
	}
	// (codex bot r4 P2 closure was originally implemented here as a
	// per-call validation loop; r5 P2 closure moved the gate into
	// loadHubTokensLocked so EVERY consumer of the load path —
	// EnsureHubTokens, RotateHubToken, ReloadHubTokens — gets the
	// same corruption-reject behavior. The check above is now dead
	// here.)
	changed := false
	for _, c := range clients {
		if c == "" {
			continue
		}
		if _, ok := tbl.Tokens[c]; ok {
			continue
		}
		fresh, gerr := generateHexToken()
		if gerr != nil {
			return HubTokenTable{}, gerr
		}
		tbl.Tokens[c] = fresh
		changed = true
	}
	if changed {
		if werr := writeHubTokensLocked(tbl); werr != nil {
			return HubTokenTable{}, werr
		}
	}
	publishTokenTable(tbl)
	return tbl, nil
}

// RotateHubToken regenerates one client's token + atomically rewrites
// the file. Called by `mcphub hub-mcp regenerate-token --client X`
// (Phase 5 CLI). The CLI holds hub-mcp.lock continuously across the
// rotate-then-publish steps so a concurrent Ensure cannot interleave
// (codex r5 MED — spec §"Token-table reload on rotation").
//
// If `client` is not yet in the table (operator running rotate before
// install completes), an entry is created with a fresh token rather
// than failing. Callers can always rely on the returned snapshot
// having an entry for `client`.
func RotateHubToken(client string) (HubTokenTable, error) {
	if client == "" {
		return HubTokenTable{}, fmt.Errorf("RotateHubToken: empty client")
	}
	lk, err := acquireHubMcpLock()
	if err != nil {
		return HubTokenTable{}, err
	}
	defer func() { _ = lk.Unlock() }()

	tbl, err := loadHubTokensLocked()
	if err != nil && !isHubMcpStateMissingErr(err) {
		return HubTokenTable{}, err
	}
	if tbl.Tokens == nil {
		tbl.Tokens = map[string]string{}
	}
	fresh, gerr := generateHexToken()
	if gerr != nil {
		return HubTokenTable{}, gerr
	}
	tbl.Tokens[client] = fresh
	if werr := writeHubTokensLocked(tbl); werr != nil {
		return HubTokenTable{}, werr
	}
	publishTokenTable(tbl)
	return tbl, nil
}

// ReloadHubTokens re-reads <state-dir>/hub-mcp-tokens.json and
// republishes the snapshot. Used by the Phase 4
// /internal/reload-tokens HTTP handler so a sibling-process rotation
// becomes visible to the live auth gate within milliseconds.
//
// ReloadHubTokens does NOT acquire hub-mcp.lock. The Phase 4 handler
// uses its own `reloadMutex sync.Mutex` to serialize swaps + enforce
// the 5s cooldown (spec §"Control endpoint contract"). Acquiring
// hub-mcp.lock here would deadlock with the CLI's outstanding flock
// during a rotate-then-reload sequence.
func ReloadHubTokens() (HubTokenTable, error) {
	tbl, err := loadHubTokensLocked()
	if err != nil {
		return HubTokenTable{}, err
	}
	if tbl.Tokens == nil {
		tbl.Tokens = map[string]string{}
	}
	publishTokenTable(tbl)
	return tbl, nil
}

// CurrentTokenTable returns a DEEP COPY of the live snapshot for the
// auth gate (Phase 4). Returns a zero table if no Ensure / Rotate /
// Reload has run yet. Callers should treat the absence of a client
// entry as "401 / unknown client" — never as "skip auth".
//
// The returned struct AND its inner `Tokens` map are independent of
// the published snapshot. Mutating the returned map cannot:
//   - corrupt the live snapshot used by other goroutines
//   - race with `ConstantTimeCompareToken` reads (which share the
//     same published map pointer) and trigger a Go runtime
//     "concurrent map read and map write" panic
//
// codex bot r1 P1 closure (PR #156): earlier `return *p` shallow-
// copied the struct but the map field was still a reference to the
// published map. A caller writing to the returned `Tokens` would
// silently mutate the live auth snapshot.
func CurrentTokenTable() HubTokenTable {
	p := liveTokenTable.Load()
	if p == nil {
		return HubTokenTable{}
	}
	cpy := HubTokenTable{Tokens: make(map[string]string, len(p.Tokens))}
	for k, v := range p.Tokens {
		cpy.Tokens[k] = v
	}
	return cpy
}

// ConstantTimeCompareToken returns 1 iff `headerToken` matches the
// stored token for `client`. The comparison is:
//
//  1. Shape gate: both stored and header MUST be 64 bytes. A mismatched
//     length returns 0 WITHOUT inspecting the bytes (avoids leaking
//     the stored length via timing).
//  2. subtle.ConstantTimeCompare on the 64-byte slices.
//
// Returns 0 (no-match) when `client` is unknown — callers should
// treat that as a 401 with no body / no header leak (spec §"Logging
// hygiene": even an audit log of "client X tried to auth" leaks the
// client roster to anyone who can read the log; the Phase 4 emitter
// pipes the auth-fail line through RedactToken regardless).
func ConstantTimeCompareToken(client, headerToken string) int {
	if len(headerToken) != 64 {
		return 0
	}
	tbl := CurrentTokenTable()
	stored, ok := tbl.Tokens[client]
	if !ok {
		return 0
	}
	if len(stored) != 64 {
		return 0
	}
	return subtle.ConstantTimeCompare([]byte(stored), []byte(headerToken))
}

// isValidHexToken returns true iff `s` is exactly 64 hex characters
// (either case). Used by ensureHubTokensLocked to reject corrupted
// pre-existing entries before they leak into the live snapshot.
// Mirrors the constant-time compare's length gate semantics so a
// token that passes isValidHexToken can also be compared via
// ConstantTimeCompare in O(64) without truncation.
func isValidHexToken(s string) bool {
	if len(s) != 64 {
		return false
	}
	for i := 0; i < 64; i++ {
		c := s[i]
		switch {
		case c >= '0' && c <= '9':
		case c >= 'a' && c <= 'f':
		case c >= 'A' && c <= 'F':
		default:
			return false
		}
	}
	return true
}

// loadHubTokensLocked reads + parses the tokens file. Returns a zero
// table + error when the file is missing OR fails the
// VerifyHubMcpStateDACL gate. Callers MUST already hold hub-mcp.lock
// when composing read-modify-write semantics.
func loadHubTokensLocked() (HubTokenTable, error) {
	raw, err := readHubMcpStateFile(hubMcpTokensFileLeaf)
	if err != nil {
		return HubTokenTable{}, err
	}
	var tbl HubTokenTable
	if uerr := json.Unmarshal(raw, &tbl); uerr != nil {
		return HubTokenTable{}, fmt.Errorf("hub-mcp-tokens.json corrupt: %w", uerr)
	}
	if tbl.Tokens == nil {
		tbl.Tokens = map[string]string{}
	}
	// codex bot r5 P2 closure: validate every entry at load time so
	// every consumer (EnsureHubTokens, RotateHubToken, ReloadHubTokens)
	// fails closed on semantic corruption rather than publishing a bad
	// entry that ConstantTimeCompareToken would then deny on every
	// request (permanent 401 with no actionable error). Centralizing
	// the gate here removes the need for each call site to repeat the
	// check.
	for client, tok := range tbl.Tokens {
		if !isValidHexToken(tok) {
			return HubTokenTable{}, fmt.Errorf("hub-mcp tokens corruption: client %q has malformed token (got %d bytes, want 64 hex); use `mcphub hub-mcp regenerate-token --client %s` to explicitly rotate, or restore the file", client, len(tok), client)
		}
	}
	return tbl, nil
}

// writeHubTokensLocked serializes the table + delegates to
// writeHubMcpStateFile (which routes through SecureWriteClientConfig).
// Caller MUST already hold hub-mcp.lock.
func writeHubTokensLocked(tbl HubTokenTable) error {
	payload, err := json.Marshal(tbl)
	if err != nil {
		return fmt.Errorf("marshal hub-mcp tokens: %w", err)
	}
	return writeHubMcpStateFile(hubMcpTokensFileLeaf, payload)
}

// publishTokenTable copies tbl + swaps the atomic.Pointer. The copy is
// deliberate so the published snapshot is independent of any
// subsequent mutation the writer makes to its local map.
func publishTokenTable(tbl HubTokenTable) {
	cpy := HubTokenTable{Tokens: make(map[string]string, len(tbl.Tokens))}
	for k, v := range tbl.Tokens {
		cpy.Tokens[k] = v
	}
	liveTokenTable.Store(&cpy)
}

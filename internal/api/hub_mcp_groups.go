// hub_mcp_groups.go — groups/namespaces Phase 4a (DATA layer).
//
// A "group" is a named set of MCP servers exposed as a scoped tool
// surface, the organizing primitive the competitor-parity keystone
// (decision work-items/decisions/2026-06-18-groups-namespaces-tool-
// visibility.md) builds on. Phase 4a ships ONLY the data layer:
//
//   - groups.yaml in the state dir — the durable, operator-owned config
//     that names each group and its member servers.
//   - kind-namespaced merge into the published ResolverSnapshot — a
//     group's scope key is "g:<group>" (GroupScopeKeyPrefix), a disjoint
//     subspace from the bare-<client> keys the manifest path produces, so
//     a group and a client of the same name CANNOT collide in the shared
//     Bindings / Tokens maps by construction (operator decision 2).
//   - a per-group hub-token row (loopback-only, no real bearer key) — the
//     §D auth seam present but inert (operator decision 1).
//
// Phase 4a is provably INERT for every existing client / gate-OFF path:
// no new route, no handler/initialize change. A group binding is read by
// nobody until Phase 4b mounts the /g/<group>/mcp route. With groups.yaml
// absent the snapshot + token table are byte-identical to before.
//
// The per-tool `tools_hidden` filter named in the decision body is
// Phase 5; the schema field is reserved here (parsed, validated as
// optional) but NO filtering is implemented.
//
// Ownership: groups.yaml is a single-owner state-dir artifact written
// through the SAME hardened pipeline (writeHubMcpStateFile →
// SecureWriteClientConfig, flock via acquireHubMcpLock, DACL gate) that
// hub-mcp-tokens.json uses. It is NOT a manifest concept (manifests are
// per-server; a group spans servers).
//
// Spec: groups/namespaces decision §"Config model" + §"DECISION (2026-06-18)".

package api

import (
	"bytes"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// ErrTokenPruneFailed is the sentinel ReadModifyWriteGroups wraps when the
// groups.yaml write SUCCEEDED (the config is durably persisted) but pruning
// the deleted group's "g:<name>" hub-token row afterward failed. It is a
// DISTINCT error class from a genuine load/write failure so the GUI delete
// path (groups.go::groupsDelete) can treat it correctly: the durable delete
// landed, so the route must STILL be republished (dropping the group from the
// snapshot so isKnownGroup → 404), with restart_required flagged to cover the
// stale token row — NOT a 500 that skips the republish and strands a routable
// deleted group. Detect with errors.Is.
var ErrTokenPruneFailed = errors.New("groups.yaml written, but pruning the deleted group's token row failed")

// hubMcpGroupsFileLeaf is the canonical state-file basename for the
// groups config. validateStateFileName accepts it (single component, no
// traversal). Kept as a named constant so future consumers reference one
// literal.
const hubMcpGroupsFileLeaf = "groups.yaml"

// GroupScopeKeyPrefix is the kind namespace for a group's scope key in
// the shared Bindings / Tokens maps. A group named "frontend" gets the
// scope key "g:frontend"; a client named "frontend" stays the bare
// "frontend". The two live in disjoint kind-prefixed subspaces so they
// can never collide (operator decision 2). The ':' separator is also the
// character a group name is forbidden to contain (see validateGroupName)
// so a group name can never forge this prefix.
const GroupScopeKeyPrefix = "g:"

// groupNameSeparator is the character that namespaces a group scope key.
// A group name MUST NOT contain it (validateGroupName) so a member name
// can never inject a kind prefix into the scope key.
const groupNameSeparator = ":"

// Group is one named set of servers. Member names are server names
// (ServerManifest.Name); each named server contributes all of its
// daemons to the group's binding set at snapshot-build time.
type Group struct {
	// Name is the group's stable identifier. Its scope key is
	// GroupScopeKeyPrefix+Name. Validated by validateGroupName.
	Name string `yaml:"name"`

	// Description is free-form human-readable metadata, surfaced in a
	// future Groups GUI screen. Optional; no shape constraint.
	Description string `yaml:"description,omitempty"`

	// Servers names the member servers (ServerManifest.Name). The group
	// binds to every daemon of each named server. A named server with no
	// live manifest/daemon degrades to a skipped binding (never a fault)
	// at resolve time — see BuildResolverSnapshotFromManifestsAndGroups.
	Servers []string `yaml:"servers"`

	// ToolsHidden is RESERVED for Phase 5 per-tool visibility filtering.
	// Parsed + carried so an operator can author it forward-compatibly,
	// but Phase 4a implements NO filtering. Keyed by server name → the
	// raw (un-namespaced) tool names hidden for that server.
	ToolsHidden map[string][]string `yaml:"tools_hidden,omitempty"`

	// ProjectPath binds this group to ONE project (per-project-GUI P3c, design
	// decision §10.1). It is the project's CanonicalProjectKey (the SAME join key
	// the /api/projects aggregate composes A+B+C on). It is purely ADDITIVE and
	// DATA-ONLY:
	//
	//   - "" (the absent/zero form for every pre-P3c group) means UNBOUND /
	//     GLOBAL — the group is visible in EVERY project lens. groups.yaml version
	//     stays 1; an existing group with no project_path key parses with
	//     ProjectPath=="" and keeps its global meaning byte-for-byte.
	//   - a non-empty value binds the group to that project: the /api/projects
	//     read filter (the SINGLE backend-side predicate owner) shows the group
	//     ONLY in that project's lens (plus it always shows the unbound globals).
	//
	// It is NOT the scope key and NOT a route segment (§5/T3): the group's
	// "g:<name>" scope key, the /g/<name>/mcp route, the per-group token row, and
	// the snapshot bindings are ALL unchanged by project_path —
	// BuildResolverSnapshotFromManifestsAndGroups never reads this field, so a
	// bind/unbind changes neither routing nor membership, only the project-lens
	// read filter. The write owner normalizes it via clients.CanonicalProjectKey
	// before persisting (the binding handler); a stored value is therefore always
	// a canonical key, never a raw operator path.
	//
	// KnownFields(true) means an OLDER binary hard-fails on a NEWER groups.yaml
	// carrying project_path — accepted per §5 (groups.yaml is local state; a
	// fail-closed unknown-key error is safe and documented).
	ProjectPath string `yaml:"project_path,omitempty"`
}

// GroupsConfig is the on-disk shape of groups.yaml plus the in-memory
// parsed type. Version is a forward-compat discriminator (v1).
type GroupsConfig struct {
	Version int     `yaml:"version"`
	Groups  []Group `yaml:"groups"`
}

// GroupScopeKey returns the kind-namespaced scope key for a group name.
// This is the ONLY place the "g:"+name composition lives, so the prefix
// invariant has a single owner.
func GroupScopeKey(group string) string {
	return GroupScopeKeyPrefix + group
}

// groupNameAllowed is the ALLOWLIST a group name must fully match to be a
// safe single URL path segment in the `/g/<group>/mcp` route. It is an
// allowlist, NOT a denylist, on purpose: a denylist of "route-unsafe"
// characters is unclosable — it kept missing '#' (a name like "ops#prod"
// makes the server see only the "/g/ops" path, the rest is a URL fragment),
// '%' (percent-encoding), and other separators http.ServeMux normalizes. The
// allowlist closes the WHOLE class by admitting only ASCII letters, digits,
// '.', '_', and '-' — every one of which survives a path segment verbatim.
// (The exact names "." and ".." match this charset but are path-traversal
// segments ServeMux rewrites via redirects, so validateGroupName rejects them
// separately below.)
var groupNameAllowed = regexp.MustCompile(`^[A-Za-z0-9._-]+$`)

// validateGroupName enforces two invariants:
//
//  1. operator-decision-2: a group name is non-empty and contains no ':' so
//     it cannot forge a kind prefix in the shared scope keyspace. (Reserved-
//     name collision against client names is MOOT given kind-namespacing — a
//     group key and a client key live in disjoint subspaces — so no name-
//     equality gate is needed; the only keyspace requirement is that the name
//     cannot inject the separator.)
//  2. route-reachability via an ALLOWLIST: the name must match
//     groupNameAllowed (`^[A-Za-z0-9._-]+$`), the set of characters that
//     survive a `/g/<group>/mcp` URL path segment verbatim. This is an
//     allowlist, not a denylist, because a denylist of "unsafe" characters is
//     unclosable — it leaked '#', '%', and other separators http.ServeMux
//     normalizes, any of which would make a persisted group UNREACHABLE by its
//     own route. The exact names "." and ".." match the charset but are
//     path-traversal segments ServeMux rewrites, so they are rejected
//     separately. The validator (the persistence gate) refuses a bad name up
//     front rather than letting a write land a dead group.
func validateGroupName(name string) error {
	if name == "" {
		return fmt.Errorf("group name is empty")
	}
	if strings.Contains(name, groupNameSeparator) {
		return fmt.Errorf("group name %q contains the reserved %q separator (group scope keys are namespaced as %s<name>; a name with %q could forge a kind prefix)", name, groupNameSeparator, GroupScopeKeyPrefix, groupNameSeparator)
	}
	if !groupNameAllowed.MatchString(name) {
		i := strings.IndexFunc(name, func(r rune) bool {
			return !(r >= 'A' && r <= 'Z' || r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '.' || r == '_' || r == '-')
		})
		bad := name
		if i >= 0 {
			bad = string([]rune(name[i:])[0])
		}
		return fmt.Errorf("group name %q contains a route-unsafe character %q (a group name may contain only ASCII letters, digits, '.', '_', and '-'; it must be reachable as the %s<name>%s route segment)", name, bad, HubGroupPrefix, HubPathSuffix)
	}
	if name == "." || name == ".." {
		return fmt.Errorf("group name %q is a path-traversal segment (a name of %q or %q is rewritten by the route mux and could never reach the %s<name>%s route)", name, ".", "..", HubGroupPrefix, HubPathSuffix)
	}
	// C5-length (consultant): cap the name length. A group name is a single
	// URL path segment; 64 chars is a generous sanity bound that keeps the
	// /g/<name>/mcp route + the "g:<name>" scope key well within any
	// reasonable limit and stops an unbounded operator typo from bloating the
	// token table / snapshot keyspace. len() (bytes) is the right measure for
	// a URL segment; the allowlist already restricts to single-byte ASCII, so
	// byte length == rune length here.
	if len(name) > maxGroupNameLen {
		return fmt.Errorf("group name %q is too long (%d characters; the maximum is %d)", name, len(name), maxGroupNameLen)
	}
	return nil
}

// maxGroupNameLen bounds a group name's length. A group name is one URL path
// segment in /g/<name>/mcp and one scope-key suffix in "g:<name>"; 64 chars is
// a generous sanity cap.
const maxGroupNameLen = 64

// ValidateGroupName is the exported wrapper over the single-owner
// validateGroupName, for the AUTHORING boundary (the GUI /api/groups
// handler, groups Phase 5b-1) to reject a bad group name with a precise
// error BEFORE the read-modify-write of the full groups set — rather than
// discovering it inside WriteGroups after the merge. It delegates verbatim
// so the invariant stays owned in one place (no duplicated rule).
func ValidateGroupName(name string) error {
	return validateGroupName(name)
}

// checkGroupNamesUnique is the SINGLE OWNER of the group-name uniqueness
// invariant (C5-case). Uniqueness is CASE-INSENSITIVE: "Frontend" and
// "frontend" are the same group as far as authoring is concerned, so a
// second row whose name case-folds onto an earlier one is rejected. The
// FIRST collision is returned (groups[i] indexed so the parse path can
// prefix "group[i]:"). Both dedup sites — the parse/Load path
// (parseGroupsConfig) and the create/write path (writeGroupsLocked) —
// route through THIS helper so the rule lives in one place and can never
// drift between the two.
//
// Only the COMPARISON case-folds (strings.ToLower); the operator's chosen
// CASING is preserved verbatim in the stored Name (this helper does not
// mutate cfg). Routing — the /g/<group>/mcp path, the "g:<name>" scope
// key, and the token lookup — stays case-SENSITIVE on the stored casing;
// case-insensitive uniqueness guarantees the operator's exact name is the
// only one that exists, so the exact name they created always resolves.
func checkGroupNamesUnique(groups []Group) (idx int, err error) {
	seen := make(map[string]int, len(groups))
	for i := range groups {
		key := strings.ToLower(groups[i].Name)
		if prior, dup := seen[key]; dup {
			return i, fmt.Errorf("duplicate group name %q (case-insensitive: collides with %q)", groups[i].Name, groups[prior].Name)
		}
		seen[key] = i
	}
	return -1, nil
}

// parseGroupsConfig decodes + validates groups.yaml bytes. Unknown YAML
// keys are rejected (KnownFields(true)) so a typo'd field surfaces rather
// than silently dropping config. Every group name is validated; the FIRST
// invalid name is a hard parse error (a bad name could corrupt the scope
// keyspace, so it must not reach the snapshot). Duplicate group names are
// rejected (two rows producing the same scope key is an authoring error).
//
// An empty / whitespace-only input yields a zero GroupsConfig with no
// groups and no error — "no groups configured" is a valid state, not a
// corruption.
func parseGroupsConfig(raw []byte) (GroupsConfig, error) {
	var cfg GroupsConfig
	if len(bytes.TrimSpace(raw)) == 0 {
		return cfg, nil
	}
	dec := yaml.NewDecoder(bytes.NewReader(raw))
	dec.KnownFields(true)
	if err := dec.Decode(&cfg); err != nil {
		return GroupsConfig{}, fmt.Errorf("groups.yaml decode: %w", err)
	}
	// C1 (consultant — cheapest paint-into-corner): reject an unknown schema
	// version up front. With KnownFields(true) a future v2 that adds a field
	// would hard-break an OLD binary anyway (unknown-key decode error), but a
	// v2 that only CHANGES the MEANING of an existing field would be silently
	// misread as v1. version 0 is the absent/default form (writeGroupsLocked
	// stamps it to 1); version 1 is this binary's schema. Anything else is a
	// config written by a newer binary this one cannot safely interpret.
	if cfg.Version != 0 && cfg.Version != 1 {
		return GroupsConfig{}, fmt.Errorf("groups.yaml: unsupported version %d (this binary supports version 1; upgrade mcphub to read a newer groups.yaml)", cfg.Version)
	}
	for i := range cfg.Groups {
		if err := validateGroupName(cfg.Groups[i].Name); err != nil {
			return GroupsConfig{}, fmt.Errorf("groups.yaml group[%d]: %w", i, err)
		}
	}
	// Case-insensitive uniqueness via the single owner (C5-case); a row whose
	// name case-folds onto an earlier one is rejected.
	if i, err := checkGroupNamesUnique(cfg.Groups); err != nil {
		return GroupsConfig{}, fmt.Errorf("groups.yaml group[%d]: %w", i, err)
	}
	return cfg, nil
}

// LoadGroups reads + parses <state-dir>/groups.yaml. A MISSING file is
// NOT an error — it returns an empty GroupsConfig (additive-by-omission:
// no groups.yaml ⇒ today's behavior exactly). Any other read error
// (DACL gate, partial read) or a parse/validation failure surfaces so a
// corrupt file is not silently treated as "no groups".
//
// LoadGroups does NOT hold hub-mcp.lock; it is the read-only snapshot the
// SERVE path and read-only GET surfaces use. A load→mutate→write sequence
// MUST use ReadModifyWriteGroups instead so the whole transition is atomic
// under one held lock (otherwise two concurrent writers lost-update).
func LoadGroups() (GroupsConfig, error) {
	return loadGroupsLocked()
}

// loadGroupsLocked is the lock-agnostic groups reader. It does not acquire
// hub-mcp.lock itself, so it is callable BOTH from the lock-free LoadGroups
// and from inside ReadModifyWriteGroups (which already holds the lock). The
// helper performs no locking of its own; the "Locked" suffix follows the
// hub_mcp_tokens.go convention (loadHubTokensLocked) meaning "the in-flock
// half — caller owns the lock when one is required".
func loadGroupsLocked() (GroupsConfig, error) {
	raw, err := readHubMcpStateFile(hubMcpGroupsFileLeaf)
	if err != nil {
		if isHubMcpStateMissingErr(err) {
			return GroupsConfig{}, nil
		}
		return GroupsConfig{}, err
	}
	return parseGroupsConfig(raw)
}

// WriteGroups serializes + persists the groups config to
// <state-dir>/groups.yaml through the hardened state-file pipeline under
// hub-mcp.lock. Every group name is validated before any bytes hit disk
// so a bad config can never be persisted. Version defaults to 1 when
// unset.
//
// This is the single owning write path for groups.yaml. It mirrors
// writeHubTokensLocked's flock-then-secure-write discipline.
//
// CAUTION: WriteGroups acquires hub-mcp.lock for the write ONLY. A
// load→modify→write sequence built from a bare LoadGroups + WriteGroups is
// NOT atomic — two concurrent POSTs each read the same baseline, each append
// their row, and the second write clobbers the first (lost update). Authoring
// callers MUST use ReadModifyWriteGroups so the read and the write share one
// held lock. WriteGroups is retained for the (rare) whole-set replacement
// caller that already owns the merge.
func WriteGroups(cfg GroupsConfig) error {
	lk, err := acquireHubMcpLock()
	if err != nil {
		return err
	}
	defer func() { _ = lk.Unlock() }()
	return writeGroupsLocked(cfg)
}

// writeGroupsLocked is the in-flock half of WriteGroups. Caller MUST already
// hold hub-mcp.lock. Validates + marshals + persists through the hardened
// state-file pipeline. Mirrors writeHubTokensLocked.
func writeGroupsLocked(cfg GroupsConfig) error {
	if cfg.Version == 0 {
		cfg.Version = 1
	}
	for i := range cfg.Groups {
		if err := validateGroupName(cfg.Groups[i].Name); err != nil {
			return fmt.Errorf("group[%d]: %w", i, err)
		}
	}
	// Case-insensitive uniqueness via the single owner (C5-case): the same
	// rule the parse path applies, so the two can never drift.
	if i, err := checkGroupNamesUnique(cfg.Groups); err != nil {
		return fmt.Errorf("group[%d]: %w", i, err)
	}
	payload, err := yaml.Marshal(cfg)
	if err != nil {
		return fmt.Errorf("marshal groups.yaml: %w", err)
	}
	return writeHubMcpStateFile(hubMcpGroupsFileLeaf, payload)
}

// ReadModifyWriteGroups is the ATOMIC load→mutate→write transition for
// groups.yaml: it acquires hub-mcp.lock ONCE, reads the current config under
// that lock, hands the caller a mutable copy to edit in place, then writes
// the result back — all under the single held lock. This closes the
// concurrent-POST lost-update window a bare LoadGroups+WriteGroups pair
// leaves open (two writers reading the same baseline; the later write
// clobbering the earlier). Both the GUI POST (create-or-update) and DELETE
// paths route through it.
//
// The mutate callback returns the set of group names it DELETED (empty for a
// create-or-update). After a successful write, ReadModifyWriteGroups prunes
// the "g:<name>" hub-token row for each deleted group UNDER THE SAME HELD
// LOCK, so the token table never keeps a stale row for a removed group (the
// gate-2 isKnownGroup source is now the resolver snapshot, but the token row
// is the gate-4 auth seam — leaving it behind would let a re-created group of
// the same name silently inherit the old token). A token-prune failure is
// NON-fatal to the groups.yaml write (the durable config already landed and
// is the source of truth); it is returned so the caller can surface it, but
// the write is not rolled back.
//
// The callback MUST NOT call any helper that re-acquires hub-mcp.lock
// (LoadGroups, WriteGroups, EnsureGroupTokens, …) — doing so would deadlock
// against the lock this helper already holds. It edits the supplied *cfg
// directly.
func ReadModifyWriteGroups(mutate func(cfg *GroupsConfig) (deletedGroups []string, err error)) error {
	lk, err := acquireHubMcpLock()
	if err != nil {
		return err
	}
	defer func() { _ = lk.Unlock() }()

	cfg, err := loadGroupsLocked()
	if err != nil {
		return err
	}
	deleted, err := mutate(&cfg)
	if err != nil {
		return err
	}
	if err := writeGroupsLocked(cfg); err != nil {
		return err
	}
	// Prune the token row for each deleted group under the SAME held lock.
	// Non-fatal: the groups.yaml write already committed.
	if len(deleted) > 0 {
		keys := make([]string, 0, len(deleted))
		for _, name := range deleted {
			if validateGroupName(name) != nil {
				continue
			}
			keys = append(keys, GroupScopeKey(name))
		}
		if len(keys) > 0 {
			if perr := pruneHubTokensLocked(keys); perr != nil {
				// B3 (bot R3): the groups.yaml write already committed (the
				// config is the source of truth). A post-write token-prune
				// failure must NOT be conflated with a genuine write failure:
				// wrap it in the DISTINCT ErrTokenPruneFailed sentinel so the
				// caller still republishes the snapshot (dropping the deleted
				// group → isKnownGroup 404) instead of 500-ing and stranding a
				// routable deleted group. errors.Is(err, ErrTokenPruneFailed)
				// detects it; %w also preserves the underlying cause.
				return fmt.Errorf("%w: %v", ErrTokenPruneFailed, perr)
			}
		}
	}
	return nil
}

// GroupScopeKeys returns the sorted set of "g:<group>" scope keys for the
// supplied groups. Used to ensure a per-group token row exists for each
// group (the §D auth seam). Sorted for deterministic token-table
// iteration / test assertions. Invalid names are skipped defensively
// (LoadGroups already rejected them at parse time; this guards a
// programmatically-constructed config).
func GroupScopeKeys(groups []Group) []string {
	keys := make([]string, 0, len(groups))
	for _, g := range groups {
		if validateGroupName(g.Name) != nil {
			continue
		}
		keys = append(keys, GroupScopeKey(g.Name))
	}
	sort.Strings(keys)
	return keys
}

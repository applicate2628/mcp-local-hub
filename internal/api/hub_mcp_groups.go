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
	"fmt"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

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

// validateGroupName enforces the operator-decision-2 invariant: a group
// name is non-empty and contains no ':' so it cannot forge a kind prefix
// in the shared scope keyspace. (Reserved-name collision against client
// names is MOOT given kind-namespacing — a group key and a client key
// live in disjoint subspaces — so no name-equality gate is needed; the
// only correctness requirement is that the name cannot inject the
// separator.)
func validateGroupName(name string) error {
	if name == "" {
		return fmt.Errorf("group name is empty")
	}
	if strings.Contains(name, groupNameSeparator) {
		return fmt.Errorf("group name %q contains the reserved %q separator (group scope keys are namespaced as %s<name>; a name with %q could forge a kind prefix)", name, groupNameSeparator, GroupScopeKeyPrefix, groupNameSeparator)
	}
	return nil
}

// ValidateGroupName is the exported wrapper over the single-owner
// validateGroupName, for the AUTHORING boundary (the GUI /api/groups
// handler, groups Phase 5b-1) to reject a bad group name with a precise
// error BEFORE the read-modify-write of the full groups set — rather than
// discovering it inside WriteGroups after the merge. It delegates verbatim
// so the invariant stays owned in one place (no duplicated rule).
func ValidateGroupName(name string) error {
	return validateGroupName(name)
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
	seen := make(map[string]bool, len(cfg.Groups))
	for i := range cfg.Groups {
		name := cfg.Groups[i].Name
		if err := validateGroupName(name); err != nil {
			return GroupsConfig{}, fmt.Errorf("groups.yaml group[%d]: %w", i, err)
		}
		if seen[name] {
			return GroupsConfig{}, fmt.Errorf("groups.yaml: duplicate group name %q", name)
		}
		seen[name] = true
	}
	return cfg, nil
}

// LoadGroups reads + parses <state-dir>/groups.yaml. A MISSING file is
// NOT an error — it returns an empty GroupsConfig (additive-by-omission:
// no groups.yaml ⇒ today's behavior exactly). Any other read error
// (DACL gate, partial read) or a parse/validation failure surfaces so a
// corrupt file is not silently treated as "no groups".
func LoadGroups() (GroupsConfig, error) {
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
// This is the single owning write path for groups.yaml (a future Groups
// GUI screen calls it). It mirrors writeHubTokensLocked's
// flock-then-secure-write discipline.
func WriteGroups(cfg GroupsConfig) error {
	if cfg.Version == 0 {
		cfg.Version = 1
	}
	seen := make(map[string]bool, len(cfg.Groups))
	for i := range cfg.Groups {
		name := cfg.Groups[i].Name
		if err := validateGroupName(name); err != nil {
			return fmt.Errorf("group[%d]: %w", i, err)
		}
		if seen[name] {
			return fmt.Errorf("duplicate group name %q", name)
		}
		seen[name] = true
	}
	payload, err := yaml.Marshal(cfg)
	if err != nil {
		return fmt.Errorf("marshal groups.yaml: %w", err)
	}
	lk, err := acquireHubMcpLock()
	if err != nil {
		return err
	}
	defer func() { _ = lk.Unlock() }()
	return writeHubMcpStateFile(hubMcpGroupsFileLeaf, payload)
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

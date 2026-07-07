// internal/api/client_install_prefs.go
//
// Operator override for the default-install client set (§9 multi-agent
// table, redesign spec §9.2 / line 204). The compile-time default-install
// set lives in internal/clients (clients.DefaultInstallClientNames(),
// derived from each clientRegistry descriptor's defaultInstall flag) and
// is the {claude-code, codex-cli, cursor} fixed trio. This file lets an
// operator override that set from the GUI Settings → Clients panel without
// editing source: the chosen set is persisted to gui-preferences.yaml and
// becomes the effective default for installs that do NOT request an
// explicit --clients / ClientsInclude target.
//
// Persistence shape. gui-preferences.yaml is read by readRawSettingsMap as
// a flat map[string]string, so the override is stored as a SINGLE
// comma-separated scalar string under the key defaultInstallClientsKey —
// the same CSV encoding the GUI /api/install-all ?servers= filter already
// uses (internal/gui/install.go parseServersFilter). A YAML sequence value
// would break the flat-map unmarshal, so a scalar CSV is the only encoding
// that coexists with the existing settings reader/writer. The key is NOT a
// SettingsRegistry entry (the registry models only scalar enum/bool/int/
// string/path/action types with per-type validators; a client-name set is
// none of those), but it lives in the SAME file via the same atomic
// temp-then-rename write under settingsMu, so it round-trips losslessly
// alongside every appearance.* / gui_server.* key.
//
// Absent / empty key ⇒ fall back to clients.DefaultInstallClientNames()
// (the compile-time trio). This keeps the override purely additive: a host
// that has never used the panel installs exactly as before, and the
// plan-builder stays hermetic in tests that do not write the file.
package api

import (
	"fmt"
	"strings"

	"mcp-local-hub/internal/clients"
)

const (
	// defaultInstallClientsKey is the gui-preferences.yaml key under which the
	// operator's chosen default-install client set is stored as a
	// comma-separated scalar string. Absent ⇒ compile-time default trio.
	defaultInstallClientsKey = "clients.default_install"

	// lspRouterDisabledClientsKey is a narrow LSP-router opt-out list, not an
	// install target set. Absent/blank means every present client remains
	// eligible for EnsureLSPRouterClientEntries; a listed client is skipped by
	// future ensure runs, and RollbackLSPRouterClientEntriesForClient remains
	// the explicit removal path for any existing router entries.
	lspRouterDisabledClientsKey = "clients.lsp_router_disabled"
)

// DefaultInstallClientNamesOverride returns the persisted operator override
// for the default-install client set, or nil when no override is configured.
// The returned slice is in the operator's chosen order with unknown / blank /
// duplicate names dropped; it is validated against
// clients.SupportedClientNames(). A nil return (no key, or every persisted
// name was unknown/blank) signals the caller to use the compile-time
// default — it never silently returns an empty install set.
func (a *API) DefaultInstallClientNamesOverride() ([]string, error) {
	return a.DefaultInstallClientNamesOverrideIn(SettingsPath())
}

// DefaultInstallClientNamesOverrideIn is the tempdir-capable form (mirrors
// SettingsListIn / SettingsSetIn so tests point at a temp gui-preferences
// file via the path argument).
func (a *API) DefaultInstallClientNamesOverrideIn(path string) ([]string, error) {
	settingsMu.Lock()
	defer settingsMu.Unlock()
	raw, err := readRawSettingsMap(path)
	if err != nil {
		return nil, err
	}
	csv, ok := raw[defaultInstallClientsKey]
	if !ok {
		return nil, nil
	}
	names := sanitizeClientNames(splitClientCSV(csv))
	if len(names) == 0 {
		// Key present but no valid name survived (all blank/unknown) —
		// treat as "no override" so the caller falls back to the
		// compile-time default rather than installing zero clients.
		return nil, nil
	}
	return names, nil
}

// DefaultInstallClientNamesEffective resolves the effective default-install
// client set: the persisted operator override when present and non-empty,
// else clients.DefaultInstallClientNames() (the compile-time trio). This is
// the single owner of "what is the default-install set" once an operator
// override may exist. Plan-building callers pass the result into
// BuildPlanOpts.DefaultClientsOverride; the plan-builder itself never reads
// disk so direct BuildPlanWithOpts tests stay hermetic.
func (a *API) DefaultInstallClientNamesEffective() ([]string, error) {
	return a.DefaultInstallClientNamesEffectiveIn(SettingsPath())
}

// DefaultInstallClientNamesEffectiveIn is the tempdir-capable form.
func (a *API) DefaultInstallClientNamesEffectiveIn(path string) ([]string, error) {
	override, err := a.DefaultInstallClientNamesOverrideIn(path)
	if err != nil {
		return nil, err
	}
	if len(override) > 0 {
		return override, nil
	}
	return clients.DefaultInstallClientNames(), nil
}

// ClientInstallEnabled reports whether name is selected in the effective
// operator-managed client install set. This is the single-owner predicate for
// backend flows that need to honor the GUI client toggle without reading or
// interpreting gui-preferences.yaml themselves.
func (a *API) ClientInstallEnabled(name string) (bool, error) {
	return a.ClientInstallEnabledIn(SettingsPath(), name)
}

// ClientInstallEnabledIn is the tempdir-capable form.
func (a *API) ClientInstallEnabledIn(path, name string) (bool, error) {
	enabled, err := a.ClientInstallEnabledSetIn(path)
	if err != nil {
		return false, err
	}
	return enabled[strings.TrimSpace(name)], nil
}

// ClientInstallEnabledSet returns the effective operator-managed client set as
// a lookup map. An absent override falls back to clients.DefaultInstallClientNames().
func (a *API) ClientInstallEnabledSet() (map[string]bool, error) {
	return a.ClientInstallEnabledSetIn(SettingsPath())
}

// ClientInstallEnabledSetIn is the tempdir-capable form.
func (a *API) ClientInstallEnabledSetIn(path string) (map[string]bool, error) {
	names, err := a.DefaultInstallClientNamesEffectiveIn(path)
	if err != nil {
		return nil, err
	}
	enabled := make(map[string]bool, len(names))
	for _, name := range names {
		enabled[name] = true
	}
	return enabled, nil
}

// LSPRouterDisabledClientSet returns the explicit per-client LSP-router
// opt-out set. This is intentionally separate from clients.default_install:
// the default-install preference controls only installs that omit an explicit
// client target, while this list means "do not auto-maintain LSP router entries
// for this present client." Absent/blank/unknown-only config returns an empty
// set, preserving pre-opt-out behavior for every client.
func (a *API) LSPRouterDisabledClientSet() (map[string]bool, error) {
	return a.LSPRouterDisabledClientSetIn(SettingsPath())
}

// LSPRouterDisabledClientSetIn is the tempdir-capable form.
func (a *API) LSPRouterDisabledClientSetIn(path string) (map[string]bool, error) {
	settingsMu.Lock()
	defer settingsMu.Unlock()
	raw, err := readRawSettingsMap(path)
	if err != nil {
		return nil, err
	}
	names := sanitizeClientNames(splitClientCSV(raw[lspRouterDisabledClientsKey]))
	disabled := make(map[string]bool, len(names))
	for _, name := range names {
		disabled[name] = true
	}
	return disabled, nil
}

// SetDefaultInstallClientNames persists the operator's chosen default-install
// client set to gui-preferences.yaml. Every name must be a supported client
// (validated against clients.SupportedClientNames()); an unknown name is a
// hard error so a typo never silently shrinks the install set. Blank entries
// and duplicates are dropped. An EMPTY resulting set is rejected — the
// operator cannot persist "install no clients" (that is what unchecking every
// box would mean, and a zero-client install is never the intent); the GUI
// guards against it too, but the api layer is the authoritative gate.
func (a *API) SetDefaultInstallClientNames(names []string) error {
	return a.SetDefaultInstallClientNamesIn(SettingsPath(), names)
}

// SetDefaultInstallClientNamesIn is the tempdir-capable form. The
// read-modify-write uses the same gui-preferences.yaml locked mutator as
// SettingsSetIn, so the override coexists with every other key across
// goroutines and sibling processes.
func (a *API) SetDefaultInstallClientNamesIn(path string, names []string) error {
	supported := map[string]bool{}
	for _, n := range clients.SupportedClientNames() {
		supported[n] = true
	}
	cleaned := make([]string, 0, len(names))
	seen := map[string]bool{}
	for _, n := range names {
		trimmed := strings.TrimSpace(n)
		if trimmed == "" {
			continue
		}
		if !supported[trimmed] {
			return fmt.Errorf("unknown client %q (expected %s)", trimmed, strings.Join(clients.SupportedClientNames(), " | "))
		}
		if seen[trimmed] {
			continue
		}
		seen[trimmed] = true
		cleaned = append(cleaned, trimmed)
	}
	if len(cleaned) == 0 {
		return fmt.Errorf("default-install client set must name at least one supported client")
	}

	return mutateRawSettingsMapLocked(path, func(raw map[string]string) error {
		raw[defaultInstallClientsKey] = strings.Join(cleaned, ",")
		return nil
	})
}

// splitClientCSV splits a comma-separated scalar into trimmed, non-empty
// tokens. Empty / whitespace-only input yields nil. Mirrors the GUI
// parseServersFilter encoding so the wire and storage forms agree.
func splitClientCSV(raw string) []string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil
	}
	var out []string
	for _, part := range strings.Split(raw, ",") {
		if t := strings.TrimSpace(part); t != "" {
			out = append(out, t)
		}
	}
	return out
}

// sanitizeClientNames drops blank, duplicate, and unsupported names while
// preserving the first-seen order of the survivors. Used on READ so a
// hand-edited or stale gui-preferences.yaml that lists a client this build
// no longer understands degrades gracefully (the unknown name is ignored)
// instead of poisoning the install set. Returns nil when nothing survives.
func sanitizeClientNames(names []string) []string {
	supported := map[string]bool{}
	for _, n := range clients.SupportedClientNames() {
		supported[n] = true
	}
	out := make([]string, 0, len(names))
	seen := map[string]bool{}
	for _, n := range names {
		trimmed := strings.TrimSpace(n)
		if trimmed == "" || !supported[trimmed] || seen[trimmed] {
			continue
		}
		seen[trimmed] = true
		out = append(out, trimmed)
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// ClientInstallToggleRow describes one client in the GUI Clients panel: its
// stable id, whether the COMPILE-TIME registry marks it default-install,
// and whether it is in the currently-effective install set (override-aware).
// Exported (with exported fields) so the internal/gui handler can map it to
// its wire DTO directly.
type ClientInstallToggleRow struct {
	Name           string
	CompileDefault bool
	Selected       bool
}

// ClientInstallToggleSnapshot is the api-shaped view the GUI GET handler
// renders: every supported client with its compile-time-default and
// currently-selected flags, plus whether an explicit operator override is
// configured (vs. falling back to the compile-time trio). The handler maps
// this to its snake_case wire DTO.
type ClientInstallToggleSnapshot struct {
	Rows           []ClientInstallToggleRow
	OverrideActive bool
}

// ClientInstallToggleView builds the snapshot from the effective set. Order
// follows clients.SupportedClientNames() (registry order) so the panel
// renders the canonical default trio first, then the opt-ins, matching
// every other client-ordered surface.
func (a *API) ClientInstallToggleView() (ClientInstallToggleSnapshot, error) {
	return a.ClientInstallToggleViewIn(SettingsPath())
}

// ClientInstallToggleViewIn is the tempdir-capable form.
func (a *API) ClientInstallToggleViewIn(path string) (ClientInstallToggleSnapshot, error) {
	override, err := a.DefaultInstallClientNamesOverrideIn(path)
	if err != nil {
		return ClientInstallToggleSnapshot{}, err
	}
	overrideActive := len(override) > 0

	selected := override
	if !overrideActive {
		selected = clients.DefaultInstallClientNames()
	}
	selectedSet := map[string]bool{}
	for _, n := range selected {
		selectedSet[n] = true
	}

	compileDefault := map[string]bool{}
	for _, n := range clients.DefaultInstallClientNames() {
		compileDefault[n] = true
	}

	rows := make([]ClientInstallToggleRow, 0, len(clients.SupportedClientNames()))
	for _, name := range clients.SupportedClientNames() {
		rows = append(rows, ClientInstallToggleRow{
			Name:           name,
			CompileDefault: compileDefault[name],
			Selected:       selectedSet[name],
		})
	}
	return ClientInstallToggleSnapshot{Rows: rows, OverrideActive: overrideActive}, nil
}

package api

import "strconv"

// effectiveBackupKeepN returns the auto-prune retention count to apply
// when install or migrate writes a fresh timestamped client-config
// backup. Reads `backups.keep_n` from the user's preferences file
// (gui-preferences.yaml), falling back to the registry default when the
// file is missing, the key is unset, or the persisted value is invalid.
//
// Why this exists: backups are appended to disk on every install and
// every Apply-from-Servers/migrate run. Without auto-prune they grow
// without bound — a workspace with 4 clients × dozens of migrate cycles
// leaves hundreds of `.bak-mcp-local-hub-<ts>` files next to each
// config. The user-facing setting always existed (registry §appearance
// row `backups.keep_n`, default 5) but no caller ever consumed it on
// the write path, so the dial did nothing.
//
// The pristine `-original` sentinel is never affected — it lives
// outside the timestamped set the prune algorithm walks, so a full
// uninstall + rollback always has somewhere to land.
func (a *API) effectiveBackupKeepN() int {
	fallback := registryDefaultKeepN()
	s, err := a.SettingsGet("backups.keep_n")
	if err != nil || s == "" {
		return fallback
	}
	n, err := strconv.Atoi(s)
	if err != nil || n < 0 {
		return fallback
	}
	return n
}

// registryDefaultKeepN returns the integer interpretation of the
// registry default for `backups.keep_n`. Sourced from the registry so
// the schema stays the single source of truth — if a future spec rev
// changes the default, this helper picks up the change automatically.
// Unparseable / missing definitions fall back to 5, matching today's
// registry value.
func registryDefaultKeepN() int {
	def := findDef("backups.keep_n")
	if def == nil {
		return 5
	}
	n, err := strconv.Atoi(def.Default)
	if err != nil {
		return 5
	}
	return n
}

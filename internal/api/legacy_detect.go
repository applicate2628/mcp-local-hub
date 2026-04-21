// Package api — legacy mcp-language-server entry detection (M4 Task 14).
//
// Detection scans Codex (TOML) + Claude Code (JSON) client configs for
// DISABLED entries whose command is "mcp-language-server". Gemini CLI is
// intentionally skipped: the historical Phase 2-era stdio form for
// language servers pre-dates the Gemini HTTP-native schema and was not
// emitted into that client. Each match becomes one LegacyLSEntry the user
// can convert via `mcphub migrate-legacy`.
//
// Active entries are ignored — migration is an opt-in flow, and a
// currently-enabled entry means the user is still using the old shape.
package api

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/pelletier/go-toml/v2"
)

// LegacyLSEntry is one disabled mcp-language-server entry found in a client
// config. The user approves converting it via `mcphub migrate-legacy`.
type LegacyLSEntry struct {
	Client     string   `json:"client"`      // "codex-cli" or "claude-code"
	EntryName  string   `json:"entry_name"`  // name in the config (e.g. "python-lsp")
	Workspace  string   `json:"workspace"`   // parsed from --workspace arg
	Language   string   `json:"language"`    // inferred from --lsp binary
	LspCommand string   `json:"lsp_command"` // e.g. "pyright-langserver"
	RawArgs    []string `json:"raw_args"`
}

// DetectLegacyLanguageServerEntries scans every installed client config for
// DISABLED stdio entries whose command is "mcp-language-server". Each match
// becomes one LegacyLSEntry the caller can migrate. Enabled entries are
// ignored (the user is still using them — migration is a conscious opt-in).
//
// Returned entries are unordered; callers that need determinism should
// sort by (Client, EntryName) at the display layer.
func DetectLegacyLanguageServerEntries() ([]LegacyLSEntry, error) {
	var out []LegacyLSEntry
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, err
	}
	// Codex TOML — ~/.codex/config.toml
	codexPath := filepath.Join(home, ".codex", "config.toml")
	if raw, err := os.ReadFile(codexPath); err == nil {
		entries, err := extractLegacyFromCodexTOML(raw)
		if err != nil {
			return nil, fmt.Errorf("parse %s: %w", codexPath, err)
		}
		out = append(out, entries...)
	} else if !os.IsNotExist(err) {
		return nil, err
	}
	// Claude Code JSON — ~/.claude.json
	claudePath := filepath.Join(home, ".claude.json")
	if raw, err := os.ReadFile(claudePath); err == nil {
		entries, err := extractLegacyFromClaudeJSON(raw)
		if err != nil {
			return nil, fmt.Errorf("parse %s: %w", claudePath, err)
		}
		out = append(out, entries...)
	} else if !os.IsNotExist(err) {
		return nil, err
	}
	return out, nil
}

// extractLegacyFromCodexTOML parses a Codex config.toml body and returns
// every [mcp_servers.*] block whose command is "mcp-language-server" and
// whose enabled flag is NOT true (absent or false both qualify as disabled
// — Codex's convention is `enabled = false` for parked servers).
func extractLegacyFromCodexTOML(raw []byte) ([]LegacyLSEntry, error) {
	var m map[string]any
	if err := toml.Unmarshal(raw, &m); err != nil {
		return nil, err
	}
	servers, _ := m["mcp_servers"].(map[string]any)
	var out []LegacyLSEntry
	for name, v := range servers {
		entry, _ := v.(map[string]any)
		if entry == nil {
			continue
		}
		if cmd, _ := entry["command"].(string); cmd != "mcp-language-server" {
			continue
		}
		// Disabled gate: only collect when `enabled` is explicitly false or
		// absent. An entry with `enabled = true` is still in active use.
		if enabled, ok := entry["enabled"].(bool); ok && enabled {
			continue
		}
		args, _ := entry["args"].([]any)
		argsStr := make([]string, 0, len(args))
		for _, a := range args {
			if s, ok := a.(string); ok {
				argsStr = append(argsStr, s)
			}
		}
		ws, lsp := parseWorkspaceAndLsp(argsStr)
		out = append(out, LegacyLSEntry{
			Client:     "codex-cli",
			EntryName:  name,
			Workspace:  ws,
			Language:   languageFromLsp(lsp),
			LspCommand: lsp,
			RawArgs:    argsStr,
		})
	}
	return out, nil
}

// extractLegacyFromClaudeJSON parses ~/.claude.json and returns every
// mcpServers entry whose command is "mcp-language-server" and whose
// disabled flag is true. Claude Code's convention is `disabled = true`
// (inverse of Codex's `enabled`), so the default case (absent) means
// actively-used — ignored.
func extractLegacyFromClaudeJSON(raw []byte) ([]LegacyLSEntry, error) {
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, err
	}
	servers, _ := m["mcpServers"].(map[string]any)
	var out []LegacyLSEntry
	for name, v := range servers {
		entry, _ := v.(map[string]any)
		if entry == nil {
			continue
		}
		if cmd, _ := entry["command"].(string); cmd != "mcp-language-server" {
			continue
		}
		// Claude convention: `disabled = true` means parked. Absent or false
		// = active use → skip.
		disabled, _ := entry["disabled"].(bool)
		if !disabled {
			continue
		}
		args, _ := entry["args"].([]any)
		argsStr := make([]string, 0, len(args))
		for _, a := range args {
			if s, ok := a.(string); ok {
				argsStr = append(argsStr, s)
			}
		}
		ws, lsp := parseWorkspaceAndLsp(argsStr)
		out = append(out, LegacyLSEntry{
			Client:     "claude-code",
			EntryName:  name,
			Workspace:  ws,
			Language:   languageFromLsp(lsp),
			LspCommand: lsp,
			RawArgs:    argsStr,
		})
	}
	return out, nil
}

// parseWorkspaceAndLsp extracts --workspace and --lsp values from an args
// slice. Accepts both double-dash (`--workspace`) and single-dash
// (`-workspace`) forms so legacy configs written by older tools still
// parse.
func parseWorkspaceAndLsp(args []string) (workspace, lsp string) {
	for i := 0; i < len(args)-1; i++ {
		switch args[i] {
		case "--workspace", "-workspace":
			workspace = args[i+1]
		case "--lsp", "-lsp":
			lsp = args[i+1]
		}
	}
	return
}

// languageFromLsp maps a known LSP binary name to the manifest language
// name used by Register. Unknown binaries return "" — the caller treats
// that as "needs manual migration" and skips the entry in MigrateLegacy.
//
// Ambiguity note: typescript-language-server backs BOTH the javascript
// and typescript manifest entries. Detection returns "typescript" for
// either; a user running pure-JS projects can manually rename the entry
// or re-register with an explicit language argument afterwards.
func languageFromLsp(lsp string) string {
	base := strings.ToLower(filepath.Base(lsp))
	switch {
	case strings.HasPrefix(base, "pyright"):
		return "python"
	case strings.HasPrefix(base, "typescript-language-server"):
		return "typescript"
	case strings.HasPrefix(base, "rust-analyzer"):
		return "rust"
	case strings.HasPrefix(base, "clangd"):
		return "clangd"
	case strings.HasPrefix(base, "fortls"):
		return "fortran"
	case strings.HasPrefix(base, "gopls"):
		return "go"
	case strings.HasPrefix(base, "vscode-css-language-server"):
		return "vscode-css"
	case strings.HasPrefix(base, "vscode-html-language-server"):
		return "vscode-html"
	}
	return ""
}

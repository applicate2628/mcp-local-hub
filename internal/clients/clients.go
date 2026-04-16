package clients

// MCPEntry describes one MCP server entry in a client's config.
// The hub uses this to add/update/remove entries idempotently.
type MCPEntry struct {
	Name    string            // server name, e.g., "serena"
	URL     string            // full URL, e.g., "http://localhost:9121/mcp"
	Headers map[string]string // optional HTTP headers
	Env     map[string]string // only used by stdio entries (for rollback); URL entries leave this nil
}

// Client is the OS-/format-abstracted interface for a single MCP client config file.
// Implementations live in one file per client.
type Client interface {
	// Name returns a stable identifier ("claude-code", "codex-cli", "gemini-cli", "antigravity")
	// used in manifest client_bindings.
	Name() string

	// ConfigPath returns the absolute path to the config file this client reads.
	// Used for display, backup, and existence checks.
	ConfigPath() string

	// Exists reports whether the config file is present. If false, AddEntry/RemoveEntry
	// are no-ops and Backup returns ErrClientNotInstalled.
	Exists() bool

	// Backup copies the current config to a sibling file ending in ".bak-mcp-local-hub-<timestamp>"
	// and returns the path. Overwrites any previous backup with the same timestamp-second.
	Backup() (string, error)

	// Restore copies the named backup over the live config, overwriting current content.
	Restore(backupPath string) error

	// AddEntry adds or replaces the MCP server entry named entry.Name.
	// Creates parent `mcpServers` / `[mcp_servers.*]` section if missing.
	AddEntry(entry MCPEntry) error

	// RemoveEntry removes the MCP server entry with the given name.
	// Returns nil if the entry does not exist (idempotent).
	RemoveEntry(name string) error

	// GetEntry returns the current value of the named entry, or nil if missing.
	GetEntry(name string) (*MCPEntry, error)
}

// ErrClientNotInstalled signals the client's config file does not exist on this machine.
type ErrClientNotInstalled struct{ Client string }

func (e *ErrClientNotInstalled) Error() string {
	return "client not installed: " + e.Client
}

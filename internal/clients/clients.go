package clients

// MCPEntry describes one MCP server entry in a client's config.
// The hub uses this to add/update/remove entries idempotently.
//
// Most adapters consume the URL directly (clients that speak HTTP MCP
// natively). Adapters for stdio-only clients — currently only Antigravity —
// consume the RelayServer/RelayDaemon/RelayExePath triple instead and
// write a 'command'+'args' entry invoking `mcp.exe relay`. Install.go
// populates all fields so individual adapters ignore what they don't need.
type MCPEntry struct {
	Name    string            // server name, e.g., "serena"
	URL     string            // full URL, e.g., "http://localhost:9121/mcp"
	Headers map[string]string // optional HTTP headers
	Env     map[string]string // only used by stdio entries (for rollback); URL entries leave this nil

	// Relay-based stdio adapters (Antigravity): these three fields identify
	// the manifest lookup the stdio client should perform when it spawns
	// mcp.exe relay as its child process.
	RelayServer  string // server name in manifest, e.g., "serena"
	RelayDaemon  string // daemon name within that manifest, e.g., "claude"
	RelayExePath string // absolute path to mcp.exe (from os.Executable() at install time)
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

// AllClients returns the map of {client-name -> Client} for every supported
// adapter. Factories that return an error (e.g. UserHomeDir failure) are
// silently skipped, so callers that iterate the map see only adapters that
// could be constructed on the current host. This is the shared accessor
// used by both internal/api and internal/cli.
func AllClients() map[string]Client {
	result := map[string]Client{}
	for _, factory := range []func() (Client, error){
		NewClaudeCode, NewCodexCLI, NewGeminiCLI, NewAntigravity,
	} {
		c, err := factory()
		if err != nil {
			continue
		}
		result[c.Name()] = c
	}
	return result
}

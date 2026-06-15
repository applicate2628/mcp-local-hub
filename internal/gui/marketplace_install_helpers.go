package gui

import (
	"encoding/json"
	"fmt"
	"os"

	"gopkg.in/yaml.v3"

	"mcp-local-hub/internal/api"
	"mcp-local-hub/internal/clients"
)

// parseManifestPorts leniently extracts the daemon ports declared by a
// manifest YAML. It uses a permissive yaml.Unmarshal (NOT config.ParseManifest,
// whose KnownFields(true) strict decode would reject a manifest with any
// unrecognized field) because the ONLY thing this scan needs is the set of
// `daemons[].port` values to avoid a port collision. A manifest that does not
// parse at all yields an error the caller treats as best-effort-skip.
func parseManifestPorts(raw string) ([]int, error) {
	var doc struct {
		Daemons []struct {
			Port int `yaml:"port"`
		} `yaml:"daemons"`
	}
	if err := yaml.Unmarshal([]byte(raw), &doc); err != nil {
		return nil, err
	}
	ports := make([]int, 0, len(doc.Daemons))
	for _, d := range doc.Daemons {
		ports = append(ports, d.Port)
	}
	return ports, nil
}

// writeDirectEntry writes the client-native MCP entry for a direct-mode
// marketplace install into one client's config.
//
//   - http transport: a remote-URL entry through the adapter's AddEntry
//     (the URL-native shape the adapter owns), routed through the production
//     SecureWriteClientConfig pipeline.
//   - stdio transport: a {command, args, env} entry written into the JSON
//     `mcpServers` map at the adapter's ConfigPath via clients.WriteConfigFile
//     (also the SecureWriteClientConfig pipeline). The clients-package
//     adapters do not expose a stdio AddEntry (their AddEntry hardcodes the
//     URL/HTTP shape), so the direct-mode stdio path writes the widely-accepted
//     `mcpServers[name] = {type:stdio, command, args, env}` JSON shape itself.
//     A client whose config is NOT the JSON `mcpServers` family (e.g. codex's
//     TOML) fails closed with a clear per-client error rather than writing a
//     shape it cannot interpret.
func writeDirectEntry(c clients.Client, entry *api.MarketplaceEntry) error {
	switch entry.Transport {
	case "http":
		if entry.URL == "" {
			return fmt.Errorf("catalog entry %q transport=http but url is empty", entry.ID)
		}
		return c.AddEntry(clients.MCPEntry{
			Name:    entry.ID,
			URL:     entry.URL,
			Headers: nil, // catalog http entries carry no headers; operator edits later
		})
	case "stdio":
		if entry.Command == "" {
			return fmt.Errorf("catalog entry %q transport=stdio but command is empty", entry.ID)
		}
		return writeStdioJSONEntry(c.ConfigPath(), entry)
	default:
		return fmt.Errorf("catalog entry %q has unsupported transport %q", entry.ID, entry.Transport)
	}
}

// writeStdioJSONEntry reads the JSON `mcpServers` config at path, upserts a
// stdio entry, and writes it back through clients.WriteConfigFile (the
// production SecureWriteClientConfig pipeline). A missing/empty file is
// treated as an empty config (the entry is created fresh). A file that is not
// a JSON object — or whose `mcpServers` is not an object — fails closed so a
// non-JSON-family client (codex TOML, etc.) does not get a JSON shape it
// cannot parse.
func writeStdioJSONEntry(path string, entry *api.MarketplaceEntry) error {
	data, err := os.ReadFile(path)
	if err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("read client config %s: %w", path, err)
	}
	doc := map[string]any{}
	if len(data) > 0 {
		if err := json.Unmarshal(data, &doc); err != nil {
			return fmt.Errorf("client config %s is not the JSON mcpServers family (direct-mode stdio supports JSON clients only): %w", path, err)
		}
	}
	servers, ok := doc["mcpServers"].(map[string]any)
	if !ok {
		if _, present := doc["mcpServers"]; present {
			return fmt.Errorf("client config %s has a non-object mcpServers block; refusing direct-mode stdio write", path)
		}
		servers = map[string]any{}
	}
	stdio := map[string]any{
		"type":    "stdio",
		"command": entry.Command,
	}
	if len(entry.Args) > 0 {
		stdio["args"] = entry.Args
	}
	if len(entry.Env) > 0 {
		stdio["env"] = entry.Env
	}
	servers[entry.ID] = stdio
	doc["mcpServers"] = servers
	out, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal client config: %w", err)
	}
	return clients.WriteConfigFile(path, append(out, '\n'))
}

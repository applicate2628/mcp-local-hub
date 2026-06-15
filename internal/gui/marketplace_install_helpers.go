package gui

import (
	"fmt"

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
// marketplace install into one client's config. Direct mode is HTTP-ONLY: an
// http entry has one remote-URL shape every adapter owns via AddEntry (routed
// through the production SecureWriteClientConfig pipeline). The handler
// (handleMarketplaceDirectInstall) gates non-http transports to a 400 before
// this is reached, because a stdio entry's native shape varies per client
// (mcpServers / servers / context_servers / mcp / mcp_servers / TOML) and the
// clients-package adapters do not yet expose a per-client stdio writer — a
// single hardcoded shape would silently land in the wrong key for several
// clients. The default branch here is a defensive fail-closed in case a future
// caller reaches this without the handler gate.
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
	default:
		return fmt.Errorf("catalog entry %q transport %q is not supported by direct-mode install (http only; use hub mode for stdio)", entry.ID, entry.Transport)
	}
}

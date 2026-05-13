// internal/api/marketplace_catalog.go — G5 Marketplace catalog parser.
//
// Spec: docs/superpowers/specs/2026-05-12-g5-marketplace-draft-import-design.md
// §"Registry source" + §"Threat model" + §"Acceptance criteria".

package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
)

const MarketplaceCatalogSchemaVersion = "1"

type MarketplaceCatalog struct {
	SchemaVersion string             `json:"schema_version"`
	GeneratedAt   string             `json:"generated_at,omitempty"`
	Entries       []MarketplaceEntry `json:"entries"`
}

type MarketplaceEntry struct {
	ID         string            `json:"id"`
	Name       string            `json:"name"`
	Summary    string            `json:"summary,omitempty"`
	Homepage   string            `json:"homepage,omitempty"`
	ReadmeURL  string            `json:"readme_url,omitempty"`
	Transport  string            `json:"transport"`
	Command    string            `json:"command,omitempty"`
	Args       []string          `json:"args,omitempty"`
	Env        map[string]string `json:"env,omitempty"`
	URL        string            `json:"url,omitempty"`
	Categories []string          `json:"categories,omitempty"`
	License    string            `json:"license,omitempty"`
}

// ParseMarketplaceCatalog decodes raw JSON. Returns the first error
// per spec §"Threat model" (malformed catalogs reject wholesale,
// never partial-accept).
//
// codex r5 P2 closure: rejects trailing bytes after the top-level
// JSON object so a valid catalog appended with garbage (or a second
// object) cannot be silently accepted. Mirrors the registry-source
// "single canonical document" contract from §"Threat model".
func ParseMarketplaceCatalog(raw []byte) (*MarketplaceCatalog, error) {
	var cat MarketplaceCatalog
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&cat); err != nil {
		return nil, fmt.Errorf("decode catalog: %w", err)
	}
	var trailing json.RawMessage
	if err := dec.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			return nil, fmt.Errorf("decode catalog: trailing bytes after top-level JSON object")
		}
		return nil, fmt.Errorf("decode catalog: trailing bytes after top-level JSON object: %w", err)
	}
	if cat.SchemaVersion != MarketplaceCatalogSchemaVersion {
		return nil, fmt.Errorf("schema_version %q: this build only accepts %q",
			cat.SchemaVersion, MarketplaceCatalogSchemaVersion)
	}
	seen := map[string]bool{}
	for i := range cat.Entries {
		e := &cat.Entries[i]
		if err := validateMarketplaceEntry(e); err != nil {
			return nil, fmt.Errorf("entry %d (id=%q): %w", i, e.ID, err)
		}
		if seen[e.ID] {
			return nil, fmt.Errorf("entry %d: duplicate id %q", i, e.ID)
		}
		seen[e.ID] = true
	}
	return &cat, nil
}

func validateMarketplaceEntry(e *MarketplaceEntry) error {
	if e.ID == "" {
		return fmt.Errorf("missing id")
	}
	if e.Name == "" {
		return fmt.Errorf("missing name")
	}
	// codex r1 P2 closure: entry id must pass CheckManifestName so
	// the projected draft can be accepted by `mcphub manifest create`
	// later — including the reserved-aggregate-name guard from r15.
	if err := CheckManifestName(e.ID); err != nil {
		return fmt.Errorf("id %q fails manifest-name gate: %w", e.ID, err)
	}
	switch e.Transport {
	case "stdio":
		if e.Command == "" {
			return fmt.Errorf("stdio entry must declare command")
		}
	case "http":
		if e.URL == "" {
			return fmt.Errorf("http entry must declare url")
		}
		if !strings.HasPrefix(e.URL, "https://") {
			return fmt.Errorf("http entry url must be https:// (got %q)", e.URL)
		}
	default:
		return fmt.Errorf("unknown transport %q (want stdio or http)", e.Transport)
	}
	return nil
}

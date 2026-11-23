package api

import (
	"bytes"
	"encoding/json"
	"fmt"
	"reflect"
	"slices"
)

// MarketplaceProjectProviderKind identifies a descriptive project-provider row.
const MarketplaceProjectProviderKind = "project-provider"

// MarketplaceProjectPackage describes the official package without launch arguments.
type MarketplaceProjectPackage struct {
	Manager string `json:"manager"`
	Name    string `json:"name"`
	Bin     string `json:"bin"`
	Channel string `json:"channel"`
}

// marketplaceProjectProviderFields is the wire projection of an explicitly
// classified project-provider row.
type marketplaceProjectProviderFields struct {
	Kind    string                     `json:"kind"`
	Service string                     `json:"service"`
	Package *MarketplaceProjectPackage `json:"package"`
}

// MarshalJSON keeps the ordinary entry projection and adds provider metadata
// only for the typed provider branch (including the persisted catalog cache).
func (e MarketplaceEntry) MarshalJSON() ([]byte, error) {
	type ordinaryEntry MarketplaceEntry
	if e.Kind != MarketplaceProjectProviderKind {
		return json.Marshal(ordinaryEntry(e))
	}
	return json.Marshal(struct {
		ordinaryEntry
		marketplaceProjectProviderFields
	}{
		ordinaryEntry: ordinaryEntry(e),
		marketplaceProjectProviderFields: marketplaceProjectProviderFields{
			Kind: e.Kind, Service: e.Service, Package: e.Package,
		},
	})
}

// projectProviderOfficialPackage maps each supported service to its catalog
// package identity.
func projectProviderOfficialPackage(service string) (name, bin string, ok bool) {
	switch service {
	case "trail":
		return "@trailhq/trail-code", "trail", true
	case "rea":
		return "rea-agents", "rea", true
	default:
		return "", "", false
	}
}

// catalogMemberOccurrences preserves every occurrence of a matching JSON key,
// including case-folded aliases, nulls, and successive container values.
type catalogMemberOccurrences []json.RawMessage

func (m *catalogMemberOccurrences) UnmarshalJSON(raw []byte) error {
	*m = append(*m, bytes.Clone(raw))
	return nil
}

// marketplaceProjectProviderRows observes every explicit provider declaration
// in any occurrence of the catalog or entries container. Ordinary rows retain
// the existing decoder semantics. A provider row needs one unambiguous source
// document, one entries array and one schema version to align raw and typed data.
func marketplaceProjectProviderRows(catalogs []json.RawMessage, entryCount int) (map[int]json.RawMessage, error) {
	var providerRows map[int]json.RawMessage
	for _, raw := range catalogs {
		var catalog struct {
			SchemaVersion catalogMemberOccurrences `json:"schema_version"`
			Entries       catalogMemberOccurrences `json:"entries"`
		}
		if err := json.Unmarshal(raw, &catalog); err != nil {
			return nil, err
		}
		for _, rawEntries := range catalog.Entries {
			var rows []json.RawMessage
			if err := json.Unmarshal(rawEntries, &rows); err != nil {
				return nil, err
			}
			for i, rawRow := range rows {
				var marker struct {
					Kind catalogMemberOccurrences `json:"kind"`
				}
				if err := json.Unmarshal(rawRow, &marker); err != nil {
					return nil, err
				}
				declaresProvider := slices.ContainsFunc(marker.Kind, func(rawKind json.RawMessage) bool {
					var kind string
					return json.Unmarshal(rawKind, &kind) == nil && kind == MarketplaceProjectProviderKind
				})
				if !declaresProvider {
					continue
				}
				if len(catalogs) != 1 || len(catalog.SchemaVersion) != 1 ||
					len(catalog.Entries) != 1 || len(rows) != entryCount {
					return nil, fmt.Errorf("project provider requires a single catalog, schema_version and entries array")
				}
				if providerRows == nil {
					providerRows = make(map[int]json.RawMessage)
				}
				providerRows[i] = rawRow
			}
		}
	}
	return providerRows, nil
}

func validateMarketplaceProjectProvider(e *MarketplaceEntry, schemaVersion string, raw json.RawMessage) error {
	if schemaVersion != MarketplaceCatalogSchemaVersionV3 {
		return fmt.Errorf("project provider requires catalog schema_version %q", MarketplaceCatalogSchemaVersionV3)
	}
	fields, err := marketplaceProviderObjectFields(raw)
	if err != nil {
		return err
	}
	for field := range fields {
		switch field {
		case "id", "name", "summary", "homepage", "readme_url", "categories", "license", "kind", "service", "package":
		default:
			return fmt.Errorf("project provider forbids field %q", field)
		}
	}
	pkgFields, err := marketplaceProviderObjectFields(fields["package"])
	if err != nil || !projectClientExactJSONFields(pkgFields, "manager", "name", "bin", "channel") {
		return fmt.Errorf("project provider requires exactly the manager, name, bin, and channel package fields")
	}
	var standalone struct {
		MarketplaceEntry
		marketplaceProjectProviderFields
	}
	if err := json.Unmarshal(raw, &standalone); err != nil {
		return err
	}
	if !reflect.DeepEqual(&standalone.MarketplaceEntry, e) {
		return fmt.Errorf("project provider record does not match the decoded catalog entry")
	}
	wire := standalone.marketplaceProjectProviderFields
	name, bin, official := projectProviderOfficialPackage(wire.Service)
	if !official || wire.Kind != MarketplaceProjectProviderKind || e.ID != wire.Service || wire.Package == nil ||
		*wire.Package != (MarketplaceProjectPackage{Manager: "npm", Name: name, Bin: bin, Channel: "latest"}) {
		return fmt.Errorf("project provider must match the official package identity")
	}
	e.Kind, e.Service, e.Package = wire.Kind, wire.Service, wire.Package
	return nil
}

// marketplaceProviderObjectFields retains raw members, refusing duplicates
// before ordinary map decoding could overwrite either value.
func marketplaceProviderObjectFields(raw json.RawMessage) (map[string]json.RawMessage, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	token, err := dec.Token()
	if err != nil || token != json.Delim('{') {
		return nil, fmt.Errorf("project provider requires a JSON object")
	}
	fields := make(map[string]json.RawMessage)
	for dec.More() {
		token, err := dec.Token()
		if err != nil {
			return nil, err
		}
		key := token.(string)
		if _, exists := fields[key]; exists {
			return nil, fmt.Errorf("project provider has duplicate field %q", key)
		}
		var value json.RawMessage
		if err := dec.Decode(&value); err != nil {
			return nil, err
		}
		fields[key] = value
	}
	if _, err := dec.Token(); err != nil {
		return nil, err
	}
	return fields, nil
}

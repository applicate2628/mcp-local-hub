package api

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestScanEntryLegacyConflictOmitemptyWhenEmpty verifies that when the
// LegacyConflict field is unset (nil), it is absent from the marshalled
// JSON. Existing consumers must see no change in payload shape.
func TestScanEntryLegacyConflictOmitemptyWhenEmpty(t *testing.T) {
	entry := ScanEntry{
		Name:           "clangd",
		Status:         "via-hub",
		ClientPresence: map[string]ClientEntry{},
	}

	raw, err := json.Marshal(entry)
	if err != nil {
		t.Fatalf("marshal failed: %v", err)
	}

	if strings.Contains(string(raw), "legacy_conflict") {
		t.Errorf("expected legacy_conflict absent from JSON when nil; got %s", raw)
	}
}

// TestScanEntryLegacyConflictPopulated verifies that when both
// ClientPresence (hub entry) and LegacyConflict (stdio entry) carry
// the same client key, both round-trip cleanly through JSON.
func TestScanEntryLegacyConflictPopulated(t *testing.T) {
	const clientKey = "codex-cli"

	original := ScanEntry{
		Name:   "clangd",
		Status: "via-hub",
		ClientPresence: map[string]ClientEntry{
			clientKey: {
				Transport: "http",
				Endpoint:  "http://127.0.0.1:9128/lsp/clangd",
				Raw:       map[string]any{"url": "http://127.0.0.1:9128/lsp/clangd"},
			},
		},
		LegacyConflict: map[string]ClientEntry{
			clientKey: {
				Transport: "stdio",
				Endpoint:  "clangd",
				Raw:       map[string]any{"command": "clangd"},
			},
		},
	}

	raw, err := json.Marshal(original)
	if err != nil {
		t.Fatalf("marshal failed: %v", err)
	}

	if !strings.Contains(string(raw), "legacy_conflict") {
		t.Errorf("expected legacy_conflict present in JSON when populated; got %s", raw)
	}

	var decoded ScanEntry
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatalf("unmarshal failed: %v", err)
	}

	hub, ok := decoded.ClientPresence[clientKey]
	if !ok {
		t.Fatalf("expected ClientPresence[%q] present after round-trip", clientKey)
	}
	if hub.Transport != "http" || hub.Endpoint != "http://127.0.0.1:9128/lsp/clangd" {
		t.Errorf("hub entry mangled after round-trip: %+v", hub)
	}

	legacy, ok := decoded.LegacyConflict[clientKey]
	if !ok {
		t.Fatalf("expected LegacyConflict[%q] present after round-trip", clientKey)
	}
	if legacy.Transport != "stdio" || legacy.Endpoint != "clangd" {
		t.Errorf("legacy entry mangled after round-trip: %+v", legacy)
	}
}

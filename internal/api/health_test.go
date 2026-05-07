package api

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestHealthSnapshot_DefaultExcludesProbesAndCapabilities(t *testing.T) {
	a := NewAPI()
	snap, err := a.HealthSnapshot(HealthOpts{})
	if err != nil {
		t.Fatalf("HealthSnapshot: %v", err)
	}
	if snap.SchemaVersion != "1" {
		t.Errorf("SchemaVersion = %q, want \"1\"", snap.SchemaVersion)
	}
	if snap.Probes != nil {
		t.Errorf("Probes = %+v, want nil (default opts must omit expensive sections)", snap.Probes)
	}
	if snap.Capabilities != nil {
		t.Errorf("Capabilities = %+v, want nil", snap.Capabilities)
	}
}

func TestHealthSnapshot_JSONOmitsNilSections(t *testing.T) {
	a := NewAPI()
	snap, _ := a.HealthSnapshot(HealthOpts{})
	b, err := json.Marshal(snap)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	body := string(b)
	if strings.Contains(body, `"probes"`) {
		t.Errorf("default JSON contains probes key, must be omitted: %s", body)
	}
	if strings.Contains(body, `"capabilities"`) {
		t.Errorf("default JSON contains capabilities key, must be omitted: %s", body)
	}
	if !strings.Contains(body, `"schema_version":"1"`) {
		t.Errorf("missing schema_version=1: %s", body)
	}
}

func TestCapabilityID_CanonicalForm(t *testing.T) {
	got := capabilityID("fs", "fs-default", "tool", "read_file")
	want := "fs/fs-default/tool/read_file"
	if got != want {
		t.Errorf("capabilityID = %q, want %q", got, want)
	}
}

func TestHealthSnapshot_HubSectionShape(t *testing.T) {
	a := NewAPI()
	snap, _ := a.HealthSnapshot(HealthOpts{})
	// Hub section is present (not optional) and carries schema-required fields,
	// even when zero-valued, so consumers can rely on the structure.
	if snap.Hub.Version == "" && snap.Hub.Commit == "" && snap.Hub.BuildDate == "" {
		// All three empty is acceptable in unit tests where build info isn't injected;
		// but the *fields* must exist (will fail compile if they don't).
		_ = snap.Hub.Version
		_ = snap.Hub.Commit
		_ = snap.Hub.BuildDate
		_ = snap.Hub.StartedAt
		_ = snap.Hub.Lock
		_ = snap.Hub.GeneratedAt
		_ = snap.Hub.TTLMs
	}
}

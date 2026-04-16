package secrets

import (
	"path/filepath"
	"testing"
)

func TestVault_ExportYAML_ImportYAML_Roundtrip(t *testing.T) {
	dir := t.TempDir()
	keyPath := filepath.Join(dir, ".age-key")
	vaultPath := filepath.Join(dir, "secrets.age")
	_ = InitVault(keyPath, vaultPath)
	v, _ := OpenVault(keyPath, vaultPath)
	v.Set("A", "1")
	v.Set("B", "two with spaces")

	raw, err := v.ExportYAML()
	if err != nil {
		t.Fatalf("ExportYAML: %v", err)
	}

	// Wipe and reimport.
	for _, k := range v.List() {
		_ = v.Delete(k)
	}
	if err := v.ImportYAML(raw); err != nil {
		t.Fatalf("ImportYAML: %v", err)
	}
	if got, _ := v.Get("A"); got != "1" {
		t.Errorf("A = %q, want 1", got)
	}
	if got, _ := v.Get("B"); got != "two with spaces" {
		t.Errorf("B = %q, want 'two with spaces'", got)
	}
}

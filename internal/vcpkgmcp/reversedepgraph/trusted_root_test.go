package reversedepgraph

import (
	"path/filepath"
	"testing"
)

func TestValidateTrustedRoot(t *testing.T) {
	trusted := t.TempDir()
	if err := ValidateTrustedRoot(trusted, trusted); err != nil {
		t.Fatalf("matching trusted root rejected: %v", err)
	}
	if err := ValidateTrustedRoot(filepath.Join(trusted, "attacker"), trusted); err == nil {
		t.Fatal("caller-selected root was accepted")
	}
	if err := ValidateTrustedRoot(trusted, ""); err == nil {
		t.Fatal("missing daemon trust configuration was accepted")
	}
}

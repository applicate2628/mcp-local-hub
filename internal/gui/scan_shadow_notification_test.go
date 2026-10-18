package gui

import (
	"crypto/sha256"
	"testing"

	"mcp-local-hub/internal/api"
)

func TestNewServerRealScannerOwnsOneShadowWarningTracker(t *testing.T) {
	first := NewServer(Config{}).scanner.(realScanner)
	second := NewServer(Config{}).scanner.(realScanner)
	if first.shadowWarnings == nil {
		t.Fatal("long-lived GUI scanner has no shadow-warning tracker")
	}
	if first.shadowWarnings == second.shadowWarnings {
		t.Fatal("separate GUI lifetimes must not share shadow-warning tracker state")
	}
	if _, ok := any(first.shadowWarnings).(*api.DiskShadowWarningTracker); !ok {
		t.Fatalf("tracker type = %T", first.shadowWarnings)
	}
	observation := api.DiskShadowWarningObservation{
		Name: "wolfram", Message: "warning", Identity: sha256.Sum256([]byte("same")),
	}
	emitted := len(first.shadowWarnings.Complete([]api.DiskShadowWarningObservation{observation}))
	emitted += len(first.shadowWarnings.Complete([]api.DiskShadowWarningObservation{observation}))
	if emitted != 1 {
		t.Fatalf("real GUI scanner repeated-warning emissions = %d, want 1", emitted)
	}
}

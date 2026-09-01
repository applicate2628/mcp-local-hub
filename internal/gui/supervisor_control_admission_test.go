package gui

import (
	"context"
	"errors"
	"testing"

	"mcp-local-hub/internal/api"
)

func TestRealRestarterUsesSharedSupervisorControlAdmission(t *testing.T) {
	want := errors.New("shared compatibility admission")
	restore := api.RegisterSupervisorControlAdmission(func(context.Context) error { return want })
	defer restore()
	if _, err := (realRestarter{}).Restart("serena", ""); !errors.Is(err, want) {
		t.Fatalf("GUI restart error=%v want shared admission", err)
	}
	if _, err := (realRestarter{}).RestartAll(); !errors.Is(err, want) {
		t.Fatalf("GUI restart-all error=%v want shared admission", err)
	}
	if _, err := (realStopper{}).Stop("serena", ""); !errors.Is(err, want) {
		t.Fatalf("GUI stop error=%v want shared admission", err)
	}
	if _, err := (realStopper{}).StopAll(); !errors.Is(err, want) {
		t.Fatalf("tray stop-all error=%v want shared admission", err)
	}
}

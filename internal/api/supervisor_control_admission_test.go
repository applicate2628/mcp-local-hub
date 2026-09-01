package api

import (
	"context"
	"errors"
	"testing"
)

func TestSupervisorControlAdmissionGuardsAllMutationEntrypoints(t *testing.T) {
	want := errors.New("compatibility admission held")
	restore := RegisterSupervisorControlAdmission(func(context.Context) error { return want })
	defer restore()
	a := NewAPI()
	for name, invoke := range map[string]func() error{
		"stop":     func() error { _, err := a.Stop("serena", ""); return err },
		"stop all": func() error { _, err := a.StopAll(); return err },
		"restart":  func() error { _, err := a.Restart("serena", ""); return err },
		"restart context": func() error {
			_, err := a.RestartContext(context.Background(), "serena", "")
			return err
		},
		"restart all": func() error { _, err := a.RestartAll(); return err },
		"direct respawn": func() error {
			_, err := DialSupervisorIPCRespawn(context.Background(), "\\mcp-local-hub-serena", false, 1)
			return err
		},
	} {
		t.Run(name, func(t *testing.T) {
			if err := invoke(); !errors.Is(err, want) {
				t.Fatalf("error=%v want admission failure", err)
			}
		})
	}
}

func TestDefaultSupervisorControlAdmissionNeverTreatsIncompleteAsLegacy(t *testing.T) {
	if errors.Is(ErrSupervisorCapabilityIncomplete, ErrSupervisorCapabilityLegacy) {
		t.Fatal("incomplete capability envelope must not authorize legacy replacement")
	}
}

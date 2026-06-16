package config

import (
	"strings"
	"testing"
)

// TestValidateRejectsDaemonOnGUIPort pins the DM-2 port-planning gate: a
// global manifest whose daemon declares the reserved GUI listener port is
// rejected at Validate(), while a daemon on a normal hand-assigned port
// passes.
func TestValidateRejectsDaemonOnGUIPort(t *testing.T) {
	mk := func(port int) *ServerManifest {
		return &ServerManifest{
			Name:      "x",
			Kind:      KindGlobal,
			Transport: TransportStdioBridge,
			Command:   "mcphub",
			Daemons:   []DaemonSpec{{Name: "default", Port: port}},
		}
	}

	err := mk(ReservedGUIPort).Validate()
	if err == nil {
		t.Fatal("daemon declaring the reserved GUI port must be rejected")
	}
	if !strings.Contains(err.Error(), "GUI listener port") {
		t.Fatalf("error must name the GUI-port collision; got %v", err)
	}

	if err := mk(9129).Validate(); err != nil {
		t.Fatalf("daemon on a normal hand-assigned port (9129) must pass; got %v", err)
	}
}

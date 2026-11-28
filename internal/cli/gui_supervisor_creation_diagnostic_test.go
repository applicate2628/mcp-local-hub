package cli

import (
	"bytes"
	"testing"

	"mcp-local-hub/internal/api"
)

func TestGUISupervisorCreatedDiagnostic(t *testing.T) {
	var output bytes.Buffer
	const birth = "2026-11-28T13:17:12.123456789Z"
	original := api.SupervisorLockOwner{PID: 1234, StartedAt: birth}
	(*guiSupervisorControlTiming)(nil).reportCreated(&output, original)
	timing := new(guiSupervisorControlTiming)
	timing.reportCreated(&output, api.SupervisorLockOwner{PID: original.PID})
	timing.reportCreated(&output, api.SupervisorLockOwner{StartedAt: birth})
	if output.Len() != 0 {
		t.Fatalf("disabled or incomplete capture emitted %q", output.String())
	}
	timing.reportCreated(&output, original)
	const want = "{\"event\":\"gui_supervisor_created_before_readiness\",\"pid\":1234,\"started_at\":\"" + birth + "\"}\n"
	if got := output.String(); got != want {
		t.Fatalf("event = %q, want %q", got, want)
	}
	if timing.attempts != 0 || timing.controlCalls != 0 || timing.ipcCalls != 0 {
		t.Fatal("pre-readiness event affected observer counters")
	}
}

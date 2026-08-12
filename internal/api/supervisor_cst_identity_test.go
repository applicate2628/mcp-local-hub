package api

import (
	"encoding/json"
	"testing"
)

func TestSupervisorCstIdentityAuthorizationMatrix(t *testing.T) {
	policy := SupervisorCstIdentityPolicyV1{
		DaemonServiceSID: "S-1-5-80-111-222-333-444-555",
		DaemonImagePath:  `C:\Program Files\mcp-local-hub\mcphub-cst-daemon.exe`,
		DaemonSessionID:  0,
		MinimumIntegrity: SupervisorIntegrityHigh,
	}
	goodPeer := SupervisorProcessIdentityV1{
		PID:           4242,
		CreationTime:  "2026-08-12T08:00:00Z",
		UserSID:       policy.DaemonServiceSID,
		SessionID:     0,
		IntegrityRID:  SupervisorIntegritySystem,
		ImagePath:     policy.DaemonImagePath,
		SCMServicePID: 4242,
	}
	current := []SupervisorCstTaskIdentityV1{{
		Task:          SupervisorCstTaskV1,
		PID:           5151,
		PIDGeneration: 7,
		CreationTime:  "2026-08-12T08:01:00Z",
	}}
	request := IPCRequest{Version: 1, ID: 91, Cmd: SupervisorCstIdentityCommandV1}

	tests := []struct {
		name    string
		request IPCRequest
		peer    SupervisorProcessIdentityV1
		rows    []SupervisorCstTaskIdentityV1
		wantOK  bool
	}{
		{name: "exact", request: request, peer: goodPeer, rows: current, wantOK: true},
		{name: "generic status", request: IPCRequest{Version: 1, ID: 92, Cmd: "status"}, peer: goodPeer, rows: current},
		{name: "control", request: IPCRequest{Version: 1, ID: 93, Cmd: "exit"}, peer: goodPeer, rows: current},
		{name: "explicit task", request: IPCRequest{Version: 1, ID: 94, Cmd: SupervisorCstIdentityCommandV1, Args: map[string]any{"task": "cst"}}, peer: goodPeer, rows: current},
		{name: "wrong sid", request: IPCRequest{Version: 1, ID: 95, Cmd: SupervisorCstIdentityCommandV1}, peer: withSupervisorIdentity(goodPeer, func(p *SupervisorProcessIdentityV1) { p.UserSID = "S-1-5-18" }), rows: current},
		{name: "wrong scm pid", request: IPCRequest{Version: 1, ID: 96, Cmd: SupervisorCstIdentityCommandV1}, peer: withSupervisorIdentity(goodPeer, func(p *SupervisorProcessIdentityV1) { p.SCMServicePID++ }), rows: current},
		{name: "wrong session", request: IPCRequest{Version: 1, ID: 97, Cmd: SupervisorCstIdentityCommandV1}, peer: withSupervisorIdentity(goodPeer, func(p *SupervisorProcessIdentityV1) { p.SessionID = 1 }), rows: current},
		{name: "low integrity", request: IPCRequest{Version: 1, ID: 98, Cmd: SupervisorCstIdentityCommandV1}, peer: withSupervisorIdentity(goodPeer, func(p *SupervisorProcessIdentityV1) { p.IntegrityRID = SupervisorIntegrityMedium }), rows: current},
		{name: "wrong image", request: IPCRequest{Version: 1, ID: 99, Cmd: SupervisorCstIdentityCommandV1}, peer: withSupervisorIdentity(goodPeer, func(p *SupervisorProcessIdentityV1) { p.ImagePath = `C:\temp\lookalike.exe` }), rows: current},
		{name: "missing", request: IPCRequest{Version: 1, ID: 100, Cmd: SupervisorCstIdentityCommandV1}, peer: goodPeer},
		{name: "ambiguous", request: IPCRequest{Version: 1, ID: 101, Cmd: SupervisorCstIdentityCommandV1}, peer: goodPeer, rows: append(append([]SupervisorCstTaskIdentityV1{}, current...), current...)},
		{name: "stale generation", request: IPCRequest{Version: 1, ID: 102, Cmd: SupervisorCstIdentityCommandV1}, peer: goodPeer, rows: []SupervisorCstTaskIdentityV1{{Task: SupervisorCstTaskV1, PID: 5151, CreationTime: "2026-08-12T08:01:00Z"}}},
		{name: "wrong task", request: IPCRequest{Version: 1, ID: 103, Cmd: SupervisorCstIdentityCommandV1}, peer: goodPeer, rows: []SupervisorCstTaskIdentityV1{{Task: "cst-other", PID: 5151, PIDGeneration: 7, CreationTime: "2026-08-12T08:01:00Z"}}},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			before, err := json.Marshal(tc.rows)
			if err != nil {
				t.Fatal(err)
			}
			a := NewSupervisorCstIdentityAuthorizerV1(policy)
			got, err := a.Authorize(tc.request, tc.peer, tc.rows)
			if (err == nil) != tc.wantOK {
				t.Fatalf("Authorize() error = %v, wantOK=%v", err, tc.wantOK)
			}
			after, err := json.Marshal(tc.rows)
			if err != nil {
				t.Fatal(err)
			}
			if string(after) != string(before) {
				t.Fatalf("task state mutated: before=%s after=%s", before, after)
			}
			if tc.wantOK && got != current[0] {
				t.Fatalf("response = %#v, want exact current row %#v", got, current[0])
			}
		})
	}
}

func TestSupervisorCstIdentityReplayDenied(t *testing.T) {
	policy := SupervisorCstIdentityPolicyV1{DaemonServiceSID: "S-1-5-80-1", DaemonImagePath: `C:\daemon.exe`, MinimumIntegrity: SupervisorIntegrityHigh}
	peer := SupervisorProcessIdentityV1{PID: 9, CreationTime: "2026-08-12T08:00:00Z", UserSID: policy.DaemonServiceSID, IntegrityRID: SupervisorIntegrityHigh, ImagePath: policy.DaemonImagePath, SCMServicePID: 9}
	row := []SupervisorCstTaskIdentityV1{{Task: SupervisorCstTaskV1, PID: 10, PIDGeneration: 1, CreationTime: "2026-08-12T08:00:01Z"}}
	req := IPCRequest{Version: 1, ID: 777, Cmd: SupervisorCstIdentityCommandV1}
	a := NewSupervisorCstIdentityAuthorizerV1(policy)
	if _, err := a.Authorize(req, peer, row); err != nil {
		t.Fatalf("first Authorize: %v", err)
	}
	if _, err := a.Authorize(req, peer, row); err == nil {
		t.Fatal("replayed request admitted")
	}
}

func TestSupervisorStatusAuthorizationKernelBinding(t *testing.T) {
	owner := SupervisorLockOwner{PID: 120, StartedAt: "2026-08-12T08:02:00Z"}
	want := SupervisorProcessIdentityV1{PID: 120, CreationTime: owner.StartedAt, UserSID: "S-1-5-21-9", SessionID: 3, IntegrityRID: SupervisorIntegrityMedium, ImagePath: "synthetic/installed/mcphub.exe"}
	if err := ValidateSupervisorStatusServerIdentityV1(owner, want, want.UserSID, want.SessionID, want.ImagePath); err != nil {
		t.Fatalf("exact server identity rejected: %v", err)
	}
	mutations := []struct {
		name string
		fn   func(*SupervisorProcessIdentityV1)
	}{
		{"pid", func(p *SupervisorProcessIdentityV1) { p.PID++ }},
		{"creation", func(p *SupervisorProcessIdentityV1) { p.CreationTime = "2026-08-12T08:02:01Z" }},
		{"token", func(p *SupervisorProcessIdentityV1) { p.UserSID = "S-1-5-21-10" }},
		{"session", func(p *SupervisorProcessIdentityV1) { p.SessionID++ }},
		{"image", func(p *SupervisorProcessIdentityV1) { p.ImagePath = "synthetic/other/mcphub.exe" }},
	}
	for _, tc := range mutations {
		t.Run(tc.name, func(t *testing.T) {
			got := withSupervisorIdentity(want, tc.fn)
			if err := ValidateSupervisorStatusServerIdentityV1(owner, got, want.UserSID, want.SessionID, want.ImagePath); err == nil {
				t.Fatal("mismatched server identity accepted")
			}
		})
	}
}

func withSupervisorIdentity(v SupervisorProcessIdentityV1, mutate func(*SupervisorProcessIdentityV1)) SupervisorProcessIdentityV1 {
	mutate(&v)
	return v
}

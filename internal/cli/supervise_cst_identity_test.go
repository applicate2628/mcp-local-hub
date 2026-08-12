package cli

import (
	"encoding/json"
	"net"
	"testing"
	"time"

	"mcp-local-hub/internal/api"
)

func TestSupervisorCstIdentityPreDispatchAndStateImmutability(t *testing.T) {
	policy := api.SupervisorCstIdentityPolicyV1{
		DaemonServiceSID: "S-1-5-80-100",
		DaemonImagePath:  `/opt/mcp-local-hub/cst-daemon`,
		MinimumIntegrity: api.SupervisorIntegrityHigh,
	}
	peer := api.SupervisorProcessIdentityV1{
		PID: 44, CreationTime: "2026-08-12T08:00:00Z", UserSID: policy.DaemonServiceSID,
		IntegrityRID: api.SupervisorIntegrityHigh, ImagePath: policy.DaemonImagePath, SCMServicePID: 44,
	}
	previousProof := supervisorCstDaemonPeerIdentityFn
	previousAuthorizers := supervisorCstAuthorizers.byPolicy
	supervisorCstDaemonPeerIdentityFn = func(net.Conn) (api.SupervisorProcessIdentityV1, api.SupervisorCstIdentityPolicyV1, bool, error) {
		return peer, policy, true, nil
	}
	supervisorCstAuthorizers.byPolicy = make(map[api.SupervisorCstIdentityPolicyV1]*api.SupervisorCstIdentityAuthorizerV1)
	t.Cleanup(func() {
		supervisorCstDaemonPeerIdentityFn = previousProof
		supervisorCstAuthorizers.byPolicy = previousAuthorizers
	})

	tracker := NewDaemonRuntimeTracker()
	started := time.Date(2026, 8, 12, 8, 1, 0, 0, time.UTC)
	generation := tracker.MarkSpawned(api.SupervisorCstTaskV1, 5151, started)
	before, err := json.Marshal(tracker.Snapshot())
	if err != nil {
		t.Fatal(err)
	}

	denied := []api.IPCRequest{
		{Version: 1, ID: 1, Cmd: "status"},
		{Version: 1, ID: 2, Cmd: "exit"},
		{Version: 1, ID: 3, Cmd: "respawn"},
		{Version: 1, ID: 4, Cmd: "reconcile"},
		{Version: 1, ID: 5, Cmd: api.SupervisorCstIdentityCommandV1, Args: map[string]any{"task": "cst"}},
	}
	for _, req := range denied {
		handled, response := dispatchSupervisorCstIdentity(nil, req, tracker)
		if !handled || response.Error == nil || response.OK {
			t.Fatalf("request %#v was not denied before generic dispatch: %#v", req, response)
		}
	}

	req := api.IPCRequest{Version: 1, ID: 6, Cmd: api.SupervisorCstIdentityCommandV1}
	handled, response := dispatchSupervisorCstIdentity(nil, req, tracker)
	if !handled || response.Error != nil || !response.OK || !response.Final {
		t.Fatalf("exact request rejected: %#v", response)
	}
	identity, ok := response.Result.(api.SupervisorCstTaskIdentityV1)
	if !ok {
		t.Fatalf("result type = %T", response.Result)
	}
	want := api.SupervisorCstTaskIdentityV1{Task: api.SupervisorCstTaskV1, PID: 5151, PIDGeneration: generation, CreationTime: started.Format(time.RFC3339Nano)}
	if identity != want {
		t.Fatalf("identity = %#v, want %#v", identity, want)
	}
	_, replay := dispatchSupervisorCstIdentity(nil, req, tracker)
	if replay.Error == nil || replay.OK {
		t.Fatalf("replay admitted: %#v", replay)
	}
	after, err := json.Marshal(tracker.Snapshot())
	if err != nil {
		t.Fatal(err)
	}
	if string(before) != string(after) {
		t.Fatalf("supervisor task state changed: before=%s after=%s", before, after)
	}
}

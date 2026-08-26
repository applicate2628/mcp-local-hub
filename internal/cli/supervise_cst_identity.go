package cli

import (
	"net"
	"sync"

	"mcp-local-hub/internal/api"
)

var supervisorCstAuthorizers = struct {
	sync.Mutex
	byPolicy map[api.SupervisorCstIdentityPolicyV1]*api.SupervisorCstIdentityAuthorizerV1
}{byPolicy: make(map[api.SupervisorCstIdentityPolicyV1]*api.SupervisorCstIdentityAuthorizerV1)}

var supervisorCstDaemonPeerIdentityFn = supervisorCstDaemonPeerIdentity

// dispatchSupervisorCstIdentity is the pre-generic-dispatch owner. handled is
// true for the special opcode and for every connection proven to be the fixed
// CST daemon service, including denied generic commands.
func dispatchSupervisorCstIdentity(conn net.Conn, req api.IPCRequest, tracker *DaemonRuntimeTracker) (bool, api.IPCResponse) {
	peer, policy, isDaemon, proofErr := supervisorCstDaemonPeerIdentityFn(conn)
	if req.Cmd != api.SupervisorCstIdentityCommandV1 && !isDaemon {
		return false, api.IPCResponse{}
	}
	denied := func() (bool, api.IPCResponse) {
		return true, api.IPCResponse{ID: req.ID, Error: &api.IPCErr{Code: "CST_IDENTITY_DENIED", Message: "CST identity status request denied"}, Final: true}
	}
	if proofErr != nil || !isDaemon || tracker == nil {
		return denied()
	}
	entry, ok := tracker.Get(api.SupervisorCstTaskV1)
	if !ok {
		return denied()
	}
	rows := []api.SupervisorCstTaskIdentityV1{{
		Task:          api.SupervisorCstTaskV1,
		PID:           entry.CurrentPID,
		PIDGeneration: entry.PIDGeneration,
		CreationTime:  daemonRuntimeStartedAt(entry.StartedAt),
	}}
	supervisorCstAuthorizers.Lock()
	a := supervisorCstAuthorizers.byPolicy[policy]
	if a == nil {
		a = api.NewSupervisorCstIdentityAuthorizerV1(policy)
		supervisorCstAuthorizers.byPolicy[policy] = a
	}
	supervisorCstAuthorizers.Unlock()
	identity, err := a.Authorize(req, peer, rows)
	if err != nil {
		return denied()
	}
	return true, api.IPCResponse{ID: req.ID, OK: true, Result: identity, Final: true}
}

package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"
)

// DialSupervisorIPCCstIdentityV1 performs the status-only query. expected*
// values come from the installed canonical supervisor descriptor; none are
// taken from hello or the server response.
func DialSupervisorIPCCstIdentityV1(ctx context.Context, expectedUserSID string, expectedSessionID uint32, expectedImagePath string) (SupervisorCstTaskIdentityV1, error) {
	var zero SupervisorCstTaskIdentityV1
	if ctx == nil {
		ctx = context.Background()
	}
	if _, ok := ctx.Deadline(); !ok {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, 5*time.Second)
		defer cancel()
	}
	stateDir, err := DaemonStateDir()
	if err != nil {
		return zero, fmt.Errorf("CST supervisor identity: resolve state dir: %w", err)
	}
	owner, err := ReadSupervisorLockOwner(filepath.Join(stateDir, "supervisor.lock"))
	if err != nil {
		return zero, fmt.Errorf("CST supervisor identity: read lock owner: %w", err)
	}
	conn, err := dialSupervisorIPC(ctx, SupervisorIPCAddress(stateDir))
	if err != nil {
		if os.IsNotExist(err) {
			return zero, fmt.Errorf("CST supervisor identity: %w: %w", ErrSupervisorIPCUnavailable, err)
		}
		return zero, fmt.Errorf("CST supervisor identity: dial: %w", err)
	}
	defer conn.Close()
	if deadline, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(deadline)
	}
	observed, err := ObserveSupervisorStatusServerIdentityV1(conn)
	if err != nil {
		return zero, fmt.Errorf("CST supervisor identity: kernel bind: %w", err)
	}
	if err := ValidateSupervisorStatusServerIdentityV1(owner, observed, expectedUserSID, expectedSessionID, expectedImagePath); err != nil {
		return zero, fmt.Errorf("CST supervisor identity: kernel bind: %w", err)
	}
	if err := validateSupervisorIPCHello(conn, owner); err != nil {
		return zero, fmt.Errorf("CST supervisor identity: %w", err)
	}
	req := IPCRequest{Version: 1, ID: time.Now().UnixNano(), Cmd: SupervisorCstIdentityCommandV1}
	if err := writeSupervisorIPCRequest(conn, req); err != nil {
		return zero, fmt.Errorf("CST supervisor identity: send: %w", err)
	}
	resp, err := readSupervisorIPCResponse(conn)
	if err != nil {
		return zero, fmt.Errorf("CST supervisor identity: read: %w", err)
	}
	if resp.ID != req.ID || resp.Error != nil || !resp.OK || !resp.Final {
		return zero, fmt.Errorf("CST supervisor identity: denied or mismatched response")
	}
	dec := json.NewDecoder(bytes.NewReader(resp.Result))
	dec.DisallowUnknownFields()
	var identity SupervisorCstTaskIdentityV1
	if err := dec.Decode(&identity); err != nil {
		return zero, fmt.Errorf("CST supervisor identity: invalid closed response")
	}
	var trailing any
	if err := dec.Decode(&trailing); !errors.Is(err, io.EOF) {
		return zero, fmt.Errorf("CST supervisor identity: trailing response data")
	}
	if identity.Task != SupervisorCstTaskV1 || identity.PID <= 0 || identity.PIDGeneration <= 0 {
		return zero, fmt.Errorf("CST supervisor identity: invalid current task identity")
	}
	if _, err := parseSupervisorIdentityTime(identity.CreationTime); err != nil {
		return zero, fmt.Errorf("CST supervisor identity: invalid task creation time: %w", err)
	}
	return identity, nil
}

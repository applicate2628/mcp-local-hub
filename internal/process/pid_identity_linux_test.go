//go:build linux

package process

import (
	"errors"
	"os"
	"testing"
	"time"
)

func TestVerifyPIDIdentity_ExitedBeforeExecutableProof(t *testing.T) {
	pid := exitedProcessPID(t)
	state, err := QueryPIDState(pid)
	if err != nil {
		t.Fatalf("QueryPIDState(%d): %v", pid, err)
	}
	if state != PIDStateDead {
		t.Skipf("pid %d was reused before verification; state=%s", pid, state)
	}

	err = VerifyPIDIdentity(PIDIdentityProof{
		PID:            pid,
		ExecutablePath: os.Args[0],
		StartedAt:      time.Now().UTC().Format(time.RFC3339Nano),
	})
	if !errors.Is(err, ErrProcessAlreadyExited) {
		t.Fatalf("VerifyPIDIdentity(%d) error = %v, want ErrProcessAlreadyExited", pid, err)
	}
	if errors.Is(err, ErrProcessIdentityMismatch) {
		t.Fatalf("VerifyPIDIdentity(%d) error = %v, must not report identity mismatch for exited PID", pid, err)
	}
}

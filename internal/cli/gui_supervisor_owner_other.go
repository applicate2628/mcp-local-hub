//go:build !windows

package cli

import (
	"errors"
	"os"
	"time"

	"mcp-local-hub/internal/api"
	"mcp-local-hub/internal/process"
	"os/exec"
	"syscall"
)

// configureSupervisorDetach is the POSIX variant: spawn the
// supervisor in a new process session via Setsid so it does NOT
// inherit the GUI's process group. The supervisor's own Job (or
// process-group on POSIX) then owns daemon children — the GUI's
// process tree stays a pure UI surface.
//
// Setsid (vs just Setpgid) additionally severs the controlling
// terminal so the supervisor can outlive a hangup that closes the
// GUI's tty. This matches the production autostart contract: the
// supervisor is detached background, not a foreground job.
func configureSupervisorDetach(cmd *exec.Cmd) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.Setsid = true
}

func captureGUISupervisorGeneration(proc *os.Process) (api.SupervisorLockOwner, error) {
	if proc == nil {
		return api.SupervisorLockOwner{}, errSupervisorControlBinding
	}
	started, ok := process.ProcessStartTime(proc.Pid)
	if !ok || started.IsZero() {
		return api.SupervisorLockOwner{}, process.ErrProcessIdentityUnsupported
	}
	return api.SupervisorLockOwner{PID: proc.Pid, StartedAt: started.UTC().Format(time.RFC3339Nano)}, nil
}

func validateCreatedSupervisorNativeControl(expected SupervisorControlExpectation, owner api.SupervisorLockOwner) error {
	if err := process.VerifyPIDIdentity(process.PIDIdentityProof{PID: owner.PID, StartedAt: owner.StartedAt, ExecutablePath: expected.ProductPath}); err != nil {
		return err
	}
	sameUser, err := process.ProcessOwnerMatchesCurrent(owner.PID)
	if err != nil {
		return err
	}
	if !sameUser {
		return errors.New("created supervisor current user mismatch")
	}
	digest, err := startedSupervisorDigest(expected.ProductPath)
	if err != nil {
		return err
	}
	if digest != expected.ProductSHA256 {
		return errors.New("created supervisor executable identity changed")
	}
	return nil
}

//go:build !windows && !linux

package process

import (
	"fmt"
	"syscall"
)

func PIDExecutableMatches(pid int, expectedPath string) bool {
	_ = pid
	_ = expectedPath
	return false
}

func HoldPIDForTermination(pid int) (HeldPIDGeneration, error) {
	return nil, fmt.Errorf("%w: held PID generation unavailable for PID %d on this platform", ErrProcessIdentityUnsupported, pid)
}

func VerifyPIDIdentity(proof PIDIdentityProof) error {
	if proof.PID <= 0 {
		return fmt.Errorf("process: invalid PID %d", proof.PID)
	}
	return fmt.Errorf("%w: PID %d start-time proof unavailable on this platform", ErrProcessIdentityUnsupported, proof.PID)
}

func TerminatePIDWithIdentity(proof PIDIdentityProof) error {
	return VerifyPIDIdentity(proof)
}

func KillPIDWithIdentity(proof PIDIdentityProof, sig syscall.Signal) error {
	_ = sig
	return VerifyPIDIdentity(proof)
}

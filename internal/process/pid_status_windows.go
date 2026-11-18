//go:build windows

package process

import (
	"errors"
	"fmt"

	"golang.org/x/sys/windows"
)

// ObservePIDStatus checks exit, the existing image/start-time proof, and optional
// working-set bytes through one query/synchronize handle. It closes that handle
// before returning and preserves mandatory query and close errors independently.
// The existing start-time tolerance is unchanged. One handle binds the fields to
// one process object; it does not make them an atomic snapshot or prove creation
// by this caller. A signaled final check discards memory read before exit.
func ObservePIDStatus(proof PIDIdentityProof) (PIDStatusObservation, error) {
	return observePIDStatus(proof, pidStatusOps{
		open: windows.OpenProcess, wait: windows.WaitForSingleObject,
		verify: verifyHandleIdentity, rss: residentSetSizeFromHandle, close: windows.CloseHandle,
	})
}

type pidStatusOps struct {
	open   func(uint32, bool, uint32) (windows.Handle, error)
	wait   func(windows.Handle, uint32) (uint32, error)
	verify func(PIDIdentityProof, windows.Handle) error
	rss    func(windows.Handle) (uint64, bool)
	close  func(windows.Handle) error
}

func observePIDStatus(proof PIDIdentityProof, ops pidStatusOps) (observation PIDStatusObservation, err error) {
	observation.State = PIDStateUnknown
	if proof.PID <= 0 || uint64(proof.PID) > uint64(^uint32(0)) {
		return observation, fmt.Errorf("process: invalid status PID %d", proof.PID)
	}
	h, err := ops.open(windows.PROCESS_QUERY_LIMITED_INFORMATION|windows.SYNCHRONIZE, false, uint32(proof.PID))
	if err != nil {
		if errors.Is(err, windows.ERROR_INVALID_PARAMETER) {
			// Match QueryPIDState's absent-PID classification without losing the
			// native error. Other acquisition failures remain Unknown.
			observation.State = PIDStateDead
		}
		return observation, fmt.Errorf("process: open PID %d for status: %w", proof.PID, err)
	}
	defer func() {
		if closeErr := ops.close(h); closeErr != nil {
			err = errors.Join(err, fmt.Errorf("process: close PID %d status handle: %w", proof.PID, closeErr))
		}
		if err != nil || observation.State != PIDStateAlive {
			observation.ResidentBytes, observation.ResidentBytesKnown = 0, false
		}
	}()

	observation.State, err = pidStatusState(h, ops.wait)
	if err != nil || observation.State != PIDStateAlive {
		return observation, err
	}
	err = ops.verify(proof, h)
	if err == nil {
		// Missing memory is independently optional; it cannot invalidate a
		// successful image/start-time proof or change the exit observation.
		observation.ResidentBytes, observation.ResidentBytesKnown = ops.rss(h)
	}
	state, waitErr := pidStatusState(h, ops.wait)
	observation.State = state
	err = errors.Join(err, waitErr)
	return observation, err
}

func pidStatusState(h windows.Handle, wait func(windows.Handle, uint32) (uint32, error)) (PIDState, error) {
	result, err := wait(h, 0)
	if err != nil {
		return PIDStateUnknown, fmt.Errorf("process: wait status handle: %w", err)
	}
	switch result {
	case windows.WAIT_OBJECT_0:
		return PIDStateDead, nil
	case uint32(windows.WAIT_TIMEOUT):
		return PIDStateAlive, nil
	default:
		return PIDStateUnknown, fmt.Errorf("process: status wait returned unexpected code %d", result)
	}
}

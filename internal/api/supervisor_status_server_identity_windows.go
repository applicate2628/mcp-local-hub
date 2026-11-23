//go:build windows

package api

import (
	"errors"
	"fmt"
	"io"
	"net"
	"time"

	"mcp-local-hub/internal/process"

	"golang.org/x/sys/windows"
)

// ObserveSupervisorStatusServerIdentityV1 derives the named-pipe server proof
// exclusively from the connected kernel handle and the exact process/token.
// The caller must compare it with ValidateSupervisorStatusServerIdentityV1
// before sending GET_CURRENT_CST_TASK_IDENTITY_V1.
func ObserveSupervisorStatusServerIdentityV1(conn net.Conn) (SupervisorProcessIdentityV1, error) {
	return observeSupervisorStatusServerIdentityV1(conn, false)
}

func observeSupervisorStatusServerIdentityV1(conn net.Conn, checked bool) (SupervisorProcessIdentityV1, error) {
	mode := process.WindowsIdentityLegacyCompatibility
	if checked {
		mode = process.WindowsIdentityCheckedCompatibility
	}
	return projectSupervisorProcessIdentityV1(process.ObserveWindowsPipeServerIdentity(conn, mode), checked)
}

// ObserveSupervisorIPCClientIdentityV1 reads the peer of this same accepted
// connection, including checked release of temporary native handles.
func ObserveSupervisorIPCClientIdentityV1(conn net.Conn) (SupervisorProcessIdentityV1, error) {
	fdConn, ok := conn.(interface{ Fd() uintptr })
	if !ok {
		return SupervisorProcessIdentityV1{}, fmt.Errorf("supervisor pipe connection does not expose a kernel handle")
	}
	var pid uint32
	if err := windows.GetNamedPipeClientProcessId(windows.Handle(fdConn.Fd()), &pid); err != nil {
		return SupervisorProcessIdentityV1{}, fmt.Errorf("query supervisor pipe client PID: %w", err)
	}
	if pid == 0 {
		return SupervisorProcessIdentityV1{}, fmt.Errorf("supervisor pipe client has no PID")
	}
	return projectSupervisorProcessIdentityV1(process.ObserveWindowsProcessIdentity(pid, process.WindowsIdentityCheckedCompatibility), true)
}

func ObserveSupervisorIPCClientIdentityObserved(conn net.Conn) (SupervisorProcessIdentityV1, *process.WindowsIdentityObservation, error) {
	observation := process.ObserveWindowsPipeClientIdentityObserved(conn)
	identity, err := projectSupervisorProcessIdentityV1(observation, true)
	if err == nil {
		err = observation.ValidateObservedPrincipal()
	}
	return identity, &observation, err
}

// ObserveSupervisorProcessIdentityV1 observes native process data. It conveys no
// creator/termination capability; callers must bind it to their original creator.
func ObserveSupervisorProcessIdentityV1(pid int) (SupervisorProcessIdentityV1, error) {
	if pid <= 0 {
		return SupervisorProcessIdentityV1{}, fmt.Errorf("%w: original process required", ErrSupervisorControlPeerIdentity)
	}
	identity, err := projectSupervisorProcessIdentityV1(process.ObserveWindowsProcessIdentity(uint32(pid), process.WindowsIdentityCheckedCompatibility), true)
	if err == nil {
		identity.PID = pid
	}
	return identity, err
}

func projectSupervisorProcessIdentityV1(observation process.WindowsIdentityObservation, checked bool) (SupervisorProcessIdentityV1, error) {
	processClose, tokenClose := observation.ProcessClose, observation.TokenClose
	var err error
	if failure := observation.QueryError; failure != nil {
		cause := failure.Cause
		// On an unsuccessful open, the temporary close belongs inside the
		// open error. Closures after successful opens follow the query error.
		if checked {
			switch failure.Query {
			case process.WindowsIdentityOpenProcess:
				if processClose.Attempted {
					cause = errors.Join(cause, processClose.Err)
					processClose.Attempted = false
				}
			case process.WindowsIdentityOpenToken:
				if tokenClose.Attempted {
					cause = errors.Join(cause, tokenClose.Err)
					tokenClose.Attempted = false
				}
			}
		}
		err = supervisorProcessIdentityQueryError(failure.Query, cause, checked)
	}
	if checked && tokenClose.Attempted {
		err = errors.Join(err, tokenClose.Err)
	}
	if checked && processClose.Attempted {
		err = errors.Join(err, processClose.Err)
	}
	if err != nil {
		return SupervisorProcessIdentityV1{}, err
	}
	identity := observation.Identity
	creation := windows.Filetime{
		HighDateTime: uint32(identity.NativeBirthFILETIME >> 32),
		LowDateTime:  uint32(identity.NativeBirthFILETIME),
	}
	return SupervisorProcessIdentityV1{
		PID:          int(identity.PID),
		CreationTime: time.Unix(0, creation.Nanoseconds()).UTC().Format(time.RFC3339Nano),
		UserSID:      identity.UserSID,
		SessionID:    identity.SessionID,
		IntegrityRID: identity.IntegrityRID,
		ImagePath:    identity.ImagePath,
	}, nil
}

func supervisorProcessIdentityQueryError(query process.WindowsIdentityQuery, cause error, checked bool) error {
	switch query {
	case process.WindowsIdentityPipeHandle:
		return fmt.Errorf("supervisor pipe connection does not expose a kernel handle")
	case process.WindowsIdentityPipeServerPID:
		return fmt.Errorf("query supervisor pipe server PID: %w", cause)
	case process.WindowsIdentityOpenProcess:
		return fmt.Errorf("open supervisor pipe server PID: %w", cause)
	case process.WindowsIdentityImage:
		if checked {
			return fmt.Errorf("query supervisor image path: %w", cause)
		}
		return fmt.Errorf("query supervisor image path")
	case process.WindowsIdentityProcessTimes:
		return fmt.Errorf("query supervisor process times: %w", cause)
	case process.WindowsIdentityOpenToken:
		return fmt.Errorf("open supervisor process token: %w", cause)
	case process.WindowsIdentityTokenUser:
		return fmt.Errorf("query supervisor token user: %w", cause)
	case process.WindowsIdentitySession:
		return fmt.Errorf("query supervisor session: %w", cause)
	case process.WindowsIdentityIntegritySize:
		return fmt.Errorf("size supervisor token integrity: %w", cause)
	case process.WindowsIdentityIntegrityQuery:
		return fmt.Errorf("query supervisor token integrity: %w", cause)
	case process.WindowsIdentityIntegrityRID:
		return fmt.Errorf("supervisor integrity SID has no RID")
	default:
		return fmt.Errorf("query supervisor process identity (%s): %w", query, cause)
	}
}

func querySupervisorStatusImage(h windows.Handle) (string, error) {
	image, err := process.QueryWindowsIdentityImageCompatible(h)
	if err != nil {
		return "", supervisorProcessIdentityQueryError(process.WindowsIdentityImage, err, false)
	}
	return image, nil
}

func supervisorStatusTokenIntegrityRID(token windows.Token) (uint32, error) {
	rid, err := process.QueryWindowsIdentityIntegrityRID(token)
	if err != nil {
		return 0, supervisorProcessIdentityQueryError(err.Query, err.Cause, false)
	}
	return rid, nil
}

func aggregateRoutePeerClosed(err error) bool {
	return errors.Is(err, io.EOF) || errors.Is(err, windows.ERROR_BROKEN_PIPE) ||
		errors.Is(err, windows.ERROR_NO_DATA) || errors.Is(err, windows.ERROR_PIPE_NOT_CONNECTED)
}

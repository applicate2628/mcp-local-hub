//go:build windows

package api

import (
	"errors"
	"fmt"
	"net"
	"time"
	"unsafe"

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
	var zero SupervisorProcessIdentityV1
	fdConn, ok := conn.(interface{ Fd() uintptr })
	if !ok {
		return zero, fmt.Errorf("supervisor pipe connection does not expose a kernel handle")
	}
	var pid uint32
	if err := windows.GetNamedPipeServerProcessId(windows.Handle(fdConn.Fd()), &pid); err != nil || pid == 0 {
		return zero, fmt.Errorf("query supervisor pipe server PID: %w", err)
	}
	return observeSupervisorProcessIdentityV1(int(pid), checked)
}

// ObserveSupervisorProcessIdentityV1 observes native process data. It conveys no
// creator/termination capability; callers must bind it to their original creator.
func ObserveSupervisorProcessIdentityV1(pid int) (SupervisorProcessIdentityV1, error) {
	if pid <= 0 {
		return SupervisorProcessIdentityV1{}, fmt.Errorf("%w: original process required", ErrSupervisorControlPeerIdentity)
	}
	return observeSupervisorProcessIdentityV1(pid, true)
}

// Both the existing CST observer and strict creator/connection projections use
// this one capture body. Only the strict projection receives closure errors.
func observeSupervisorProcessIdentityV1(pid int, checked bool) (identity SupervisorProcessIdentityV1, retErr error) {
	var zero SupervisorProcessIdentityV1
	h, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(pid))
	if err != nil {
		if checked && h != 0 {
			err = errors.Join(err, windows.CloseHandle(h))
		}
		return zero, fmt.Errorf("open supervisor pipe server PID: %w", err)
	}
	defer func() {
		closeErr := windows.CloseHandle(h)
		if checked {
			retErr = errors.Join(retErr, closeErr)
			if retErr != nil {
				identity = zero
			}
		}
	}()
	image, err := querySupervisorStatusImageWithResult(h, checked)
	if err != nil {
		return zero, err
	}
	var creation, exit, kernel, user windows.Filetime
	if err := windows.GetProcessTimes(h, &creation, &exit, &kernel, &user); err != nil {
		return zero, fmt.Errorf("query supervisor process times: %w", err)
	}
	var token windows.Token
	if err := windows.OpenProcessToken(h, windows.TOKEN_QUERY, &token); err != nil {
		if checked && token != 0 {
			err = errors.Join(err, token.Close())
		}
		return zero, fmt.Errorf("open supervisor process token: %w", err)
	}
	defer func() {
		closeErr := token.Close()
		if checked {
			retErr = errors.Join(retErr, closeErr)
		}
	}()
	tokenUser, err := token.GetTokenUser()
	if err != nil {
		return zero, fmt.Errorf("query supervisor token user: %w", err)
	}
	var session uint32
	if err := windows.ProcessIdToSessionId(uint32(pid), &session); err != nil {
		return zero, fmt.Errorf("query supervisor session: %w", err)
	}
	integrity, err := supervisorStatusTokenIntegrityRID(token)
	if err != nil {
		return zero, err
	}
	return SupervisorProcessIdentityV1{
		PID:          int(pid),
		CreationTime: time.Unix(0, creation.Nanoseconds()).UTC().Format(time.RFC3339Nano),
		UserSID:      tokenUser.User.Sid.String(),
		SessionID:    session,
		IntegrityRID: integrity,
		ImagePath:    image,
	}, nil
}

func querySupervisorStatusImage(h windows.Handle) (string, error) {
	return querySupervisorStatusImageWithResult(h, false)
}

func querySupervisorStatusImageWithResult(h windows.Handle, checked bool) (string, error) {
	var lastErr error
	for size := uint32(windows.MAX_PATH); size <= 32768; size *= 2 {
		buf := make([]uint16, size)
		n := size
		if err := windows.QueryFullProcessImageName(h, 0, &buf[0], &n); err == nil {
			return windows.UTF16ToString(buf[:n]), nil
		} else {
			lastErr = err
		}
	}
	if checked {
		return "", fmt.Errorf("query supervisor image path: %w", lastErr)
	}
	return "", fmt.Errorf("query supervisor image path")
}

func supervisorStatusTokenIntegrityRID(token windows.Token) (uint32, error) {
	var needed uint32
	err := windows.GetTokenInformation(token, windows.TokenIntegrityLevel, nil, 0, &needed)
	if err != windows.ERROR_INSUFFICIENT_BUFFER || needed == 0 {
		return 0, fmt.Errorf("size supervisor token integrity: %w", err)
	}
	buf := make([]byte, needed)
	if err := windows.GetTokenInformation(token, windows.TokenIntegrityLevel, &buf[0], needed, &needed); err != nil {
		return 0, fmt.Errorf("query supervisor token integrity: %w", err)
	}
	ml := (*windows.Tokenmandatorylabel)(unsafe.Pointer(&buf[0]))
	sid := ml.Label.Sid
	count := sid.SubAuthorityCount()
	if count == 0 {
		return 0, fmt.Errorf("supervisor integrity SID has no RID")
	}
	return sid.SubAuthority(uint32(count - 1)), nil
}

//go:build windows

package api

import (
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
	var zero SupervisorProcessIdentityV1
	fdConn, ok := conn.(interface{ Fd() uintptr })
	if !ok {
		return zero, fmt.Errorf("supervisor pipe connection does not expose a kernel handle")
	}
	var pid uint32
	if err := windows.GetNamedPipeServerProcessId(windows.Handle(fdConn.Fd()), &pid); err != nil || pid == 0 {
		return zero, fmt.Errorf("query supervisor pipe server PID: %w", err)
	}
	h, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, pid)
	if err != nil {
		return zero, fmt.Errorf("open supervisor pipe server PID: %w", err)
	}
	defer windows.CloseHandle(h)
	image, err := querySupervisorStatusImage(h)
	if err != nil {
		return zero, err
	}
	var creation, exit, kernel, user windows.Filetime
	if err := windows.GetProcessTimes(h, &creation, &exit, &kernel, &user); err != nil {
		return zero, fmt.Errorf("query supervisor process times: %w", err)
	}
	var token windows.Token
	if err := windows.OpenProcessToken(h, windows.TOKEN_QUERY, &token); err != nil {
		return zero, fmt.Errorf("open supervisor process token: %w", err)
	}
	defer token.Close()
	tokenUser, err := token.GetTokenUser()
	if err != nil {
		return zero, fmt.Errorf("query supervisor token user: %w", err)
	}
	var session uint32
	if err := windows.ProcessIdToSessionId(pid, &session); err != nil {
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
	for size := uint32(windows.MAX_PATH); size <= 32768; size *= 2 {
		buf := make([]uint16, size)
		n := size
		if err := windows.QueryFullProcessImageName(h, 0, &buf[0], &n); err == nil {
			return windows.UTF16ToString(buf[:n]), nil
		}
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

//go:build windows

package cli

import (
	"fmt"
	"net"
	"path/filepath"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/svc/mgr"

	"mcp-local-hub/internal/api"
)

const supervisorCstDaemonServiceName = "McpLocalHubCstDaemon"

func supervisorCstDaemonPeerIdentity(conn net.Conn) (api.SupervisorProcessIdentityV1, api.SupervisorCstIdentityPolicyV1, bool, error) {
	var zeroPeer api.SupervisorProcessIdentityV1
	var zeroPolicy api.SupervisorCstIdentityPolicyV1
	fdConn, ok := conn.(interface{ Fd() uintptr })
	if !ok {
		return zeroPeer, zeroPolicy, false, fmt.Errorf("supervisor pipe connection does not expose a kernel handle")
	}
	pipe := windows.Handle(fdConn.Fd())
	var pid uint32
	if err := windows.GetNamedPipeClientProcessId(pipe, &pid); err != nil || pid == 0 {
		return zeroPeer, zeroPolicy, false, fmt.Errorf("query supervisor pipe client PID: %w", err)
	}

	serviceSID, _, _, err := windows.LookupSID("", `NT SERVICE\`+supervisorCstDaemonServiceName)
	if err != nil {
		return zeroPeer, zeroPolicy, false, fmt.Errorf("resolve CST daemon service SID: %w", err)
	}
	manager, err := mgr.Connect()
	if err != nil {
		return zeroPeer, zeroPolicy, false, fmt.Errorf("open SCM: %w", err)
	}
	defer manager.Disconnect()
	service, err := manager.OpenService(supervisorCstDaemonServiceName)
	if err != nil {
		return zeroPeer, zeroPolicy, false, fmt.Errorf("open CST daemon service: %w", err)
	}
	defer service.Close()
	status, err := service.Query()
	if err != nil {
		return zeroPeer, zeroPolicy, false, fmt.Errorf("query CST daemon service status: %w", err)
	}
	config, err := service.Config()
	if err != nil {
		return zeroPeer, zeroPolicy, false, fmt.Errorf("query CST daemon service config: %w", err)
	}
	argv, err := windows.DecomposeCommandLine(config.BinaryPathName)
	if err != nil || len(argv) == 0 || argv[0] == "" {
		return zeroPeer, zeroPolicy, false, fmt.Errorf("parse CST daemon service image path: %w", err)
	}
	policy := api.SupervisorCstIdentityPolicyV1{
		DaemonServiceSID: serviceSID.String(),
		DaemonImagePath:  filepath.Clean(argv[0]),
		DaemonSessionID:  0,
		MinimumIntegrity: api.SupervisorIntegrityHigh,
	}

	h, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, pid)
	if err != nil {
		return zeroPeer, policy, false, fmt.Errorf("open CST daemon pipe client PID: %w", err)
	}
	defer windows.CloseHandle(h)
	image, err := querySupervisorCstProcessImage(h)
	if err != nil {
		return zeroPeer, policy, false, err
	}
	var creation, exit, kernel, user windows.Filetime
	if err := windows.GetProcessTimes(h, &creation, &exit, &kernel, &user); err != nil {
		return zeroPeer, policy, false, fmt.Errorf("query CST daemon process times: %w", err)
	}
	var token windows.Token
	if err := windows.OpenProcessToken(h, windows.TOKEN_QUERY, &token); err != nil {
		return zeroPeer, policy, false, fmt.Errorf("open CST daemon process token: %w", err)
	}
	defer token.Close()
	tokenUser, err := token.GetTokenUser()
	if err != nil {
		return zeroPeer, policy, false, fmt.Errorf("query CST daemon token user: %w", err)
	}
	var session uint32
	if err := windows.ProcessIdToSessionId(pid, &session); err != nil {
		return zeroPeer, policy, false, fmt.Errorf("query CST daemon session: %w", err)
	}
	integrity, err := supervisorCstTokenIntegrityRID(token)
	if err != nil {
		return zeroPeer, policy, false, err
	}
	peer := api.SupervisorProcessIdentityV1{
		PID:           int(pid),
		CreationTime:  time.Unix(0, creation.Nanoseconds()).UTC().Format(time.RFC3339Nano),
		UserSID:       tokenUser.User.Sid.String(),
		SessionID:     session,
		IntegrityRID:  integrity,
		ImagePath:     image,
		SCMServicePID: int(status.ProcessId),
	}
	return peer, policy, tokenUser.User.Sid.Equals(serviceSID), nil
}

func querySupervisorCstProcessImage(h windows.Handle) (string, error) {
	for size := uint32(windows.MAX_PATH); size <= 32768; size *= 2 {
		buf := make([]uint16, size)
		n := size
		if err := windows.QueryFullProcessImageName(h, 0, &buf[0], &n); err == nil {
			return windows.UTF16ToString(buf[:n]), nil
		}
	}
	return "", fmt.Errorf("query CST daemon image path")
}

func supervisorCstTokenIntegrityRID(token windows.Token) (uint32, error) {
	var needed uint32
	err := windows.GetTokenInformation(token, windows.TokenIntegrityLevel, nil, 0, &needed)
	if err != windows.ERROR_INSUFFICIENT_BUFFER || needed == 0 {
		return 0, fmt.Errorf("size CST daemon token integrity: %w", err)
	}
	buf := make([]byte, needed)
	if err := windows.GetTokenInformation(token, windows.TokenIntegrityLevel, &buf[0], needed, &needed); err != nil {
		return 0, fmt.Errorf("query CST daemon token integrity: %w", err)
	}
	ml := (*windows.Tokenmandatorylabel)(unsafe.Pointer(&buf[0]))
	sid := ml.Label.Sid
	count := sid.SubAuthorityCount()
	if count == 0 {
		return 0, fmt.Errorf("CST daemon token integrity SID has no RID")
	}
	return sid.SubAuthority(uint32(count - 1)), nil
}

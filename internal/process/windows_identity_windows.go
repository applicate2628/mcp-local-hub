//go:build windows

package process

import (
	"errors"
	"net"
	"unsafe"

	"golang.org/x/sys/windows"
)

// WindowsProcessIdentity is native process data without a retained handle or
// creator capability. NativeBirthFILETIME is the raw Windows creation time.
type WindowsProcessIdentity struct {
	PID                 uint32
	NativeBirthFILETIME uint64
	UserSID             string
	SessionID           uint32
	IntegrityRID        uint32
	ImagePath           string
}

type WindowsIdentityQuery string

const (
	WindowsIdentityPipeHandle     WindowsIdentityQuery = "pipe connection does not expose a kernel handle"
	WindowsIdentityPipeServerPID  WindowsIdentityQuery = "query pipe server PID"
	WindowsIdentityPipeClientPID  WindowsIdentityQuery = "query pipe client PID"
	WindowsIdentityOpenProcess    WindowsIdentityQuery = "open process"
	WindowsIdentityImage          WindowsIdentityQuery = "query process image path"
	WindowsIdentityProcessTimes   WindowsIdentityQuery = "query process times"
	WindowsIdentityOpenToken      WindowsIdentityQuery = "open process token"
	WindowsIdentityTokenUser      WindowsIdentityQuery = "query token user"
	WindowsIdentityTokenUserSID   WindowsIdentityQuery = "format token user SID"
	WindowsIdentitySession        WindowsIdentityQuery = "query process session"
	WindowsIdentityIntegritySize  WindowsIdentityQuery = "size token integrity"
	WindowsIdentityIntegrityQuery WindowsIdentityQuery = "query token integrity"
	WindowsIdentityIntegrityRID   WindowsIdentityQuery = "integrity SID has no RID"
)

// WindowsIdentityQueryError retains the failed operation and its native cause.
type WindowsIdentityQueryError struct {
	Query WindowsIdentityQuery
	Cause error
}

func (e *WindowsIdentityQueryError) Error() string {
	if e.Cause == nil {
		return string(e.Query)
	}
	return string(e.Query) + ": " + e.Cause.Error()
}

func (e *WindowsIdentityQueryError) Unwrap() error { return e.Cause }

// WindowsIdentityQueryMode selects the native query and cleanup behavior.
// Compatibility modes preserve the existing Supervisor observation contract.
type WindowsIdentityQueryMode uint8

const (
	WindowsIdentityStrict WindowsIdentityQueryMode = iota
	WindowsIdentityCheckedCompatibility
	WindowsIdentityLegacyCompatibility
	WindowsIdentityObservedCompatibility
)

// WindowsIdentityObservation preserves the query and temporary close outcomes.
// Call Strict to obtain identity data for a new qualification decision.
type WindowsIdentityObservation struct {
	Identity     WindowsProcessIdentity
	QueryError   *WindowsIdentityQueryError
	TokenClose   WindowsIdentityClose
	ProcessClose WindowsIdentityClose

	// SIDFormatting retains the original conversion and allocation outcomes.
	SIDFormatting *SIDFormatObservation

	strictQueries bool
	finalized     bool
}

func (o *WindowsIdentityObservation) ResourcesFinalized() bool {
	return o != nil && o.finalized && o.ProcessClose.ResourcesFinalized() && o.TokenClose.ResourcesFinalized() &&
		(o.SIDFormatting == nil || o.SIDFormatting.ResourcesFinalized())
}

func (o *WindowsIdentityObservation) ValidateObservedPrincipal() error {
	if o == nil || !o.ResourcesFinalized() || o.SIDFormatting == nil ||
		!o.ProcessClose.Acquired || !o.TokenClose.Acquired {
		return errors.New("original observed Windows process principal required")
	}
	return o.SIDFormatting.Validate(o.Identity.UserSID)
}

func (o *WindowsIdentityObservation) observedUserSID() *windows.SID {
	if o == nil || o.SIDFormatting == nil {
		return nil
	}
	sid, _ := o.SIDFormatting.inputOwner.(*windows.SID)
	return sid
}

func (o *WindowsIdentityObservation) IsLocalSystemPrincipal() bool {
	sid := o.observedUserSID()
	return sid != nil && sid.IsWellKnown(windows.WinLocalSystemSid)
}

func (o *WindowsIdentityObservation) MatchesPrincipalSID(expected *windows.SID) bool {
	sid := o.observedUserSID()
	return sid != nil && expected != nil && sid.Equals(expected)
}

func closeWindowsIdentityHandle(value uintptr, close func() error) (result WindowsIdentityClose) {
	result = WindowsIdentityClose{Acquired: value != 0, Attempted: true, native: value}
	result.Err = close()
	result.Returned = true
	return result
}

// Strict requires a complete capture and successful temporary handle closure.
func (o WindowsIdentityObservation) Strict() (WindowsProcessIdentity, error) {
	var queryErr error
	if o.QueryError != nil {
		queryErr = o.QueryError
	}
	if err := errors.Join(queryErr, o.TokenClose.Err, o.ProcessClose.Err); err != nil {
		return WindowsProcessIdentity{}, err
	}
	if !o.strictQueries {
		return WindowsProcessIdentity{}, errors.New("strict Windows process identity queries required")
	}
	identity := o.Identity
	if !o.TokenClose.Attempted || !o.ProcessClose.Attempted || identity.PID == 0 || identity.NativeBirthFILETIME == 0 || identity.ImagePath == "" || identity.UserSID == "" {
		return WindowsProcessIdentity{}, errors.New("incomplete Windows process identity observation")
	}
	return identity, nil
}

// ObserveWindowsPipeServerIdentity borrows the connection's kernel handle.
// The caller keeps the same connection open until the observation returns.
func ObserveWindowsPipeServerIdentity(conn net.Conn, mode WindowsIdentityQueryMode) WindowsIdentityObservation {
	fdConn, ok := conn.(interface{ Fd() uintptr })
	if !ok {
		return WindowsIdentityObservation{QueryError: &WindowsIdentityQueryError{Query: WindowsIdentityPipeHandle}}
	}
	var pid uint32
	if err := windows.GetNamedPipeServerProcessId(windows.Handle(fdConn.Fd()), &pid); err != nil || pid == 0 {
		return WindowsIdentityObservation{QueryError: &WindowsIdentityQueryError{Query: WindowsIdentityPipeServerPID, Cause: err}}
	}
	return ObserveWindowsProcessIdentity(pid, mode)
}

// ObserveWindowsPipeClientIdentityObserved derives the actual accepted
// pipe's peer and captures one checked identity with observed SID release.
func ObserveWindowsPipeClientIdentityObserved(conn net.Conn) WindowsIdentityObservation {
	fdConn, ok := conn.(interface{ Fd() uintptr })
	if !ok {
		return WindowsIdentityObservation{finalized: true, QueryError: &WindowsIdentityQueryError{Query: WindowsIdentityPipeHandle}}
	}
	var pid uint32
	if err := windows.GetNamedPipeClientProcessId(windows.Handle(fdConn.Fd()), &pid); err != nil || pid == 0 {
		return WindowsIdentityObservation{finalized: true, QueryError: &WindowsIdentityQueryError{Query: WindowsIdentityPipeClientPID, Cause: err}}
	}
	return ObserveWindowsProcessIdentity(pid, WindowsIdentityObservedCompatibility)
}

// ObserveWindowsProcessIdentity owns and closes only its temporary query handles.
func ObserveWindowsProcessIdentity(pid uint32, mode WindowsIdentityQueryMode) (observation WindowsIdentityObservation) {
	defer func() { observation.finalized = true }()
	strict := mode == WindowsIdentityStrict
	observation.strictQueries = strict
	closePartial := mode != WindowsIdentityLegacyCompatibility
	h, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, pid)
	if err != nil {
		if closePartial && h != 0 {
			observation.ProcessClose = closeWindowsIdentityHandle(uintptr(h), func() error { return windows.CloseHandle(h) })
		}
		observation.QueryError = &WindowsIdentityQueryError{Query: WindowsIdentityOpenProcess, Cause: err}
		return observation
	}
	defer func() {
		observation.ProcessClose = closeWindowsIdentityHandle(uintptr(h), func() error { return windows.CloseHandle(h) })
	}()
	queryImage := queryProcessImagePath
	if !strict {
		queryImage = QueryWindowsIdentityImageCompatible
	}
	image, err := queryImage(h)
	if err != nil {
		observation.QueryError = &WindowsIdentityQueryError{Query: WindowsIdentityImage, Cause: err}
		return observation
	}
	var creation, exit, kernel, user windows.Filetime
	if err := windows.GetProcessTimes(h, &creation, &exit, &kernel, &user); err != nil {
		observation.QueryError = &WindowsIdentityQueryError{Query: WindowsIdentityProcessTimes, Cause: err}
		return observation
	}
	var token windows.Token
	if err := windows.OpenProcessToken(h, windows.TOKEN_QUERY, &token); err != nil {
		if closePartial && token != 0 {
			observation.TokenClose = closeWindowsIdentityHandle(uintptr(token), token.Close)
		}
		observation.QueryError = &WindowsIdentityQueryError{Query: WindowsIdentityOpenToken, Cause: err}
		return observation
	}
	defer func() {
		observation.TokenClose = closeWindowsIdentityHandle(uintptr(token), token.Close)
	}()
	tokenUser, err := token.GetTokenUser()
	if err != nil {
		observation.QueryError = &WindowsIdentityQueryError{Query: WindowsIdentityTokenUser, Cause: err}
		return observation
	}
	var session uint32
	if err := windows.ProcessIdToSessionId(pid, &session); err != nil {
		observation.QueryError = &WindowsIdentityQueryError{Query: WindowsIdentitySession, Cause: err}
		return observation
	}
	integrity, integrityErr := QueryWindowsIdentityIntegrityRID(token)
	if integrityErr != nil {
		observation.QueryError = integrityErr
		return observation
	}
	var userSID string
	if strict || mode == WindowsIdentityObservedCompatibility {
		var formatErr error
		userSID, observation.SIDFormatting, formatErr = FormatSIDObserved(tokenUser.User.Sid)
		if formatErr == nil && mode == WindowsIdentityObservedCompatibility {
			formatErr = observation.SIDFormatting.ValidateInput(tokenUser.User.Sid, userSID)
		}
		if formatErr != nil {
			observation.QueryError = &WindowsIdentityQueryError{Query: WindowsIdentityTokenUserSID, Cause: formatErr}
			return observation
		}
	} else {
		userSID = tokenUser.User.Sid.String()
	}
	observation.Identity = WindowsProcessIdentity{
		PID:                 pid,
		NativeBirthFILETIME: uint64(creation.HighDateTime)<<32 | uint64(creation.LowDateTime),
		UserSID:             userSID,
		SessionID:           session,
		IntegrityRID:        integrity,
		ImagePath:           image,
	}
	return observation
}

// QueryWindowsIdentityImageCompatible keeps the existing image retry behavior
// for Supervisor callers and their peer fixtures. It borrows the process handle.
func QueryWindowsIdentityImageCompatible(h windows.Handle) (string, error) {
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
	return "", lastErr
}

// QueryWindowsIdentityIntegrityRID borrows an existing token for the query.
func QueryWindowsIdentityIntegrityRID(token windows.Token) (uint32, *WindowsIdentityQueryError) {
	var needed uint32
	err := windows.GetTokenInformation(token, windows.TokenIntegrityLevel, nil, 0, &needed)
	if err != windows.ERROR_INSUFFICIENT_BUFFER || needed == 0 {
		return 0, &WindowsIdentityQueryError{Query: WindowsIdentityIntegritySize, Cause: err}
	}
	buf := make([]byte, needed)
	if err := windows.GetTokenInformation(token, windows.TokenIntegrityLevel, &buf[0], needed, &needed); err != nil {
		return 0, &WindowsIdentityQueryError{Query: WindowsIdentityIntegrityQuery, Cause: err}
	}
	ml := (*windows.Tokenmandatorylabel)(unsafe.Pointer(&buf[0]))
	sid := ml.Label.Sid
	count := sid.SubAuthorityCount()
	if count == 0 {
		return 0, &WindowsIdentityQueryError{Query: WindowsIdentityIntegrityRID}
	}
	return sid.SubAuthority(uint32(count - 1)), nil
}

package gui

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"mcp-local-hub/internal/api"
)

func closeManagedRouterIdentityForTest(identity *ProcessIdentity) error {
	if identity != nil {
		identity.Handle = 0
	}
	return nil
}

func TestManagedRouterLease_CloseCachesFailureOnce(t *testing.T) {
	closeErr := errors.New("native retained-handle close failed")
	closeCalls := 0
	lease := managedRouterLease{closeFn: func() error {
		closeCalls++
		return closeErr
	}}

	if err := lease.Close(); !errors.Is(err, closeErr) {
		t.Fatalf("first Close() error = %v, want native close error", err)
	}
	if err := lease.Close(); !errors.Is(err, closeErr) {
		t.Fatalf("second Close() error = %v, want cached native close error", err)
	}
	if closeCalls != 1 {
		t.Fatalf("native close calls = %d, want 1", closeCalls)
	}
}

func TestManagedRouterAuthorizer_V2RecordRequiresExactProcessStart(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "gui.pidport")
	start := time.Now().Add(-time.Minute).UTC().Round(0)
	record := GUIOwnerRecord{
		Version: guiOwnerRecordVersion, State: guiOwnerStateActive, PID: 4242,
		StartTime: start, Port: 19125, Generation: guiOwnerGeneration(4242, start),
	}
	if err := writeGUIOwnerRecord(path, record); err != nil {
		t.Fatal(err)
	}
	executable := filepath.Join(root, "mcphub.exe")
	authorize := newManagedRouterAuthorizerWithDeps(path, executable, "1.2.3", managedRouterAuthorizerDeps{
		readOwnerRecord: func(string) (GUIOwnerRecord, error) { return record, nil },
		statPidport:     os.Stat,
		portOwner:       func(context.Context, int) (int, bool, error) { return 4242, true, nil },
		processID: func(int) (ProcessIdentity, error) {
			return ProcessIdentity{Alive: true, ImagePath: executable, Cmdline: []string{executable, "gui"}, StartTime: start.Add(time.Nanosecond), Handle: 1}, nil
		},
		closeID:    closeManagedRouterIdentityForTest,
		ownerMatch: func(int) (bool, error) { return true, nil },
		ping: func(context.Context, int) (managedRouterPing, string) {
			t.Fatal("ping after generation mismatch")
			return managedRouterPing{}, ""
		},
	})
	result := authorize(context.Background(), record.Port)
	if result.Lease != nil || result.FailureClass != api.ManagedRouterFailureProcessGenerationInvalid {
		t.Fatalf("authorization = %#v, want exact-start refusal", result)
	}
}

type managedRouterReleaseEventForTest struct {
	class string
	err   error
}

type managedRouterProcessObservationForTest struct {
	identity ProcessIdentity
	err      error
}

type managedRouterPingObservationForTest struct {
	ping    managedRouterPing
	failure string
}

type managedRouterAuthorizerPathHarness struct {
	t           *testing.T
	pid         int
	port        int
	version     string
	pidportPath string
	executable  string
	start       time.Time

	readPID        int
	readPort       int
	readErr        error
	statErr        error
	portOwnerPID   int
	portOwnerFound bool
	portOwnerErr   error
	ownerMatches   bool
	ownerMatchErr  error
	processes      []managedRouterProcessObservationForTest
	pings          []managedRouterPingObservationForTest
	processIndex   int
	pingIndex      int

	closeFailures map[uintptr]error
	closed        map[uintptr]int
	events        []managedRouterReleaseEventForTest
}

func newManagedRouterAuthorizerPathHarness(t *testing.T) *managedRouterAuthorizerPathHarness {
	t.Helper()
	const (
		pid     = 4242
		port    = 19125
		version = "1.2.3"
	)
	root := t.TempDir()
	pidportPath := filepath.Join(root, "gui.pidport")
	if err := os.WriteFile(pidportPath, []byte("fixture"), 0o600); err != nil {
		t.Fatal(err)
	}
	mtime := time.Now().Add(-time.Minute).Round(0)
	if err := os.Chtimes(pidportPath, mtime, mtime); err != nil {
		t.Fatal(err)
	}
	h := &managedRouterAuthorizerPathHarness{
		t:              t,
		pid:            pid,
		port:           port,
		version:        version,
		pidportPath:    pidportPath,
		executable:     filepath.Join(root, "mcphub.exe"),
		start:          mtime.Add(-time.Minute),
		readPID:        pid,
		readPort:       port,
		portOwnerPID:   pid,
		portOwnerFound: true,
		ownerMatches:   true,
		closeFailures:  map[uintptr]error{},
		closed:         map[uintptr]int{},
	}
	h.pings = []managedRouterPingObservationForTest{{ping: managedRouterPing{OK: true, PID: h.pid, Version: h.version}}}
	return h
}

func (h *managedRouterAuthorizerPathHarness) identity(handle uintptr) ProcessIdentity {
	return ProcessIdentity{
		Alive:     true,
		ImagePath: h.executable,
		Cmdline:   []string{h.executable, "gui"},
		StartTime: h.start,
		Handle:    handle,
	}
}

func (h *managedRouterAuthorizerPathHarness) changedIdentity(handle uintptr) ProcessIdentity {
	identity := h.identity(handle)
	identity.StartTime = identity.StartTime.Add(-time.Second)
	return identity
}

func (h *managedRouterAuthorizerPathHarness) authorizer() api.ManagedRouterAuthorizer {
	return newManagedRouterAuthorizerWithDeps(
		h.pidportPath,
		h.executable,
		h.version,
		managedRouterAuthorizerDeps{
			readPidport: func(string) (int, int, error) {
				return h.readPID, h.readPort, h.readErr
			},
			statPidport: func(string) (os.FileInfo, error) {
				if h.statErr != nil {
					return nil, h.statErr
				}
				return os.Stat(h.pidportPath)
			},
			portOwner: func(context.Context, int) (int, bool, error) {
				return h.portOwnerPID, h.portOwnerFound, h.portOwnerErr
			},
			processID: func(int) (ProcessIdentity, error) {
				if h.processIndex >= len(h.processes) {
					h.t.Fatalf("unexpected process observation %d", h.processIndex+1)
				}
				observation := h.processes[h.processIndex]
				h.processIndex++
				return observation.identity, observation.err
			},
			closeID: func(identity *ProcessIdentity) error {
				if identity == nil {
					return nil
				}
				handle := identity.Handle
				h.closed[handle]++
				identity.Handle = 0
				return h.closeFailures[handle]
			},
			ownerMatch: func(int) (bool, error) {
				return h.ownerMatches, h.ownerMatchErr
			},
			ping: func(context.Context, int) (managedRouterPing, string) {
				if h.pingIndex >= len(h.pings) {
					h.t.Fatalf("unexpected ping observation %d", h.pingIndex+1)
				}
				observation := h.pings[h.pingIndex]
				h.pingIndex++
				return observation.ping, observation.failure
			},
			releaseDiagnostic: func(class string, err error) {
				h.events = append(h.events, managedRouterReleaseEventForTest{class: class, err: err})
			},
		},
	)
}

func (h *managedRouterAuthorizerPathHarness) requireClosedExactlyOnce(t *testing.T, handles ...uintptr) {
	t.Helper()
	for _, handle := range handles {
		if got := h.closed[handle]; got != 1 {
			t.Fatalf("identity handle %d close count = %d, want 1", handle, got)
		}
	}
	for handle, calls := range h.closed {
		if calls != 1 {
			t.Fatalf("identity handle %d close count = %d, want 1", handle, calls)
		}
	}
}

func TestManagedRouterTemporaryIdentitySettlementMatrix(t *testing.T) {
	processErr := errors.New("process observation failed")
	ownerErr := errors.New("owner observation failed")
	statErr := errors.New("pidport stat failed")
	t.Run("authorizer preconditions reject before identity acquisition", func(t *testing.T) {
		cases := []struct {
			name         string
			candidate    int
			hasCandidate bool
			configure    func(*managedRouterAuthorizerPathHarness)
			wantClass    string
		}{
			{name: "invalid candidate port", candidate: 0, hasCandidate: true, wantClass: api.ManagedRouterFailurePortInvalid},
			{
				name: "empty pidport path",
				configure: func(h *managedRouterAuthorizerPathHarness) {
					h.pidportPath = ""
				},
				wantClass: api.ManagedRouterFailurePIDPortUnavailable,
			},
			{
				name: "empty executable path",
				configure: func(h *managedRouterAuthorizerPathHarness) {
					h.executable = ""
				},
				wantClass: api.ManagedRouterFailureExecutableUnavailable,
			},
			{
				name: "uninformative version",
				configure: func(h *managedRouterAuthorizerPathHarness) {
					h.version = "dev"
				},
				wantClass: api.ManagedRouterFailureVersionUninformative,
			},
		}
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				h := newManagedRouterAuthorizerPathHarness(t)
				if tc.configure != nil {
					tc.configure(h)
				}
				candidate := h.port
				if tc.hasCandidate {
					candidate = tc.candidate
				}
				got := h.authorizer()(context.Background(), candidate)
				if got.Lease != nil || got.FailureClass != tc.wantClass {
					t.Fatalf("authorization = %+v, want failure %q", got, tc.wantClass)
				}
				if len(h.closed) != 0 || len(h.events) != 0 {
					t.Fatalf("precondition rejection acquired identity: closed=%v events=%v", h.closed, h.events)
				}
			})
		}
	})
	cases := []struct {
		name      string
		configure func(*managedRouterAuthorizerPathHarness)
		wantClass string
		closed    []uintptr
	}{
		{
			name: "pidport unavailable has no acquired identity",
			configure: func(h *managedRouterAuthorizerPathHarness) {
				h.readErr = errors.New("pidport unavailable")
			},
			wantClass: api.ManagedRouterFailurePIDPortUnavailable,
		},
		{
			name: "pidport port mismatch has no acquired identity",
			configure: func(h *managedRouterAuthorizerPathHarness) {
				h.readPort++
			},
			wantClass: api.ManagedRouterFailurePIDPortPortMismatch,
		},
		{
			name: "pidport stat failure has no acquired identity",
			configure: func(h *managedRouterAuthorizerPathHarness) {
				h.statErr = statErr
			},
			wantClass: api.ManagedRouterFailurePIDPortUnavailable,
		},
		{
			name: "socket owner unavailable has no acquired identity",
			configure: func(h *managedRouterAuthorizerPathHarness) {
				h.portOwnerFound = false
			},
			wantClass: api.ManagedRouterFailureSocketOwnerUnavailable,
		},
		{
			name: "socket owner mismatch has no acquired identity",
			configure: func(h *managedRouterAuthorizerPathHarness) {
				h.portOwnerPID++
			},
			wantClass: api.ManagedRouterFailureSocketOwnerMismatch,
		},
		{
			name: "process error settles acquired identity",
			configure: func(h *managedRouterAuthorizerPathHarness) {
				h.processes = []managedRouterProcessObservationForTest{{identity: h.identity(11), err: processErr}}
			},
			wantClass: api.ManagedRouterFailureProcessUnavailable,
			closed:    []uintptr{11},
		},
		{
			name: "dead identity settles once",
			configure: func(h *managedRouterAuthorizerPathHarness) {
				identity := h.identity(12)
				identity.Alive = false
				h.processes = []managedRouterProcessObservationForTest{{identity: identity}}
			},
			wantClass: api.ManagedRouterFailureProcessUnavailable,
			closed:    []uintptr{12},
		},
		{
			name: "denied identity settles once",
			configure: func(h *managedRouterAuthorizerPathHarness) {
				identity := h.identity(13)
				identity.Denied = true
				h.processes = []managedRouterProcessObservationForTest{{identity: identity}}
			},
			wantClass: api.ManagedRouterFailureProcessUnavailable,
			closed:    []uintptr{13},
		},
		{
			name: "executable mismatch settles once",
			configure: func(h *managedRouterAuthorizerPathHarness) {
				identity := h.identity(14)
				identity.ImagePath = filepath.Join(t.TempDir(), "foreign.exe")
				h.processes = []managedRouterProcessObservationForTest{{identity: identity}}
			},
			wantClass: api.ManagedRouterFailureExecutableMismatch,
			closed:    []uintptr{14},
		},
		{
			name: "argv mismatch settles once",
			configure: func(h *managedRouterAuthorizerPathHarness) {
				identity := h.identity(15)
				identity.Cmdline = []string{h.executable, "server"}
				h.processes = []managedRouterProcessObservationForTest{{identity: identity}}
			},
			wantClass: api.ManagedRouterFailureArgvRoleMismatch,
			closed:    []uintptr{15},
		},
		{
			name: "invalid generation settles once",
			configure: func(h *managedRouterAuthorizerPathHarness) {
				identity := h.identity(16)
				identity.StartTime = time.Time{}
				h.processes = []managedRouterProcessObservationForTest{{identity: identity}}
			},
			wantClass: api.ManagedRouterFailureProcessGenerationInvalid,
			closed:    []uintptr{16},
		},
		{
			name: "owner unavailable settles once",
			configure: func(h *managedRouterAuthorizerPathHarness) {
				h.ownerMatchErr = ownerErr
				h.processes = []managedRouterProcessObservationForTest{{identity: h.identity(17)}}
			},
			wantClass: api.ManagedRouterFailureProcessOwnerUnavailable,
			closed:    []uintptr{17},
		},
		{
			name: "owner mismatch settles once",
			configure: func(h *managedRouterAuthorizerPathHarness) {
				h.ownerMatches = false
				h.processes = []managedRouterProcessObservationForTest{{identity: h.identity(18)}}
			},
			wantClass: api.ManagedRouterFailureProcessOwnerMismatch,
			closed:    []uintptr{18},
		},
		{
			name: "ping transport failure settles first observation",
			configure: func(h *managedRouterAuthorizerPathHarness) {
				h.processes = []managedRouterProcessObservationForTest{{identity: h.identity(19)}}
				h.pings = []managedRouterPingObservationForTest{{failure: api.ManagedRouterFailurePingTransport}}
			},
			wantClass: api.ManagedRouterFailurePingTransport,
			closed:    []uintptr{19},
		},
		{
			name: "ping identity mismatch settles first observation",
			configure: func(h *managedRouterAuthorizerPathHarness) {
				h.processes = []managedRouterProcessObservationForTest{{identity: h.identity(20)}}
				h.pings = []managedRouterPingObservationForTest{{ping: managedRouterPing{OK: true, PID: h.pid + 1, Version: h.version}}}
			},
			wantClass: api.ManagedRouterFailurePingIdentityMismatch,
			closed:    []uintptr{20},
		},
		{
			name: "second observation failure settles both temporary identities",
			configure: func(h *managedRouterAuthorizerPathHarness) {
				second := h.identity(22)
				second.Alive = false
				h.processes = []managedRouterProcessObservationForTest{{identity: h.identity(21)}, {identity: second}}
			},
			wantClass: api.ManagedRouterFailureIdentityChanged,
			closed:    []uintptr{21, 22},
		},
		{
			name: "second generation mismatch settles both temporary identities",
			configure: func(h *managedRouterAuthorizerPathHarness) {
				h.processes = []managedRouterProcessObservationForTest{{identity: h.identity(23)}, {identity: h.changedIdentity(24)}}
			},
			wantClass: api.ManagedRouterFailureIdentityChanged,
			closed:    []uintptr{23, 24},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newManagedRouterAuthorizerPathHarness(t)
			tc.configure(h)
			got := h.authorizer()(context.Background(), h.port)
			if got.Lease != nil || got.FailureClass != tc.wantClass {
				t.Fatalf("authorization = %+v, want failure %q", got, tc.wantClass)
			}
			h.requireClosedExactlyOnce(t, tc.closed...)
			if len(h.events) != 0 {
				t.Fatalf("unexpected release diagnostics: %+v", h.events)
			}
		})
	}

	t.Run("stable initial acquisition settles the second temporary identity after first close failure", func(t *testing.T) {
		firstCloseCause := errors.New("first initial temporary close failed")
		h := newManagedRouterAuthorizerPathHarness(t)
		h.processes = []managedRouterProcessObservationForTest{{identity: h.identity(25)}, {identity: h.identity(26)}}
		h.closeFailures[25] = firstCloseCause

		authorization := h.authorizer()(context.Background(), h.port)
		if authorization.Lease != nil || authorization.FailureClass != api.ManagedRouterFailureIdentityUnavailable {
			t.Fatalf("authorization = %+v, want identity-unavailable without lease", authorization)
		}
		h.requireClosedExactlyOnce(t, 25, 26)
		if len(h.events) != 1 || h.events[0].class != api.ManagedRouterFailureIdentityUnavailable || !errors.Is(h.events[0].err, firstCloseCause) {
			t.Fatalf("release diagnostic = %+v, want identity-unavailable with %v", h.events, firstCloseCause)
		}
	})

	t.Run("initial generation mismatch settles both temporary identities after independent close failures", func(t *testing.T) {
		firstCloseCause := errors.New("first initial mismatch close failed")
		secondCloseCause := errors.New("second initial mismatch close failed")
		h := newManagedRouterAuthorizerPathHarness(t)
		h.processes = []managedRouterProcessObservationForTest{{identity: h.identity(27)}, {identity: h.changedIdentity(28)}}
		h.closeFailures[27] = firstCloseCause
		h.closeFailures[28] = secondCloseCause

		authorization := h.authorizer()(context.Background(), h.port)
		if authorization.Lease != nil || authorization.FailureClass != api.ManagedRouterFailureIdentityChanged {
			t.Fatalf("authorization = %+v, want identity-changed without lease", authorization)
		}
		h.requireClosedExactlyOnce(t, 27, 28)
		if len(h.events) != 1 || h.events[0].class != api.ManagedRouterFailureIdentityChanged || !errors.Is(h.events[0].err, firstCloseCause) || !errors.Is(h.events[0].err, secondCloseCause) {
			t.Fatalf("release diagnostic = %+v, want identity-changed with both close causes", h.events)
		}
	})

	t.Run("revalidation settlement branches", func(t *testing.T) {
		releaseCause := errors.New("native temporary close failed")
		firstMismatchCloseCause := errors.New("first revalidation mismatch close failed")
		secondMismatchCloseCause := errors.New("second revalidation mismatch close failed")
		cases := []struct {
			name      string
			configure func(*managedRouterAuthorizerPathHarness)
			want      string
			closed    []uintptr
			retained  uintptr
			causes    []error
		}{
			{
				name: "first revalidation observation rejects",
				configure: func(h *managedRouterAuthorizerPathHarness) {
					identity := h.identity(103)
					identity.Alive = false
					h.processes = []managedRouterProcessObservationForTest{{identity: h.identity(101)}, {identity: h.identity(102)}, {identity: identity}}
				},
				want:     api.ManagedRouterFailureIdentityChanged,
				closed:   []uintptr{101, 103},
				retained: 102,
			},
			{
				name: "first revalidation generation changes",
				configure: func(h *managedRouterAuthorizerPathHarness) {
					h.processes = []managedRouterProcessObservationForTest{{identity: h.identity(111)}, {identity: h.identity(112)}, {identity: h.changedIdentity(113)}}
				},
				want:     api.ManagedRouterFailureIdentityChanged,
				closed:   []uintptr{111, 113},
				retained: 112,
			},
			{
				name: "revalidation ping rejects",
				configure: func(h *managedRouterAuthorizerPathHarness) {
					h.processes = []managedRouterProcessObservationForTest{{identity: h.identity(121)}, {identity: h.identity(122)}, {identity: h.identity(123)}}
					h.pings = []managedRouterPingObservationForTest{
						{ping: managedRouterPing{OK: true, PID: h.pid, Version: h.version}},
						{failure: api.ManagedRouterFailurePingTransport},
					}
				},
				want:     api.ManagedRouterFailureIdentityChanged,
				closed:   []uintptr{121, 123},
				retained: 122,
			},
			{
				name: "second revalidation observation rejects",
				configure: func(h *managedRouterAuthorizerPathHarness) {
					second := h.identity(134)
					second.Denied = true
					h.processes = []managedRouterProcessObservationForTest{{identity: h.identity(131)}, {identity: h.identity(132)}, {identity: h.identity(133)}, {identity: second}}
					h.pings = []managedRouterPingObservationForTest{
						{ping: managedRouterPing{OK: true, PID: h.pid, Version: h.version}},
						{ping: managedRouterPing{OK: true, PID: h.pid, Version: h.version}},
					}
				},
				want:     api.ManagedRouterFailureIdentityChanged,
				closed:   []uintptr{131, 133, 134},
				retained: 132,
			},
			{
				name: "second revalidation generation changes",
				configure: func(h *managedRouterAuthorizerPathHarness) {
					h.processes = []managedRouterProcessObservationForTest{{identity: h.identity(141)}, {identity: h.identity(142)}, {identity: h.identity(143)}, {identity: h.changedIdentity(144)}}
					h.pings = []managedRouterPingObservationForTest{
						{ping: managedRouterPing{OK: true, PID: h.pid, Version: h.version}},
						{ping: managedRouterPing{OK: true, PID: h.pid, Version: h.version}},
					}
				},
				want:     api.ManagedRouterFailureIdentityChanged,
				closed:   []uintptr{141, 143, 144},
				retained: 142,
			},
			{
				name: "temporary close failure remains diagnostic",
				configure: func(h *managedRouterAuthorizerPathHarness) {
					h.processes = []managedRouterProcessObservationForTest{{identity: h.identity(151)}, {identity: h.identity(152)}, {identity: h.identity(153)}, {identity: h.identity(154)}}
					h.pings = []managedRouterPingObservationForTest{
						{ping: managedRouterPing{OK: true, PID: h.pid, Version: h.version}},
						{ping: managedRouterPing{OK: true, PID: h.pid, Version: h.version}},
					}
					h.closeFailures[153] = releaseCause
				},
				want:     api.ManagedRouterFailureIdentityChanged,
				closed:   []uintptr{151, 153, 154},
				retained: 152,
				causes:   []error{releaseCause},
			},
			{
				name: "revalidation generation mismatch settles both temporary identities after independent close failures",
				configure: func(h *managedRouterAuthorizerPathHarness) {
					h.processes = []managedRouterProcessObservationForTest{{identity: h.identity(171)}, {identity: h.identity(172)}, {identity: h.identity(173)}, {identity: h.changedIdentity(174)}}
					h.pings = []managedRouterPingObservationForTest{
						{ping: managedRouterPing{OK: true, PID: h.pid, Version: h.version}},
						{ping: managedRouterPing{OK: true, PID: h.pid, Version: h.version}},
					}
					h.closeFailures[173] = firstMismatchCloseCause
					h.closeFailures[174] = secondMismatchCloseCause
				},
				want:     api.ManagedRouterFailureIdentityChanged,
				closed:   []uintptr{171, 173, 174},
				retained: 172,
				causes:   []error{firstMismatchCloseCause, secondMismatchCloseCause},
			},
			{
				name: "stable revalidation settles both temporary identities",
				configure: func(h *managedRouterAuthorizerPathHarness) {
					h.processes = []managedRouterProcessObservationForTest{{identity: h.identity(161)}, {identity: h.identity(162)}, {identity: h.identity(163)}, {identity: h.identity(164)}}
					h.pings = []managedRouterPingObservationForTest{
						{ping: managedRouterPing{OK: true, PID: h.pid, Version: h.version}},
						{ping: managedRouterPing{OK: true, PID: h.pid, Version: h.version}},
					}
				},
				closed:   []uintptr{161, 163, 164},
				retained: 162,
			},
		}
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				h := newManagedRouterAuthorizerPathHarness(t)
				tc.configure(h)
				authorization := h.authorizer()(context.Background(), h.port)
				if authorization.Lease == nil || authorization.FailureClass != "" {
					t.Fatalf("initial authorization = %+v, want retained lease", authorization)
				}
				if got := authorization.Lease.Revalidate(context.Background()); got != tc.want {
					t.Fatalf("revalidation failure = %q, want %q", got, tc.want)
				}
				h.requireClosedExactlyOnce(t, tc.closed...)
				if len(tc.causes) == 0 {
					if len(h.events) != 0 {
						t.Fatalf("unexpected release diagnostics: %+v", h.events)
					}
				} else {
					if len(h.events) != 1 || h.events[0].class != api.ManagedRouterFailureIdentityChanged {
						t.Fatalf("release diagnostic = %+v, want one identity-changed event", h.events)
					}
					for _, cause := range tc.causes {
						if !errors.Is(h.events[0].err, cause) {
							t.Fatalf("release diagnostic = %+v, want identity-changed with %v", h.events, cause)
						}
					}
				}
				if err := authorization.Lease.Close(); err != nil {
					t.Fatalf("close retained lease: %v", err)
				}
				h.requireClosedExactlyOnce(t, append(tc.closed, tc.retained)...)
			})
		}
	})
}

func TestManagedRouterRetainedIdentityTransfersExactlyOnce(t *testing.T) {
	h := newManagedRouterAuthorizerPathHarness(t)
	h.processes = []managedRouterProcessObservationForTest{{identity: h.identity(201)}, {identity: h.identity(202)}}
	authorization := h.authorizer()(context.Background(), h.port)
	if authorization.Lease == nil || authorization.FailureClass != "" {
		t.Fatalf("authorization = %+v, want retained lease", authorization)
	}
	h.requireClosedExactlyOnce(t, 201)
	if got := h.closed[202]; got != 0 {
		t.Fatalf("retained identity close count before Lease.Close = %d, want 0", got)
	}
	if err := authorization.Lease.Close(); err != nil {
		t.Fatalf("first Lease.Close: %v", err)
	}
	if err := authorization.Lease.Close(); err != nil {
		t.Fatalf("second Lease.Close: %v", err)
	}
	h.requireClosedExactlyOnce(t, 201, 202)
	if len(h.events) != 0 {
		t.Fatalf("unexpected release diagnostics: %+v", h.events)
	}
}

func TestManagedRouterReleaseCauseIsRedacted(t *testing.T) {
	releaseCause := errors.New("sensitive native close cause")
	h := newManagedRouterAuthorizerPathHarness(t)
	identity := h.identity(301)
	identity.ImagePath = filepath.Join(t.TempDir(), "sensitive-executable-path.exe")
	identity.Cmdline = []string{identity.ImagePath, "sensitive-argv-marker"}
	h.processes = []managedRouterProcessObservationForTest{{identity: identity}}
	h.closeFailures[301] = releaseCause

	got := h.authorizer()(context.Background(), h.port)
	if got.Lease != nil || got.FailureClass != api.ManagedRouterFailureExecutableMismatch {
		t.Fatalf("authorization = %+v, want executable mismatch", got)
	}
	if len(h.events) != 1 || h.events[0].class != api.ManagedRouterFailureExecutableMismatch || !errors.Is(h.events[0].err, releaseCause) {
		t.Fatalf("release diagnostic = %+v, want executable mismatch carrying native cause", h.events)
	}
	encoded, err := json.Marshal(got)
	if err != nil {
		t.Fatalf("marshal authorization: %v", err)
	}
	serialized := fmt.Sprintf("%+v", got)
	for _, sensitive := range []string{
		releaseCause.Error(),
		h.pidportPath,
		identity.ImagePath,
		"sensitive-argv-marker",
		strconv.Itoa(int(identity.Handle)),
	} {
		if strings.Contains(string(encoded), sensitive) || strings.Contains(serialized, sensitive) {
			t.Fatalf("public authorization leaked %q: json=%s text=%s", sensitive, encoded, serialized)
		}
	}
}

func TestManagedRouterAuthorizer(t *testing.T) {
	const (
		pid     = 4242
		port    = 19125
		version = "1.2.3"
	)
	root := t.TempDir()
	pidportPath := filepath.Join(root, "gui.pidport")
	if err := os.WriteFile(pidportPath, []byte("fixture"), 0o600); err != nil {
		t.Fatal(err)
	}
	mtime := time.Now().Add(-time.Minute).Round(0)
	if err := os.Chtimes(pidportPath, mtime, mtime); err != nil {
		t.Fatal(err)
	}
	executable := filepath.Join(root, "mcphub.exe")
	start := mtime.Add(-time.Minute)

	tests := []struct {
		name             string
		expectedVersion  string
		ownerPID         int
		secondStart      time.Time
		ping             managedRouterPing
		wantAuthorized   bool
		wantFailureClass string
	}{
		{name: "stable owned generation", expectedVersion: version, ownerPID: pid, secondStart: start, ping: managedRouterPing{OK: true, PID: pid, Version: version}, wantAuthorized: true},
		{name: "socket owner mismatch", expectedVersion: version, ownerPID: pid + 1, secondStart: start, ping: managedRouterPing{OK: true, PID: pid, Version: version}, wantFailureClass: "socket-owner-mismatch"},
		{name: "generation changes after ping", expectedVersion: version, ownerPID: pid, secondStart: start.Add(time.Second), ping: managedRouterPing{OK: true, PID: pid, Version: version}, wantFailureClass: "identity-changed"},
		{name: "uninformative build version", expectedVersion: "dev", ownerPID: pid, secondStart: start, ping: managedRouterPing{OK: true, PID: pid, Version: version}, wantFailureClass: "version-uninformative"},
		{name: "ping not ok", expectedVersion: version, ownerPID: pid, secondStart: start, ping: managedRouterPing{PID: pid, Version: version}, wantFailureClass: "ping-identity-mismatch"},
		{name: "ping zero pid", expectedVersion: version, ownerPID: pid, secondStart: start, ping: managedRouterPing{OK: true, Version: version}, wantFailureClass: "ping-identity-mismatch"},
		{name: "ping wrong pid", expectedVersion: version, ownerPID: pid, secondStart: start, ping: managedRouterPing{OK: true, PID: pid + 1, Version: version}, wantFailureClass: "ping-identity-mismatch"},
		{name: "ping empty version", expectedVersion: version, ownerPID: pid, secondStart: start, ping: managedRouterPing{OK: true, PID: pid}, wantFailureClass: "ping-identity-mismatch"},
		{name: "ping wrong version", expectedVersion: version, ownerPID: pid, secondStart: start, ping: managedRouterPing{OK: true, PID: pid, Version: "9.9.9"}, wantFailureClass: "ping-identity-mismatch"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			observations := 0
			authorize := newManagedRouterAuthorizerWithDeps(
				pidportPath,
				executable,
				tc.expectedVersion,
				managedRouterAuthorizerDeps{
					readPidport: func(string) (int, int, error) { return pid, port, nil },
					statPidport: os.Stat,
					portOwner: func(context.Context, int) (int, bool, error) {
						return tc.ownerPID, true, nil
					},
					processID: func(int) (ProcessIdentity, error) {
						observations++
						observedStart := start
						if observations > 1 {
							observedStart = tc.secondStart
						}
						return ProcessIdentity{
							Alive: true, ImagePath: executable,
							Cmdline: []string{executable, "gui"}, StartTime: observedStart,
							Handle: 1,
						}, nil
					},
					closeID:    closeManagedRouterIdentityForTest,
					ownerMatch: func(int) (bool, error) { return true, nil },
					ping: func(context.Context, int) (managedRouterPing, string) {
						return tc.ping, ""
					},
				},
			)
			got := authorize(context.Background(), port)
			if (got.Lease != nil) != tc.wantAuthorized || got.FailureClass != tc.wantFailureClass {
				t.Fatalf("authorization = %+v, want authorized=%v failure=%q", got, tc.wantAuthorized, tc.wantFailureClass)
			}
			if got.Lease != nil {
				if err := got.Lease.Close(); err != nil {
					t.Fatalf("close authorization lease: %v", err)
				}
			}
		})
	}
}

func TestManagedRouterAuthorizer_AbuseAndUncertaintyMatrix(t *testing.T) {
	const (
		pid     = 424242
		port    = 19125
		version = "1.2.3"
	)
	type pidportObservation struct {
		pid  int
		port int
		err  error
	}
	type portOwnerObservation struct {
		pid   int
		found bool
		err   error
	}
	type processObservation struct {
		identity ProcessIdentity
		err      error
	}
	type ownerMatchObservation struct {
		matches bool
		err     error
	}
	type callCounts struct {
		readPidport int
		statPidport int
		portOwner   int
		processID   int
		ownerMatch  int
		ping        int
	}
	type scenario struct {
		pidports     []pidportObservation
		statErr      error
		portOwners   []portOwnerObservation
		processes    []processObservation
		ownerMatches []ownerMatchObservation
		ping         managedRouterPing
		pingFailure  string
	}

	root := t.TempDir()
	pidportPath := filepath.Join(root, "sensitive-pidport-path")
	if err := os.WriteFile(pidportPath, []byte("fixture"), 0o600); err != nil {
		t.Fatal(err)
	}
	mtime := time.Now().Add(-time.Minute).Round(0)
	if err := os.Chtimes(pidportPath, mtime, mtime); err != nil {
		t.Fatal(err)
	}
	executable := filepath.Join(root, "sensitive-executable-path.exe")
	start := mtime.Add(-time.Minute)
	validIdentity := func() ProcessIdentity {
		return ProcessIdentity{
			Alive:     true,
			ImagePath: executable,
			Cmdline:   []string{executable, "gui", "sensitive-argv-marker"},
			StartTime: start,
			Handle:    1,
		}
	}
	validScenario := func() scenario {
		return scenario{
			pidports: []pidportObservation{
				{pid: pid, port: port},
				{pid: pid, port: port},
			},
			portOwners: []portOwnerObservation{
				{pid: pid, found: true},
				{pid: pid, found: true},
			},
			processes: []processObservation{
				{identity: validIdentity()},
				{identity: validIdentity()},
			},
			ownerMatches: []ownerMatchObservation{
				{matches: true},
				{matches: true},
			},
			ping: managedRouterPing{OK: true, PID: pid, Version: version},
		}
	}

	malformedErr := errors.New("sensitive-error-marker: malformed pidport")
	permissionErr := fmt.Errorf("sensitive-error-marker: %w", os.ErrPermission)
	unsupportedErr := fmt.Errorf("sensitive-error-marker: %w", errMacOSProbeUnsupported)
	unqueryableErr := errors.New("sensitive-error-marker: process query failed")
	fullCalls := callCounts{readPidport: 2, statPidport: 2, portOwner: 2, processID: 2, ownerMatch: 2, ping: 1}
	tests := []struct {
		name             string
		configure        func(*scenario)
		wantAuthorized   bool
		wantFailureClass string
		wantCalls        callCounts
	}{
		{
			name:           "positive control stable owned generation",
			wantAuthorized: true,
			wantCalls:      fullCalls,
		},
		{
			name: "absent pidport",
			configure: func(s *scenario) {
				s.pidports[0].err = os.ErrNotExist
			},
			wantFailureClass: "pidport-unavailable",
			wantCalls:        callCounts{readPidport: 1},
		},
		{
			name: "malformed pidport",
			configure: func(s *scenario) {
				s.pidports[0].err = malformedErr
			},
			wantFailureClass: "pidport-unavailable",
			wantCalls:        callCounts{readPidport: 1},
		},
		{
			name: "invalid pidport pid",
			configure: func(s *scenario) {
				s.pidports[0].pid = 0
			},
			wantFailureClass: "pidport-unavailable",
			wantCalls:        callCounts{readPidport: 1},
		},
		{
			name: "invalid pidport port",
			configure: func(s *scenario) {
				s.pidports[0].port = 65536
			},
			wantFailureClass: "pidport-unavailable",
			wantCalls:        callCounts{readPidport: 1},
		},
		{
			name: "wrong candidate port",
			configure: func(s *scenario) {
				s.pidports[0].port = port + 1
			},
			wantFailureClass: "pidport-port-mismatch",
			wantCalls:        callCounts{readPidport: 1},
		},
		{
			name: "pidport stat permission denied",
			configure: func(s *scenario) {
				s.statErr = permissionErr
			},
			wantFailureClass: "pidport-unavailable",
			wantCalls:        callCounts{readPidport: 1, statPidport: 1},
		},
		{
			name: "socket owner not found",
			configure: func(s *scenario) {
				s.portOwners[0].found = false
			},
			wantFailureClass: "socket-owner-unavailable",
			wantCalls:        callCounts{readPidport: 1, statPidport: 1, portOwner: 1},
		},
		{
			name: "socket owner permission denied",
			configure: func(s *scenario) {
				s.portOwners[0].err = permissionErr
			},
			wantFailureClass: "socket-owner-unavailable",
			wantCalls:        callCounts{readPidport: 1, statPidport: 1, portOwner: 1},
		},
		{
			name: "socket owner unsupported",
			configure: func(s *scenario) {
				s.portOwners[0].err = unsupportedErr
			},
			wantFailureClass: "socket-owner-unavailable",
			wantCalls:        callCounts{readPidport: 1, statPidport: 1, portOwner: 1},
		},
		{
			name: "dead process",
			configure: func(s *scenario) {
				s.processes[0].identity = ProcessIdentity{Alive: false}
			},
			wantFailureClass: "process-unavailable",
			wantCalls:        callCounts{readPidport: 1, statPidport: 1, portOwner: 1, processID: 1},
		},
		{
			name: "process inspection permission denied",
			configure: func(s *scenario) {
				s.processes[0].identity = ProcessIdentity{Alive: true, Denied: true}
			},
			wantFailureClass: "process-unavailable",
			wantCalls:        callCounts{readPidport: 1, statPidport: 1, portOwner: 1, processID: 1},
		},
		{
			name: "process unqueryable",
			configure: func(s *scenario) {
				s.processes[0].err = unqueryableErr
			},
			wantFailureClass: "process-unavailable",
			wantCalls:        callCounts{readPidport: 1, statPidport: 1, portOwner: 1, processID: 1},
		},
		{
			name: "process inspection unsupported",
			configure: func(s *scenario) {
				s.processes[0].err = unsupportedErr
			},
			wantFailureClass: "process-unavailable",
			wantCalls:        callCounts{readPidport: 1, statPidport: 1, portOwner: 1, processID: 1},
		},
		{
			name: "process owner does not match",
			configure: func(s *scenario) {
				s.ownerMatches[0].matches = false
			},
			wantFailureClass: "process-owner-mismatch",
			wantCalls:        callCounts{readPidport: 1, statPidport: 1, portOwner: 1, processID: 1, ownerMatch: 1},
		},
		{
			name: "process owner inspection permission denied",
			configure: func(s *scenario) {
				s.ownerMatches[0].err = permissionErr
			},
			wantFailureClass: "process-owner-unavailable",
			wantCalls:        callCounts{readPidport: 1, statPidport: 1, portOwner: 1, processID: 1, ownerMatch: 1},
		},
		{
			name: "post ping pidport pid and process generation replacement",
			configure: func(s *scenario) {
				s.pidports[1].pid = pid + 1
				s.portOwners[1].pid = pid + 1
			},
			wantFailureClass: "identity-changed",
			wantCalls:        fullCalls,
		},
		{
			name: "post ping pidport port replacement",
			configure: func(s *scenario) {
				s.pidports[1].port = port + 1
			},
			wantFailureClass: "identity-changed",
			wantCalls:        callCounts{readPidport: 2, statPidport: 1, portOwner: 1, processID: 1, ownerMatch: 1, ping: 1},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s := validScenario()
			if tc.configure != nil {
				tc.configure(&s)
			}
			var calls callCounts
			next := func(stage string, call int, available int) int {
				t.Helper()
				if call >= available {
					t.Fatalf("unexpected extra %s call %d", stage, call+1)
				}
				return call
			}
			authorize := newManagedRouterAuthorizerWithDeps(
				pidportPath,
				executable,
				version,
				managedRouterAuthorizerDeps{
					readPidport: func(string) (int, int, error) {
						index := next("readPidport", calls.readPidport, len(s.pidports))
						calls.readPidport++
						observed := s.pidports[index]
						return observed.pid, observed.port, observed.err
					},
					statPidport: func(string) (os.FileInfo, error) {
						calls.statPidport++
						if s.statErr != nil {
							return nil, s.statErr
						}
						return os.Stat(pidportPath)
					},
					portOwner: func(context.Context, int) (int, bool, error) {
						index := next("portOwner", calls.portOwner, len(s.portOwners))
						calls.portOwner++
						observed := s.portOwners[index]
						return observed.pid, observed.found, observed.err
					},
					processID: func(int) (ProcessIdentity, error) {
						index := next("processID", calls.processID, len(s.processes))
						calls.processID++
						observed := s.processes[index]
						return observed.identity, observed.err
					},
					closeID: closeManagedRouterIdentityForTest,
					ownerMatch: func(int) (bool, error) {
						index := next("ownerMatch", calls.ownerMatch, len(s.ownerMatches))
						calls.ownerMatch++
						observed := s.ownerMatches[index]
						return observed.matches, observed.err
					},
					ping: func(context.Context, int) (managedRouterPing, string) {
						calls.ping++
						return s.ping, s.pingFailure
					},
				},
			)

			got := authorize(context.Background(), port)
			if (got.Lease != nil) != tc.wantAuthorized || got.FailureClass != tc.wantFailureClass {
				t.Fatalf("authorization = %+v, want authorized=%v failure=%q", got, tc.wantAuthorized, tc.wantFailureClass)
			}
			if calls != tc.wantCalls {
				t.Fatalf("dependency calls = %+v, want %+v", calls, tc.wantCalls)
			}
			serializedResult := fmt.Sprintf("%+v", got)
			for _, raw := range []string{
				pidportPath,
				executable,
				"sensitive-argv-marker",
				strconv.Itoa(pid),
				strconv.Itoa(pid + 1),
				"sensitive-error-marker",
			} {
				if strings.Contains(serializedResult, raw) {
					t.Fatalf("authorization result leaked raw diagnostic %q: %s", raw, serializedResult)
				}
			}
		})
	}
}

func TestNewManagedRouterAuthorizer_ProductionOwnerBinding(t *testing.T) {
	file, err := parser.ParseFile(token.NewFileSet(), "managed_router_authorizer.go", nil, 0)
	if err != nil {
		t.Fatalf("parse production authorizer: %v", err)
	}
	found := false
	ast.Inspect(file, func(node ast.Node) bool {
		kv, ok := node.(*ast.KeyValueExpr)
		if !ok {
			return true
		}
		key, ok := kv.Key.(*ast.Ident)
		if !ok || key.Name != "ownerMatch" {
			return true
		}
		sel, ok := kv.Value.(*ast.SelectorExpr)
		if !ok || sel.Sel.Name != "ProcessOwnerMatchesCurrent" {
			return true
		}
		pkg, ok := sel.X.(*ast.Ident)
		if ok && pkg.Name == "processutil" {
			found = true
		}
		return true
	})
	if !found {
		t.Fatal("production ownerMatch is not bound directly to processutil.ProcessOwnerMatchesCurrent")
	}
}

func TestManagedRouterAuthorizer_PostPingObservationFailureIsIdentityChanged(t *testing.T) {
	const (
		pid  = 4242
		port = 19125
	)
	root := t.TempDir()
	pidportPath := filepath.Join(root, "gui.pidport")
	if err := os.WriteFile(pidportPath, []byte("fixture"), 0o600); err != nil {
		t.Fatal(err)
	}
	mtime := time.Now().Add(-time.Minute).Round(0)
	if err := os.Chtimes(pidportPath, mtime, mtime); err != nil {
		t.Fatal(err)
	}
	executable := filepath.Join(root, "mcphub.exe")
	ownerCalls := 0
	authorize := newManagedRouterAuthorizerWithDeps(
		pidportPath, executable, "1.2.3",
		managedRouterAuthorizerDeps{
			readPidport: func(string) (int, int, error) { return pid, port, nil },
			statPidport: os.Stat,
			portOwner: func(context.Context, int) (int, bool, error) {
				ownerCalls++
				if ownerCalls == 1 {
					return pid, true, nil
				}
				return pid + 1, true, nil
			},
			processID: func(int) (ProcessIdentity, error) {
				return ProcessIdentity{Alive: true, ImagePath: executable, Cmdline: []string{executable}, StartTime: mtime.Add(-time.Minute), Handle: 1}, nil
			},
			closeID:    closeManagedRouterIdentityForTest,
			ownerMatch: func(int) (bool, error) { return true, nil },
			ping: func(context.Context, int) (managedRouterPing, string) {
				return managedRouterPing{OK: true, PID: pid, Version: "1.2.3"}, ""
			},
		},
	)
	if got := authorize(context.Background(), port); got.Lease != nil || got.FailureClass != "identity-changed" {
		t.Fatalf("authorization = %+v, want identity-changed", got)
	}
}

func TestManagedRouterLease_RetainsGenerationRevalidatesAndClosesOnce(t *testing.T) {
	const (
		pid     = 4242
		port    = 19125
		version = "1.2.3"
	)
	root := t.TempDir()
	pidportPath := filepath.Join(root, "gui.pidport")
	if err := os.WriteFile(pidportPath, []byte("fixture"), 0o600); err != nil {
		t.Fatal(err)
	}
	mtime := time.Now().Add(-time.Minute).Round(0)
	if err := os.Chtimes(pidportPath, mtime, mtime); err != nil {
		t.Fatal(err)
	}
	executable := filepath.Join(root, "mcphub.exe")
	start := mtime.Add(-time.Minute)
	processCalls := 0
	closeCalls := 0
	takeover := false
	authorize := newManagedRouterAuthorizerWithDeps(
		pidportPath,
		executable,
		version,
		managedRouterAuthorizerDeps{
			readPidport: func(string) (int, int, error) { return pid, port, nil },
			statPidport: os.Stat,
			portOwner:   func(context.Context, int) (int, bool, error) { return pid, true, nil },
			processID: func(int) (ProcessIdentity, error) {
				processCalls++
				observedStart := start
				if takeover && processCalls > 4 {
					observedStart = start.Add(time.Second)
				}
				return ProcessIdentity{
					Alive: true, ImagePath: executable,
					Cmdline: []string{executable, "gui"}, StartTime: observedStart,
					Handle: uintptr(processCalls),
				}, nil
			},
			closeID: func(identity *ProcessIdentity) error {
				closeCalls++
				identity.Handle = 0
				return nil
			},
			ownerMatch: func(int) (bool, error) { return true, nil },
			ping: func(context.Context, int) (managedRouterPing, string) {
				return managedRouterPing{OK: true, PID: pid, Version: version}, ""
			},
		},
	)

	authorization := authorize(context.Background(), port)
	if authorization.Lease == nil || authorization.FailureClass != "" {
		t.Fatalf("authorization = %+v, want retained lease", authorization)
	}
	if failure := authorization.Lease.Revalidate(context.Background()); failure != "" {
		t.Fatalf("stable revalidation failure = %q", failure)
	}
	takeover = true
	if failure := authorization.Lease.Revalidate(context.Background()); failure != api.ManagedRouterFailureIdentityChanged {
		t.Fatalf("takeover revalidation failure = %q, want identity-changed", failure)
	}
	if err := authorization.Lease.Close(); err != nil {
		t.Fatalf("close lease: %v", err)
	}
	if err := authorization.Lease.Close(); err != nil {
		t.Fatalf("second close lease: %v", err)
	}
	if closeCalls != 5 {
		t.Fatalf("identity close calls = %d, want acquisition-first + stable temps + takeover temp + retained = 5", closeCalls)
	}
	if failure := authorization.Lease.Revalidate(context.Background()); failure != api.ManagedRouterFailureIdentityChanged {
		t.Fatalf("closed lease revalidation failure = %q, want identity-changed", failure)
	}
}

type managedRouterGenerationLeaseForTest struct {
	revalidations []string
	closeCalls    int
}

func (l *managedRouterGenerationLeaseForTest) Revalidate(context.Context) string {
	if len(l.revalidations) == 0 {
		return ""
	}
	result := l.revalidations[0]
	l.revalidations = l.revalidations[1:]
	return result
}

func (l *managedRouterGenerationLeaseForTest) Close() error {
	l.closeCalls++
	return nil
}

func TestVerifyManagedRouterGeneration_BindsVersionCommitAndFinalGeneration(t *testing.T) {
	version := managedRouterVersion{Version: "0.4.33", Commit: "abcdef0"}
	t.Run("exact proof", func(t *testing.T) {
		lease := &managedRouterGenerationLeaseForTest{}
		err := verifyManagedRouterGeneration(context.Background(), 9125, version.Version, version.Commit,
			func(context.Context, int) api.ManagedRouterAuthorization {
				return api.ManagedRouterAuthorization{Lease: lease}
			},
			func(context.Context, int) (managedRouterVersion, error) { return version, nil })
		if err != nil {
			t.Fatalf("exact generation proof: %v", err)
		}
		if lease.closeCalls != 1 {
			t.Fatalf("lease close calls=%d, want 1", lease.closeCalls)
		}
	})
	t.Run("version commit mismatch is refused", func(t *testing.T) {
		lease := &managedRouterGenerationLeaseForTest{}
		err := verifyManagedRouterGeneration(context.Background(), 9125, version.Version, version.Commit,
			func(context.Context, int) api.ManagedRouterAuthorization {
				return api.ManagedRouterAuthorization{Lease: lease}
			},
			func(context.Context, int) (managedRouterVersion, error) {
				return managedRouterVersion{Version: version.Version, Commit: "other"}, nil
			})
		if err == nil || !strings.Contains(err.Error(), "version or commit mismatch") {
			t.Fatalf("mismatch error=%v", err)
		}
		if lease.closeCalls != 1 {
			t.Fatalf("lease close calls=%d, want 1", lease.closeCalls)
		}
	})
	t.Run("final generation change is refused", func(t *testing.T) {
		lease := &managedRouterGenerationLeaseForTest{revalidations: []string{api.ManagedRouterFailureIdentityChanged}}
		err := verifyManagedRouterGeneration(context.Background(), 9125, version.Version, version.Commit,
			func(context.Context, int) api.ManagedRouterAuthorization {
				return api.ManagedRouterAuthorization{Lease: lease}
			},
			func(context.Context, int) (managedRouterVersion, error) { return version, nil })
		if err == nil || !strings.Contains(err.Error(), "final generation revalidation") {
			t.Fatalf("generation-change error=%v", err)
		}
		if lease.closeCalls != 1 {
			t.Fatalf("lease close calls=%d, want 1", lease.closeCalls)
		}
	})
}

func TestStrictManagedRouterPing(t *testing.T) {
	tests := []struct {
		name        string
		status      int
		contentType string
		body        string
		wantFailure string
	}{
		{name: "exact", status: http.StatusOK, contentType: "application/json; charset=utf-8", body: `{"ok":true,"pid":42,"version":"1.2.3"}`},
		{name: "redirect refused", status: http.StatusFound, contentType: "application/json", body: `{}`, wantFailure: "ping-http-status"},
		{name: "wrong content type", status: http.StatusOK, contentType: "text/plain", body: `{"ok":true,"pid":42,"version":"1.2.3"}`, wantFailure: "ping-content-type"},
		{name: "multiple objects", status: http.StatusOK, contentType: "application/json", body: `{"ok":true,"pid":42,"version":"1.2.3"}{}`, wantFailure: "ping-malformed"},
		{name: "oversized", status: http.StatusOK, contentType: "application/json", body: strings.Repeat("x", managedRouterPingBodyMax+1), wantFailure: "ping-response-too-large"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/api/ping" {
					http.NotFound(w, r)
					return
				}
				w.Header().Set("Content-Type", tc.contentType)
				if tc.status == http.StatusFound {
					w.Header().Set("Location", "/api/ping")
				}
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(tc.body))
			}))
			defer server.Close()
			hostPort := strings.TrimPrefix(server.URL, "http://")
			_, rawPort, err := net.SplitHostPort(hostPort)
			if err != nil {
				t.Fatal(err)
			}
			port, err := strconv.Atoi(rawPort)
			if err != nil {
				t.Fatal(err)
			}
			got, failure := strictManagedRouterPing(context.Background(), port)
			if failure != tc.wantFailure {
				t.Fatalf("failure = %q, want %q", failure, tc.wantFailure)
			}
			if tc.wantFailure == "" && (!got.OK || got.PID != 42 || got.Version != "1.2.3") {
				t.Fatalf("ping = %+v", got)
			}
		})
	}
}

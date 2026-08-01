package gui

import (
	"context"
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

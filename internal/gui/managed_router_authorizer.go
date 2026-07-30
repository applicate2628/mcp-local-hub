package gui

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"mime"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	"mcp-local-hub/internal/api"
	processutil "mcp-local-hub/internal/process"
)

const (
	managedRouterAuthorizationTimeout = 2 * time.Second
	managedRouterPingTimeout          = 500 * time.Millisecond
	managedRouterPingBodyMax          = 4 * 1024
)

type managedRouterProcessGeneration struct {
	pid        int
	port       int
	ownerPID   int
	executable string
	startTime  time.Time
}

type managedRouterAuthorizerDeps struct {
	readPidport func(string) (int, int, error)
	statPidport func(string) (os.FileInfo, error)
	portOwner   func(context.Context, int) (int, bool, error)
	processID   func(int) (ProcessIdentity, error)
	closeID     func(*ProcessIdentity) error
	ownerMatch  func(int) (bool, error)
	ping        func(context.Context, int) (managedRouterPing, string)
}

type managedRouterPing struct {
	OK      bool   `json:"ok"`
	PID     int    `json:"pid"`
	Version string `json:"version"`
}

// NewManagedRouterAuthorizer constructs the only production verifier for
// destructive direct-LSP replacement cleanup. The returned closure performs
// read-only pidport, socket-owner, process-generation, and strict /api/ping
// checks; it never creates state and never mutates the GUI process.
func NewManagedRouterAuthorizer(pidportPath, currentExecutable, expectedVersion string) api.ManagedRouterAuthorizer {
	return newManagedRouterAuthorizerWithDeps(
		pidportPath,
		currentExecutable,
		expectedVersion,
		managedRouterAuthorizerDeps{
			readPidport: ReadPidport,
			statPidport: os.Stat,
			portOwner:   api.LoopbackPortOwnerPIDContext,
			processID:   retainedProcessID,
			closeID: func(identity *ProcessIdentity) error {
				identity.Close()
				return nil
			},
			ownerMatch: processutil.ProcessOwnerMatchesCurrent,
			ping:       strictManagedRouterPing,
		},
	)
}

func newManagedRouterAuthorizerWithDeps(
	pidportPath, currentExecutable, expectedVersion string,
	deps managedRouterAuthorizerDeps,
) api.ManagedRouterAuthorizer {
	if deps.closeID == nil {
		deps.closeID = func(identity *ProcessIdentity) error {
			identity.Close()
			return nil
		}
	}
	return func(ctx context.Context, candidatePort int) api.ManagedRouterAuthorization {
		if ctx == nil {
			ctx = context.Background()
		}
		version := strings.TrimSpace(expectedVersion)
		if candidatePort <= 0 || candidatePort > 65535 {
			return api.ManagedRouterAuthorization{FailureClass: api.ManagedRouterFailurePortInvalid}
		}
		if strings.TrimSpace(pidportPath) == "" {
			return api.ManagedRouterAuthorization{FailureClass: api.ManagedRouterFailurePIDPortUnavailable}
		}
		executable, ok := canonicalManagedRouterExecutable(currentExecutable)
		if !ok {
			return api.ManagedRouterAuthorization{FailureClass: api.ManagedRouterFailureExecutableUnavailable}
		}
		switch strings.ToLower(version) {
		case "", "dev", "unknown":
			return api.ManagedRouterAuthorization{FailureClass: api.ManagedRouterFailureVersionUninformative}
		}

		authorizationCtx, cancel := context.WithTimeout(ctx, managedRouterAuthorizationTimeout)
		defer cancel()
		first, firstIdentity, failure := observeManagedRouterGeneration(authorizationCtx, candidatePort, pidportPath, executable, deps)
		if failure != "" {
			return api.ManagedRouterAuthorization{FailureClass: failure}
		}
		ping, failure := deps.ping(authorizationCtx, candidatePort)
		if failure != "" {
			_ = deps.closeID(&firstIdentity)
			return api.ManagedRouterAuthorization{FailureClass: failure}
		}
		if !ping.OK || ping.PID != first.pid || strings.TrimSpace(ping.Version) == "" || ping.Version != version {
			_ = deps.closeID(&firstIdentity)
			return api.ManagedRouterAuthorization{FailureClass: api.ManagedRouterFailurePingIdentityMismatch}
		}
		second, secondIdentity, failure := observeManagedRouterGeneration(authorizationCtx, candidatePort, pidportPath, executable, deps)
		if failure != "" {
			_ = deps.closeID(&firstIdentity)
			// The same observation already succeeded before the ping. Any
			// post-response refusal means the authorized generation is no longer
			// stable; keep the external taxonomy independent of which signal raced.
			return api.ManagedRouterAuthorization{FailureClass: api.ManagedRouterFailureIdentityChanged}
		}
		if first != second {
			_ = deps.closeID(&firstIdentity)
			_ = deps.closeID(&secondIdentity)
			return api.ManagedRouterAuthorization{FailureClass: api.ManagedRouterFailureIdentityChanged}
		}
		if err := deps.closeID(&firstIdentity); err != nil {
			_ = deps.closeID(&secondIdentity)
			return api.ManagedRouterAuthorization{FailureClass: api.ManagedRouterFailureIdentityUnavailable}
		}
		retainedIdentity := secondIdentity
		return api.ManagedRouterAuthorization{
			Lease: &managedRouterLease{
				revalidateFn: func(revalidateCtx context.Context) string {
					return revalidateManagedRouterGeneration(
						revalidateCtx,
						second,
						pidportPath,
						executable,
						version,
						deps,
					)
				},
				closeFn: func() error {
					return deps.closeID(&retainedIdentity)
				},
			},
		}
	}
}

type managedRouterLease struct {
	mu           sync.Mutex
	revalidateFn func(context.Context) string
	closeFn      func() error
	closed       bool
	closeErr     error
}

func (l *managedRouterLease) Revalidate(ctx context.Context) string {
	if l == nil {
		return api.ManagedRouterFailureIdentityUnavailable
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.closed || l.revalidateFn == nil {
		return api.ManagedRouterFailureIdentityChanged
	}
	return l.revalidateFn(ctx)
}

func revalidateManagedRouterGeneration(
	ctx context.Context,
	generation managedRouterProcessGeneration,
	pidportPath string,
	expectedExecutable string,
	expectedVersion string,
	deps managedRouterAuthorizerDeps,
) string {
	if ctx == nil {
		ctx = context.Background()
	}
	revalidationCtx, cancel := context.WithTimeout(ctx, managedRouterAuthorizationTimeout)
	defer cancel()
	first, firstIdentity, failure := observeManagedRouterGeneration(
		revalidationCtx,
		generation.port,
		pidportPath,
		expectedExecutable,
		deps,
	)
	if failure != "" {
		return api.ManagedRouterFailureIdentityChanged
	}
	closeFirst := func() bool {
		return deps.closeID(&firstIdentity) == nil
	}
	if first != generation {
		_ = closeFirst()
		return api.ManagedRouterFailureIdentityChanged
	}
	ping, failure := deps.ping(revalidationCtx, generation.port)
	if failure != "" ||
		!ping.OK ||
		ping.PID != generation.pid ||
		strings.TrimSpace(ping.Version) == "" ||
		ping.Version != expectedVersion {
		_ = closeFirst()
		return api.ManagedRouterFailureIdentityChanged
	}
	second, secondIdentity, failure := observeManagedRouterGeneration(
		revalidationCtx,
		generation.port,
		pidportPath,
		expectedExecutable,
		deps,
	)
	if failure != "" {
		_ = closeFirst()
		return api.ManagedRouterFailureIdentityChanged
	}
	firstClosed := closeFirst()
	secondClosed := deps.closeID(&secondIdentity) == nil
	if !firstClosed || !secondClosed || second != generation {
		return api.ManagedRouterFailureIdentityChanged
	}
	return ""
}

func (l *managedRouterLease) Close() error {
	if l == nil {
		return nil
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.closed {
		return l.closeErr
	}
	l.closed = true
	if l.closeFn != nil {
		l.closeErr = l.closeFn()
	}
	return l.closeErr
}

func observeManagedRouterGeneration(
	ctx context.Context,
	candidatePort int,
	pidportPath string,
	expectedExecutable string,
	deps managedRouterAuthorizerDeps,
) (managedRouterProcessGeneration, ProcessIdentity, string) {
	pid, port, err := deps.readPidport(pidportPath)
	if err != nil || pid <= 0 || port <= 0 || port > 65535 {
		return managedRouterProcessGeneration{}, ProcessIdentity{}, api.ManagedRouterFailurePIDPortUnavailable
	}
	if port != candidatePort {
		return managedRouterProcessGeneration{}, ProcessIdentity{}, api.ManagedRouterFailurePIDPortPortMismatch
	}
	info, err := deps.statPidport(pidportPath)
	if err != nil {
		return managedRouterProcessGeneration{}, ProcessIdentity{}, api.ManagedRouterFailurePIDPortUnavailable
	}
	ownerPID, found, err := deps.portOwner(ctx, candidatePort)
	if err != nil || !found || ownerPID <= 0 {
		return managedRouterProcessGeneration{}, ProcessIdentity{}, api.ManagedRouterFailureSocketOwnerUnavailable
	}
	if ownerPID != pid {
		return managedRouterProcessGeneration{}, ProcessIdentity{}, api.ManagedRouterFailureSocketOwnerMismatch
	}
	identity, err := deps.processID(pid)
	if err != nil || !identity.Alive || identity.Denied || identity.Handle == 0 {
		_ = deps.closeID(&identity)
		return managedRouterProcessGeneration{}, ProcessIdentity{}, api.ManagedRouterFailureProcessUnavailable
	}
	reject := func(failure string) (managedRouterProcessGeneration, ProcessIdentity, string) {
		_ = deps.closeID(&identity)
		return managedRouterProcessGeneration{}, ProcessIdentity{}, failure
	}
	observedExecutable, ok := canonicalManagedRouterExecutable(identity.ImagePath)
	if !ok || !managedRouterPathsEqual(observedExecutable, expectedExecutable) {
		return reject(api.ManagedRouterFailureExecutableMismatch)
	}
	if !cmdlineIsGui(identity.Cmdline) {
		return reject(api.ManagedRouterFailureArgvRoleMismatch)
	}
	if identity.StartTime.IsZero() || !startTimeBeforeMtime(identity.StartTime, info.ModTime(), time.Second) {
		return reject(api.ManagedRouterFailureProcessGenerationInvalid)
	}
	ownerMatches, err := deps.ownerMatch(pid)
	if err != nil {
		return reject(api.ManagedRouterFailureProcessOwnerUnavailable)
	}
	if !ownerMatches {
		return reject(api.ManagedRouterFailureProcessOwnerMismatch)
	}
	return managedRouterProcessGeneration{
		pid:        pid,
		port:       port,
		ownerPID:   ownerPID,
		executable: observedExecutable,
		startTime:  identity.StartTime,
	}, identity, ""
}

func canonicalManagedRouterExecutable(raw string) (string, bool) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", false
	}
	abs, err := filepath.Abs(filepath.Clean(raw))
	if err != nil {
		return "", false
	}
	if resolved, err := filepath.EvalSymlinks(abs); err == nil {
		abs = resolved
	}
	return filepath.Clean(abs), true
}

func managedRouterPathsEqual(a, b string) bool {
	if runtime.GOOS == "windows" {
		return strings.EqualFold(filepath.Clean(a), filepath.Clean(b))
	}
	return filepath.Clean(a) == filepath.Clean(b)
}

func strictManagedRouterPing(ctx context.Context, port int) (managedRouterPing, string) {
	requestCtx, cancel := context.WithTimeout(ctx, managedRouterPingTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(requestCtx, http.MethodGet, "http://127.0.0.1:"+strconv.Itoa(port)+"/api/ping", nil)
	if err != nil {
		return managedRouterPing{}, api.ManagedRouterFailurePingTransport
	}
	client := &http.Client{
		Timeout:       managedRouterPingTimeout,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	resp, err := client.Do(req)
	if err != nil {
		return managedRouterPing{}, api.ManagedRouterFailurePingTransport
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return managedRouterPing{}, api.ManagedRouterFailurePingHTTPStatus
	}
	mediaType, _, err := mime.ParseMediaType(resp.Header.Get("Content-Type"))
	if err != nil || !strings.EqualFold(mediaType, "application/json") {
		return managedRouterPing{}, api.ManagedRouterFailurePingContentType
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, managedRouterPingBodyMax+1))
	if err != nil || len(body) > managedRouterPingBodyMax {
		return managedRouterPing{}, api.ManagedRouterFailurePingResponseTooLarge
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	var ping managedRouterPing
	if err := decoder.Decode(&ping); err != nil {
		return managedRouterPing{}, api.ManagedRouterFailurePingMalformed
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		return managedRouterPing{}, api.ManagedRouterFailurePingMalformed
	}
	return ping, ""
}

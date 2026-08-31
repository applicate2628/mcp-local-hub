package gui

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
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
	readPidport       func(string) (int, int, error)
	statPidport       func(string) (os.FileInfo, error)
	portOwner         func(context.Context, int) (int, bool, error)
	processID         func(int) (ProcessIdentity, error)
	closeID           func(*ProcessIdentity) error
	releaseDiagnostic func(string, error)
	ownerMatch        func(int) (bool, error)
	ping              func(context.Context, int) (managedRouterPing, string)
}

type managedRouterPing struct {
	OK      bool   `json:"ok"`
	PID     int    `json:"pid"`
	Version string `json:"version"`
}

type managedRouterVersion struct {
	Version string `json:"version"`
	Commit  string `json:"commit"`
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
				return identity.Close()
			},
			releaseDiagnostic: func(primaryClass string, err error) {
				log.Printf("managed-router-identity-release-unconfirmed: class=%s: %v", primaryClass, err)
			},
			ownerMatch: processutil.ProcessOwnerMatchesCurrent,
			ping:       strictManagedRouterPing,
		},
	)
}

// VerifyManagedRouterGeneration is the recovery-grade GUI verifier. It reuses
// NewManagedRouterAuthorizer as the sole owner of pidport, socket, retained
// PID, executable, argv, strict-ping, and generation-revalidation checks, then
// binds that retained generation to /api/version's exact version and commit.
func VerifyManagedRouterGeneration(ctx context.Context, pidportPath, currentExecutable, expectedVersion, expectedCommit string, candidatePort int) error {
	return verifyManagedRouterGeneration(ctx, candidatePort, expectedVersion, expectedCommit,
		NewManagedRouterAuthorizer(pidportPath, currentExecutable, expectedVersion), strictManagedRouterVersion)
}

func verifyManagedRouterGeneration(ctx context.Context, candidatePort int, expectedVersion, expectedCommit string, authorize api.ManagedRouterAuthorizer, probeVersion func(context.Context, int) (managedRouterVersion, error)) error {
	if ctx == nil {
		ctx = context.Background()
	}
	if strings.TrimSpace(expectedVersion) == "" || strings.EqualFold(strings.TrimSpace(expectedVersion), "dev") || strings.EqualFold(strings.TrimSpace(expectedVersion), "unknown") {
		return fmt.Errorf("managed router generation: expected version is uninformative")
	}
	if strings.TrimSpace(expectedCommit) == "" || strings.EqualFold(strings.TrimSpace(expectedCommit), "unknown") {
		return fmt.Errorf("managed router generation: expected commit is uninformative")
	}
	if authorize == nil || probeVersion == nil {
		return fmt.Errorf("managed router generation: verifier dependency unavailable")
	}
	auth := authorize(ctx, candidatePort)
	if auth.Lease == nil {
		return fmt.Errorf("managed router generation: authorization refused: %s", auth.FailureClass)
	}
	finish := func(primary error) error {
		closeErr := auth.Lease.Close()
		if primary != nil && closeErr != nil {
			return errors.Join(primary, fmt.Errorf("managed router generation: release retained identity: %w", closeErr))
		}
		if primary != nil {
			return primary
		}
		if closeErr != nil {
			return fmt.Errorf("managed router generation: release retained identity: %w", closeErr)
		}
		return nil
	}
	version, err := probeVersion(ctx, candidatePort)
	if err != nil {
		return finish(fmt.Errorf("managed router generation: version readback: %w", err))
	}
	if version.Version != expectedVersion || version.Commit != expectedCommit {
		return finish(fmt.Errorf("managed router generation: version or commit mismatch"))
	}
	if failure := auth.Lease.Revalidate(ctx); failure != "" {
		return finish(fmt.Errorf("managed router generation: final generation revalidation: %s", failure))
	}
	return finish(nil)
}

func newManagedRouterAuthorizerWithDeps(
	pidportPath, currentExecutable, expectedVersion string,
	deps managedRouterAuthorizerDeps,
) api.ManagedRouterAuthorizer {
	if deps.closeID == nil {
		deps.closeID = func(identity *ProcessIdentity) error {
			return identity.Close()
		}
	}
	if deps.releaseDiagnostic == nil {
		deps.releaseDiagnostic = func(string, error) {}
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
			_ = settleTemporaryProcessIdentities(deps, failure, &firstIdentity)
			return api.ManagedRouterAuthorization{FailureClass: failure}
		}
		if !ping.OK || ping.PID != first.pid || strings.TrimSpace(ping.Version) == "" || ping.Version != version {
			_ = settleTemporaryProcessIdentities(deps, api.ManagedRouterFailurePingIdentityMismatch, &firstIdentity)
			return api.ManagedRouterAuthorization{FailureClass: api.ManagedRouterFailurePingIdentityMismatch}
		}
		second, secondIdentity, failure := observeManagedRouterGeneration(authorizationCtx, candidatePort, pidportPath, executable, deps)
		if failure != "" {
			_ = settleTemporaryProcessIdentities(deps, api.ManagedRouterFailureIdentityChanged, &firstIdentity)
			// The same observation already succeeded before the ping. Any
			// post-response refusal means the authorized generation is no longer
			// stable; keep the external taxonomy independent of which signal raced.
			return api.ManagedRouterAuthorization{FailureClass: api.ManagedRouterFailureIdentityChanged}
		}
		if first != second {
			_ = settleTemporaryProcessIdentities(deps, api.ManagedRouterFailureIdentityChanged, &firstIdentity, &secondIdentity)
			return api.ManagedRouterAuthorization{FailureClass: api.ManagedRouterFailureIdentityChanged}
		}
		if err := settleTemporaryProcessIdentities(deps, api.ManagedRouterFailureIdentityUnavailable, &firstIdentity); err != nil {
			_ = settleTemporaryProcessIdentities(deps, api.ManagedRouterFailureIdentityUnavailable, &secondIdentity)
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

func settleTemporaryProcessIdentities(deps managedRouterAuthorizerDeps, primaryClass string, identities ...*ProcessIdentity) error {
	var releaseErrs []error
	for _, identity := range identities {
		if identity == nil {
			continue
		}
		if err := deps.closeID(identity); err != nil {
			releaseErrs = append(releaseErrs, err)
		}
	}
	if err := errors.Join(releaseErrs...); err != nil {
		deps.releaseDiagnostic(primaryClass, err)
		return err
	}
	return nil
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
	if first != generation {
		_ = settleTemporaryProcessIdentities(deps, api.ManagedRouterFailureIdentityChanged, &firstIdentity)
		return api.ManagedRouterFailureIdentityChanged
	}
	ping, failure := deps.ping(revalidationCtx, generation.port)
	if failure != "" ||
		!ping.OK ||
		ping.PID != generation.pid ||
		strings.TrimSpace(ping.Version) == "" ||
		ping.Version != expectedVersion {
		_ = settleTemporaryProcessIdentities(deps, api.ManagedRouterFailureIdentityChanged, &firstIdentity)
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
		_ = settleTemporaryProcessIdentities(deps, api.ManagedRouterFailureIdentityChanged, &firstIdentity)
		return api.ManagedRouterFailureIdentityChanged
	}
	if err := settleTemporaryProcessIdentities(deps, api.ManagedRouterFailureIdentityChanged, &firstIdentity, &secondIdentity); err != nil || second != generation {
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
		_ = settleTemporaryProcessIdentities(deps, api.ManagedRouterFailureProcessUnavailable, &identity)
		return managedRouterProcessGeneration{}, ProcessIdentity{}, api.ManagedRouterFailureProcessUnavailable
	}
	reject := func(failure string) (managedRouterProcessGeneration, ProcessIdentity, string) {
		_ = settleTemporaryProcessIdentities(deps, failure, &identity)
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

func strictManagedRouterVersion(ctx context.Context, port int) (managedRouterVersion, error) {
	if port <= 0 || port > 65535 {
		return managedRouterVersion{}, fmt.Errorf("invalid GUI port %d", port)
	}
	requestCtx, cancel := context.WithTimeout(ctx, managedRouterPingTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(requestCtx, http.MethodGet, "http://127.0.0.1:"+strconv.Itoa(port)+"/api/version", nil)
	if err != nil {
		return managedRouterVersion{}, err
	}
	client := &http.Client{Timeout: managedRouterPingTimeout, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := client.Do(req)
	if err != nil {
		return managedRouterVersion{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return managedRouterVersion{}, fmt.Errorf("unexpected HTTP status %d", resp.StatusCode)
	}
	mediaType, _, err := mime.ParseMediaType(resp.Header.Get("Content-Type"))
	if err != nil || !strings.EqualFold(mediaType, "application/json") {
		return managedRouterVersion{}, fmt.Errorf("unexpected content type")
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, managedRouterPingBodyMax+1))
	if err != nil || len(body) > managedRouterPingBodyMax {
		return managedRouterVersion{}, fmt.Errorf("invalid response body")
	}
	var value managedRouterVersion
	decoder := json.NewDecoder(bytes.NewReader(body))
	if err := decoder.Decode(&value); err != nil {
		return managedRouterVersion{}, fmt.Errorf("decode response: %w", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		return managedRouterVersion{}, fmt.Errorf("trailing response data")
	}
	if strings.TrimSpace(value.Version) == "" || strings.TrimSpace(value.Commit) == "" {
		return managedRouterVersion{}, fmt.Errorf("response version or commit missing")
	}
	return value, nil
}

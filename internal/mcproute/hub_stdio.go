package mcproute

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"time"

	"mcp-local-hub/internal/api"
	"mcp-local-hub/internal/daemon"
)

// Preparation belongs to the Route composition: it uses the captured parent
// environment and the existing CLI secret/overlay materializer. No API or HTTP
// caller can supply a command, an environment map or a launch capability.
type HubStdioPreparation func(context.Context, *api.API, api.HubStdioEndpoint) (*daemon.StdioLaunchContext, daemon.HostConfig, error)

type hubStdioSessions struct {
	providers map[api.HubStdioEndpoint]*daemon.IsolatedStdioSessions
}

type hubStdioRequestKey struct{}
type hubStdioRequest struct {
	owner    *hubStdioSessions
	endpoint api.HubStdioEndpoint
	session  context.Context
	current  func() bool
}

func prepareHubStdioSessions(ctx context.Context, a *api.API, prepare HubStdioPreparation) (*hubStdioSessions, error) {
	owner := &hubStdioSessions{providers: make(map[api.HubStdioEndpoint]*daemon.IsolatedStdioSessions)}
	for _, endpoint := range api.LoadResolverSnapshot().SessionLocalStdioEndpoints() {
		if endpoint.Configuration == "" {
			return owner, daemon.ErrIsolatedStdioUnavailable
		}
		launch, cfg, err := prepare(ctx, a, endpoint)
		if err != nil {
			return owner, err
		}
		// Application policy: four simultaneous front sessions per provider,
		// charging eight reservation units per original launcher/provider tree.
		// These are not OS process-count limits or MCP requirements. Ordinary
		// shared daemons do not use this allocation. Original settlement alone
		// returns a reservation; native provider qualification remains separate.
		limits := daemon.IsolatedStdioLimits{
			TreeCapacity: 32, TreeCost: 8, RetryAfterSeconds: 2,
			MaxRequestIDs: 4096, MaxIDBytes: 512, Startup: 60 * time.Second,
			Operation: 60 * time.Second, Idle: 30 * time.Minute,
			Teardown: 5 * time.Second, EOFGrace: time.Second,
		}
		provider, err := launch.NewObservedIsolatedStdioSessions(cfg, limits, func(r *http.Request) daemon.IsolatedStdioAuthorization {
			grant, _ := r.Context().Value(hubStdioRequestKey{}).(*hubStdioRequest)
			if grant == nil || grant.owner != owner || grant.endpoint != endpoint || grant.session == nil || grant.current == nil || !grant.current() {
				return daemon.IsolatedStdioAuthorization{}
			}
			return daemon.IsolatedStdioAuthorization{Role: daemon.IsolatedStdioHub, Session: grant.session}
		}, func(d daemon.IsolatedStdioSettlementDiagnostic) {
			_ = routeReadOnlySink("info", "private_stdio_settlement", hubStdioSettlementFields(d))
		})
		if err != nil {
			return owner, err
		}
		owner.providers[endpoint] = provider
	}
	return owner, nil
}

// hubStdioSettlementFields permits only receipt metadata; it has no access to
// a session token, context, launch inputs or endpoint configuration.
func hubStdioSettlementFields(d daemon.IsolatedStdioSettlementDiagnostic) map[string]any {
	known := d.OriginalPID > 0 && d.OriginalBirthFILETIME > 0
	fields := map[string]any{
		"role": "hub", "session_sequence": d.Sequence, "complete": d.Complete, "original_known": known,
	}
	if known {
		fields["original_pid"] = d.OriginalPID
		fields["original_birth_filetime"] = d.OriginalBirthFILETIME
	}
	return fields
}

func (p *hubStdioSessions) Allows(endpoint api.HubStdioEndpoint) bool {
	return p != nil && p.providers[endpoint] != nil
}

func (p *hubStdioSessions) RoundTrip(r *http.Request, session context.Context, endpoint api.HubStdioEndpoint, current func() bool) (*http.Response, error) {
	if !p.Allows(endpoint) || session == nil || session.Err() != nil || current == nil || !current() || r.URL.Path != "/mcp" ||
		(r.Method != http.MethodPost && r.Method != http.MethodDelete) {
		return nil, daemon.ErrIsolatedStdioUnavailable
	}
	grant := &hubStdioRequest{owner: p, endpoint: endpoint, session: session, current: current}
	r = r.WithContext(context.WithValue(r.Context(), hubStdioRequestKey{}, grant))
	if r.Body != nil {
		defer r.Body.Close()
	}
	response := &hubStdioResponse{header: make(http.Header)}
	// This call is synchronous: no transport worker can outlive the aggregate
	// request. The private handler's operation/startup contexts bound its wait.
	p.providers[endpoint].HTTPHandler().ServeHTTP(response, r)
	if response.err != nil {
		return nil, response.err
	}
	if response.status == 0 {
		response.status = http.StatusOK
	}
	return &http.Response{StatusCode: response.status, Header: response.header,
		Body: io.NopCloser(bytes.NewReader(response.body.Bytes())), Request: r}, nil
}

func (p *hubStdioSessions) Close(ctx context.Context) error {
	if p == nil {
		return nil
	}
	var result error
	for _, provider := range p.providers {
		result = errors.Join(result, provider.Close(ctx))
	}
	return result
}

func (p *hubStdioSessions) CaptureRetirement(session context.Context, endpoint api.HubStdioEndpoint) (func(context.Context) error, error) {
	if !p.Allows(endpoint) {
		return nil, daemon.ErrIsolatedStdioUnavailable
	}
	receipt := p.providers[endpoint].RetirementForSession(session)
	if receipt == nil {
		return nil, daemon.ErrIsolatedStdioUnavailable
	}
	return func(ctx context.Context) error {
		settled, err := receipt.Await(ctx)
		if !settled.Complete() {
			return errors.Join(err, daemon.ErrIsolatedStdioUnavailable)
		}
		return err
	}, nil
}

// Capture under every original provider's admission gate after authority was
// revoked. An old credential cannot reserve a later row. The original receipt
// retains its front lifetime even after public SID deletion, so retry cannot
// mistake a missing HubSessionStore row for native settlement.
func (p *hubStdioSessions) AwaitRevokedSessions(ctx context.Context, revoked func(context.Context) (bool, error)) error {
	var result error
	for _, provider := range p.providers {
		for _, original := range provider.Retirements() {
			selected, err := revoked(original.Session())
			if err != nil {
				result = errors.Join(result, err)
				continue
			}
			if !selected {
				continue
			}
			if original.Session() == nil || original.Session().Err() == nil {
				result = errors.Join(result, daemon.ErrIsolatedStdioUnavailable)
				continue
			}
			receipt, err := original.Await(ctx)
			if !receipt.Complete() {
				err = errors.Join(err, daemon.ErrIsolatedStdioUnavailable)
			}
			result = errors.Join(result, err)
		}
	}
	return result
}

// Only bounded POST/DELETE responses cross this private in-process boundary.
// It is deliberately not mounted on a socket and does not accept GET streams.
type hubStdioResponse struct {
	header http.Header
	status int
	body   bytes.Buffer
	err    error
}

func (w *hubStdioResponse) Header() http.Header { return w.header }
func (w *hubStdioResponse) WriteHeader(status int) {
	if w.status == 0 {
		w.status = status
	}
}
func (w *hubStdioResponse) Write(data []byte) (int, error) {
	w.WriteHeader(http.StatusOK)
	if w.body.Len()+len(data) > 4<<20 {
		w.err = errors.New("private MCP response exceeds aggregate body limit")
		return 0, w.err
	}
	return w.body.Write(data)
}

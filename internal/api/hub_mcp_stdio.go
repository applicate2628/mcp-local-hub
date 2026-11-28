package api

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"slices"
	"time"

	"mcp-local-hub/internal/config"
)

// HubStdioEndpoint is a captured descriptor, not permission to launch. The
// listener prepares its private transport from the same descriptor. Changes to
// launch inputs refuse dispatch until that listener generation is replaced.
type HubStdioEndpoint struct {
	Server, Daemon string
	Port           int
	Configuration  string
}

func SessionLocalStdioEndpoint(m *config.ServerManifest, d *config.DaemonSpec) HubStdioEndpoint {
	if m == nil || d == nil || m.Transport != config.TransportStdioBridge || d.MCPStdioState != config.MCPStdioStateSessionLocal {
		return HubStdioEndpoint{}
	}
	// Binding and visibility edits do not change the provider's launch. Preserve
	// the existing resolver's ability to narrow those independently.
	data, _ := json.Marshal(struct {
		Command string
		Args    []string
		Env     map[string]string
		Forward []string
		Daemon  config.DaemonSpec
	}{m.Command, m.BaseArgs, m.Env, m.EnvForwardLocal, *d})
	sum := sha256.Sum256(data)
	return HubStdioEndpoint{Server: m.Name, Daemon: d.Name, Port: d.Port, Configuration: hex.EncodeToString(sum[:])}
}

// HubStdioSessions belongs to the original protected listener. The session
// context is created by HubSessionStore after front authentication; it is not
// accepted from a wire header and remains stable across individual requests.
// RoundTrip must receive its handler result before returning, including errors.
type HubStdioSessions interface {
	Allows(HubStdioEndpoint) bool
	RoundTrip(*http.Request, context.Context, HubStdioEndpoint, func() bool) (*http.Response, error)
}

type hubStdioCredential struct {
	digest  [sha256.Size]byte
	project *hubProjectRebindReceiving
}

// Carried only by the lifetime created in HubSessionStore. The original
// provider retains this value until its own receipt is complete, even after
// the public SID has been deleted. It is never supplied by an HTTP caller.
type hubStdioFrontContextKey struct{}

type hubStdioRetirementCapture interface {
	CaptureRetirement(context.Context, HubStdioEndpoint) (func(context.Context) error, error)
}

// AdmitSessionLocalStdio belongs to the original front/session owner. The
// caller has already passed its independent control-token gate. A positive
// probe is returned only after initialize, tools/list and the original native
// process/stream receipt; DELETE/HTTP acknowledgement is not settlement.
func (h *HubMcpHandler) AdmitSessionLocalStdio(ctx context.Context, request HubStdioAdmissionRequest) (resultErr error) {
	snapshot := LoadResolverSnapshot()
	endpoint := request.Endpoint
	ref := canonicalDaemonRef{Server: endpoint.Server, Daemon: endpoint.Daemon, Port: endpoint.Port}
	if snapshot == nil || snapshot.sessionLocalEndpoints[ref] != endpoint || endpoint.Configuration == "" || !h.sessions.allowsEndpoint(snapshot, ref) {
		return errHubDaemonAdmission
	}
	if !request.Probe {
		return nil
	}
	if !isSupportedClient(request.Scope) || !slices.Contains(snapshot.Bindings[request.Scope], ref) {
		return errHubDaemonAdmission
	}
	current := liveTokenTable.Load()
	if current == nil || current.guarded(request.Scope) || !isLowerHex64(current.tokens.Tokens[request.Scope]) {
		return errHubDaemonAdmission
	}
	capture, ok := h.sessions.opts.StdioSessions.(hubStdioRetirementCapture)
	if !ok {
		return errHubDaemonAdmission
	}
	session, err := h.sessions.Create(request.Scope, hubProtocolVersionFallback, snapshot)
	if err != nil {
		return err
	}
	session.mu.Lock()
	session.InstanceID = h.currentInstanceID()
	session.stdioCredential = &hubStdioCredential{digest: sha256.Sum256([]byte(current.tokens.Tokens[request.Scope]))}
	session.IntendedParticipants = []canonicalDaemonRef{ref}
	session.mu.Unlock()
	defer func() {
		// Capture before cancellation can remove a completely settled row.
		await, captureErr := capture.CaptureRetirement(session.stdioLifetime, endpoint)
		h.sessions.Delete(session.ClientSessionID)
		if captureErr != nil {
			resultErr = errors.Join(resultErr, captureErr)
			return
		}
		cleanup, cancel := context.WithTimeout(context.Background(), 6*time.Second)
		defer cancel()
		resultErr = errors.Join(resultErr, await(cleanup))
	}()
	if _, err := AggregateInitialize(ctx, session, json.RawMessage(`1`)); err != nil {
		return err
	}
	session.mu.Lock()
	initialized := len(session.InitSuccesses) == 1 && len(session.InitFailures) == 0
	session.mu.Unlock()
	if !initialized {
		return errHubDaemonAdmission
	}
	body, err := AggregateToolsList(ctx, session, json.RawMessage(`2`), h.currentInstanceID())
	if err != nil {
		return err
	}
	var reply struct {
		Error  json.RawMessage `json:"error"`
		Result struct {
			Tools json.RawMessage `json:"tools"`
			Meta  struct {
				Hub struct {
					Failures []json.RawMessage `json:"partialFailures"`
				} `json:"mcphub"`
			} `json:"_meta"`
		} `json:"result"`
	}
	if json.Unmarshal(body, &reply) != nil || len(reply.Error) != 0 || len(reply.Result.Tools) == 0 || len(reply.Result.Meta.Hub.Failures) != 0 {
		return errHubDaemonAdmission
	}
	return nil
}

func (s *hubSession) stdioCredentialCurrent() bool {
	if s.stdioCredential == nil {
		return false
	}
	current := liveTokenTable.Load()
	if current != nil {
		token := current.tokens.Tokens[s.ScopeKey]
		digest := sha256.Sum256([]byte(token))
		if token != "" && subtle.ConstantTimeCompare(digest[:], s.stdioCredential.digest[:]) == 1 {
			if s.stdioCredential.project != nil {
				_, ok := currentHubProjectEnrollment(s.ScopeKey, token, *s.stdioCredential.project)
				if ok {
					return true
				}
			} else if !current.guarded(s.ScopeKey) {
				return true
			}
		}
	}
	// The old credential cannot retain a usable private conversation after
	// rotation. Cancellation also retires providers whose initialize is late.
	if s.stdioCancel != nil {
		s.stdioCancel()
	}
	return false
}

func sessionLocalHubEndpoints(manifests []config.ServerManifest) map[canonicalDaemonRef]HubStdioEndpoint {
	result := make(map[canonicalDaemonRef]HubStdioEndpoint)
	ports := make(map[int]int)
	identities := make(map[canonicalDaemonRef]int)
	for _, m := range manifests {
		for _, d := range m.Daemons {
			ports[d.Port]++
			identities[canonicalDaemonRef{Server: m.Name, Daemon: d.Name}]++
		}
	}
	for _, m := range manifests {
		for _, d := range m.Daemons {
			endpoint := SessionLocalStdioEndpoint(&m, &d)
			if endpoint.Configuration != "" {
				if m.Name == "" || d.Name == "" || d.Port <= 0 || d.Port > 65535 || ports[d.Port] != 1 || identities[canonicalDaemonRef{Server: m.Name, Daemon: d.Name}] != 1 {
					// Preserve its private classification while withholding dispatch.
					// An ambiguous descriptor must not become an ordinary fallback.
					endpoint.Configuration = ""
				}
				result[canonicalDaemonRef{Server: m.Name, Daemon: d.Name, Port: d.Port}] = endpoint
			}
		}
	}
	return result
}

// SessionLocalStdioEndpoints returns immutable descriptor values for the
// listener's preparation. It exposes neither credentials nor launch inputs.
func (s *ResolverSnapshot) SessionLocalStdioEndpoints() []HubStdioEndpoint {
	if s == nil {
		return nil
	}
	result := make([]HubStdioEndpoint, 0, len(s.sessionLocalEndpoints))
	for _, endpoint := range s.sessionLocalEndpoints {
		result = append(result, endpoint)
	}
	slices.SortFunc(result, func(a, b HubStdioEndpoint) int { return a.Port - b.Port })
	return result
}

func (s *HubSessionStore) allowsEndpoint(snapshot *ResolverSnapshot, ref canonicalDaemonRef) bool {
	if snapshot == nil {
		return false
	}
	if endpoint, private := snapshot.sessionLocalEndpoints[ref]; private {
		return endpoint.Configuration != "" && s.opts.StdioSessions != nil && s.opts.StdioSessions.Allows(endpoint)
	}
	return snapshot.allowsOrdinaryEndpoint(ref)
}

func (s *hubSession) privateEndpoint(ref canonicalDaemonRef) (HubStdioEndpoint, bool) {
	if s == nil || s.SnapshotAtInit == nil {
		return HubStdioEndpoint{}, false
	}
	for original, endpoint := range s.SnapshotAtInit.sessionLocalEndpoints {
		if sameDaemonIdentity(original, ref) {
			return endpoint, true
		}
	}
	return HubStdioEndpoint{}, false
}

func (s *hubSession) allowsDaemonBinding(snapshot *ResolverSnapshot, ref canonicalDaemonRef) bool {
	if snapshot == nil {
		return false
	}
	if endpoint, private := s.privateEndpoint(ref); private {
		return s.stdioSessions != nil && s.stdioLifetime != nil && s.stdioLifetime.Err() == nil && s.stdioCredentialCurrent() &&
			snapshot.sessionLocalEndpoints[ref] == endpoint && s.stdioSessions.Allows(endpoint) &&
			slices.Contains(snapshot.Bindings[s.ScopeKey], ref)
	}
	return snapshot.allowsOrdinaryBinding(s.ScopeKey, ref)
}

func (s *hubSession) daemonRequest(ref canonicalDaemonRef, captured bool) hubDaemonRequest {
	return hubDaemonRequest{ref: ref, scope: s.ScopeKey, captured: captured, session: s}
}

type hubStdioTransport struct {
	session  *hubSession
	endpoint HubStdioEndpoint
	target   hubDaemonRequest
}

func (t hubStdioTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	if t.session.stdioSessions == nil || t.session.stdioLifetime == nil {
		return nil, errHubDaemonAdmission
	}
	return t.session.stdioSessions.RoundTrip(r, t.session.stdioLifetime, t.endpoint, t.target.allowed)
}

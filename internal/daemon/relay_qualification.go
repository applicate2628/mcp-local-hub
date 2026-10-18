package daemon

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mcp-local-hub/internal/mcpcompat"
	"mcp-local-hub/internal/process"
	"mime"
	"net"
	"net/http"
	"net/url"
	"reflect"
	"sync"
)

type MCPOriginalHostEndpointEvidence interface {
	ValidateOriginalEndpoint(context.Context, string, string) error
	ValidateOriginalConnection(context.Context, *MCPHTTPOriginalConnection) error
	CloseObserved(context.Context) (*process.OriginalJobMemberCloseObservation, error)
	ValidateOriginalClose(context.Context, *process.OriginalJobMemberCloseObservation, string) error
}

func (c *MCPHTTPOriginalConnection) ValidateEndpointSource(ctx context.Context, source MCPOriginalHostEndpointEvidence, endpointSHA, startSHA string) error {
	if c == nil || c.owner == nil || c.owner.binding.EndpointSource != source || c.owner.binding.EndpointSHA256 != endpointSHA || c.owner.binding.HostStartSHA256 != startSHA {
		return errors.New("MCP_ORIGINAL_CONNECTION_ENDPOINT_SOURCE_DRIFT")
	}
	return c.ValidateBinding(ctx, c.owner.binding)
}

type MCPHTTPOriginalConnection struct {
	owner         *mcpHTTPQualificationOwner
	connection    *mcpObservedConnection
	local, remote net.TCPAddr
	bindingSHA    string
}
type MCPHTTPOriginalConnectionSnapshot struct {
	BindingSHA256         string
	LocalHost, RemoteHost string
	LocalPort, RemotePort int
}

func (c *MCPHTTPOriginalConnection) Snapshot() MCPHTTPOriginalConnectionSnapshot {
	if c == nil || c.owner == nil || c.connection == nil {
		return MCPHTTPOriginalConnectionSnapshot{}
	}
	return MCPHTTPOriginalConnectionSnapshot{c.bindingSHA, c.local.IP.String(), c.remote.IP.String(), c.local.Port, c.remote.Port}
}
func (c *MCPHTTPOriginalConnection) ValidateBinding(ctx context.Context, b MCPHTTPQualificationBinding) error {
	if c == nil || ctx == nil || ctx.Err() != nil || c.owner == nil || c.connection == nil || c.bindingSHA != qualificationBindingSHA(b) || !reflect.DeepEqual(c.owner.binding, b) {
		return errors.New("MCP_QUALIFICATION_CONNECTION_SOURCE_MISMATCH")
	}
	c.connection.mu.Lock()
	defer c.connection.mu.Unlock()
	if c.connection.closing || c.connection.closed {
		return errors.New("MCP_QUALIFICATION_CONNECTION_NOT_LIVE")
	}
	return nil
}

type MCPHTTPQualificationBinding struct {
	OperationID, QualificationID, RequestIdentitySHA256, GenerationID string
	SourceSHA256, HostStartSHA256, EndpointSHA256                     string
	EndpointSource                                                    MCPOriginalHostEndpointEvidence
}
type MCPHTTPBodyObservation struct {
	Method                 string `json:"method"`
	Status                 int    `json:"status"`
	Bytes                  int64  `json:"bytes"`
	ReadDisposition        string `json:"read_disposition"`
	CloseAttempted, Closed bool
	Error                  string `json:"error,omitempty"`
}
type MCPHTTPConnectionObservation struct {
	CloseAttempted, Closed, IOComplete bool
	Error                              string `json:"error,omitempty"`
}
type MCPHTTPQualificationSnapshot struct {
	Schema                                                 string `json:"schema"`
	Version                                                int    `json:"version"`
	BindingSHA256                                          string `json:"binding_sha256"`
	RequestIDs, ReplyIDs                                   []string
	ProtocolVersion, ServerInfoSHA256, ToolCatalogueSHA256 string
	SessionAcquired                                        bool
	SessionDeleteDisposition                               string
	Bodies                                                 []MCPHTTPBodyObservation
	Connections                                            []MCPHTTPConnectionObservation
	ProtocolComplete, ResourcesComplete, Complete          bool
	PrimaryError, CleanupError                             string
}
type MCPHTTPQualificationObservation struct{ owner *mcpHTTPQualificationOwner }
type MCPHTTPQualificationCloseObservation struct {
	owner    *mcpHTTPQualificationOwner
	complete bool
	err      error
}
type mcpHTTPQualificationOwner struct {
	relay                             *HTTPToStdioRelay
	binding                           MCPHTTPQualificationBinding
	client                            *http.Client
	transport                         *http.Transport
	mu                                sync.Mutex
	conns                             []*mcpObservedConnection
	active, activeDials               int
	finished, closing, closeAttempted bool
	pendingSession                    string
	close                             *MCPHTTPQualificationCloseObservation
	snapshot                          MCPHTTPQualificationSnapshot
}

func qualificationSHA(raw []byte) string { v := sha256.Sum256(raw); return hex.EncodeToString(v[:]) }
func qualificationDigest(v string) bool  { b, e := hex.DecodeString(v); return e == nil && len(b) == 32 }
func (b MCPHTTPQualificationBinding) validate(ctx context.Context, endpoint string) error {
	if ctx == nil || ctx.Err() != nil {
		return errors.New("MCP_QUALIFICATION_CONTEXT_INVALID")
	}
	if _, ok := ctx.Deadline(); !ok {
		return errors.New("MCP_QUALIFICATION_DEADLINE_REQUIRED")
	}
	if b.OperationID == "" || b.QualificationID == "" || b.GenerationID == "" || b.EndpointSource == nil {
		return errors.New("MCP_QUALIFICATION_BINDING_MISSING")
	}
	for _, v := range []string{b.RequestIdentitySHA256, b.SourceSHA256, b.HostStartSHA256, b.EndpointSHA256} {
		if !qualificationDigest(v) {
			return errors.New("MCP_QUALIFICATION_DIGEST_INVALID")
		}
	}
	u, e := url.Parse(endpoint)
	if e != nil || u.Scheme != "http" || u.User != nil || u.Hostname() != "127.0.0.1" || u.Port() == "" || u.Path != "/mcp" || u.RawQuery != "" || u.Fragment != "" || qualificationSHA([]byte(endpoint)) != b.EndpointSHA256 {
		return errors.New("MCP_QUALIFICATION_ENDPOINT_INVALID")
	}
	return b.EndpointSource.ValidateOriginalEndpoint(ctx, endpoint, b.HostStartSHA256)
}
func qualificationBindingSHA(b MCPHTTPQualificationBinding) string {
	raw, _ := json.Marshal(struct{ OperationID, QualificationID, RequestIdentitySHA256, GenerationID, SourceSHA256, HostStartSHA256, EndpointSHA256 string }{b.OperationID, b.QualificationID, b.RequestIdentitySHA256, b.GenerationID, b.SourceSHA256, b.HostStartSHA256, b.EndpointSHA256})
	return qualificationSHA(raw)
}
func (r *HTTPToStdioRelay) QualifyMCPObserved(ctx context.Context, b MCPHTTPQualificationBinding) (observation *MCPHTTPQualificationObservation, retErr error) {
	if err := b.validate(ctx, r.URL); err != nil {
		return nil, err
	}
	r.qualificationMu.Lock()
	if r.qualification != nil || r.getSessionID() != "" {
		r.qualificationMu.Unlock()
		return nil, errors.New("MCP_QUALIFICATION_REENTRY_REFUSED")
	}
	o := &mcpHTTPQualificationOwner{relay: r, binding: b}
	o.snapshot = MCPHTTPQualificationSnapshot{Schema: "mcp-http-qualification-v1", Version: 1, BindingSHA256: qualificationBindingSHA(b), Bodies: []MCPHTTPBodyObservation{}, Connections: []MCPHTTPConnectionObservation{}}
	o.transport = &http.Transport{Proxy: nil, DisableKeepAlives: true, ForceAttemptHTTP2: false}
	o.transport.DialContext = func(c context.Context, network, address string) (net.Conn, error) {
		o.mu.Lock()
		if o.closing {
			o.mu.Unlock()
			return nil, errors.New("MCP_QUALIFICATION_TRANSPORT_CLOSING")
		}
		o.activeDials++
		o.mu.Unlock()
		raw, e := (&net.Dialer{}).DialContext(c, network, address)
		o.mu.Lock()
		defer o.mu.Unlock()
		o.activeDials--
		if raw == nil {
			return nil, e
		}
		conn := &mcpObservedConnection{Conn: raw, idle: make(chan struct{})}
		close(conn.idle)
		o.conns = append(o.conns, conn)
		// Return from metadata exclusion before membership/scanner IO.
		o.mu.Unlock()
		local, localOK := raw.LocalAddr().(*net.TCPAddr)
		remote, remoteOK := raw.RemoteAddr().(*net.TCPAddr)
		if e == nil && localOK && remoteOK && local.IP.To4() != nil && remote.IP.To4() != nil {
			actual := &MCPHTTPOriginalConnection{owner: o, connection: conn, local: *local, remote: *remote, bindingSHA: qualificationBindingSHA(b)}
			actual.local.IP = append(net.IP(nil), local.IP...)
			actual.remote.IP = append(net.IP(nil), remote.IP...)
			e = b.EndpointSource.ValidateOriginalConnection(c, actual)
		} else if e == nil {
			e = errors.New("MCP_QUALIFICATION_CONNECTION_ADDRESS_UNQUALIFIED")
		}
		if e != nil {
			e = errors.Join(e, conn.Close())
			o.mu.Lock()
			return nil, e
		}
		o.mu.Lock()
		return conn, nil
	}
	o.client = &http.Client{Transport: o.transport, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	r.qualification = o
	r.qualificationMu.Unlock()
	observation = &MCPHTTPQualificationObservation{owner: o}
	defer func() {
		o.mu.Lock()
		o.finished = true
		if retErr != nil {
			o.snapshot.PrimaryError = retErr.Error()
		}
		o.mu.Unlock()
	}()
	initRaw, err := o.exchange(ctx, "initialize", "1", map[string]any{"protocolVersion": "2025-03-26", "capabilities": map[string]any{}, "clientInfo": mcpBootstrapClientInfo}, false)
	if err != nil {
		return observation, err
	}
	var init struct {
		ProtocolVersion string                         `json:"protocolVersion"`
		Capabilities    map[string]json.RawMessage     `json:"capabilities"`
		ServerInfo      struct{ Name, Version string } `json:"serverInfo"`
	}
	if err = json.Unmarshal(initRaw, &init); err != nil {
		return observation, err
	}
	if !(mcpcompat.ProtocolCompatibilityPolicy{}).Supports(init.ProtocolVersion) || init.Capabilities == nil || init.ServerInfo.Name == "" || init.ServerInfo.Version == "" {
		return observation, errors.New("MCP_QUALIFICATION_INITIALIZE_RESULT_INVALID")
	}
	o.snapshot.ProtocolVersion = init.ProtocolVersion
	server, _ := json.Marshal(init.ServerInfo)
	o.snapshot.ServerInfoSHA256 = qualificationSHA(server)
	if o.pendingSession != "" {
		r.setSessionID(o.pendingSession)
		o.snapshot.SessionAcquired = true
	}
	if _, err = o.exchange(ctx, "notifications/initialized", "", nil, true); err != nil {
		return observation, err
	}
	toolsRaw, err := o.exchange(ctx, "tools/list", "2", map[string]any{}, false)
	if err != nil {
		return observation, err
	}
	var tools struct {
		Tools []struct {
			Name        string                     `json:"name"`
			InputSchema map[string]json.RawMessage `json:"inputSchema"`
		} `json:"tools"`
		NextCursor *string `json:"nextCursor"`
	}
	if err = json.Unmarshal(toolsRaw, &tools); err != nil {
		return observation, err
	}
	if tools.Tools == nil || tools.NextCursor != nil {
		return observation, errors.New("MCP_QUALIFICATION_CATALOGUE_INCOMPLETE")
	}
	names := map[string]bool{}
	for _, tool := range tools.Tools {
		if tool.Name == "" || tool.InputSchema == nil || names[tool.Name] {
			return observation, errors.New("MCP_QUALIFICATION_TOOL_INVALID")
		}
		names[tool.Name] = true
	}
	o.snapshot.ToolCatalogueSHA256 = qualificationSHA(toolsRaw)
	o.snapshot.ProtocolComplete = true
	return observation, nil
}

func (o *mcpHTTPQualificationOwner) exchange(ctx context.Context, method, id string, params any, notification bool) (result json.RawMessage, retErr error) {
	msg := struct {
		JSONRPC string          `json:"jsonrpc"`
		ID      json.RawMessage `json:"id,omitempty"`
		Method  string          `json:"method"`
		Params  any             `json:"params,omitempty"`
	}{JSONRPC: "2.0", Method: method, Params: params}
	if id != "" {
		msg.ID = json.RawMessage(id)
		o.snapshot.RequestIDs = append(o.snapshot.RequestIDs, id)
	}
	payload, e := json.Marshal(msg)
	if e != nil {
		return nil, e
	}
	request, e := http.NewRequestWithContext(ctx, http.MethodPost, o.relay.URL, bytes.NewReader(payload))
	if e != nil {
		return nil, e
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Accept", "application/json, text/event-stream")
	if sid := o.relay.getSessionID(); sid != "" {
		request.Header.Set(mcpSessionIDHeader, sid)
	}
	if o.snapshot.ProtocolVersion != "" {
		request.Header.Set("MCP-Protocol-Version", o.snapshot.ProtocolVersion)
	}
	o.mu.Lock()
	o.active++
	o.mu.Unlock()
	defer func() { o.mu.Lock(); o.active--; o.mu.Unlock() }()
	response, e := o.client.Do(request)
	if response == nil {
		return nil, e
	}
	body := MCPHTTPBodyObservation{Method: method, Status: response.StatusCode}
	limited := &io.LimitedReader{R: response.Body, N: maxStdinLineBytes + 1}
	defer func() {
		body.Bytes = maxStdinLineBytes + 1 - limited.N
		body.CloseAttempted = true
		closeErr := response.Body.Close()
		body.Closed = closeErr == nil
		retErr = errors.Join(retErr, closeErr, ctx.Err())
		if retErr != nil {
			body.Error = retErr.Error()
		}
		o.snapshot.Bodies = append(o.snapshot.Bodies, body)
	}()
	if e != nil {
		return nil, e
	}
	if notification {
		raw, readErr := io.ReadAll(limited)
		body.ReadDisposition = "eof"
		if readErr != nil {
			return nil, readErr
		}
		if len(raw) > maxStdinLineBytes {
			return nil, errStdinLineTooLong
		}
		if response.StatusCode != http.StatusAccepted || len(bytes.TrimSpace(raw)) != 0 {
			return nil, errors.New("MCP_QUALIFICATION_INITIALIZED_REJECTED")
		}
		return nil, nil
	}
	if response.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("MCP_QUALIFICATION_HTTP_STATUS:%d", response.StatusCode)
	}
	receive := func(raw []byte) (bool, error) {
		meta, e := decodeJSONRPCMeta(raw)
		if e != nil {
			return false, e
		}
		if meta.Method != "" {
			if len(meta.ID) > 0 {
				return false, errors.New("MCP_QUALIFICATION_UNSUPPORTED_SERVER_REQUEST")
			}
			return false, nil
		}
		if string(meta.ID) != id {
			return false, errors.New("MCP_QUALIFICATION_REPLY_ID_MISMATCH")
		}
		var envelope struct {
			Result json.RawMessage `json:"result"`
			Error  json.RawMessage `json:"error"`
		}
		if e = json.Unmarshal(raw, &envelope); e != nil {
			return false, e
		}
		if len(envelope.Error) > 0 && string(envelope.Error) != "null" {
			return false, errors.New("MCP_QUALIFICATION_RPC_ERROR")
		}
		if len(envelope.Result) == 0 || bytes.Equal(envelope.Result, []byte("null")) {
			return false, errors.New("MCP_QUALIFICATION_RESULT_MISSING")
		}
		result = append(json.RawMessage(nil), envelope.Result...)
		o.snapshot.ReplyIDs = append(o.snapshot.ReplyIDs, id)
		return true, nil
	}
	media, _, e := mime.ParseMediaType(response.Header.Get("Content-Type"))
	if e != nil {
		return nil, e
	}
	switch media {
	case "application/json":
		raw, readErr := io.ReadAll(limited)
		body.ReadDisposition = "eof"
		if readErr != nil {
			return nil, readErr
		}
		if len(raw) > maxStdinLineBytes {
			return nil, errStdinLineTooLong
		}
		matched, err := receive(raw)
		if err != nil {
			return nil, err
		}
		if !matched {
			return nil, errors.New("MCP_QUALIFICATION_REPLY_MISSING")
		}
	case "text/event-stream":
		_, e = readRelaySSE(ctx, limited, "", maxStdinLineBytes, receive)
		if e != nil {
			return nil, e
		}
		if result == nil {
			return nil, errors.New("MCP_QUALIFICATION_SSE_REPLY_MISSING")
		}
		body.ReadDisposition = "matched"
	default:
		return nil, errors.New("MCP_QUALIFICATION_CONTENT_TYPE_INVALID")
	}
	if limited.N <= 0 {
		return nil, errStdinLineTooLong
	}
	if method == "initialize" {
		o.pendingSession = response.Header.Get(mcpSessionIDHeader)
	} else if sid := response.Header.Get(mcpSessionIDHeader); sid != "" && sid != o.relay.getSessionID() {
		return nil, errors.New("MCP_QUALIFICATION_SESSION_DRIFT")
	}
	return result, nil
}

func (r *HTTPToStdioRelay) CloseSessionObserved(ctx context.Context) (*MCPHTTPQualificationCloseObservation, error) {
	r.qualificationMu.Lock()
	o := r.qualification
	r.qualificationMu.Unlock()
	if o == nil || ctx == nil {
		return nil, errors.New("MCP_QUALIFICATION_CLOSE_UNAVAILABLE")
	}
	o.mu.Lock()
	if o.close != nil {
		v := o.close
		o.mu.Unlock()
		return v, v.err
	}
	if o.closeAttempted || !o.finished || o.active != 0 || o.activeDials != 0 {
		o.mu.Unlock()
		return nil, errors.New("MCP_QUALIFICATION_CLOSE_INFLIGHT")
	}
	o.closeAttempted = true
	o.closing = true
	o.mu.Unlock()
	var err error
	if _, ok := ctx.Deadline(); !ok {
		err = errors.New("MCP_QUALIFICATION_CLEANUP_DEADLINE_REQUIRED")
	}
	if sid := r.takeSessionID(); sid != "" {
		if err == nil {
			req, e := http.NewRequestWithContext(ctx, http.MethodDelete, r.URL, nil)
			if e == nil {
				req.Header.Set(mcpSessionIDHeader, sid)
				req.Header.Set("MCP-Protocol-Version", o.snapshot.ProtocolVersion)
			}
			// Closing denies new independent dial requests. Permit exactly this
			// synchronous owned DELETE through the same original transport.
			o.mu.Lock()
			o.closing = false
			o.mu.Unlock()
			var resp *http.Response
			if e == nil {
				resp, e = o.client.Do(req)
			}
			o.mu.Lock()
			o.closing = true
			o.mu.Unlock()
			if resp != nil {
				row := MCPHTTPBodyObservation{Method: "DELETE", Status: resp.StatusCode}
				raw, readErr := io.ReadAll(io.LimitReader(resp.Body, maxStdinLineBytes+1))
				row.Bytes = int64(len(raw))
				row.ReadDisposition = "eof"
				row.CloseAttempted = true
				closeErr := resp.Body.Close()
				row.Closed = closeErr == nil
				e = errors.Join(e, readErr, closeErr)
				if len(raw) > maxStdinLineBytes {
					e = errors.Join(e, errStdinLineTooLong)
				}
				if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusNoContent {
					e = errors.Join(e, errors.New("MCP_QUALIFICATION_DELETE_REJECTED"))
				}
				if e != nil {
					row.Error = e.Error()
				}
				o.snapshot.Bodies = append(o.snapshot.Bodies, row)
			}
			err = errors.Join(err, e)
			if e == nil {
				o.snapshot.SessionDeleteDisposition = "received"
			} else {
				o.snapshot.SessionDeleteDisposition = "failed"
			}
		} else {
			o.snapshot.SessionDeleteDisposition = "not_attempted"
		}
	} else {
		o.snapshot.SessionDeleteDisposition = "no_acquired_session"
	}
	o.transport.CloseIdleConnections()
	o.mu.Lock()
	conns := append([]*mcpObservedConnection(nil), o.conns...)
	o.mu.Unlock()
	for _, conn := range conns {
		v, e := conn.closeObserved(ctx)
		o.snapshot.Connections = append(o.snapshot.Connections, v)
		err = errors.Join(err, e)
	}
	err = errors.Join(err, ctx.Err())
	complete := err == nil
	for _, row := range o.snapshot.Bodies {
		if !row.CloseAttempted || !row.Closed {
			complete = false
		}
	}
	o.snapshot.ResourcesComplete = complete
	o.snapshot.Complete = complete && o.snapshot.ProtocolComplete && o.snapshot.PrimaryError == ""
	if err != nil {
		o.snapshot.CleanupError = err.Error()
	}
	proof := &MCPHTTPQualificationCloseObservation{owner: o, complete: complete, err: err}
	o.mu.Lock()
	o.close = proof
	o.mu.Unlock()
	return proof, err
}
func (o *MCPHTTPQualificationObservation) ReceiveClose(ctx context.Context, close *MCPHTTPQualificationCloseObservation) error {
	if o == nil || o.owner == nil || close == nil || close.owner != o.owner || ctx == nil || ctx.Err() != nil {
		return errors.New("MCP_QUALIFICATION_CLOSE_FOREIGN")
	}
	o.owner.mu.Lock()
	defer o.owner.mu.Unlock()
	if o.owner.close != close || !close.complete || close.err != nil || !o.owner.snapshot.Complete {
		return errors.New("MCP_QUALIFICATION_INCOMPLETE")
	}
	return nil
}
func (o *MCPHTTPQualificationObservation) Validate(ctx context.Context, b MCPHTTPQualificationBinding) error {
	if o == nil || o.owner == nil || !reflect.DeepEqual(o.owner.binding, b) {
		return errors.New("MCP_QUALIFICATION_BINDING_DRIFT")
	}
	if e := b.validate(ctx, o.owner.relay.URL); e != nil {
		return e
	}
	return o.ReceiveClose(ctx, o.owner.close)
}
func (o *MCPHTTPQualificationObservation) ReceiptSHA256() string {
	if o == nil || o.owner == nil {
		return ""
	}
	o.owner.mu.Lock()
	defer o.owner.mu.Unlock()
	if !o.owner.snapshot.Complete {
		return ""
	}
	raw, _ := json.Marshal(o.owner.snapshot)
	return qualificationSHA(raw)
}

type mcpObservedConnection struct {
	net.Conn
	mu                         sync.Mutex
	active                     int
	idle                       chan struct{}
	closing, attempted, closed bool
	closeErr                   error
}

func (c *mcpObservedConnection) enter() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closing {
		return net.ErrClosed
	}
	if c.active == 0 {
		c.idle = make(chan struct{})
	}
	c.active++
	return nil
}
func (c *mcpObservedConnection) leave() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.active--
	if c.active == 0 {
		close(c.idle)
	}
}
func (c *mcpObservedConnection) Read(p []byte) (int, error) {
	if e := c.enter(); e != nil {
		return 0, e
	}
	defer c.leave()
	return c.Conn.Read(p)
}
func (c *mcpObservedConnection) Write(p []byte) (int, error) {
	if e := c.enter(); e != nil {
		return 0, e
	}
	defer c.leave()
	return c.Conn.Write(p)
}
func (c *mcpObservedConnection) Close() error {
	c.mu.Lock()
	if c.attempted {
		e := c.closeErr
		if !c.closed && e == nil {
			e = errors.New("MCP_CONNECTION_CLOSE_PENDING")
		}
		c.mu.Unlock()
		return e
	}
	c.attempted = true
	c.closing = true
	c.mu.Unlock()
	e := c.Conn.Close()
	c.mu.Lock()
	c.closeErr = e
	c.closed = e == nil
	c.mu.Unlock()
	return e
}
func (c *mcpObservedConnection) closeObserved(ctx context.Context) (MCPHTTPConnectionObservation, error) {
	e := c.Close()
	c.mu.Lock()
	idle := c.idle
	c.mu.Unlock()
	select {
	case <-idle:
	case <-ctx.Done():
		e = errors.Join(e, ctx.Err())
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	v := MCPHTTPConnectionObservation{CloseAttempted: c.attempted, Closed: c.closed, IOComplete: c.active == 0}
	if e != nil {
		v.Error = e.Error()
	}
	if !v.Closed || !v.IOComplete {
		e = errors.Join(e, errors.New("MCP_CONNECTION_CLOSURE_UNSETTLED"))
	}
	return v, e
}

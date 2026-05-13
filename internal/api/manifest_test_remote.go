// internal/api/manifest_test_remote.go — G6 sub-PR 3 smoke command.
//
// Spec: docs/superpowers/specs/2026-05-12-g6-remote-mcp-manifests-design.md
// §"NEW: mcphub manifest test-remote".
//
// Sends a one-shot MCP `initialize` JSON-RPC POST to the manifest's
// expanded URL with expanded headers and returns the upstream's
// protocolVersion + serverInfo. Lets the operator confirm a
// remote-http manifest is reachable + credentialed correctly BEFORE
// running install. Standalone — no client config side effects, no
// scheduler/daemon mutation, no backup.

package api

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"

	"mcp-local-hub/internal/buildinfo"
	"mcp-local-hub/internal/config"
)

const (
	testRemoteResponseMaxBytes = 1 * 1024 * 1024 // 1 MB ceiling on the initialize response
	testRemoteProtocolVersion  = "2025-11-25"    // pinned MCP Streamable HTTP version
	testRemoteRPCRequestID     = 1               // JSON-RPC id we send and expect echoed back
)

// RemoteInitializeResult is the upstream server's response to a
// successful `initialize` handshake. Empty fields mean the server
// omitted them.
type RemoteInitializeResult struct {
	ProtocolVersion string
	ServerName      string
	ServerVersion   string
	Capabilities    map[string]any
}

// ManifestTestRemote loads the named manifest from the production
// manifest source (embed first, then disk), expands ${secret:KEY}
// placeholders, and POSTs an MCP `initialize` JSON-RPC to its URL.
// Returns the parsed result, or an error naming the failure mode
// (manifest gate, secret expansion, network, upstream error).
func (a *API) ManifestTestRemote(ctx context.Context, name string) (*RemoteInitializeResult, error) {
	yamlStr, err := a.ManifestGet(name)
	if err != nil {
		return nil, err
	}
	return a.manifestTestRemoteFromYAML(ctx, name, yamlStr, TestRemoteClientForCmd())
}

// ManifestTestRemoteIn is the tempdir-capable form of
// ManifestTestRemote. Used by tests that seed manifests into a temp
// directory and don't want to touch the embed FS.
func (a *API) ManifestTestRemoteIn(ctx context.Context, dir, name string) (*RemoteInitializeResult, error) {
	yamlStr, err := a.ManifestGetIn(dir, name)
	if err != nil {
		return nil, err
	}
	return a.manifestTestRemoteFromYAML(ctx, name, yamlStr, TestRemoteClientForCmd())
}

// manifestTestRemoteWithClient is the test-injectable form. Tests
// pass an *http.Client backed by httptest.NewTLSServer + the shared
// TLS-trusting helper. Production callers go through the public
// methods above which resolve the client via TestRemoteClientForCmd.
func (a *API) manifestTestRemoteWithClient(ctx context.Context, dir, name string, client *http.Client) (*RemoteInitializeResult, error) {
	yamlStr, err := a.ManifestGetIn(dir, name)
	if err != nil {
		return nil, err
	}
	return a.manifestTestRemoteFromYAML(ctx, name, yamlStr, client)
}

func (a *API) manifestTestRemoteFromYAML(ctx context.Context, name, yamlStr string, client *http.Client) (*RemoteInitializeResult, error) {
	m, err := config.ParseManifest(strings.NewReader(yamlStr))
	if err != nil {
		return nil, fmt.Errorf("parse manifest %s: %w", name, err)
	}
	if m.Transport != config.TransportRemoteHTTP {
		return nil, fmt.Errorf("manifest %s: transport=%q (test-remote requires transport=remote-http)", name, m.Transport)
	}
	expandedURL, err := ExpandSecrets(m.URL, nil)
	if err != nil {
		return nil, fmt.Errorf("expand url: %w", err)
	}
	expandedHeaders, err := ExpandSecretsMap(m.Headers, nil)
	if err != nil {
		return nil, fmt.Errorf("expand headers: %w", err)
	}
	return sendRemoteInitialize(ctx, client, expandedURL, expandedHeaders)
}

// newTestRemoteClient builds the production HTTPS-only client used
// for test-remote. Shares transport defaults (proxy, keep-alive,
// disable-compression) with the marketplace fetcher but enforces a
// stricter redirect policy.
//
// Timeout is intentionally zero: the operator-supplied --timeout on
// the CLI wraps the request in a context.WithTimeout, and that
// deadline is the single source of truth for cancellation. Setting a
// hard http.Client.Timeout would silently cap user-supplied values
// above the constant — bot r2 P2 closure (PR #171): slow but healthy
// remote endpoints were reported as failed at the 15s cap even when
// operators explicitly requested a longer window.
//
// CheckRedirect = rejectAllRedirects: this request carries
// manifest-defined headers (Authorization, X-API-Key, custom bearer
// tokens) sourced from the encrypted vault. Go's http.Client strips
// Authorization on cross-host redirects but forwards arbitrary
// custom headers, so a hostile or misconfigured endpoint could 302
// to a different host and harvest credentials in a single redirect.
// Smoke checks should never silently re-credential — refuse all
// redirects and let the operator update the manifest URL instead
// (bot r5 P1 closure, PR #171).
func newTestRemoteClient() *http.Client {
	return &http.Client{
		Transport:     newMarketplaceTransport(),
		CheckRedirect: rejectAllRedirects,
	}
}

// rejectAllRedirects forces http.Client to surface the redirect
// status to the caller instead of following it. Returning an error
// here propagates as the client's error; the original response
// (with Location header) is still returned to the caller, so we can
// surface "upstream returned 302" to the operator clearly.
func rejectAllRedirects(req *http.Request, via []*http.Request) error {
	return fmt.Errorf("refusing to follow redirect to %s — manifest test-remote does not follow redirects so credential-bearing headers can never reach an unintended host; update the manifest url to the final endpoint instead", req.URL.Redacted())
}

func sendRemoteInitialize(ctx context.Context, client *http.Client, rawURL string, headers map[string]string) (*RemoteInitializeResult, error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return nil, fmt.Errorf("parse url %q: %w", rawURL, err)
	}
	if u.Scheme != "https" {
		return nil, fmt.Errorf("remote url must be https:// (got scheme %q)", u.Scheme)
	}
	version, _, _ := buildinfo.Get()
	body := map[string]any{
		"jsonrpc": "2.0",
		"id":      testRemoteRPCRequestID,
		"method":  "initialize",
		"params": map[string]any{
			"protocolVersion": testRemoteProtocolVersion,
			"capabilities":    map[string]any{},
			"clientInfo": map[string]any{
				"name":    "mcphub",
				"version": version,
			},
		},
	}
	raw, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("marshal initialize body: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, rawURL, bytes.NewReader(raw))
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}
	// Apply manifest-supplied headers FIRST (typically Authorization,
	// custom auth tokens, X-* metadata), then force the protocol
	// headers so a manifest cannot redefine MCP transport semantics.
	// ExpandSecretsMap already rejected CRLF in values, so
	// http.Header.Set won't be tricked into header injection.
	//
	// Bot r3 P2 closure (PR #171): the prior order put protocol
	// headers first and user headers last, letting a manifest
	// override MCP-Protocol-Version, Accept, or Content-Type. That
	// either silenced the pinned protocol version or made smoke
	// reports diverge from production transport behavior — both
	// failure modes break this command's smoke-test contract.
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	req.Header.Set("MCP-Protocol-Version", testRemoteProtocolVersion)
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("post initialize: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		errBody, _ := io.ReadAll(io.LimitReader(resp.Body, 4*1024))
		return nil, fmt.Errorf("upstream returned HTTP %d: %s", resp.StatusCode, truncateForError(string(errBody), 500))
	}
	// Streamable HTTP lets the server reply either with
	// application/json (one-shot) or with text/event-stream. For SSE
	// the upstream may keep the connection open after the initialize
	// reply (queueing notifications), so we must scan incrementally
	// and return as soon as the matching JSON-RPC id arrives —
	// io.ReadAll would block until the upstream closes the stream.
	//
	// Bot r4 P1 closure (PR #171): the prior path used ReadAll on the
	// whole body before deciding format, hanging healthy SSE
	// endpoints to ctx deadline.
	var rpc *rpcReply
	if strings.HasPrefix(strings.ToLower(resp.Header.Get("Content-Type")), "text/event-stream") {
		rpc, err = consumeSSE(ctx, resp.Body, testRemoteRPCRequestID)
		if err != nil {
			return nil, err
		}
	} else {
		rawResp, err := io.ReadAll(io.LimitReader(resp.Body, testRemoteResponseMaxBytes+1))
		if err != nil {
			return nil, fmt.Errorf("read response: %w", err)
		}
		if int64(len(rawResp)) > testRemoteResponseMaxBytes {
			return nil, fmt.Errorf("response body exceeds %d-byte cap", testRemoteResponseMaxBytes)
		}
		rpc, err = findMatchingRPCReply([][]byte{rawResp}, testRemoteRPCRequestID)
		if err != nil {
			return nil, err
		}
	}
	if rpc.Error != nil {
		return nil, fmt.Errorf("upstream rpc error %d: %s", rpc.Error.Code, rpc.Error.Message)
	}
	if rpc.Result == nil {
		return nil, errors.New("decode response: missing result and error fields")
	}
	out := &RemoteInitializeResult{}
	if pv, ok := rpc.Result["protocolVersion"].(string); ok {
		out.ProtocolVersion = pv
	}
	if caps, ok := rpc.Result["capabilities"].(map[string]any); ok {
		out.Capabilities = caps
	}
	if si, ok := rpc.Result["serverInfo"].(map[string]any); ok {
		if n, ok := si["name"].(string); ok {
			out.ServerName = n
		}
		if v, ok := si["version"].(string); ok {
			out.ServerVersion = v
		}
	}
	return out, nil
}

// consumeSSE incrementally scans a text/event-stream body and
// returns the first JSON-RPC 2.0 reply whose id equals wantID.
// Returns as soon as that reply arrives — the SSE stream may stay
// open afterwards for unrelated notifications, and we don't wait
// for the upstream to close it.
//
// Reads at most testRemoteResponseMaxBytes total bytes across the
// whole stream (defense against unbounded SSE streams that never
// produce a matching reply). Honors ctx cancellation between lines
// and through the underlying http request body, which closes on
// ctx.Done.
//
// Bot r4 P1 closure (PR #171): the prior path called io.ReadAll on
// the SSE body before parsing, which blocks until the server closes
// the connection. Spec-compliant Streamable HTTP servers may hold
// SSE connections open after sending the initialize reply, so smoke
// checks hung to ctx deadline against otherwise healthy endpoints.
func consumeSSE(ctx context.Context, body io.Reader, wantID int) (*rpcReply, error) {
	sc := bufio.NewScanner(body)
	sc.Buffer(make([]byte, 0, 4096), testRemoteResponseMaxBytes)
	var lines []string
	var bytesRead int

	tryFlush := func() *rpcReply {
		defer func() { lines = nil }()
		var parts []string
		for _, ln := range lines {
			if ln == "" || strings.HasPrefix(ln, ":") {
				continue
			}
			if rest, ok := strings.CutPrefix(ln, "data:"); ok {
				parts = append(parts, strings.TrimSpace(rest))
			}
		}
		if len(parts) == 0 {
			return nil
		}
		raw := []byte(strings.Join(parts, "\n"))
		var probe rpcReply
		if err := json.Unmarshal(raw, &probe); err != nil {
			// Don't tear down the whole stream on one bad event —
			// stay friendly to servers that interleave unrelated
			// non-JSON frames (e.g. heartbeat text).
			return nil
		}
		if probe.JSONRPC != "2.0" {
			return nil
		}
		if rpcIDEquals(probe.ID, wantID) {
			return &probe
		}
		return nil
	}

	for sc.Scan() {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		ln := strings.TrimRight(sc.Text(), "\r")
		bytesRead += len(ln) + 1
		if bytesRead > testRemoteResponseMaxBytes {
			return nil, fmt.Errorf("response stream exceeds %d-byte cap before a JSON-RPC 2.0 reply with id=%d arrived", testRemoteResponseMaxBytes, wantID)
		}
		if ln == "" {
			if r := tryFlush(); r != nil {
				return r, nil
			}
			continue
		}
		lines = append(lines, ln)
	}
	if err := sc.Err(); err != nil {
		// Surface ctx-driven cancellation (the http body Read returns
		// ctx.Err() wrapped) so the operator sees the deadline rather
		// than a generic scan error.
		if cerr := ctx.Err(); cerr != nil {
			return nil, cerr
		}
		return nil, fmt.Errorf("scan SSE stream: %w", err)
	}
	// Last event without a trailing blank line.
	if r := tryFlush(); r != nil {
		return r, nil
	}
	return nil, fmt.Errorf("SSE stream ended without a JSON-RPC 2.0 reply with id=%d", wantID)
}

// rpcReply is the JSON-RPC 2.0 envelope shape we accept from the
// upstream server's initialize response.
type rpcReply struct {
	JSONRPC string         `json:"jsonrpc"`
	ID      any            `json:"id"`
	Result  map[string]any `json:"result"`
	Error   *struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

// findMatchingRPCReply scans events and returns the first valid
// JSON-RPC 2.0 envelope whose id equals wantID. Returns a clear
// diagnostic if no event qualifies — distinguishes "received N events
// but none had matching id" from "couldn't decode any event".
//
// Bot r1 P2 closure (PR #171): success was previously inferred from
// `result != nil` without verifying the response is a JSON-RPC 2.0
// envelope with the id we sent. A non-MCP endpoint returning any 2xx
// JSON shaped like `{"result":{...}}` would be reported as reachable,
// undermining this command's purpose as an MCP handshake smoke test.
func findMatchingRPCReply(events [][]byte, wantID int) (*rpcReply, error) {
	var lastDecodeErr error
	envelopes := 0
	for _, raw := range events {
		var probe rpcReply
		if err := json.Unmarshal(raw, &probe); err != nil {
			lastDecodeErr = fmt.Errorf("decode response: %w (body=%q)", err, truncateForError(string(raw), 300))
			continue
		}
		if probe.JSONRPC != "2.0" {
			continue
		}
		envelopes++
		if rpcIDEquals(probe.ID, wantID) {
			return &probe, nil
		}
	}
	if lastDecodeErr != nil && envelopes == 0 {
		// No valid envelope decoded — surface the parse error so
		// operators see what the upstream sent.
		return nil, lastDecodeErr
	}
	return nil, fmt.Errorf("no JSON-RPC 2.0 reply with id=%d among %d event(s); the upstream may not be an MCP endpoint", wantID, len(events))
}

// rpcIDEquals compares a JSON-decoded id (any) to an int. The MCP
// spec allows numeric or string ids; we always send a number, so any
// non-number type is a mismatch regardless of value.
func rpcIDEquals(got any, want int) bool {
	switch v := got.(type) {
	case float64:
		return v == float64(want)
	case json.Number:
		if n, err := v.Int64(); err == nil {
			return n == int64(want)
		}
	case int:
		return v == want
	case int64:
		return v == int64(want)
	}
	return false
}

func truncateForError(s string, n int) string {
	if len(s) > n {
		return s[:n] + "..."
	}
	return s
}

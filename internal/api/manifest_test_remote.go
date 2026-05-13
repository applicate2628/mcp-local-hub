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
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"mcp-local-hub/internal/buildinfo"
	"mcp-local-hub/internal/config"
)

const (
	testRemoteHTTPTimeout      = 15 * time.Second
	testRemoteResponseMaxBytes = 1 * 1024 * 1024 // 1 MB ceiling on the initialize response
	testRemoteProtocolVersion  = "2025-11-25"    // pinned MCP Streamable HTTP version
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
// disable-compression) + redirect policy with the marketplace
// fetcher; only the timeout name differs for readability.
func newTestRemoteClient() *http.Client {
	return &http.Client{
		Transport:     newMarketplaceTransport(),
		CheckRedirect: rejectNonHTTPSRedirect,
		Timeout:       testRemoteHTTPTimeout,
	}
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
		"id":      1,
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
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	req.Header.Set("MCP-Protocol-Version", testRemoteProtocolVersion)
	for k, v := range headers {
		// ExpandSecretsMap rejected CRLF in values before we got here,
		// so http.Header.Set won't be tricked into injecting headers.
		req.Header.Set(k, v)
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("post initialize: %w", err)
	}
	defer resp.Body.Close()
	rawResp, err := io.ReadAll(io.LimitReader(resp.Body, testRemoteResponseMaxBytes+1))
	if err != nil {
		return nil, fmt.Errorf("read response: %w", err)
	}
	if int64(len(rawResp)) > testRemoteResponseMaxBytes {
		return nil, fmt.Errorf("response body exceeds %d-byte cap", testRemoteResponseMaxBytes)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("upstream returned HTTP %d: %s", resp.StatusCode, truncateForError(string(rawResp), 500))
	}
	// Streamable HTTP lets the server reply either with
	// application/json or with a one-event text/event-stream. Handle
	// both so we don't fail against compliant servers that prefer SSE
	// for streaming flows but emit a single message for initialize.
	rawJSON := rawResp
	if strings.HasPrefix(strings.ToLower(resp.Header.Get("Content-Type")), "text/event-stream") {
		rawJSON, err = extractSingleSSEMessage(rawResp)
		if err != nil {
			return nil, err
		}
	}
	var rpc struct {
		JSONRPC string         `json:"jsonrpc"`
		ID      any            `json:"id"`
		Result  map[string]any `json:"result"`
		Error   *struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rawJSON, &rpc); err != nil {
		return nil, fmt.Errorf("decode response: %w (body=%q)", err, truncateForError(string(rawJSON), 300))
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

// extractSingleSSEMessage pulls a single JSON-RPC message out of an
// SSE response body. Spec-compliant initialize responses use a single
// `data:` line, but RFC allows multi-line data so we concatenate
// every `data:` we see with newlines. Lines starting with `:` are
// SSE comments and ignored; other field labels are skipped.
func extractSingleSSEMessage(buf []byte) ([]byte, error) {
	lines := strings.Split(string(buf), "\n")
	var dataParts []string
	for _, ln := range lines {
		ln = strings.TrimRight(ln, "\r")
		if ln == "" || strings.HasPrefix(ln, ":") {
			continue
		}
		if rest, ok := strings.CutPrefix(ln, "data:"); ok {
			dataParts = append(dataParts, strings.TrimSpace(rest))
		}
	}
	if len(dataParts) == 0 {
		return nil, errors.New("text/event-stream response had no data: lines")
	}
	return []byte(strings.Join(dataParts, "\n")), nil
}

func truncateForError(s string, n int) string {
	if len(s) > n {
		return s[:n] + "..."
	}
	return s
}

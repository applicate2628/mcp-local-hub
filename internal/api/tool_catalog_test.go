package api

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"os/exec"
	"strings"
	"testing"
	"time"
)

func TestToolCatalog_KnownKinds(t *testing.T) {
	for _, kind := range []string{"mcp-language-server", "gopls-mcp"} {
		cat, ok := ToolCatalogForBackend(kind)
		if !ok {
			t.Errorf("missing catalog for backend %q", kind)
			continue
		}
		if cat.CatalogVersion == "" {
			t.Errorf("%s: CatalogVersion empty", kind)
		}
		if len(cat.Tools) == 0 {
			t.Errorf("%s: Tools empty", kind)
		}
	}
}

func TestToolCatalog_UnknownKind(t *testing.T) {
	if _, ok := ToolCatalogForBackend("nope"); ok {
		t.Error("unknown kind should return false")
	}
	if _, err := SyntheticInitializeResponse(json.RawMessage(`1`), "nope"); err == nil {
		t.Error("SyntheticInitializeResponse should error for unknown kind")
	}
	if _, err := SyntheticToolsListResponse(json.RawMessage(`1`), "nope"); err == nil {
		t.Error("SyntheticToolsListResponse should error for unknown kind")
	}
}

func TestToolCatalog_SyntheticInitializeShape(t *testing.T) {
	reqID := json.RawMessage(`1`)
	resp, err := SyntheticInitializeResponse(reqID, "mcp-language-server")
	if err != nil {
		t.Fatal(err)
	}
	var parsed map[string]any
	if err := json.Unmarshal(resp, &parsed); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}
	if parsed["jsonrpc"] != "2.0" {
		t.Errorf("jsonrpc = %v, want 2.0", parsed["jsonrpc"])
	}
	if _, ok := parsed["result"]; !ok {
		t.Error("missing result")
	}
	result := parsed["result"].(map[string]any)
	if _, ok := result["capabilities"]; !ok {
		t.Error("missing capabilities")
	}
	si, _ := result["serverInfo"].(map[string]any)
	if si == nil {
		t.Error("missing serverInfo")
	}
}

func TestToolCatalog_SyntheticToolsListEnvelope(t *testing.T) {
	reqID := json.RawMessage(`42`)
	resp, err := SyntheticToolsListResponse(reqID, "mcp-language-server")
	if err != nil {
		t.Fatal(err)
	}
	var parsed struct {
		Jsonrpc string `json:"jsonrpc"`
		ID      json.RawMessage
		Result  struct {
			Tools []map[string]any `json:"tools"`
		} `json:"result"`
	}
	if err := json.Unmarshal(resp, &parsed); err != nil {
		t.Fatal(err)
	}
	if parsed.Jsonrpc != "2.0" || !bytes.Equal(parsed.ID, reqID) {
		t.Errorf("envelope mismatch: %+v", parsed)
	}
	if len(parsed.Result.Tools) == 0 {
		t.Error("no tools")
	}
}

// TestToolCatalog_GoldenAgainstUpstream spawns the real upstream binary when
// available and compares its tools/list reply to the embedded static catalog.
// Drift means the catalog is out of date; the maintainer must bump
// CatalogVersion and resync the Tools slice from the live output.
func TestToolCatalog_GoldenAgainstUpstream(t *testing.T) {
	cases := []struct {
		kind string
		bin  string
		args []string
	}{
		{
			kind: "mcp-language-server",
			bin:  "mcp-language-server",
			args: []string{"-workspace", "", "-lsp", "gopls"},
		},
		{
			kind: "gopls-mcp",
			bin:  "gopls",
			args: []string{"mcp"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.kind, func(t *testing.T) {
			bin, err := exec.LookPath(tc.bin)
			if err != nil {
				t.Skipf("%s not on PATH; skipping live golden test", tc.bin)
			}
			workspace := t.TempDir()
			args := make([]string, len(tc.args))
			copy(args, tc.args)
			for i := range args {
				if args[i] == "" {
					args[i] = workspace
				}
			}
			upstream, err := captureToolsList(t, bin, args, workspace)
			if err != nil {
				t.Skipf("upstream probe failed (acceptable in CI): %v", err)
			}
			cat, _ := ToolCatalogForBackend(tc.kind)
			if !strings.EqualFold(upstream.serverName, cat.ServerInfoName) {
				t.Errorf("serverInfo.name drift: upstream=%q catalog=%q (bump CatalogVersion %q and regenerate)",
					upstream.serverName, cat.ServerInfoName, cat.CatalogVersion)
			}
			upstreamNames := map[string]bool{}
			for _, n := range upstream.toolNames {
				upstreamNames[n] = true
			}
			for _, tool := range cat.Tools {
				if !upstreamNames[tool.Name] {
					t.Errorf("catalog has tool %q not present in upstream (bump CatalogVersion %q; live names: %v)",
						tool.Name, cat.CatalogVersion, upstream.toolNames)
				}
			}
			for n := range upstreamNames {
				found := false
				for _, tool := range cat.Tools {
					if tool.Name == n {
						found = true
						break
					}
				}
				if !found {
					t.Errorf("upstream has tool %q missing from catalog (bump CatalogVersion %q; add %q)",
						n, cat.CatalogVersion, n)
				}
			}
		})
	}
}

// upstreamProbe is the subset of upstream metadata the golden test inspects.
type upstreamProbe struct {
	serverName string
	toolNames  []string
	// tools keeps the full captured tool schemas so operators can diff
	// schema drift (not just name drift) when regenerating the catalog.
	tools []upstreamTool
}

type upstreamTool struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	InputSchema json.RawMessage `json:"inputSchema,omitempty"`
}

// captureToolsList spawns bin with args, performs the minimal JSON-RPC
// handshake (initialize + initialized notification + tools/list) over stdio,
// reads the responses, and returns the observed metadata. The helper has a
// hard deadline so a wedged subprocess never hangs the test.
//
// MCP framing on stdio uses newline-delimited JSON (one JSON object per line)
// for most servers. mcp-language-server and gopls mcp both accept that form.
func captureToolsList(t *testing.T, bin string, args []string, workspace string) (*upstreamProbe, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, bin, args...)
	if workspace != "" {
		cmd.Dir = workspace
	}
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	// Drain stderr in background so the child does not block on a full pipe.
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	defer func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	}()
	go func() { _, _ = io.Copy(io.Discard, stderr) }()

	send := func(id int, method string, params any) error {
		payload := map[string]any{"jsonrpc": "2.0", "method": method, "params": params}
		if id >= 0 {
			payload["id"] = id
		}
		b, err := json.Marshal(payload)
		if err != nil {
			return err
		}
		_, err = stdin.Write(append(b, '\n'))
		return err
	}

	scanner := bufio.NewScanner(stdout)
	scanner.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)

	// Step 1: initialize request (id=1)
	if err := send(1, "initialize", map[string]any{
		"protocolVersion": "2024-11-05",
		"capabilities":    map[string]any{},
		"clientInfo":      map[string]any{"name": "tool-catalog-test", "version": "0"},
	}); err != nil {
		return nil, err
	}

	var serverName string
	for scanner.Scan() {
		line := scanner.Bytes()
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		var probe struct {
			ID     json.RawMessage `json:"id"`
			Result struct {
				ServerInfo struct {
					Name string `json:"name"`
				} `json:"serverInfo"`
			} `json:"result"`
		}
		if err := json.Unmarshal(line, &probe); err != nil {
			continue
		}
		if len(probe.ID) == 0 {
			continue // notification
		}
		serverName = probe.Result.ServerInfo.Name
		break
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}

	// Step 2: initialized notification (no id)
	if err := send(-1, "notifications/initialized", map[string]any{}); err != nil {
		return nil, err
	}

	// Step 3: tools/list request (id=2)
	if err := send(2, "tools/list", map[string]any{}); err != nil {
		return nil, err
	}

	var tools []upstreamTool
	for scanner.Scan() {
		line := scanner.Bytes()
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		var probe struct {
			ID     json.RawMessage `json:"id"`
			Result struct {
				Tools []upstreamTool `json:"tools"`
			} `json:"result"`
		}
		if err := json.Unmarshal(line, &probe); err != nil {
			continue
		}
		if len(probe.ID) == 0 {
			continue
		}
		tools = probe.Result.Tools
		break
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	names := make([]string, 0, len(tools))
	for _, tl := range tools {
		names = append(names, tl.Name)
	}
	return &upstreamProbe{serverName: serverName, toolNames: names, tools: tools}, nil
}

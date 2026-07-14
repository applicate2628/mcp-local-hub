package main

import (
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	"mcp-local-hub/cmd/mcp-cbuild/internal/cbuild"
	"mcp-local-hub/cmd/mcp-cbuild/internal/mcp"
)

// rpcResp is the inbound JSON-RPC 2.0 response shape the test client decodes.
type rpcResp struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  json.RawMessage `json:"result"`
	Error   *struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

// TestStdioProtocolRoundTrip drives a real server (with the real cbuild tool
// set) over in-process pipes and asserts a well-formed JSON-RPC 2.0 exchange:
// initialize -> tools/list -> tools/call. cmake_list_presets is used for the
// call because it needs no toolchain — only a CMakePresets.json on disk.
func TestStdioProtocolRoundTrip(t *testing.T) {
	proj := t.TempDir()
	if err := os.WriteFile(filepath.Join(proj, "CMakePresets.json"), []byte(`{
      "version": 3,
      "configurePresets": [ { "name": "default", "binaryDir": "${sourceDir}/build" } ]
    }`), 0o644); err != nil {
		t.Fatalf("write presets: %v", err)
	}

	srv := mcp.NewServer("mcp-cbuild", "test", cbuild.Tools(""))

	srvIn, clientW := io.Pipe()  // client -> server stdin
	clientR, srvOut := io.Pipe() // server stdout -> client

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	serveDone := make(chan error, 1)
	go func() { serveDone <- srv.Serve(ctx, srvIn, srvOut) }()

	dec := json.NewDecoder(clientR)

	send := func(id int, method string, params any) {
		t.Helper()
		req := map[string]any{"jsonrpc": "2.0", "id": id, "method": method}
		if params != nil {
			req["params"] = params
		}
		b, err := json.Marshal(req)
		if err != nil {
			t.Fatalf("marshal request: %v", err)
		}
		if _, err := clientW.Write(append(b, '\n')); err != nil {
			t.Fatalf("write request: %v", err)
		}
	}

	recv := func() rpcResp {
		t.Helper()
		type out struct {
			r   rpcResp
			err error
		}
		ch := make(chan out, 1)
		go func() {
			var r rpcResp
			err := dec.Decode(&r)
			ch <- out{r, err}
		}()
		select {
		case o := <-ch:
			if o.err != nil {
				t.Fatalf("decode response: %v", o.err)
			}
			if o.r.JSONRPC != "2.0" {
				t.Fatalf("response jsonrpc = %q, want 2.0", o.r.JSONRPC)
			}
			return o.r
		case <-time.After(10 * time.Second):
			t.Fatal("timed out waiting for a response")
			return rpcResp{}
		}
	}

	// 1) initialize
	send(1, "initialize", map[string]any{"protocolVersion": "2025-06-18"})
	resp := recv()
	if resp.Error != nil {
		t.Fatalf("initialize error: %+v", resp.Error)
	}
	var initRes struct {
		ProtocolVersion string `json:"protocolVersion"`
		ServerInfo      struct {
			Name string `json:"name"`
		} `json:"serverInfo"`
		Capabilities struct {
			Tools map[string]any `json:"tools"`
		} `json:"capabilities"`
	}
	if err := json.Unmarshal(resp.Result, &initRes); err != nil {
		t.Fatalf("decode initialize result: %v", err)
	}
	if initRes.ProtocolVersion != "2025-06-18" {
		t.Errorf("protocolVersion = %q", initRes.ProtocolVersion)
	}
	if initRes.ServerInfo.Name != "mcp-cbuild" {
		t.Errorf("serverInfo.name = %q", initRes.ServerInfo.Name)
	}
	if initRes.Capabilities.Tools == nil {
		t.Error("initialize did not advertise a tools capability")
	}

	// 2) tools/list — all 10 tools present.
	send(2, "tools/list", nil)
	resp = recv()
	if resp.Error != nil {
		t.Fatalf("tools/list error: %+v", resp.Error)
	}
	var listRes struct {
		Tools []struct {
			Name        string         `json:"name"`
			Description string         `json:"description"`
			InputSchema map[string]any `json:"inputSchema"`
		} `json:"tools"`
	}
	if err := json.Unmarshal(resp.Result, &listRes); err != nil {
		t.Fatalf("decode tools/list result: %v", err)
	}
	wantTools := map[string]bool{
		"cmake_list_presets": true, "cmake_configure": true, "cmake_build": true,
		"cmake_test": true, "cmake_workflow": true, "cmake_clean": true,
		"vcpkg_install": true, "vcpkg_list": true, "vcpkg_manifest": true, "vcpkg_search": true,
	}
	if len(listRes.Tools) != len(wantTools) {
		t.Errorf("tools/list returned %d tools, want %d", len(listRes.Tools), len(wantTools))
	}
	for _, tool := range listRes.Tools {
		if !wantTools[tool.Name] {
			t.Errorf("unexpected tool %q", tool.Name)
		}
		if tool.Description == "" || tool.InputSchema == nil {
			t.Errorf("tool %q missing description/inputSchema", tool.Name)
		}
		delete(wantTools, tool.Name)
	}
	if len(wantTools) != 0 {
		t.Errorf("tools/list missing: %v", wantTools)
	}

	// 3) tools/call cmake_list_presets — structured result, not isError.
	send(3, "tools/call", map[string]any{
		"name":      "cmake_list_presets",
		"arguments": map[string]any{"working_dir": proj},
	})
	resp = recv()
	if resp.Error != nil {
		t.Fatalf("tools/call protocol error: %+v", resp.Error)
	}
	var callRes struct {
		IsError bool `json:"isError"`
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
		StructuredContent struct {
			ConfigurePresets []struct {
				Name string `json:"name"`
			} `json:"configurePresets"`
		} `json:"structuredContent"`
	}
	if err := json.Unmarshal(resp.Result, &callRes); err != nil {
		t.Fatalf("decode tools/call result: %v", err)
	}
	if callRes.IsError {
		t.Fatalf("cmake_list_presets returned isError=true; content=%+v", callRes.Content)
	}
	if len(callRes.Content) == 0 || callRes.Content[0].Type != "text" {
		t.Errorf("tools/call result missing text content: %+v", callRes.Content)
	}
	if len(callRes.StructuredContent.ConfigurePresets) != 1 ||
		callRes.StructuredContent.ConfigurePresets[0].Name != "default" {
		t.Errorf("structuredContent.configurePresets = %+v", callRes.StructuredContent.ConfigurePresets)
	}

	// EOF ends the serve loop cleanly.
	if err := clientW.Close(); err != nil {
		t.Fatalf("close client writer: %v", err)
	}
	select {
	case err := <-serveDone:
		if err != nil {
			t.Errorf("Serve returned error: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Serve did not return after stdin EOF")
	}
}

package cli

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestDaemonCmdCodeGraphDiskManifestPropagatesLegacyProfile(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := listener.Addr().(*net.TCPAddr).Port
	if err := listener.Close(); err != nil {
		t.Fatal(err)
	}

	manifestRoot := t.TempDir()
	t.Setenv("MCPHUB_MANIFEST_DIR_OVERRIDE", manifestRoot)
	t.Setenv("LOCALAPPDATA", t.TempDir())
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	t.Setenv("MCPHUB_CODEGRAPH_COMPAT_HELPER", "1")
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	fixture := fmt.Sprintf(`name: codegraph
kind: global
transport: stdio-bridge
command: %s
base_args:
  - %s
  - "--"
daemons:
  - name: default
    port: %d
    mcp_protocol_compatibility_profile: stdio-http-legacy-2024-11-05
`, strconv.Quote(executable), strconv.Quote("-test.run=TestDaemonCmdCodeGraphDiskManifestCompatibilityHelper"), port)
	manifestPath := filepath.Join(manifestRoot, "codegraph", "manifest.yaml")
	if err := os.MkdirAll(filepath.Dir(manifestPath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(manifestPath, []byte(fixture), 0o600); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cmd := newDaemonCmdReal()
	cmd.SetContext(ctx)
	cmd.SetOut(io.Discard)
	cmd.SetErr(io.Discard)
	cmd.SetArgs([]string{"--server", "codegraph", "--daemon", "default"})
	done := make(chan error, 1)
	go func() { done <- cmd.Execute() }()

	initBody := `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2024-11-05","capabilities":{}}}`
	endpoint := fmt.Sprintf("http://127.0.0.1:%d/mcp", port)
	deadline := time.Now().Add(3 * time.Second)
	for {
		req, err := http.NewRequest(http.MethodPost, endpoint, strings.NewReader(initBody))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("MCP-Protocol-Version", "2024-11-05")
		resp, err := http.DefaultClient.Do(req)
		if err == nil {
			body, readErr := io.ReadAll(resp.Body)
			_ = resp.Body.Close()
			if readErr != nil {
				t.Fatal(readErr)
			}
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("CLI-hosted initialize status=%d body=%s", resp.StatusCode, body)
			}
			if !strings.Contains(string(body), `"protocolVersion":"2024-11-05"`) || resp.Header.Get("Mcp-Session-Id") == "" {
				t.Fatalf("CLI-hosted initialize body=%s session=%q", body, resp.Header.Get("Mcp-Session-Id"))
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("CLI host did not become ready: %v", err)
		}
		time.Sleep(25 * time.Millisecond)
	}

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("daemon command shutdown: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("daemon command did not shut down")
	}
}

func TestDaemonCmdCodeGraphDiskManifestCompatibilityHelper(t *testing.T) {
	if os.Getenv("MCPHUB_CODEGRAPH_COMPAT_HELPER") != "1" {
		return
	}
	scanner := bufio.NewScanner(os.Stdin)
	for scanner.Scan() {
		var request map[string]json.RawMessage
		if err := json.Unmarshal(scanner.Bytes(), &request); err != nil {
			os.Exit(2)
		}
		var method string
		_ = json.Unmarshal(request["method"], &method)
		result := map[string]any{"tools": []any{}}
		if method == "initialize" {
			result = map[string]any{"protocolVersion": "2024-11-05", "capabilities": map[string]any{}}
		}
		response, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": json.RawMessage(request["id"]), "result": result})
		_, _ = bytes.NewBuffer(response).WriteTo(os.Stdout)
		_, _ = os.Stdout.Write([]byte("\n"))
	}
	os.Exit(0)
}

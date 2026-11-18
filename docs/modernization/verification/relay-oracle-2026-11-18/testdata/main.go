// Finite, loopback-only negative control for TestRXRelayBinaryProbe.
// Uses the unchanged repository relay; only its visible stdout is corrupted.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"sync"

	"mcp-local-hub/internal/daemon"
)

type controlledOutput struct {
	mu      sync.Mutex
	mode    string
	pending []byte
	last    []byte
	changed bool
}

func (w *controlledOutput) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.pending = append(w.pending, p...)
	for {
		end := bytes.IndexByte(w.pending, '\n')
		if end < 0 {
			return len(p), nil
		}
		line := append([]byte(nil), w.pending[:end]...)
		w.pending = w.pending[end+1:]
		var response struct {
			ID json.RawMessage `json:"id"`
		}
		if err := json.Unmarshal(line, &response); err != nil {
			return 0, err
		}
		init := string(response.ID) == "0"
		if !w.changed && ((w.mode == "wrong-init-version" && init) || (w.mode == "wrong-call-version" && !init)) {
			if !bytes.Contains(line, []byte(`"jsonrpc":"2.0"`)) {
				return 0, fmt.Errorf("control did not find the original protocol version")
			}
			line = bytes.Replace(line, []byte(`"jsonrpc":"2.0"`), []byte(`"jsonrpc":"1.0"`), 1)
			w.changed = true
		}
		w.last = append(w.last[:0], line...)
		if _, err := os.Stdout.Write(append(line, '\n')); err != nil {
			return 0, err
		}
	}
}

func run() error {
	if len(os.Args) != 4 || os.Args[1] != "relay" || os.Args[2] != "--url" {
		return fmt.Errorf("expected relay --url <owned-loopback-fixture>")
	}
	endpoint, err := url.Parse(os.Args[3])
	if err != nil || endpoint.Scheme != "http" || endpoint.User != nil || !net.ParseIP(endpoint.Hostname()).IsLoopback() {
		return fmt.Errorf("only an owned numeric loopback HTTP fixture is supported")
	}
	mode := os.Getenv("MCPHUB_RX_PROBE_CONTROL")
	switch mode {
	case "clean", "extra-tail", "wrong-init-version", "wrong-call-version":
	default:
		return fmt.Errorf("explicit known control mode required")
	}
	output := &controlledOutput{mode: mode}
	relay := daemon.HTTPToStdioRelay{URL: endpoint.String(), Stdin: os.Stdin, Stdout: output, Stderr: os.Stderr}
	if err := relay.Run(context.Background()); err != nil {
		return err
	}
	if len(output.pending) != 0 {
		return fmt.Errorf("unterminated relay output")
	}
	if mode == "extra-tail" {
		if len(output.last) == 0 {
			return fmt.Errorf("no original response available for duplication")
		}
		_, err := os.Stdout.Write(append(output.last, '\n'))
		return err
	}
	if mode != "clean" && !output.changed {
		return fmt.Errorf("control did not alter the requested response")
	}
	return nil
}

func main() {
	if err := run(); err != nil {
		_, _ = io.WriteString(os.Stderr, err.Error()+"\n")
		os.Exit(1)
	}
}

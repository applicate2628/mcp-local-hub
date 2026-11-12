//go:build windows

package daemon

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	hubprocess "mcp-local-hub/internal/process"
)

// Scratch-only executable resource probe. Never runs unless the exact locally
// built fixture binary and an owned evidence directory are explicitly supplied.
func TestRXRelayBinaryProbe(t *testing.T) {
	executable := os.Getenv("MCPHUB_RX_RELAY_BINARY")
	evidenceDir := os.Getenv("MCPHUB_RX_RELAY_EVIDENCE")
	if executable == "" || evidenceDir == "" {
		t.Skip("owned binary probe only")
	}
	if !filepath.IsAbs(executable) || !filepath.IsAbs(evidenceDir) {
		t.Fatal("absolute owned paths required")
	}
	for _, count := range []int{1, 8, 32} {
		t.Run(fmt.Sprintf("processes_%d", count), func(t *testing.T) {
			f := newRelayLoadServer(t)
			ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
			defer cancel()
			type child struct {
				cmd      *exec.Cmd
				stdin    io.WriteCloser
				lines    chan []byte
				readDone chan error
				stderr   bytes.Buffer
				session  string
				joined   bool
			}
			children := make([]*child, 0, count)
			defer func() {
				for _, c := range children {
					if !c.joined {
						_ = c.stdin.Close()
						_ = c.cmd.Process.Kill()
						_ = c.cmd.Wait()
						c.joined = true
						select {
						case <-c.readDone:
						case <-time.After(time.Second):
							t.Error("reader did not join after owned child termination")
						}
					}
				}
			}()
			next := func(c *child) ([]byte, error) {
				select {
				case line, ok := <-c.lines:
					if !ok {
						return nil, fmt.Errorf("child stdout EOF")
					}
					return line, nil
				case <-ctx.Done():
					return nil, ctx.Err()
				}
			}
			startups := make([]float64, 0, count)
			for i := 0; i < count; i++ {
				c := &child{cmd: exec.CommandContext(ctx, executable, "relay", "--url", f.server.URL), lines: make(chan []byte, 4), readDone: make(chan error, 1)}
				c.cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: 0x08000000}
				c.cmd.Env = append(os.Environ(), "MCPHUB_NO_AUTO_GUI=1", "GOMAXPROCS=4")
				c.cmd.Dir = evidenceDir
				c.cmd.Stderr = &c.stderr
				var err error
				c.stdin, err = c.cmd.StdinPipe()
				if err != nil {
					t.Fatal(err)
				}
				stdout, err := c.cmd.StdoutPipe()
				if err != nil {
					c.stdin.Close()
					t.Fatal(err)
				}
				start := time.Now()
				if err = c.cmd.Start(); err != nil {
					c.stdin.Close()
					stdout.Close()
					t.Fatal(err)
				}
				children = append(children, c)
				go func() {
					scanner := bufio.NewScanner(stdout)
					scanner.Buffer(make([]byte, 4096), maxStdinLineBytes)
					for scanner.Scan() {
						line := append([]byte(nil), scanner.Bytes()...)
						select {
						case c.lines <- line:
						case <-ctx.Done():
							c.readDone <- ctx.Err()
							close(c.lines)
							return
						}
					}
					c.readDone <- scanner.Err()
					close(c.lines)
				}()
				if _, err = io.WriteString(c.stdin, "{\"jsonrpc\":\"2.0\",\"id\":0,\"method\":\"initialize\"}\n"); err != nil {
					t.Fatal(err)
				}
				line, err := next(c)
				if err != nil {
					t.Fatal(err)
				}
				var init relayLoadResponse
				if err = json.Unmarshal(line, &init); err != nil || string(init.ID) != "0" || !strings.HasPrefix(init.Result.Session, "rx-session-") {
					t.Fatalf("bad init %q err=%v", line, err)
				}
				c.session = init.Result.Session
				startups = append(startups, float64(time.Since(start).Nanoseconds())/1e6)
				if _, err = io.WriteString(c.stdin, "{\"jsonrpc\":\"2.0\",\"method\":\"notifications/initialized\"}\n"); err != nil {
					t.Fatal(err)
				}
			}
			// Explicit idle settling interval; not used to assert concurrency correctness.
			time.Sleep(150 * time.Millisecond)
			readyRSS := uint64(0)
			for _, c := range children {
				rss, ok := hubprocess.ResidentSetSizeByPID(c.cmd.Process.Pid)
				if !ok {
					t.Fatal("owned running child RSS unknown")
				}
				readyRSS += rss
			}
			samples := make(chan float64, count*16)
			failures := make(chan error, count)
			var wg sync.WaitGroup
			for _, c := range children {
				wg.Add(1)
				go func(c *child) {
					defer wg.Done()
					for i := 1; i <= 16; i++ {
						request := relayLoadMessage{JSONRPC: "2.0", ID: json.RawMessage(fmt.Sprintf("%d", i)), Method: "tools/call", Params: relayLoadParams{Payload: strings.Repeat("x", 1024)}}
						body, _ := json.Marshal(request)
						start := time.Now()
						if _, err := c.stdin.Write(append(body, '\n')); err != nil {
							failures <- err
							return
						}
						line, err := next(c)
						if err != nil {
							failures <- err
							return
						}
						var response relayLoadResponse
						if err = json.Unmarshal(line, &response); err != nil || string(response.ID) != string(request.ID) || response.Result.Session != c.session || response.Result.Payload != request.Params.Payload {
							failures <- fmt.Errorf("wrong binary relay response, err=%v", err)
							return
						}
						samples <- float64(time.Since(start).Nanoseconds()) / 1e6
					}
				}(c)
			}
			wg.Wait()
			close(samples)
			close(failures)
			for err := range failures {
				if err != nil {
					t.Fatal(err)
				}
			}
			latency := make([]float64, 0, count*16)
			for value := range samples {
				latency = append(latency, value)
			}
			sort.Float64s(latency)
			activeRSS := uint64(0)
			for _, c := range children {
				rss, ok := hubprocess.ResidentSetSizeByPID(c.cmd.Process.Pid)
				if !ok {
					t.Fatal("owned running child post-load RSS unknown")
				}
				activeRSS += rss
			}
			cpu := time.Duration(0)
			for _, c := range children {
				if err := c.stdin.Close(); err != nil {
					t.Fatal(err)
				}
				select {
				case err := <-c.readDone:
					if err != nil {
						t.Fatal(err)
					}
				case <-ctx.Done():
					t.Fatal("child failed to drain")
				}
				err := c.cmd.Wait()
				c.joined = true
				if err != nil {
					t.Fatalf("relay exit: %v stderr=%q", err, c.stderr.String())
				}
				if c.stderr.Len() != 0 {
					t.Fatalf("unexpected relay diagnostic %q", c.stderr.String())
				}
				cpu += c.cmd.ProcessState.UserTime() + c.cmd.ProcessState.SystemTime()
			}
			if f.deleted.Load() != int64(count) || f.calls.Load() != int64(count*16) {
				t.Fatal("binary session/request count mismatch")
			}
			sort.Float64s(startups)
			result := map[string]any{"processes": count, "payload_bytes": 1024, "samples": len(latency), "ready_working_set_sum_bytes": readyRSS, "post_load_working_set_sum_bytes": activeRSS, "startup_median_ms": startups[len(startups)/2], "closed_loop_p50_ms": latency[len(latency)/2], "closed_loop_p95_ms": latency[(len(latency)-1)*95/100], "owned_process_lifetime_cpu_ms": float64(cpu.Nanoseconds()) / 1e6, "qualification": "default relay binary; warm loopback; 16 sequential requests per child; no open-loop p99 claim"}
			encoded, err := json.MarshalIndent(result, "", "  ")
			if err != nil {
				t.Fatal(err)
			}
			if err = os.WriteFile(filepath.Join(evidenceDir, fmt.Sprintf("relay-binary-%d.json", count)), encoded, 0600); err != nil {
				t.Fatal(err)
			}
			t.Log(string(encoded))
		})
	}
}

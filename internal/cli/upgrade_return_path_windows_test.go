//go:build windows

package cli

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"reflect"
	"strings"
	"testing"
	"time"

	"mcp-local-hub/internal/api"
)

func countUpgradeReturnPathCall(calls []string, want string) int {
	count := 0
	for _, call := range calls {
		if call == want {
			count++
		}
	}
	return count
}

func upgradeReturnPathBefore(calls []string, first, second string) bool {
	firstIndex, secondIndex := -1, -1
	for i, call := range calls {
		if call == first && firstIndex == -1 {
			firstIndex = i
		}
		if call == second && secondIndex == -1 {
			secondIndex = i
		}
	}
	return firstIndex >= 0 && secondIndex >= 0 && firstIndex < secondIndex
}

func TestV5UpgradeIPCRequiresObservedOwnerBeforeDial(t *testing.T) {
	for _, tc := range []struct {
		name       string
		owner      api.SupervisorLockOwner
		readErr    error
		wantSender int
		wantErr    bool
	}{
		{name: "read_error", readErr: errors.New("injected owner read failure"), wantErr: true},
		{name: "invalid_pid", owner: api.SupervisorLockOwner{PID: 0, StartedAt: "2031-02-03T04:05:06Z"}, wantErr: true},
		{name: "missing_started_at", owner: api.SupervisorLockOwner{PID: 77}, wantErr: true},
		{name: "valid_owner", owner: api.SupervisorLockOwner{PID: 77, StartedAt: "2031-02-03T04:05:06Z"}, wantSender: 1},
	} {
		for _, operation := range []struct {
			name string
			call func(*v5UpgradeDeps) (api.IPCResponse, error)
			args map[string]any
		}{
			{name: "quiesce-timers", call: func(d *v5UpgradeDeps) (api.IPCResponse, error) {
				return d.QuiesceTimers(context.Background(), "injected-pipe", 17)
			}, args: map[string]any{"timeout_ms": 17}},
			{name: "exit", call: func(d *v5UpgradeDeps) (api.IPCResponse, error) {
				return d.ExitGraceful(context.Background(), "injected-pipe", 17)
			}, args: map[string]any{"graceful": true, "timeout_ms": 17}},
		} {
			t.Run(tc.name+"_"+operation.name, func(t *testing.T) {
				senderCalls := 0
				var gotOwner api.SupervisorLockOwner
				var gotPipe, gotCommand string
				var gotArgs map[string]any
				var gotTimeout time.Duration
				d := &v5UpgradeDeps{
					supervisorLockDir: "injected-owner-path",
					readSupervisorOwner: func(path string) (api.SupervisorLockOwner, error) {
						if path != "injected-owner-path" {
							t.Fatalf("owner path=%q", path)
						}
						return tc.owner, tc.readErr
					},
					sendIPC: func(_ context.Context, pipe string, owner api.SupervisorLockOwner, command string, args map[string]any, timeout time.Duration) (api.IPCResponse, error) {
						senderCalls++
						gotPipe, gotOwner, gotCommand, gotArgs, gotTimeout = pipe, owner, command, args, timeout
						if tc.wantErr {
							return api.IPCResponse{}, errors.New("injected sender reached")
						}
						return api.IPCResponse{OK: true, Final: true}, nil
					},
				}
				response, err := operation.call(d)
				if tc.wantErr {
					if err == nil || !strings.Contains(err.Error(), operation.name) || senderCalls != 0 {
						t.Fatalf("error=%v senderCalls=%d, want labelled owner error before sender", err, senderCalls)
					}
					return
				}
				if err != nil || senderCalls != tc.wantSender || !response.OK || !response.Final {
					t.Fatalf("response=(%+v,%v) senderCalls=%d", response, err, senderCalls)
				}
				if gotPipe != "injected-pipe" || gotOwner != tc.owner || gotCommand != operation.name || !reflect.DeepEqual(gotArgs, operation.args) || gotTimeout != 5017*time.Millisecond {
					t.Fatalf("sender=(pipe=%q owner=%+v command=%q args=%v timeout=%s)", gotPipe, gotOwner, gotCommand, gotArgs, gotTimeout)
				}
			})
		}
	}

	for _, tc := range []struct {
		name     string
		expected api.SupervisorLockOwner
		hello    api.SupervisorLockOwner
		wantErr  bool
	}{
		{name: "explicit_zero_owner_compatibility", expected: api.SupervisorLockOwner{}, hello: api.SupervisorLockOwner{PID: 77, StartedAt: "2031-02-03T04:05:06Z"}},
		{name: "nonzero_generation_mismatch", expected: api.SupervisorLockOwner{PID: 88, StartedAt: "2031-02-03T04:05:06Z"}, hello: api.SupervisorLockOwner{PID: 77, StartedAt: "2031-02-03T04:05:06Z"}, wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server, client := net.Pipe()
			defer client.Close()
			writeDone := make(chan error, 1)
			go func() {
				defer server.Close()
				frame, err := json.Marshal(map[string]any{"hello": api.IPCHello{Version: 1, PID: tc.hello.PID, StartedAt: tc.hello.StartedAt}})
				if err == nil {
					_, err = server.Write(append(frame, '\n'))
				}
				writeDone <- err
			}()
			err := verifyHelloFrame(client, tc.expected)
			if (err != nil) != tc.wantErr {
				t.Fatalf("verifyHelloFrame error=%v wantErr=%v", err, tc.wantErr)
			}
			if writeErr := <-writeDone; writeErr != nil {
				t.Fatalf("write hello: %v", writeErr)
			}
		})
	}
}

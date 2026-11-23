package mcproute

import (
	"context"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/sys/windows"
	"mcp-local-hub/internal/api"
	"mcp-local-hub/internal/api/apitest"
)

func TestFixedHubStartRetainsCleanupOwner(t *testing.T) {
	stateDir := apitest.HardenedTempDir(t)
	t.Setenv("MCPHUB_STATE_DIR_OVERRIDE", stateDir)
	t.Cleanup(api.SetDaemonStateRootForTest(stateDir))
	if _, err := api.EnsureHubTokens([]string{"claude-code"}); err != nil {
		t.Fatal(err)
	}
	probe, err := api.NewListenerWithSOExclusiveContext(t.Context(), "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	address := probe.Addr().String()
	port := probe.Addr().(*net.TCPAddr).Port
	if err := probe.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := api.EnsureHubEndpoint(port, os.Getpid()); err != nil {
		t.Fatal(err)
	}
	identity, err := api.ObserveHubRouteStateDirectory(t.Context(), stateDir)
	if err != nil {
		t.Fatal(err)
	}
	failure := errors.New("fixture original enclosing lease failed")
	var blocker windows.Handle
	releaseBlocker := func() {
		if blocker != 0 {
			if err := windows.CloseHandle(blocker); err != nil {
				t.Error("close fixture-owned blocker:", err)
			}
			blocker = 0
		}
	}
	defer releaseBlocker()

	listener, err := StartHubListener(t.Context(), api.NewAPI(), HubListenerOptions{
		BindExisting: func(ctx context.Context, _ func() error) (*api.HubMcpBindResult, error) {
			var acquired *api.HubMcpBindResult
			err := api.WithHubRouteState(ctx, stateDir, identity, func(h *api.HubRouteState) error {
				endpoint, err := h.ReadEndpoint()
				if err != nil {
					return err
				}
				row := api.HubRouteQualification{
					TransactionID: strings.Repeat("a", 32), SuccessorSHA256: strings.Repeat("b", 64), ScopeSHA256: strings.Repeat("c", 64),
					EndpointAuthoritySHA256: endpoint.AuthoritySHA256(), EndpointProvenanceSHA256: endpoint.ProvenanceSHA256(),
				}
				if err := h.StageQualification("", endpoint, row); err != nil {
					return err
				}
				acquired, err = h.BindExistingHubMcpListener(endpoint, row)
				if err != nil {
					return err
				}
				path, err := windows.UTF16PtrFromString(filepath.Join(stateDir, "hub-mcp-control.token"))
				if err != nil {
					return err
				}
				blocker, err = windows.CreateFile(path, windows.GENERIC_READ, windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE, nil,
					windows.OPEN_EXISTING, windows.FILE_FLAG_OPEN_REPARSE_POINT, 0)
				if err != nil {
					blocker = 0
					return err
				}
				return failure
			})
			return acquired, err
		},
	})
	if !errors.Is(err, failure) || !errors.Is(err, windows.STATUS_SHARING_VIOLATION) || listener == nil || listener.Alive() {
		t.Fatalf("assembly dropped pending ownership or reported readiness: listener=%v err=%v", listener != nil, err)
	}
	contender, err := api.NewListenerWithSOExclusiveContext(t.Context(), address)
	if err != nil {
		t.Fatalf("assembly left the original socket active: %v", err)
	}
	if err := contender.Close(); err != nil {
		t.Fatal(err)
	}
	if result := listener.RetireOriginal(t.Context()); result.Settled() || !errors.Is(result.Err, windows.STATUS_SHARING_VIOLATION) {
		t.Fatalf("attachment reported retirement with a blocked original file: %+v", result)
	}
	releaseBlocker()
	if result := listener.RetireOriginal(t.Context()); !result.Settled() || !errors.Is(result.Err, failure) {
		t.Fatalf("attachment lost original settlement or failure: %+v", result)
	}
	if err := api.WithHubRouteState(t.Context(), stateDir, identity, func(h *api.HubRouteState) error {
		return h.CheckReloadTokenAbsent()
	}); err != nil {
		t.Fatal(err)
	}
}

package pinstatus

import (
	"context"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"mcp-local-hub/internal/vcpkgmcp/evidence"
)

func TestR15RelativeLocalRemoteIsRefusedBeforeQuery(t *testing.T) {
	dir := newPort(t, "relative-remote", `vcpkg_from_git(
    OUT_SOURCE_PATH SOURCE_PATH
    URL ../upstream
    REF `+commitA+`
)`)
	var calls atomic.Int32
	result := PinStatus(context.Background(), Args{PortDirs: []string{dir}}, Deps{
		FS: DefaultFS(),
		RemoteRefs: func(context.Context, approvedRemoteURL) (map[string]string, error) {
			calls.Add(1)
			return map[string]string{"HEAD": commitA}, nil
		},
		Now: fixedNow(),
	}).Ports[0]

	if got := calls.Load(); got != 0 {
		t.Fatalf("relative remote launched %d queries, want 0", got)
	}
	if result.Status != evidence.StatusUnknown || string(result.Reason) != "remote_url_relative" {
		t.Fatalf("status/reason = %s/%s, want unknown/remote_url_relative", result.Status, result.Reason)
	}
}

func TestR15BatchDeduplicatesOneRemoteSnapshot(t *testing.T) {
	portDirs := make([]string, 4)
	for i := range portDirs {
		portDirs[i] = newPort(t, fmt.Sprintf("dedup-%d", i), `vcpkg_from_github(REPO acme/shared REF `+commitA+` SHA512 0)`)
	}
	var calls atomic.Int32
	result := PinStatus(context.Background(), Args{PortDirs: portDirs}, Deps{
		FS: DefaultFS(),
		RemoteRefs: func(context.Context, approvedRemoteURL) (map[string]string, error) {
			calls.Add(1)
			return map[string]string{"HEAD": commitA}, nil
		},
		Now: fixedNow(),
	})

	if got := calls.Load(); got != 1 {
		t.Fatalf("same approved remote queried %d times, want one call-scoped snapshot", got)
	}
	for i, port := range result.Ports {
		if port.Status != evidence.StatusOK {
			t.Fatalf("ports[%d] = %s/%s, want ok", i, port.Status, port.Reason)
		}
	}
}

func TestR15ConcurrentBatchesShareFourRemoteQuerySlots(t *testing.T) {
	makeBatch := func(prefix string) []string {
		portDirs := make([]string, 4)
		for i := range portDirs {
			portDirs[i] = newPort(t, fmt.Sprintf("%s-%d", prefix, i), fmt.Sprintf(
				"vcpkg_from_github(REPO acme/%s-%d REF %s SHA512 0)", prefix, i, commitA))
		}
		return portDirs
	}
	batches := [][]string{makeBatch("a"), makeBatch("b")}
	started := make(chan struct{}, 8)
	release := make(chan struct{})
	query := func(ctx context.Context, _ approvedRemoteURL) (map[string]string, error) {
		started <- struct{}{}
		select {
		case <-release:
			return map[string]string{"HEAD": commitA}, nil
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	done := make(chan Result, len(batches))
	for _, dirs := range batches {
		go func(portDirs []string) {
			done <- PinStatus(context.Background(), Args{PortDirs: portDirs}, Deps{
				FS: DefaultFS(), RemoteRefs: query, Now: fixedNow(),
			})
		}(dirs)
	}

	for i := 0; i < 4; i++ {
		select {
		case <-started:
		case <-time.After(time.Second):
			close(release)
			for range batches {
				<-done
			}
			t.Fatalf("only %d remote queries entered before release; want four bounded concurrent slots", i)
		}
	}
	select {
	case <-started:
		close(release)
		for range batches {
			<-done
		}
		t.Fatal("a fifth remote query entered before release; process-wide slot bound exceeded")
	case <-time.After(100 * time.Millisecond):
	}
	close(release)
	for range batches {
		result := <-done
		for i, port := range result.Ports {
			if port.Status != evidence.StatusOK {
				t.Fatalf("ports[%d] = %s/%s, want ok", i, port.Status, port.Reason)
			}
		}
	}
}

func TestR15BatchDeadlineBoundsWholeCall(t *testing.T) {
	portDirs := make([]string, 6)
	for i := range portDirs {
		portDirs[i] = newPort(t, fmt.Sprintf("deadline-%d", i), fmt.Sprintf(
			"vcpkg_from_github(REPO acme/deadline-%d REF %s SHA512 0)", i, commitA))
	}
	started := time.Now()
	result := PinStatus(context.Background(), Args{PortDirs: portDirs}, Deps{
		FS: DefaultFS(),
		RemoteRefs: func(ctx context.Context, _ approvedRemoteURL) (map[string]string, error) {
			<-ctx.Done()
			return nil, ctx.Err()
		},
		Now:          fixedNow(),
		BatchTimeout: 50 * time.Millisecond,
	})
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("PinStatus elapsed %s, want whole-batch deadline below 1s", elapsed)
	}
	for i, port := range result.Ports {
		if port.Status != evidence.StatusUnknown || port.Reason != ReasonRemoteQueryTimeout {
			t.Fatalf("ports[%d] = %s/%s, want unknown/%s", i, port.Status, port.Reason, ReasonRemoteQueryTimeout)
		}
	}
}

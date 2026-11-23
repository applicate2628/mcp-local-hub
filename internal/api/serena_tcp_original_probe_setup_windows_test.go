//go:build windows

package api

import (
	"context"
	"errors"
	"io"
	"os/exec"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"golang.org/x/sys/windows"
	"mcp-local-hub/internal/process"
)

func TestOriginalTCPProbeRetainsQualifierParentFailureAndChildClose(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	end, _ := ctx.Deadline()
	parentErr := errors.New("fixture original parent close failed")
	var closed []windows.Handle
	var starts int
	original, err := observeQualifiedLoopbackTCPFrame(ctx, end, filepath.Join(`C:\`, "fixture", "netstat.exe"),
		&LoopbackTCPRowsObservation{issuer: &struct{ marker byte }{1}},
		func(ctx context.Context, path string, cap int64) (PreparedSerenaFile, *preparedSerenaExecutableObservation, error) {
			return qualifyPreparedSerenaExecutableWithOps(ctx, path, cap, preparedSerenaExecutableOps{
				root: func(string) stateReadNativeOpenOutcome {
					return stateReadNativeOpenOutcome{handle: 7, entered: true, returned: true}
				},
				child: func(parent windows.Handle, _ string, _ bool) stateReadNativeOpenOutcome {
					if parent != 7 {
						t.Fatalf("wrong original parent: %v", parent)
					}
					return stateReadNativeOpenOutcome{handle: 8, entered: true, returned: true}
				},
				close: func(handle windows.Handle) error {
					closed = append(closed, handle)
					if handle == 7 {
						return parentErr
					}
					return nil
				},
			})
		}, func(context.Context, *exec.Cmd, process.ContainedStreamOptions, func(io.Reader) error) (process.ContainedStreamReceiptV1, error) {
			starts++
			return process.ContainedStreamReceiptV1{}, nil
		})
	if !errors.Is(err, parentErr) || starts != 0 || original == nil || original.runEntered ||
		!reflect.DeepEqual(closed, []windows.Handle{7, 8}) {
		t.Fatalf("qualifier failure lost closure or dispatched a process: %v", err)
	}
	proof := original.qualification.(*preparedSerenaExecutableObservation)
	if len(proof.slots) != 2 || !proof.slots[1].closeReturned || !errors.Is(proof.slots[0].closeErr, parentErr) ||
		proof.ResourcesFinalized() {
		t.Fatal("original qualifier did not retain both native close outcomes")
	}
}

func TestRouteLoopbackFrameUsesFirstIPv4ListeningMatch(t *testing.T) {
	raw := []byte("unparsed native header\n TCP [::1]:9411 [::]:0 LISTENING 17\n TCP 127.0.0.1:9411 0.0.0.0:0 LISTENING 18\n TCP 127.0.0.1:9411 0.0.0.0:0 LISTENING 19\n")
	if pid, found := firstOriginalLoopbackFrameOwner(raw, 9411); !found || pid != 18 {
		t.Fatal("Route changed the original first IPv4 owner projection")
	}
	if _, err := parseOriginalLoopbackTCPRows(raw); err == nil {
		t.Fatal("Route projection weakened the independent strict Serena format")
	}
}

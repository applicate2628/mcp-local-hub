package patchesapply

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"mcp-local-hub/internal/vcpkgmcp/boundedio"
	"mcp-local-hub/internal/vcpkgmcp/evidence"
)

func TestTripletRootChainAdmissionPrecedesEveryFilesystemCall(t *testing.T) {
	portDir := writeFixture(t, "# no patches\n")
	triplet := "x64-r12"

	assertNoFS := func(t *testing.T, args Args, want Reason) {
		t.Helper()
		calls := 0
		deps := Deps{
			Stat: func(string) (os.FileInfo, error) { calls++; return nil, os.ErrNotExist },
			Open: func(string) (io.ReadCloser, error) { calls++; return nil, os.ErrNotExist },
			OpenDir: func(string) (boundedio.DirReader, error) {
				calls++
				return nil, os.ErrNotExist
			},
		}
		result := applyOrderContext(context.Background(), args, deps)
		if result.Status != evidence.StatusFailed || result.Reason != want || calls != 0 {
			t.Fatalf("result=%+v fsCalls=%d, want failed/%s and zero filesystem work", result, calls, want)
		}
	}

	over := make([]string, MaxOverlayTripletRoots+1)
	for i := range over {
		over[i] = filepath.Join(portDir, fmt.Sprintf("over-%02d", i))
	}
	assertNoFS(t, Args{PortDir: portDir, Triplet: triplet, OverlayTriplets: over}, ReasonTooManyOverlayTripletRoots)

	for position := 0; position < MaxOverlayTripletRoots; position++ {
		position := position
		t.Run(fmt.Sprintf("relative-overlay-%02d", position), func(t *testing.T) {
			roots := append([]string(nil), over[:MaxOverlayTripletRoots]...)
			roots[position] = "relative/triplets"
			assertNoFS(t, Args{PortDir: portDir, Triplet: triplet, OverlayTriplets: roots}, ReasonRelativeOverlayTripletRoot)
		})
	}
	assertNoFS(t, Args{PortDir: portDir, Triplet: triplet, VcpkgRoot: "relative/vcpkg"}, ReasonRelativeVcpkgRoot)

	accepted := over[:MaxOverlayTripletRoots]
	deps := DefaultDeps()
	var probed []string
	realStat := deps.Stat
	deps.Stat = func(path string) (os.FileInfo, error) {
		if filepath.Base(path) == triplet+".cmake" {
			probed = append(probed, path)
		}
		return realStat(path)
	}
	result := applyOrderContext(context.Background(), Args{PortDir: portDir, Triplet: triplet, OverlayTriplets: accepted}, deps)
	if result.Status == evidence.StatusFailed && (result.Reason == ReasonTooManyOverlayTripletRoots || result.Reason == ReasonRelativeOverlayTripletRoot) {
		t.Fatalf("maximum-sized absolute chain was rejected by admission: %+v", result)
	}
	want := make([]string, len(accepted))
	for i, root := range accepted {
		want[i] = filepath.Join(root, triplet+".cmake")
	}
	if !reflect.DeepEqual(probed, want) {
		t.Fatalf("probe order changed\ngot:  %v\nwant: %v", probed, want)
	}
}

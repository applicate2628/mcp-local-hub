package pinstatus

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"mcp-local-hub/internal/vcpkgmcp/evidence"
)

type boundedRecordingFS struct {
	files map[string][]byte
	reads []string
}

var _ FS = (*boundedRecordingFS)(nil)

func TestDefaultFSBoundedSemanticReadHasExactBoundary(t *testing.T) {
	path := filepath.Join(t.TempDir(), "semantic-input")
	for _, tc := range []struct {
		name     string
		size     int
		complete bool
	}{
		{name: "exact limit", size: MaxSemanticFileBytes, complete: true},
		{name: "one byte over limit", size: MaxSemanticFileBytes + 1, complete: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := os.WriteFile(path, []byte(strings.Repeat("x", tc.size)), 0o600); err != nil {
				t.Fatalf("write fixture: %v", err)
			}
			data, complete, err := DefaultFS().ReadFile(path)
			if err != nil {
				t.Fatalf("ReadFile: %v", err)
			}
			if complete != tc.complete || len(data) != MaxSemanticFileBytes {
				t.Fatalf("complete=%v bytes=%d, want complete=%v bytes=%d", complete, len(data), tc.complete, MaxSemanticFileBytes)
			}
		})
	}
}

func (f *boundedRecordingFS) ReadFile(path string) ([]byte, bool, error) {
	f.reads = append(f.reads, path)
	data := f.files[path]
	if len(data) > MaxSemanticFileBytes {
		return data[:MaxSemanticFileBytes], false, nil
	}
	return data, true, nil
}

func TestPinStatusRejectsOversizedBatchBeforeFSClockOrRemote(t *testing.T) {
	portDirs := make([]string, MaxPortDirs+1)
	for i := range portDirs {
		portDirs[i] = filepath.Join(t.TempDir(), "port")
	}

	fsys := &boundedRecordingFS{}
	clockCalls := 0
	remoteCalls := 0
	res := PinStatus(context.Background(), Args{PortDirs: portDirs}, Deps{
		FS: fsys,
		Now: func() time.Time {
			clockCalls++
			return time.Time{}
		},
		RemoteRefs: func(context.Context, approvedRemoteURL) (map[string]string, error) {
			remoteCalls++
			return nil, nil
		},
	})

	if res.Status != evidence.StatusUnknown || res.Reason != BatchReasonTooManyPortDirs || len(res.Ports) != 0 {
		t.Fatalf("result = %+v, want unknown(too_many_port_dirs) with no port rows", res)
	}
	if len(fsys.reads) != 0 || clockCalls != 0 || remoteCalls != 0 {
		t.Fatalf("rejected batch performed work: reads=%v clocks=%d remotes=%d", fsys.reads, clockCalls, remoteCalls)
	}
	projected, ok := res.PublicResultProjection().(projectedPinStatus)
	if !ok || projected.Status != evidence.StatusUnknown || projected.Reason != BatchReasonTooManyPortDirs {
		t.Fatalf("projection = %#v, want preserved batch reason", res.PublicResultProjection())
	}
}

func TestPinStatusMaxBatchIsAdmitted(t *testing.T) {
	portDirs := make([]string, MaxPortDirs)
	for i := range portDirs {
		portDirs[i] = filepath.Join(t.TempDir(), "port")
	}

	res := PinStatus(context.Background(), Args{PortDirs: portDirs, DisableNetwork: true}, Deps{Now: fixedNow()})
	if res.Status != evidence.StatusOK || res.Reason != "" || len(res.Ports) != MaxPortDirs {
		t.Fatalf("result = %+v, want admitted %d-port batch", res, MaxPortDirs)
	}
}

func TestPinStatusSemanticFileBudgetIsExplicitIncompleteEvidence(t *testing.T) {
	portDir := filepath.Join(t.TempDir(), "bounded-port")
	portfilePath := filepath.Join(portDir, "portfile.cmake")
	manifestPath := filepath.Join(portDir, "vcpkg.json")
	completePortfile := []byte(`vcpkg_from_github(REPO acme/widget REF ` + commitA + ` SHA512 0)`)
	completePortfile = append(completePortfile, []byte(strings.Repeat(" ", MaxSemanticFileBytes-len(completePortfile)))...)

	for _, tc := range []struct {
		name      string
		portfile  []byte
		manifest  []byte
		wantReads int
	}{
		{
			name:      "portfile exact limit remains complete",
			portfile:  completePortfile,
			manifest:  []byte(`{}`),
			wantReads: 2,
		},
		{
			name:      "portfile one byte over limit is incomplete",
			portfile:  append(append([]byte(nil), completePortfile...), 'x'),
			wantReads: 1,
		},
		{
			name:      "manifest one byte over limit is incomplete",
			portfile:  completePortfile,
			manifest:  append([]byte(`{}`), []byte(strings.Repeat(" ", MaxSemanticFileBytes-1))...),
			wantReads: 2,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fsys := &boundedRecordingFS{files: map[string][]byte{portfilePath: tc.portfile, manifestPath: tc.manifest}}
			remoteCalls := 0
			res := PinStatus(context.Background(), Args{PortDirs: []string{portDir}}, Deps{
				FS:  fsys,
				Now: fixedNow(),
				RemoteRefs: func(context.Context, approvedRemoteURL) (map[string]string, error) {
					remoteCalls++
					return map[string]string{"HEAD": commitA}, nil
				},
			})

			if len(fsys.reads) != tc.wantReads {
				t.Fatalf("read count = %d, want %d; reads=%v", len(fsys.reads), tc.wantReads, fsys.reads)
			}
			port := res.Ports[0]
			if tc.name == "portfile exact limit remains complete" {
				if port.Status != evidence.StatusOK || remoteCalls != 1 {
					t.Fatalf("exact-budget port = %+v remoteCalls=%d, want ok and one query", port, remoteCalls)
				}
				return
			}
			if port.Status != evidence.StatusUnknown || port.Reason != ReasonSemanticFileIncomplete || remoteCalls != 0 {
				t.Fatalf("over-budget port = %+v remoteCalls=%d, want unknown(semantic_file_incomplete) and no query", port, remoteCalls)
			}
		})
	}
}

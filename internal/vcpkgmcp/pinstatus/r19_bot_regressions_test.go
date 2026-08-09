package pinstatus

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type r19SpecialSemanticInfo struct{}

func (r19SpecialSemanticInfo) Name() string       { return "portfile.cmake" }
func (r19SpecialSemanticInfo) Size() int64        { return 0 }
func (r19SpecialSemanticInfo) Mode() os.FileMode  { return os.ModeNamedPipe }
func (r19SpecialSemanticInfo) ModTime() time.Time { return time.Time{} }
func (r19SpecialSemanticInfo) IsDir() bool        { return false }
func (r19SpecialSemanticInfo) Sys() any           { return nil }

func TestR19SemanticFileRejectsSpecialFileBeforeOpen(t *testing.T) {
	opened := false
	_, _, err := readSemanticFileWithOps("portfile.cmake",
		func(string) (os.FileInfo, error) { return r19SpecialSemanticInfo{}, nil },
		func(string) (io.ReadCloser, error) { opened = true; return nil, io.EOF })
	if err == nil || opened {
		t.Fatalf("err=%v opened=%v, want special-file rejection before Open", err, opened)
	}
}

func TestR19PortfileCapsRetainedFetchCandidates(t *testing.T) {
	var source strings.Builder
	for i := 0; i <= MaxFetchCandidatesPerPort; i++ {
		source.WriteString("vcpkg_from_github(REPO owner/repo REF main)\n")
	}
	parsed, ok := parsePortfile(source.String())
	if !ok || !parsed.CandidateLimitExceeded {
		t.Fatalf("ok=%v parsed=%+v, want explicit candidate-retention limit", ok, parsed)
	}
	if len(parsed.Candidates) > MaxFetchCandidatesPerPort {
		t.Fatalf("retained=%d, max=%d", len(parsed.Candidates), MaxFetchCandidatesPerPort)
	}
	portDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(portDir, "portfile.cmake"), []byte(source.String()), 0o600); err != nil {
		t.Fatal(err)
	}
	result := PinStatus(context.Background(), Args{PortDirs: []string{portDir}}, Deps{
		FS: DefaultFS(),
		RemoteRefs: func(context.Context, approvedRemoteURL) (map[string]string, error) {
			t.Fatal("candidate-limited port reached the network")
			return nil, nil
		},
		Now: func() time.Time { return time.Unix(1, 0) },
	})
	if len(result.Ports) != 1 || result.Ports[0].Reason != ReasonFetchCandidateLimit {
		t.Fatalf("result=%+v, want explicit fetch_candidate_limit", result)
	}
	if MaxFetchCandidatesPerBatch != MaxPortDirs*MaxFetchCandidatesPerPort ||
		MaxRetainedFetchCandidateBytesPerBatch != MaxPortDirs*MaxRetainedFetchCandidateBytesPerPort {
		t.Fatal("batch candidate bounds drifted from admitted per-port limits")
	}
}

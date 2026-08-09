package pinstatus

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"mcp-local-hub/internal/vcpkgmcp/boundedio"
)

type r19SpecialSemanticInfo struct{}

func (r19SpecialSemanticInfo) Name() string       { return "portfile.cmake" }
func (r19SpecialSemanticInfo) Size() int64        { return 0 }
func (r19SpecialSemanticInfo) Mode() os.FileMode  { return os.ModeNamedPipe }
func (r19SpecialSemanticInfo) ModTime() time.Time { return time.Time{} }
func (r19SpecialSemanticInfo) IsDir() bool        { return false }
func (r19SpecialSemanticInfo) Sys() any           { return nil }

func TestR19SemanticFileRejectsSpecialFileBeforeOpen(t *testing.T) {
	file := &r19SemanticFile{ReadCloser: io.NopCloser(strings.NewReader("ignored"))}
	_, _, err := readSemanticFileWithOpener("portfile.cmake", func(string) (boundedio.RegularFile, error) { return file, nil })
	if err == nil || file.statCalls != 1 {
		t.Fatalf("err=%v stat calls=%d, want same-handle special-file rejection", err, file.statCalls)
	}
}

type r19SemanticFile struct {
	io.ReadCloser
	statCalls int
}

func (f *r19SemanticFile) Stat() (os.FileInfo, error) {
	f.statCalls++
	return r19SpecialSemanticInfo{}, nil
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

package lastfailure

import (
	"context"
	"io"
	"os"
	"testing"
	"time"
)

type r18SpecialFileInfo struct{}

func (r18SpecialFileInfo) Name() string       { return "build.log" }
func (r18SpecialFileInfo) Size() int64        { return 0 }
func (r18SpecialFileInfo) Mode() os.FileMode  { return os.ModeNamedPipe }
func (r18SpecialFileInfo) ModTime() time.Time { return time.Time{} }
func (r18SpecialFileInfo) IsDir() bool        { return false }
func (r18SpecialFileInfo) Sys() any           { return nil }

type r18SpecialLogFS struct {
	FS
	openCalls int
}

func (f *r18SpecialLogFS) Stat(string) (os.FileInfo, error) { return r18SpecialFileInfo{}, nil }
func (f *r18SpecialLogFS) Open(string) (io.ReadCloser, error) {
	f.openCalls++
	return nil, io.EOF
}

func TestR18PhaseLogRejectsSpecialFileBeforeOpen(t *testing.T) {
	fsys := &r18SpecialLogFS{FS: DefaultFS()}
	_, err := newPhaseLogStreamScanner().scan(context.Background(), fsys,
		phaseLogFile{Phase: PhaseBuild, Path: "build.log"}, 1, 1, 1, 1, newDiagnosticAccumulator(1))
	if err == nil {
		t.Fatal("special phase log was accepted")
	}
	if fsys.openCalls != 0 {
		t.Fatalf("Open calls=%d, want zero for a special phase log", fsys.openCalls)
	}
}

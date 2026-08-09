package cmaketrace

import (
	"context"
	"io"
	"io/fs"
	"testing"

	"mcp-local-hub/internal/vcpkgmcp/evidence"
)

type specialTraceFS struct{ opened bool }

func (*specialTraceFS) Stat(string) (fs.FileInfo, error) {
	return modeTraceFileInfo{regularTraceFileInfo: regularTraceFileInfo{}, mode: fs.ModeNamedPipe}, nil
}
func (f *specialTraceFS) Open(string) (io.ReadCloser, error) {
	f.opened = true
	return nil, fs.ErrInvalid
}

type modeTraceFileInfo struct {
	regularTraceFileInfo
	mode fs.FileMode
}

func (f modeTraceFileInfo) Mode() fs.FileMode { return f.mode }
func (f modeTraceFileInfo) IsDir() bool       { return f.mode.IsDir() }

func TestTraceRejectsSpecialFileBeforeOpen(t *testing.T) {
	spy := &specialTraceFS{}
	res := Trace(context.Background(), Args{TracePath: unusedTracePath(t)}, Deps{FS: spy})
	if res.Status != evidence.StatusUnknown || res.Reason != ReasonTraceUnreadable {
		t.Fatalf("result=%+v, want unknown/%s", res, ReasonTraceUnreadable)
	}
	if spy.opened {
		t.Fatal("special trace file reached Open; FIFO input could block the daemon")
	}
}

func TestRelativeTracePathIsRefusedBeforeFilesystemAccess(t *testing.T) {
	spy := &recordingFS{}
	res := Trace(context.Background(), Args{TracePath: "relative/trace.json"}, Deps{FS: spy})

	if res.Status != evidence.StatusFailed || res.Reason != ReasonRelativeTracePath {
		t.Fatalf("status/reason = %v/%v, want failed/%v", res.Status, res.Reason, ReasonRelativeTracePath)
	}
	if len(spy.opened) != 0 {
		t.Fatalf("relative input opened %v, want no filesystem access", spy.opened)
	}
	if len(res.Evidence.Paths) != 0 {
		t.Fatalf("evidence.paths = %v, want empty: relative paths are not filesystem evidence", res.Evidence.Paths)
	}
}

func TestUnsupportedTraceMajorFailsClosedWithoutPartialRecords(t *testing.T) {
	path := writeTrace(t, `{"file":"/proj/CMakeLists.txt","line":1,"cmd":"project","args":["p"]}
{"version":{"major":2,"minor":0}}
{"file":"/proj/CMakeLists.txt","line":2,"cmd":"message","args":["must not escape"]}
`)

	res := Trace(context.Background(), Args{TracePath: path}, defaultDeps())

	if res.Status != evidence.StatusUnknown || res.Reason != ReasonUnsupportedTraceVersion {
		t.Fatalf("status/reason = %v/%v, want unknown/%v", res.Status, res.Reason, ReasonUnsupportedTraceVersion)
	}
	if !res.VersionHeaderPresent {
		t.Fatal("version_header_present = false, want true for the explicit unsupported header")
	}
	if len(res.Records) != 0 || len(res.ExecutedLines) != 0 || len(res.FilesInTrace) != 0 {
		t.Fatalf("unsupported format leaked partial v1 evidence: records=%v lines=%v files=%v", res.Records, res.ExecutedLines, res.FilesInTrace)
	}
}

package portresolution

import (
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestR33OverlayWhitespaceIsPreservedAfterBlankFiltering(t *testing.T) {
	want := filepath.Join(t.TempDir(), " overlay ")
	var statPath string
	result := ResolvePort(Args{
		Port:         "zlib",
		OverlayPorts: []string{" \t ", want},
	}, Deps{
		Stat: func(path string) (os.FileInfo, error) {
			statPath = path
			return nil, fs.ErrPermission
		},
	})

	if statPath != want {
		t.Fatalf("filesystem probe path=%q, want original nonblank overlay %q", statPath, want)
	}
	if len(result.AllCandidates) == 0 || !strings.Contains(result.AllCandidates[0].Source, want) {
		t.Fatalf("candidates=%+v, want source identity containing original overlay %q", result.AllCandidates, want)
	}
}

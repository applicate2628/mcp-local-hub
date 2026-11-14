package api

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestPreparedSerenaUVDiscovery(t *testing.T) {
	// These ordinary files prove selection only. They are never executed and
	// provide no native image, version, interpreter, or package qualification.
	name := "uv"
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	candidate := func(directory string) string {
		t.Helper()
		path := filepath.Join(directory, name)
		if err := os.WriteFile(path, []byte("discovery-only fixture\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		return path
	}
	first, second, fallback, ambient := candidate(t.TempDir()), candidate(t.TempDir()), candidate(t.TempDir()), candidate(t.TempDir())
	empty := t.TempDir()
	t.Setenv("PATH", filepath.Dir(ambient))
	literalDirectory := filepath.Join(t.TempDir(), "$SERENA_UV_DISCOVERY_TEST_SEGMENT")
	if err := os.Mkdir(literalDirectory, 0o700); err != nil {
		t.Fatal(err)
	}
	literal := candidate(literalDirectory)
	t.Setenv("SERENA_UV_DISCOVERY_TEST_SEGMENT", "expanded")

	for _, tc := range []struct {
		name     string
		pathDirs []string
		hints    []string
		want     string
	}{
		{
			name:     "qualified-path-before-hints-and-ambient",
			pathDirs: []string{filepath.Dir(first), filepath.Dir(second)},
			hints:    []string{filepath.Dir(fallback)},
			want:     first,
		},
		{
			name:     "qualified-path-order",
			pathDirs: []string{filepath.Dir(second), filepath.Dir(first)},
			hints:    []string{filepath.Dir(fallback)},
			want:     second,
		},
		{
			name:     "qualified-path-is-literal",
			pathDirs: []string{literalDirectory},
			hints:    []string{filepath.Dir(fallback)},
			want:     literal,
		},
		{
			name:     "hints-after-qualified-path-miss",
			pathDirs: []string{empty},
			hints:    []string{empty, filepath.Dir(fallback)},
			want:     fallback,
		},
		{
			name:     "missing-does-not-search-ambient-path",
			pathDirs: []string{empty},
			hints:    []string{empty},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			execution := SerenaPreparationEnvironment{values: []string{"PATH=" + strings.Join(tc.pathDirs, string(os.PathListSeparator))}}
			got, err := discoverPreparedSerenaUV(context.Background(), execution, tc.hints)
			if tc.want == "" {
				if err == nil || got != "" || !errors.Is(err, exec.ErrNotFound) || !strings.Contains(err.Error(), "SERENA_RUNTIME_INSTALL_NEEDED") {
					t.Fatalf("missing UV: got path %q, error %v", got, err)
				}
				return
			}
			if err != nil || got != tc.want {
				t.Fatalf("UV selection: got path %q, error %v; want %q", got, err, tc.want)
			}
		})
	}

	t.Run("cancelled-before-selection", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		execution := SerenaPreparationEnvironment{values: []string{"PATH=" + filepath.Dir(first)}}
		got, err := discoverPreparedSerenaUV(ctx, execution, []string{filepath.Dir(fallback)})
		if got != "" || !errors.Is(err, context.Canceled) {
			t.Fatalf("cancelled UV discovery: got path %q, error %v", got, err)
		}
	})
}

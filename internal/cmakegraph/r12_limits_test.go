package cmakegraph

import (
	"context"
	"errors"
	"io/fs"
	"path/filepath"
	"strings"
	"testing"
)

func TestOptionsRejectEveryHardMaximumPlusOneBeforeWork(t *testing.T) {
	tests := []struct {
		name string
		max  int64
		set  func(*Options, int64)
	}{
		{"depth", int64(MaxDepthLimit), func(o *Options, v int64) { o.MaxDepth = int(v) }},
		{"nodes", int64(MaxNodesLimit), func(o *Options, v int64) { o.MaxNodes = int(v) }},
		{"file-bytes", MaxFileBytesLimit, func(o *Options, v int64) { o.MaxFileBytes = v }},
		{"roots", int64(MaxRootsLimit), func(o *Options, v int64) { o.MaxRoots = int(v) }},
		{"visited-entries", int64(MaxVisitedEntriesLimit), func(o *Options, v int64) { o.MaxVisitedEntries = int(v) }},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			accepted := Options{}
			tc.set(&accepted, tc.max)
			if _, err := accepted.normalized(); err != nil {
				t.Fatalf("maximum rejected: %v", err)
			}

			rejected := Options{}
			tc.set(&rejected, tc.max+1)
			if _, err := Walk(context.Background(), filepath.Join(t.TempDir(), "must-not-open.cmake"), t.TempDir(), rejected); !errors.Is(err, ErrInvalidOptions) {
				t.Fatalf("Walk err=%v, want ErrInvalidOptions before path work", err)
			}
			walkDirCalls := 0
			_, err := walkTreeWithOperations(context.Background(), t.TempDir(), "", []string{"CMakeLists.txt"}, rejected, treeOperations{
				walkDir:            func(string, fs.WalkDirFunc) error { walkDirCalls++; return nil },
				isDirectorySymlink: func(string) bool { return false },
				walkRoot:           func(*walker, string) (string, error) { t.Fatal("walkRoot called"); return "", nil },
			})
			if !errors.Is(err, ErrInvalidOptions) || walkDirCalls != 0 {
				t.Fatalf("WalkTree err=%v walkDirCalls=%d, want ErrInvalidOptions and zero work", err, walkDirCalls)
			}
		})
	}
}

func TestEntryFiltersRejectCountAndByteLimitsBeforeWalkDir(t *testing.T) {
	tests := []struct {
		name     string
		patterns []string
		wantErr  bool
	}{
		{"count-boundary", make([]string, MaxEntryFilters), false},
		{"count-plus-one", make([]string, MaxEntryFilters+1), true},
		{"byte-boundary", []string{strings.Repeat("a", MaxEntryFilterBytes)}, false},
		{"byte-plus-one", []string{strings.Repeat("a", MaxEntryFilterBytes+1)}, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			for i := range tc.patterns {
				if tc.patterns[i] == "" {
					tc.patterns[i] = "x"
				}
			}
			walkDirCalls := 0
			_, err := walkTreeWithOperations(context.Background(), t.TempDir(), "", tc.patterns, Options{}, treeOperations{
				walkDir:            func(string, fs.WalkDirFunc) error { walkDirCalls++; return nil },
				isDirectorySymlink: func(string) bool { return false },
				walkRoot:           func(*walker, string) (string, error) { t.Fatal("walkRoot called"); return "", nil },
			})
			if tc.wantErr {
				if !errors.Is(err, ErrInvalidOptions) || walkDirCalls != 0 {
					t.Fatalf("err=%v walkDirCalls=%d, want pre-walk rejection", err, walkDirCalls)
				}
				return
			}
			if err != nil || walkDirCalls != 1 {
				t.Fatalf("err=%v walkDirCalls=%d, want accepted boundary", err, walkDirCalls)
			}
		})
	}
}

func TestCompiledEntryFiltersPreserveExactAndSuffixSemantics(t *testing.T) {
	filters, err := compileEntryFilters([]string{"CMakeLists.txt", "*.CmAkE", "literal.*"})
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name string
		want bool
	}{
		{"CMakeLists.txt", true},
		{"portfile.cmake", true},
		{"nested.name.CMAKE", true},
		{"literal.*", true},
		{"literal.txt", false},
		{"cmakelists.txt.bak", false},
		{"file.cmake.bak", false},
	} {
		if got := filters.matches(tc.name); got != tc.want {
			t.Errorf("matches(%q)=%v, want %v", tc.name, got, tc.want)
		}
	}
}

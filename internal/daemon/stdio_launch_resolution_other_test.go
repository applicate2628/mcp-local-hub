//go:build !windows

package daemon

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"
)

func TestPreparedStdioPreservesOriginalArgvZero(t *testing.T) {
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	directory := filepath.Dir(executable)
	originalDirectory, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(directory); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.Chdir(originalDirectory); err != nil {
			t.Error(err)
		}
	})
	t.Setenv("PATH", directory)
	for _, command := range []string{filepath.Base(executable), "./" + filepath.Base(executable)} {
		t.Run(command, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			snapshot, err := CaptureStdioLaunchContext(ctx)
			if err != nil {
				t.Fatal(err)
			}
			args := []string{"-test.run=^$", "synthetic-argument"}
			h, err := NewPreparedStdioHost(ctx, StdioLaunchInputs{Context: snapshot, Config: HostConfig{Command: command, Args: args}})
			if err != nil {
				t.Fatal(err)
			}
			p, err := h.PreparedLaunch()
			if err != nil {
				t.Fatal(err)
			}
			if p.command.Path != executable || p.image.path != executable || p.command.Args[0] != command || !slices.Equal(p.command.Args[1:], args) {
				t.Fatal("prepared image or original argv changed meaning")
			}
			if p.command.Process != nil || h.original != nil {
				t.Fatal("argv comparison created an actor")
			}
		})
	}
}

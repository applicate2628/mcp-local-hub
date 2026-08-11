//go:build windows

package process

import (
	"context"
	"io"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/windows"
)

func TestR35NoConsole(t *testing.T) {
	flags := containedWindowsCreationFlags()
	if flags&uint32(windows.CREATE_NO_WINDOW) == 0 {
		t.Fatalf("creation flags=%#x, missing CREATE_NO_WINDOW", flags)
	}

	cmd := containedWindowsHelperCommand(t, "probe")
	var stdout strings.Builder
	var stderr strings.Builder
	err := RunContainedStream(
		context.Background(),
		cmd,
		ContainedStreamOptions{CleanupTimeout: 5 * time.Second, Stderr: &stderr},
		func(reader io.Reader) error {
			_, copyErr := io.Copy(&stdout, reader)
			return copyErr
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	if got := stdout.String(); !strings.Contains(got, "job=1") {
		t.Fatalf("stdout=%q, want contained child probe", got)
	}
	if got := stderr.String(); !strings.Contains(got, "bounded diagnostic") {
		t.Fatalf("stderr=%q, want redirected diagnostic", got)
	}
}

package perftools

import (
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const versionProbeHelperSelector = "-test.run=^TestVersionProbeHelper$"

// This helper only runs the current test image. It launches no descendants,
// uses no shell, and exits without the testing framework's PASS output.
func TestVersionProbeHelper(t *testing.T) {
	if len(os.Args) < 4 || os.Args[1] != versionProbeHelperSelector || os.Args[2] != "--" {
		return
	}
	mode := os.Args[3]
	if (mode == "wait" && len(os.Args) != 5) || (mode != "wait" && len(os.Args) != 4) {
		os.Exit(2)
	}
	write := func(w io.Writer, s string) {
		if _, err := io.WriteString(w, s); err != nil {
			os.Exit(3)
		}
	}
	switch mode {
	case "stdout-first":
		write(os.Stdout, "\nstdout 1.2\n")
		write(os.Stderr, "stderr 2.3\n")
	case "stderr-first":
		write(os.Stderr, "\nstderr 2.3\n")
		write(os.Stdout, "stdout 1.2\n")
	case "exact-limit":
		write(os.Stdout, strings.Repeat("x", versionProbeOutputLimit))
	case "stdout-overflow":
		write(os.Stdout, strings.Repeat("x", versionProbeOutputLimit+1))
	case "stderr-overflow":
		write(os.Stderr, strings.Repeat("x", versionProbeOutputLimit+1))
	case "combined-overflow":
		write(os.Stdout, strings.Repeat("x", versionProbeOutputLimit/2))
		write(os.Stderr, strings.Repeat("y", versionProbeOutputLimit/2+1))
	case "exit-error":
		write(os.Stdout, "plausible 1.2\n")
		os.Exit(7)
	case "wait":
		if err := os.WriteFile(os.Args[4], []byte("ready"), 0600); err != nil {
			os.Exit(4)
		}
		// A finite fallback prevents an orphan if a future regression ignores
		// cancellation. The parent requires cancellation well before this.
		time.Sleep(20 * time.Second)
		os.Exit(5)
	default:
		os.Exit(2)
	}
	os.Exit(0)
}

func TestVersionProbeOutput(t *testing.T) {
	image, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		mode      string
		line      string
		wantBytes int
		overflow  bool
		exitCode  int
	}{
		{mode: "stdout-first", line: "stdout 1.2", wantBytes: 23},
		{mode: "stderr-first", line: "stderr 2.3", wantBytes: 23},
		{mode: "exact-limit", wantBytes: versionProbeOutputLimit},
		{mode: "stdout-overflow", overflow: true},
		{mode: "stderr-overflow", overflow: true},
		{mode: "combined-overflow", overflow: true},
		{mode: "exit-error", exitCode: 7},
	} {
		t.Run(tc.mode, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), versionProbeTimeout)
			defer cancel()
			out, err := runVersionProbe(ctx, image, []string{versionProbeHelperSelector, "--", tc.mode})
			if tc.overflow {
				if !errors.Is(err, errOutputLimitExceeded) || out != nil {
					t.Fatalf("overflow accepted: bytes=%d err=%v", len(out), err)
				}
				return
			}
			if tc.exitCode != 0 {
				var exitErr *exec.ExitError
				if !errors.As(err, &exitErr) || exitErr.ExitCode() != tc.exitCode || out != nil {
					t.Fatalf("failed version accepted: bytes=%d err=%v", len(out), err)
				}
				return
			}
			if err != nil || len(out) != tc.wantBytes {
				t.Fatalf("output bytes=%d want=%d err=%v", len(out), tc.wantBytes, err)
			}
			if tc.line != "" && firstLine(string(out)) != tc.line {
				t.Fatalf("combined stream order changed: %q", out)
			}
		})
	}
}

func TestVersionProbeCancellation(t *testing.T) {
	image, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	for _, mode := range []string{"before-start", "after-ready", "deadline"} {
		t.Run(mode, func(t *testing.T) {
			ready := filepath.Join(t.TempDir(), "ready")
			timeout := 10 * time.Second
			if mode == "deadline" {
				timeout = 3 * time.Second
			}
			ctx, cancel := context.WithTimeout(t.Context(), timeout)
			defer cancel()
			returnBy, _ := ctx.Deadline()
			if mode == "before-start" {
				returnBy = time.Now()
				cancel()
			}
			type result struct {
				out      []byte
				err      error
				finished time.Time
			}
			done := make(chan result, 1)
			go func() {
				out, err := runVersionProbe(ctx, image, []string{versionProbeHelperSelector, "--", "wait", ready})
				done <- result{out: out, err: err, finished: time.Now()}
			}()
			if mode != "before-start" {
				for {
					if data, err := os.ReadFile(ready); err == nil && string(data) == "ready" {
						break
					}
					select {
					case got := <-done:
						t.Fatalf("helper did not reach ready: %v", got.err)
					case <-time.After(10 * time.Millisecond):
					}
				}
				if mode == "after-ready" {
					returnBy = time.Now()
					cancel()
				}
			}
			got := <-done // runVersionProbe owns the original command and its only Wait.
			if got.out != nil {
				t.Fatalf("canceled probe returned output: %q", got.out)
			}
			var exitErr *exec.ExitError
			if errors.As(got.err, &exitErr) && exitErr.ExitCode() == 5 {
				t.Fatal("helper reached its fallback exit instead of being canceled")
			}
			if got.finished.After(returnBy.Add(versionProbeWaitDelay + 2*time.Second)) {
				t.Fatalf("probe returned too late after cancellation: %s", got.finished.Sub(returnBy))
			}
			want := error(context.Canceled)
			if mode == "deadline" {
				want = context.DeadlineExceeded
			}
			if !errors.Is(got.err, want) {
				t.Fatalf("cancellation cause lost: got %v, want %v", got.err, want)
			}
			if mode == "before-start" {
				if _, err := os.Stat(ready); !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("pre-canceled helper ran: %v", err)
				}
			}
		})
	}
}

func TestProbeMissingPath(t *testing.T) {
	info := probe(filepath.Join(t.TempDir(), "missing-version-tool"), firstLine)
	if info.Installed || info.Path != "" || info.Version != "" || info.Error == "" {
		t.Fatalf("missing executable reported as available: %+v", info)
	}
}

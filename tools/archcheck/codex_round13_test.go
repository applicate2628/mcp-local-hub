package main

import (
	"errors"
	"strings"
	"testing"
)

type failingOutputWriter struct {
	short bool
}

func (w failingOutputWriter) Write(p []byte) (int, error) {
	if w.short {
		if len(p) == 0 {
			return 0, nil
		}
		return len(p) - 1, nil
	}
	return 0, errors.New("output failed")
}

func TestRunPropagatesStdoutWriteFailures(t *testing.T) {
	for _, tc := range []struct {
		name   string
		writer failingOutputWriter
	}{
		{name: "writer error"},
		{name: "short write", writer: failingOutputWriter{short: true}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root, policy := fixture(t)
			var stderr strings.Builder
			code := run([]string{"scan", "--root", root, "--policy", policy}, tc.writer, &stderr)
			if code != 2 {
				t.Fatalf("code=%d stderr=%s, want output failure", code, stderr.String())
			}
			if !strings.Contains(stderr.String(), "write stdout") {
				t.Fatalf("stderr=%q, want stdout write diagnostic", stderr.String())
			}
		})
	}
}

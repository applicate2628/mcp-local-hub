package api

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLogsGetTailsFromFile(t *testing.T) {
	tmp := t.TempDir()
	logPath := filepath.Join(tmp, "memory-default.log")
	body := ""
	for i := 0; i < 100; i++ {
		body += "line" + string(rune('0'+i%10)) + "\n"
	}
	if err := os.WriteFile(logPath, []byte(body), 0600); err != nil {
		t.Fatal(err)
	}
	a := NewAPI()
	content, err := a.LogsGetFrom(LogsOpts{
		LogDir: tmp,
		Server: "memory",
		Daemon: "default",
		Tail:   5,
	})
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(content), "\n")
	if len(lines) != 5 {
		t.Errorf("expected 5 tailed lines, got %d", len(lines))
	}
}

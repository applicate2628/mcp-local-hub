//go:build windows

package scheduler

import (
	"crypto/rand"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"
)

// TestSchedulerCOMTargetLifecycle is deliberately opt-in: it creates a
// uniquely named task on the executing Windows host, then proves the COM
// bridge's typed lifecycle and removes the task on every test exit path.
// The random suffix is only a collision-avoidance token; test assertions do
// not depend on its value.
func TestSchedulerCOMTargetLifecycle(t *testing.T) {
	if os.Getenv("MCPHUB_TARGET_SCHEDULER_QA") != "1" {
		t.Skip("set MCPHUB_TARGET_SCHEDULER_QA=1 to run the target Scheduler COM smoke")
	}

	var suffix [8]byte
	if _, err := rand.Read(suffix[:]); err != nil {
		t.Fatalf("random task suffix: %v", err)
	}
	name := fmt.Sprintf(`\mcp-local-hub-qa-scheduler-%d-%x`, time.Now().UTC().UnixNano(), suffix)
	prefix := `mcp-local-hub-qa-scheduler-`

	s, err := New()
	if err != nil {
		t.Fatalf("scheduler.New: %v", err)
	}
	cleanup := func() {
		t.Helper()
		if err := s.Stop(name); err != nil {
			t.Errorf("cleanup stop %q: %v", name, err)
		}
		if err := s.Delete(name); err != nil {
			t.Errorf("cleanup delete %q: %v", name, err)
		}
		if _, err := s.Status(name); !errors.Is(err, ErrTaskNotFound) {
			t.Errorf("cleanup status %q err=%v, want ErrTaskNotFound", name, err)
		}
	}
	t.Cleanup(cleanup)

	if _, err := s.Status(name); !errors.Is(err, ErrTaskNotFound) {
		t.Fatalf("status absent err=%v, want ErrTaskNotFound", err)
	}
	if _, err := s.ExportXML(name); !errors.Is(err, ErrTaskNotFound) {
		t.Fatalf("export absent err=%v, want ErrTaskNotFound", err)
	}
	if err := s.Delete(name); err != nil {
		t.Fatalf("delete absent: %v", err)
	}
	if err := s.Delete(name); err != nil {
		t.Fatalf("delete absent twice: %v", err)
	}

	command := os.Getenv("SystemRoot") + `\System32\WindowsPowerShell\v1.0\powershell.exe`
	if _, err := os.Stat(command); err != nil {
		t.Fatalf("target command %q: %v", command, err)
	}
	if err := s.Create(TaskSpec{
		Name:         name,
		Description:  "QA-only Scheduler COM target smoke",
		Command:      command,
		Args:         []string{"-NoProfile", "-NonInteractive", "-Command", "Start-Sleep -Seconds 30 # " + name},
		LogonTrigger: true,
	}); err != nil {
		t.Fatalf("create %q: %v", name, err)
	}

	st, err := s.Status(name)
	if err != nil {
		t.Fatalf("status created: %v", err)
	}
	if st.Name != name || st.RuntimeState == TaskRuntimeUnknown || st.RuntimeState == TaskRuntimeRunning {
		t.Fatalf("status created=%#v, want exact name and stopped numeric state", st)
	}
	xml, err := s.ExportXML(name)
	if err != nil || len(xml) == 0 || !strings.Contains(string(xml), "QA-only Scheduler COM target smoke") {
		t.Fatalf("export err=%v bytes=%d contains marker=%t", err, len(xml), strings.Contains(string(xml), "QA-only Scheduler COM target smoke"))
	}
	listed, err := s.List(prefix)
	if err != nil {
		t.Fatalf("list %q: %v", prefix, err)
	}
	found := false
	for _, item := range listed {
		if item.Name == name {
			found = item.RuntimeState != TaskRuntimeUnknown
		}
	}
	if !found {
		t.Fatalf("list %q omitted %q with a numeric state: %#v", prefix, name, listed)
	}
	if err := s.Stop(name); err != nil {
		t.Fatalf("stop stopped task: %v", err)
	}
	if err := s.Stop(name); err != nil {
		t.Fatalf("stop stopped task twice: %v", err)
	}

	if err := s.Run(name); err != nil {
		t.Fatalf("run: %v", err)
	}
	deadline := time.Now().Add(10 * time.Second)
	for {
		st, err = s.Status(name)
		if err != nil {
			t.Fatalf("status after run: %v", err)
		}
		if st.RuntimeState == TaskRuntimeRunning {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("status after run=%#v, never observed TaskRuntimeRunning", st)
		}
		time.Sleep(100 * time.Millisecond)
	}
	if err := s.Stop(name); err != nil {
		t.Fatalf("stop running task: %v", err)
	}
	if err := s.Stop(name); err != nil {
		t.Fatalf("stop running task twice: %v", err)
	}
	if err := s.Delete(name); err != nil {
		t.Fatalf("delete created task: %v", err)
	}
	if err := s.Delete(name); err != nil {
		t.Fatalf("delete created task twice: %v", err)
	}
}

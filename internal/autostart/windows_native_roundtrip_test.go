//go:build windows

package autostart

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"mcp-local-hub/internal/scheduler"
)

// This opt-in shares the existing Scheduler QA prefix and factory seam. Only
// registration, native export, Ready, and repeat Enable are qualified: the
// command deliberately does not exist, and the fixture never requests Run.
func TestWindowsBackend_EnableNativeRegistrationRoundTrip(t *testing.T) {
	if os.Getenv("MCPHUB_TARGET_SCHEDULER_QA") != "1" {
		t.Skip("set MCPHUB_TARGET_SCHEDULER_QA=1 for the native autostart registration round trip")
	}
	if os.Getenv("MCPHUB_E2E_SCHEDULER") == "none" {
		t.Fatal("native autostart qualification cannot use the noop Scheduler")
	}

	// The native command owner sets TEMP/TMP to its isolated fixture root.
	command := filepath.Join(t.TempDir(), "absent-mcphub.exe")
	if !filepath.IsAbs(command) {
		t.Fatal("native fixture requires an absolute temporary directory")
	}
	if _, err := os.Lstat(command); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("fixture command must not exist: %v", err)
	}
	var suffix [8]byte
	if _, err := rand.Read(suffix[:]); err != nil {
		t.Fatalf("random task suffix: %v", err)
	}
	name := fmt.Sprintf(`\mcp-local-hub-qa-scheduler-%d-%x`, time.Now().UTC().UnixNano(), suffix)
	native, err := scheduler.New()
	if err != nil {
		t.Fatalf("scheduler.New: %v", err)
	}
	fixture := &nativeAutostartScheduler{fakeScheduler: &fakeScheduler{}, native: native, name: name}
	previous := schedulerFactoryFn
	schedulerFactoryFn = func() (scheduler.Scheduler, error) { return fixture, nil }
	t.Cleanup(func() { schedulerFactoryFn = previous })
	t.Cleanup(func() {
		// Randomness avoids collisions; it grants no generation authority.
		// This explicit QA-name cleanup follows only a successful own Create.
		if !fixture.created {
			return
		}
		if err := native.Delete(name); err != nil {
			t.Errorf("cleanup delete %q: %v", name, err)
		}
		if raw, err := native.ExportXML(name); len(raw) != 0 || !errors.Is(err, scheduler.ErrTaskNotFound) {
			t.Errorf("cleanup export %q: bytes=%d err=%v, want typed absence", name, len(raw), err)
		} else {
			t.Log("native fixture cleanup confirmed typed absence")
		}
	})
	t.Logf("native fixture task: %q", name)

	opts := Options{MCPHubPath: command}
	backend := &windowsBackend{}
	if err := backend.Enable(opts); err != nil {
		t.Fatalf("fresh Enable: %v", err)
	}
	if !fixture.created || len(fixture.createCalls) != 1 {
		t.Fatal("fresh Enable did not create exactly one native fixture registration")
	}
	before, err := native.ExportXML(name)
	if err != nil || len(before) == 0 {
		t.Fatalf("export created task: bytes=%d err=%v", len(before), err)
	}
	status, err := native.Status(name)
	if err != nil || status.Name != name || status.RuntimeState != scheduler.TaskRuntimeReady {
		t.Fatalf("created task state=%v name=%q err=%v, want exact fixture in Ready", status.RuntimeState, status.Name, err)
	}
	t.Logf("native export bytes=%d sha256=%x; state=Ready", len(before), sha256.Sum256(before))
	for range 2 {
		if err := backend.Enable(opts); err != nil {
			t.Fatalf("repeat Enable: %v", err)
		}
	}
	if len(fixture.createCalls) != 1 || len(fixture.deleteCalls) != 0 || len(fixture.exportCalls) != 3 || len(fixture.statusCalls) != 2 {
		t.Fatalf("repeat Enable mutated or skipped native observations: create=%d delete=%d export=%d status=%d", len(fixture.createCalls), len(fixture.deleteCalls), len(fixture.exportCalls), len(fixture.statusCalls))
	}
	after, err := native.ExportXML(name)
	if err != nil || !bytes.Equal(before, after) {
		t.Fatalf("repeat Enable changed the native definition: before=%x after=%x err=%v", sha256.Sum256(before), sha256.Sum256(after), err)
	}
}

// Reuse the existing recording fake for forbidden operations: Delete is
// counted, while Run/Stop/ImportXML/List panic. Only these three overrides
// reach the real Scheduler, and none forwards the product task name.
type nativeAutostartScheduler struct {
	*fakeScheduler
	native  scheduler.Scheduler
	name    string
	created bool
}

func (s *nativeAutostartScheduler) Create(spec scheduler.TaskSpec) error {
	s.createCalls = append(s.createCalls, spec)
	if spec.Name != WindowsTaskName || len(s.createCalls) != 1 {
		return errors.New("native fixture refuses unexpected or repeated Create")
	}
	spec.Name = s.name
	if err := s.native.Create(spec); err != nil {
		return fmt.Errorf("fixture Create %q failed; any observed registration is retained: %w", s.name, err)
	}
	s.created = true
	return nil
}

func (s *nativeAutostartScheduler) ExportXML(name string) ([]byte, error) {
	s.exportCalls = append(s.exportCalls, name)
	if name != WindowsTaskName {
		return nil, errors.New("native fixture refuses unexpected ExportXML name")
	}
	return s.native.ExportXML(s.name)
}

func (s *nativeAutostartScheduler) Status(name string) (scheduler.TaskStatus, error) {
	s.statusCalls = append(s.statusCalls, name)
	if name != WindowsTaskName {
		return scheduler.TaskStatus{}, errors.New("native fixture refuses unexpected Status name")
	}
	return s.native.Status(s.name)
}

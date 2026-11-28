//go:build windows

package api

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

// This separate opt-in registers a temporary QA name with the production XML.
// Running it requires authorization for that registration and its QA cleanup.
// The native command owner must direct TEMP/TMP to the approved test root.
// The action deliberately does not exist; the fixture never requests Run.
func TestEnsureLivenessTaskResolved_NativeXMLRoundTrip(t *testing.T) {
	if os.Getenv("MCPHUB_TARGET_LIVENESS_QA") != "1" {
		t.Skip("set MCPHUB_TARGET_LIVENESS_QA=1 for the authorized native liveness round trip")
	}
	if os.Getenv("MCPHUB_E2E_SCHEDULER") == "none" {
		t.Fatal("native liveness qualification cannot use the noop Scheduler")
	}

	command := filepath.Join(t.TempDir(), "absent-mcphub.exe")
	if !filepath.IsAbs(command) {
		t.Fatal("native fixture requires an absolute temporary directory")
	}
	if _, err := os.Lstat(command); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("fixture command must not exist: %v", err)
	}
	sid, err := livenessPrincipalSID()
	if err != nil {
		t.Fatalf("current principal SID: %v", err)
	}
	account, err := defaultCurrentWindowsUser()
	if err != nil {
		t.Fatalf("current logon account: %v", err)
	}
	var suffix [16]byte
	if _, err := rand.Read(suffix[:]); err != nil {
		t.Fatalf("random task suffix: %v", err)
	}
	name := fmt.Sprintf(`\mcp-local-hub-qa-scheduler-liveness-%d-%x`, time.Now().UTC().UnixNano(), suffix)
	native, err := scheduler.New()
	if err != nil {
		t.Fatalf("scheduler.New: %v", err)
	}
	creator, ok := native.(scheduler.XMLCreator)
	if !ok {
		t.Fatal("native Scheduler has no XMLCreator")
	}
	fixture := &nativeLivenessScheduler{native: native, creator: creator, name: name}
	t.Cleanup(func() {
		// Cleanup is the separately authorized QA-name operation, not product
		// rollback. Randomness and XML equality confer no generation ownership.
		if !fixture.created {
			return
		}
		current, err := native.ExportXML(name)
		if errors.Is(err, scheduler.ErrTaskNotFound) && len(current) == 0 {
			t.Log("native liveness fixture cleanup observed typed absence")
			return
		}
		if err != nil || len(fixture.firstReadback) == 0 || !bytes.Equal(current, fixture.firstReadback) {
			t.Errorf("retain QA task %q: cleanup definition is unconfirmed or changed (read error: %v)", name, err)
			return
		}
		if err := native.Delete(name); err != nil {
			t.Errorf("cleanup delete QA task %q: %v", name, err)
		}
		if raw, err := native.ExportXML(name); len(raw) != 0 || !errors.Is(err, scheduler.ErrTaskNotFound) {
			t.Errorf("cleanup export QA task %q: bytes=%d err=%v, want typed absence", name, len(raw), err)
		} else {
			t.Log("native liveness fixture cleanup confirmed typed absence")
		}
	})
	t.Logf("native liveness fixture task: %q", name)

	workingDir := livenessWorkingDir(command)
	receipt, err := ensureLivenessTaskResolved(command, workingDir, sid, account, fixture)
	if err != nil || receipt.Result != LivenessTaskCreated {
		t.Fatalf("fresh Ensure: result=%q err=%v, want created-from-absent", receipt.Result, err)
	}
	if !fixture.created || fixture.createCalls != 1 || fixture.exportCalls != 2 || len(fixture.firstReadback) == 0 {
		t.Fatalf("fresh Ensure: created=%t create=%d export=%d readback=%d", fixture.created, fixture.createCalls, fixture.exportCalls, len(fixture.firstReadback))
	}
	status, err := native.Status(name)
	if err != nil || status.Name != name || status.RuntimeState != scheduler.TaskRuntimeReady {
		t.Fatalf("created task state=%v name=%q err=%v, want exact QA task in Ready", status.RuntimeState, status.Name, err)
	}
	receipt, err = ensureLivenessTaskResolved(command, workingDir, sid, account, fixture)
	if err != nil || receipt.Result != LivenessTaskUnchanged {
		t.Fatalf("repeat Ensure: result=%q err=%v, want unchanged", receipt.Result, err)
	}
	if fixture.createCalls != 1 || fixture.exportCalls != 3 || !bytes.Equal(fixture.firstReadback, fixture.lastReadback) {
		t.Fatalf("repeat Ensure changed registration: create=%d export=%d before=%x after=%x", fixture.createCalls, fixture.exportCalls, sha256.Sum256(fixture.firstReadback), sha256.Sum256(fixture.lastReadback))
	}
	actual, ok := parseLivenessTaskXML(fixture.lastReadback)
	if !ok || len(actual.CalendarTrigger) != 1 || len(actual.LogonTrigger) != 1 {
		t.Fatal("successful Ensure did not yield one calendar and one logon trigger")
	}
	calendar := actual.CalendarTrigger[0]
	t.Logf("native liveness export bytes=%d sha256=%x; state=Ready; create=1; export=3", len(fixture.lastReadback), sha256.Sum256(fixture.lastReadback))
	t.Logf("native calendar StartBoundary=%q Enabled=%q interval=%q days=%q; logon Enabled=%q; settings Enabled=%q", calendar.StartBoundary, calendar.Enabled, calendar.Repetition.Interval, calendar.ScheduleByDay.DaysInterval, actual.LogonTrigger[0].Enabled, actual.Settings.Enabled)
}

// The nil embedded Scheduler makes any unexpected production operation panic.
// Only these two overrides can reach Windows, always with the private QA name.
// No global factory or resolver is replaced, and the XML is forwarded unchanged.
type nativeLivenessScheduler struct {
	scheduler.Scheduler
	native        scheduler.Scheduler
	creator       scheduler.XMLCreator
	name          string
	created       bool
	createCalls   int
	exportCalls   int
	firstReadback []byte
	lastReadback  []byte
}

func (s *nativeLivenessScheduler) CreateXMLIfAbsent(name string, rawXML []byte) error {
	s.createCalls++
	if name != LivenessTaskName || s.createCalls != 1 {
		return errors.New("native liveness fixture refuses unexpected or repeated CreateXMLIfAbsent")
	}
	if err := s.creator.CreateXMLIfAbsent(s.name, rawXML); err != nil {
		return fmt.Errorf("fixture CreateXMLIfAbsent %q failed; any observed registration is retained: %w", s.name, err)
	}
	s.created = true
	return nil
}

func (s *nativeLivenessScheduler) ExportXML(name string) ([]byte, error) {
	s.exportCalls++
	if name != LivenessTaskName {
		return nil, errors.New("native liveness fixture refuses unexpected ExportXML name")
	}
	raw, err := s.native.ExportXML(s.name)
	if s.created && err == nil && len(raw) != 0 {
		if s.firstReadback == nil {
			s.firstReadback = bytes.Clone(raw)
		}
		s.lastReadback = bytes.Clone(raw)
	}
	return raw, err
}

var _ scheduler.XMLCreator = (*nativeLivenessScheduler)(nil)

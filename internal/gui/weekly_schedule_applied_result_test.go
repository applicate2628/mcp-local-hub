package gui

import (
	"bytes"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"mcp-local-hub/internal/api"
	"mcp-local-hub/internal/api/apitest"
	"mcp-local-hub/internal/scheduler"
)

// This fake owns scheduler storage only. Settings, locking, post-create
// verification, rollback, result classification, and HTTP projection remain real.
type weeklyScheduleRouteScheduler struct {
	currentXML    []byte
	createErr     error
	writeThenFail bool
	created       scheduler.TaskSpec
	createCalls   int
	deleteCalls   int
	restoreCalls  int
}

func (s *weeklyScheduleRouteScheduler) Create(spec scheduler.TaskSpec) error {
	s.createCalls++
	s.created = spec
	if s.createErr != nil && !s.writeThenFail {
		return s.createErr
	}
	if s.currentXML != nil {
		return scheduler.ErrTaskAlreadyExists
	}
	s.currentXML = weeklyScheduleRouteTaskXML(spec)
	return s.createErr
}

func (s *weeklyScheduleRouteScheduler) Delete(string) error {
	s.deleteCalls++
	s.currentXML = nil
	return nil
}

func (s *weeklyScheduleRouteScheduler) ExportXML(string) ([]byte, error) {
	if s.currentXML == nil {
		return nil, scheduler.ErrTaskNotFound
	}
	return append([]byte(nil), s.currentXML...), nil
}

func (s *weeklyScheduleRouteScheduler) CreateXMLIfAbsent(_ string, raw []byte) error {
	s.restoreCalls++
	if s.currentXML != nil {
		return scheduler.ErrTaskAlreadyExists
	}
	s.currentXML = append([]byte(nil), raw...)
	return nil
}

func (*weeklyScheduleRouteScheduler) ImportXML(string, []byte) error {
	return errors.New("unexpected replacing XML import")
}

func (*weeklyScheduleRouteScheduler) Run(string) error {
	return errors.New("unexpected task execution")
}

func weeklyScheduleRouteTaskXML(spec scheduler.TaskSpec) []byte {
	if spec.WeeklyTrigger == nil {
		return []byte("<Task/>")
	}
	escape := func(value string) string {
		var out bytes.Buffer
		_ = xml.EscapeText(&out, []byte(value))
		return out.String()
	}
	days := []string{"Sunday", "Monday", "Tuesday", "Wednesday", "Thursday", "Friday", "Saturday"}
	return []byte(fmt.Sprintf(`<Task>
<RegistrationInfo><Description>%s</Description></RegistrationInfo>
<Triggers><CalendarTrigger><StartBoundary>2026-01-04T%02d:%02d:00</StartBoundary><Enabled>true</Enabled><ScheduleByWeek><DaysOfWeek><%s/></DaysOfWeek><WeeksInterval>1</WeeksInterval></ScheduleByWeek></CalendarTrigger></Triggers>
<Principals><Principal id="Author"><LogonType>InteractiveToken</LogonType><RunLevel>LeastPrivilege</RunLevel></Principal></Principals>
<Settings><AllowHardTerminate>true</AllowHardTerminate><StartWhenAvailable>false</StartWhenAvailable><RunOnlyIfNetworkAvailable>false</RunOnlyIfNetworkAvailable><MultipleInstancesPolicy>StopExisting</MultipleInstancesPolicy><DisallowStartIfOnBatteries>false</DisallowStartIfOnBatteries><StopIfGoingOnBatteries>false</StopIfGoingOnBatteries><IdleSettings><StopOnIdleEnd>false</StopOnIdleEnd><RestartOnIdle>false</RestartOnIdle></IdleSettings><AllowStartOnDemand>true</AllowStartOnDemand><Enabled>true</Enabled><Hidden>false</Hidden><RunOnlyIfIdle>false</RunOnlyIfIdle><WakeToRun>false</WakeToRun><ExecutionTimeLimit>PT0S</ExecutionTimeLimit><Priority>7</Priority></Settings>
<Actions Context="Author"><Exec><Command>%s</Command><Arguments>%s</Arguments><WorkingDirectory>%s</WorkingDirectory></Exec></Actions>
</Task>`, escape(spec.Description), spec.WeeklyTrigger.HourLocal, spec.WeeklyTrigger.MinuteLocal,
		days[spec.WeeklyTrigger.DayOfWeek], escape(spec.Command), escape(strings.Join(spec.Args, " ")), escape(spec.WorkingDir)))
}

func TestWeeklyScheduleHandler_RealApplyReleaseSettlement(t *testing.T) {
	for _, tc := range []struct {
		name            string
		weeklyRelease   bool
		settingsRelease bool
		failCreate      bool
		writeThenFail   bool
		priorTask       bool
		wantStatus      string
	}{
		{name: "committed-weekly-release", weeklyRelease: true, wantStatus: "n/a"},
		{name: "committed-settings-release", settingsRelease: true, wantStatus: "n/a"},
		{name: "committed-both-releases", weeklyRelease: true, settingsRelease: true, wantStatus: "n/a"},
		{name: "failed-create-weekly-release", weeklyRelease: true, failCreate: true, wantStatus: "n/a"},
		{name: "create-error-after-write-both-releases", weeklyRelease: true, settingsRelease: true, failCreate: true, writeThenFail: true, wantStatus: "degraded"},
		{name: "prior-restored-settings-release", settingsRelease: true, failCreate: true, priorTask: true, wantStatus: "ok"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			localData := t.TempDir()
			root := apitest.HardenedDir(t, filepath.Join(localData, "mcp-local-hub"))
			t.Setenv("LOCALAPPDATA", localData)
			t.Setenv("XDG_DATA_HOME", localData)
			t.Cleanup(api.SetDaemonStateRootForTest(root))
			canonical := filepath.Join(root, api.MCPHubBinaryName())
			t.Cleanup(api.SetTestCanonicalMcphubPath(canonical))
			sch := &weeklyScheduleRouteScheduler{writeThenFail: tc.writeThenFail}
			if tc.failCreate {
				sch.createErr = errors.New("private-create-failure")
			}
			t.Cleanup(api.InstallTestHooks(func() (api.TestSchedulerIface, error) { return sch, nil },
				func() map[string]api.TestClientIface { return nil }, filepath.Join(root, "workspaces.yaml")))
			priorXML := []byte("<Task><original-definition/></Task>")
			if tc.priorTask {
				sch.currentXML = append([]byte(nil), priorXML...)
			}
			originalSettings := []byte("# exact prior preferences\ndaemons.weekly_schedule: weekly Sun 03:00\nunknown-key: retained\n")
			if err := api.WriteStateFileBytesAtomic(api.SettingsPath(), originalSettings); err != nil {
				t.Fatal(err)
			}
			var weeklyCause, settingsCause error
			if tc.weeklyRelease {
				weeklyCause = errors.New("private-weekly-release-failure")
			}
			if tc.settingsRelease {
				settingsCause = errors.New("private-settings-release-failure")
			}
			api.SetWeeklyScheduleUnlockFailuresForTest(t, root, weeklyCause, settingsCause)

			// Use the registered production route and its default Apply owner.
			// The server remains in-process; no listener or broadcaster is started.
			srv := &Server{mux: http.NewServeMux()}
			srv.port.Store(9125)
			registerDaemonsRoutes(srv)
			req := httptest.NewRequest(http.MethodPut, "http://127.0.0.1:9125/api/daemons/weekly-schedule",
				strings.NewReader(`{"schedule":"  weekly tue 14:30  "}`))
			req.Header = sameOriginHeaders()
			rec := httptest.NewRecorder()
			srv.ServeHTTP(rec, req)
			wantHTTPStatus := http.StatusOK
			if tc.failCreate {
				wantHTTPStatus = http.StatusInternalServerError
			}
			if rec.Code != wantHTTPStatus {
				t.Fatalf("status = %d, want %d; body=%s", rec.Code, wantHTTPStatus, rec.Body.String())
			}
			var response map[string]any
			if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil {
				t.Fatal(err)
			}
			if response["updated"] != !tc.failCreate || response["restore_status"] != tc.wantStatus || len(response) != 5 {
				t.Fatalf("Apply response = %#v, want applied=%t restore=%q and five fields", response, !tc.failCreate, tc.wantStatus)
			}
			if tc.failCreate {
				if response["error"] != "scheduler_swap_failed" || response["detail"] != "internal error" || response["manual_recovery"] != weeklyScheduleReleaseUnconfirmedRecoveryHint {
					t.Fatalf("failed Apply response = %#v", response)
				}
			} else if response["schedule"] != "weekly Tue 14:30" || response["warning"] != "lock_release_unconfirmed" || response["manual_recovery"] != weeklyScheduleAppliedReleaseUnconfirmedHint {
				t.Fatalf("committed Apply response = %#v", response)
			}
			if strings.Contains(rec.Body.String(), "private-") {
				t.Fatalf("response leaked raw failure: %s", rec.Body.String())
			}

			wantSpec := scheduler.TaskSpec{
				Name: api.WeeklyRefreshTaskName, Description: "mcp-local-hub: weekly refresh of workspace-scoped lazy proxies",
				Command: canonical, Args: []string{"workspace-weekly-refresh"}, WorkingDir: root,
				WeeklyTrigger: &scheduler.WeeklyTrigger{DayOfWeek: 2, HourLocal: 14, MinuteLocal: 30},
			}
			if sch.createCalls != 1 || !reflect.DeepEqual(sch.created, wantSpec) {
				t.Fatalf("real Apply requested calls=%d spec=%+v, want one requested weekly task", sch.createCalls, sch.created)
			}
			var wantXML []byte
			if !tc.failCreate || tc.writeThenFail {
				wantXML = weeklyScheduleRouteTaskXML(wantSpec)
			} else if tc.priorTask {
				wantXML = priorXML
			}
			if !bytes.Equal(sch.currentXML, wantXML) {
				t.Fatalf("settled task = %s, want %s", sch.currentXML, wantXML)
			}
			wantDeletes, wantRestores := 0, 0
			if tc.priorTask {
				wantDeletes, wantRestores = 1, 1
			}
			if sch.deleteCalls != wantDeletes || sch.restoreCalls != wantRestores {
				t.Fatalf("task delete/restore calls = %d/%d, want %d/%d", sch.deleteCalls, sch.restoreCalls, wantDeletes, wantRestores)
			}
			persisted, err := os.ReadFile(api.SettingsPath())
			if err != nil {
				t.Fatal(err)
			}
			if tc.failCreate {
				if !bytes.Equal(persisted, originalSettings) {
					t.Fatalf("failed Apply did not restore exact preferences: %q", persisted)
				}
			} else if value, err := api.NewAPI().SettingsGet("daemons.weekly_schedule"); err != nil || value != "weekly Tue 14:30" {
				t.Fatalf("persisted schedule = %q, err=%v", value, err)
			}
		})
	}
}

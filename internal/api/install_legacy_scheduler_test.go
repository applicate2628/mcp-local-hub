package api

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"mcp-local-hub/internal/config"
	"mcp-local-hub/internal/scheduler"
)

type legacyCleanupScheduler struct {
	*fakeScheduler
	snapshot    []byte
	exportErr   error
	replacement []byte
	generation  int
}

func (s *legacyCleanupScheduler) ExportXML(name string) ([]byte, error) {
	if s.replacement != nil {
		s.generation++
		s.xml[name] = bytes.Clone(s.replacement)
	}
	return bytes.Clone(s.snapshot), s.exportErr
}

func TestCleanupLegacySchedulerTasksRetainsObservedGeneration(t *testing.T) {
	const name = "mcp-local-hub-demo-alpha"
	prior := []byte("<Task>observed generation</Task>")
	for _, tc := range []struct {
		name        string
		filter      string
		snapshot    []byte
		exportErr   error
		replacement []byte
		wantWarning string
	}{
		{"full", "", prior, nil, nil, "exclusive ownership"},
		{"filtered", "alpha", prior, nil, nil, "exclusive ownership"},
		{"external-identical-xml", "", prior, nil, prior, "exclusive ownership"},
		{"external-different-xml", "", prior, nil, []byte("<Task>another generation</Task>"), "exclusive ownership"},
		{"absent-after-list", "alpha", nil, scheduler.ErrTaskNotFound, nil, ""},
		{"partial-error", "", prior[:6], scheduler.ErrPermissionDenied, nil, "cannot observe definition"},
		{"conflicting-absence", "", prior, scheduler.ErrTaskNotFound, nil, "cannot observe definition"},
		{"empty-success", "", nil, nil, nil, "cannot observe definition"},
		{"retention-failure", "", bytes.Repeat([]byte{'x'}, maxStateFileBytes), nil, nil, "retain observed definition"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := daemonIntentTestHelper(t)
			f := newInstallFakeScheduler()
			f.listSeed = []scheduler.TaskStatus{{Name: `\` + name}}
			f.tasks[name], f.xml[name] = true, bytes.Clone(prior)
			sch := &legacyCleanupScheduler{fakeScheduler: f, snapshot: tc.snapshot, exportErr: tc.exportErr, replacement: tc.replacement, generation: 1}
			oldFactory := schedulerFactoryFn
			schedulerFactoryFn = func() (scheduler.Scheduler, error) { return sch, nil }
			t.Cleanup(func() { schedulerFactoryFn = oldFactory })
			oldLookup := lookupProcess
			lookupProcess = nil
			t.Cleanup(func() { lookupProcess = oldLookup })
			oldKill := forceKillByPortFn
			forceKillByPortFn = func(int, time.Duration) (portKillOutcome, error) {
				t.Error("unowned task cleanup attempted a dependent port kill")
				return portKillNoListener, nil
			}
			t.Cleanup(func() { forceKillByPortFn = oldKill })
			m := &config.ServerManifest{Name: "demo", Daemons: []config.DaemonSpec{{Name: "alpha", Port: 9313}}}
			var out bytes.Buffer
			cleanupLegacySchedulerTasksForSupervisorInstall(m, tc.filter, &out)
			wantXML := prior
			if tc.replacement != nil {
				wantXML = tc.replacement
			}
			if len(f.deleteNames) != 0 || f.createCount != 0 || len(f.importNames) != 0 || f.runCount != 0 || !bytes.Equal(f.xml[name], wantXML) {
				t.Fatalf("unowned generation mutated: delete=%v create=%d import=%v run=%d current=%q", f.deleteNames, f.createCount, f.importNames, f.runCount, f.xml[name])
			}
			if tc.replacement != nil && sch.generation != 2 {
				t.Fatalf("replacement was not observed: %d", sch.generation)
			}
			if tc.wantWarning == "" {
				if out.Len() != 0 {
					t.Fatalf("absence reported a mutation: %s", out.String())
				}
			} else if !strings.Contains(out.String(), tc.wantWarning) || !strings.Contains(out.String(), name) || !strings.Contains(out.String(), "daemon stop skipped") {
				t.Fatalf("missing named refusal/cause: %s", out.String())
			}
			if tc.wantWarning == "exclusive ownership" {
				readInstallRecoverySnapshot(t, root, name, tc.snapshot)
			} else if paths, err := filepath.Glob(filepath.Join(root, "scheduler-recovery-*.json")); err != nil || len(paths) != 0 {
				t.Fatalf("unavailable definition reported retained: %v, %v", paths, err)
			}
		})
	}
}

func TestLegacySchedulerFilteredPreviewUsesListedScopeWithoutWriting(t *testing.T) {
	root := daemonIntentTestHelper(t)
	f := newInstallFakeScheduler()
	installFakeScheduler(t, f)
	m := &config.ServerManifest{Name: "demo", Daemons: []config.DaemonSpec{{Name: "alpha", Port: 9313}}}
	// A readable named definition alone cannot establish the List owner scope.
	f.xml["mcp-local-hub-demo-alpha"] = []byte("<Task>unlisted task</Task>")
	for _, present := range []bool{false, true} {
		f.listSeed = []scheduler.TaskStatus{{Name: `\mcp-local-hub-demo-alpha-sibling`}}
		if present {
			f.listSeed = append(f.listSeed, scheduler.TaskStatus{Name: `\mcp-local-hub-demo-alpha`})
		}
		got, err := legacySchedulerTasksForSupervisorInstallDryRun(m, "alpha")
		if err != nil || len(got) != map[bool]int{false: 0, true: 1}[present] || (present && got[0] != "mcp-local-hub-demo-alpha") {
			t.Fatalf("present=%t preview=%v error=%v", present, got, err)
		}
	}
	if paths, err := filepath.Glob(filepath.Join(root, "scheduler-recovery-*.json")); err != nil || len(paths) != 0 || len(f.deleteNames) != 0 || f.createCount != 0 {
		t.Fatalf("preview wrote state: snapshots=%v deletes=%v creates=%d err=%v", paths, f.deleteNames, f.createCount, err)
	}
}

func TestWeeklyRefreshSetPreservesExistingTaskAndCreatesOnlyAbsent(t *testing.T) {
	const name = "mcp-local-hub-weekly-refresh"
	for _, present := range []bool{false, true} {
		t.Run(map[bool]string{false: "absent", true: "present"}[present], func(t *testing.T) {
			root := daemonIntentTestHelper(t)
			exe := filepath.Join(t.TempDir(), mcphubShortName)
			if err := os.WriteFile(exe, []byte("fixture"), 0700); err != nil {
				t.Fatal(err)
			}
			oldPath := testCanonicalMcphubPathOverride
			testCanonicalMcphubPathOverride = exe
			t.Cleanup(func() { testCanonicalMcphubPathOverride = oldPath })
			f := newInstallFakeScheduler()
			prior := []byte("<Task>weekly prior</Task>")
			if present {
				f.tasks[name], f.xml[name] = true, prior
			}
			installFakeScheduler(t, f)
			err := NewAPI().WeeklyRefreshSet("MON 14:30")
			if len(f.deleteNames) != 0 || len(f.importNames) != 0 {
				t.Fatalf("set destructively replaced task: deletes=%v imports=%v", f.deleteNames, f.importNames)
			}
			if present {
				if !errors.Is(err, ErrSchedulerReplacementUnowned) || f.createCount != 0 || !bytes.Equal(f.xml[name], prior) {
					t.Fatalf("present task result=%v creates=%d current=%q", err, f.createCount, f.xml[name])
				}
				readInstallRecoverySnapshot(t, root, name, prior)
			} else {
				if err != nil || f.createCount != 1 {
					t.Fatalf("absent task result=%v creates=%d", err, f.createCount)
				}
				spec := f.createdSpecs[0]
				if spec.Name != name || spec.Command != exe || spec.WeeklyTrigger == nil || *spec.WeeklyTrigger != (scheduler.WeeklyTrigger{DayOfWeek: 1, HourLocal: 14, MinuteLocal: 30}) || strings.Join(spec.Args, " ") != "restart --all" {
					t.Fatalf("wrong weekly task: %+v", spec)
				}
			}
		})
	}
}

func TestLegacySchedulerCleanupNilInputs(t *testing.T) {
	cleanupLegacySchedulerTasksForSupervisorInstall(nil, "", nil)
	cleanupLegacySchedulerTasksForSupervisorInstall(&config.ServerManifest{}, "", io.Discard)
}

func TestInstallPlanCoreGlobalDryRunReportsUnknownLegacyInventory(t *testing.T) {
	daemonIntentTestHelper(t)
	preparePreflightBinaryChecks(t)
	oldFactory := schedulerFactoryFn
	schedulerFactoryFn = func() (scheduler.Scheduler, error) { return nil, scheduler.ErrUnavailable }
	t.Cleanup(func() { schedulerFactoryFn = oldFactory })
	m := globalTwoDaemonManifest()
	plan, err := BuildPlan(m, "")
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := NewAPI().installPlanCore(context.Background(), m, bindTestPlanManifestHash(plan), "", true, &out); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "Legacy scheduler tasks retained (unknown)") ||
		!strings.Contains(out.String(), scheduler.ErrUnavailable.Error()) ||
		strings.Contains(out.String(), "Legacy scheduler tasks retained (observed: 0)") {
		t.Fatalf("unknown inventory reported as observed: %s", out.String())
	}
}

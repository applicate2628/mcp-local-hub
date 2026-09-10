package api

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"
)

func TestProviderDirectProcessObservationFromRows(t *testing.T) {
	started := time.Unix(100, 0).UTC()
	runtimePath, err := os.Executable()
	if err != nil {
		t.Fatalf("os.Executable: %v", err)
	}
	identity := providerDirectProcessIdentityV1{ExecutablePath: runtimePath, Args: []string{"--serve", "plugin"}, WorkingDirectoryObject: providerDirectoryObjectIdentityForTest(1)}
	root := procRow{pid: 10, ppid: 1, created: started, exePath: identity.ExecutablePath, cmdline: `"` + identity.ExecutablePath + `" --serve plugin`}
	childPath := filepath.Join(filepath.Dir(runtimePath), "provider-child")
	child := procRow{pid: 11, ppid: 10, created: started.Add(time.Second), exePath: childPath, cmdline: `"` + childPath + `"`}
	priorPath := filepath.Join(filepath.Dir(runtimePath), "provider-old-child")
	prior := providerProcessGenerationV1{PID: 12, ParentPID: 10, RootPID: 10, StartedAt: started.Add(2 * time.Second), ExecutablePath: priorPath, CommandLine: `"` + priorPath + `"`}
	unrelatedPath := filepath.Join(filepath.Dir(runtimePath), "provider-unrelated")
	for _, tc := range []struct {
		name  string
		id    providerDirectProcessIdentityV1
		prior []providerProcessGenerationV1
		rows  []procRow
		state providerProcessObservationStateV1
		count int
	}{
		{"root-and-descendant", identity, nil, []procRow{root, child}, providerProcessObservationComplete, 2},
		{"reordered-args-not-root", providerDirectProcessIdentityV1{ExecutablePath: identity.ExecutablePath, Args: []string{"plugin", "--serve"}, WorkingDirectoryObject: identity.WorkingDirectoryObject}, nil, []procRow{root}, providerProcessObservationComplete, 0},
		{"pid-reuse-not-prior", identity, []providerProcessGenerationV1{prior}, []procRow{{pid: 12, ppid: 99, created: prior.StartedAt.Add(time.Second), exePath: prior.ExecutablePath, cmdline: prior.CommandLine}}, providerProcessObservationComplete, 0},
		{"reparented-prior-active", identity, []providerProcessGenerationV1{prior}, []procRow{{pid: 12, ppid: 99, created: prior.StartedAt, exePath: prior.ExecutablePath, cmdline: prior.CommandLine}}, providerProcessObservationComplete, 1},
		{"wrapper-relative-rejected", providerDirectProcessIdentityV1{ExecutablePath: "runtime.exe", Args: identity.Args, WorkingDirectoryObject: identity.WorkingDirectoryObject}, nil, []procRow{root}, providerProcessObservationUnavailable, 0},
		{"missing-root-command-evidence", identity, nil, []procRow{{pid: root.pid, ppid: root.ppid, created: root.created, exePath: root.exePath}}, providerProcessObservationUnavailable, 0},
		{"matching-args-missing-root-executable-evidence", identity, nil, []procRow{{pid: root.pid, ppid: root.ppid, created: root.created, cmdline: root.cmdline}}, providerProcessObservationUnavailable, 0},
		{"unrelated-missing-executable-is-not-candidate", identity, nil, []procRow{{pid: root.pid, ppid: root.ppid, created: root.created, cmdline: `"` + unrelatedPath + `" --other`}}, providerProcessObservationComplete, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := observeProviderDirectProcessRowsWithProbe(context.Background(), tc.id, tc.prior, tc.rows, providerWorkingDirectoryEqualTestProbe)
			if got.State != tc.state || len(got.Active) != tc.count {
				t.Fatalf("observation=%+v", got)
			}
		})
	}
}

func TestProviderDirectProcessObservationUnavailableSnapshot(t *testing.T) {
	identity := providerDirectProcessIdentityV1{ExecutablePath: `C:\tools\runtime.exe`, Args: []string{"--serve"}, WorkingDirectoryObject: providerDirectoryObjectIdentityForTest(1)}
	got := providerDirectProcessObservationUnavailable(identity, "process-snapshot-unavailable", nil)
	if got.State != providerProcessObservationUnavailable || got.FailureID != "process-snapshot-unavailable" || got.Err == nil {
		t.Fatalf("observation=%+v", got)
	}
}

func providerWorkingDirectoryEqualTestProbe(context.Context, providerProcessWorkingDirectoryCandidateV1, providerDirectoryObjectIdentityV1) providerProcessWorkingDirectoryResultV1 {
	return providerProcessWorkingDirectoryResultV1{State: providerProcessWorkingDirectoryEqual}
}

func providerDirectoryObjectIdentityForTest(seed byte) providerDirectoryObjectIdentityV1 {
	var fileID [16]byte
	fileID[0] = seed
	return providerDirectoryObjectIdentityV1{volumeSerialNumber: uint64(seed), fileID: fileID, valid: true}
}

// TestProviderDirectProcessObservationDistinguishesSameCommandByWorkingDirectory
// catches the regression where every executable/argv collision is admitted as
// the selected provider root. Only root candidates are probed; descendants keep
// using the existing parent expansion after the selected root is known.
func TestProviderDirectProcessObservationDistinguishesSameCommandByWorkingDirectory(t *testing.T) {
	started := time.Unix(100, 0).UTC()
	runtimePath, err := os.Executable()
	if err != nil {
		t.Fatalf("os.Executable: %v", err)
	}
	identity := providerDirectProcessIdentityV1{
		ExecutablePath:         runtimePath,
		Args:                   []string{"--serve", "plugin"},
		WorkingDirectoryObject: providerDirectoryObjectIdentityForTest(1),
	}
	commandLine := `"` + runtimePath + `" --serve plugin`
	rows := []procRow{
		{pid: 21, ppid: 20, created: started.Add(3 * time.Second), exePath: runtimePath, cmdline: `"` + runtimePath + `" --child-b`},
		{pid: 10, ppid: 1, created: started, exePath: runtimePath, cmdline: commandLine},
		{pid: 11, ppid: 10, created: started.Add(time.Second), exePath: runtimePath, cmdline: `"` + runtimePath + `" --child-a`},
		{pid: 20, ppid: 1, created: started.Add(2 * time.Second), exePath: runtimePath, cmdline: commandLine},
	}
	var probed []int
	probe := func(_ context.Context, candidate providerProcessWorkingDirectoryCandidateV1, selected providerDirectoryObjectIdentityV1) providerProcessWorkingDirectoryResultV1 {
		probed = append(probed, candidate.PID)
		if selected != identity.WorkingDirectoryObject {
			t.Fatalf("selected identity=%+v", selected)
		}
		if candidate.PID == 10 {
			return providerProcessWorkingDirectoryResultV1{State: providerProcessWorkingDirectoryEqual}
		}
		return providerProcessWorkingDirectoryResultV1{State: providerProcessWorkingDirectoryDifferent}
	}

	got := observeProviderDirectProcessRowsWithProbe(context.Background(), identity, nil, rows, probe)
	if got.State != providerProcessObservationComplete {
		t.Fatalf("observation=%+v", got)
	}
	if !reflect.DeepEqual(probed, []int{10, 20}) {
		t.Fatalf("probed PIDs=%v, want roots only [10 20]", probed)
	}
	var activePIDs []int
	for _, generation := range got.Active {
		activePIDs = append(activePIDs, generation.PID)
	}
	if !reflect.DeepEqual(activePIDs, []int{10, 11}) {
		t.Fatalf("active PIDs=%v, want selected A root and descendant [10 11]", activePIDs)
	}
}

func TestProviderDirectProcessObservationCandidateMatrix(t *testing.T) {
	started := time.Unix(100, 0).UTC()
	runtimePath, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	identity := providerDirectProcessIdentityV1{ExecutablePath: runtimePath, Args: []string{"child"}, WorkingDirectoryObject: providerDirectoryObjectIdentityForTest(1)}
	commandLine := `"` + runtimePath + `" child`
	row := func(pid int) procRow {
		return procRow{pid: pid, ppid: 1, created: started.Add(time.Duration(pid) * time.Second), exePath: runtimePath, cmdline: commandLine}
	}

	t.Run("zero-candidates", func(t *testing.T) {
		called := false
		got := observeProviderDirectProcessRowsWithProbe(context.Background(), identity, nil, nil, func(context.Context, providerProcessWorkingDirectoryCandidateV1, providerDirectoryObjectIdentityV1) providerProcessWorkingDirectoryResultV1 {
			called = true
			return providerProcessWorkingDirectoryResultV1{State: providerProcessWorkingDirectoryEqual}
		})
		if got.State != providerProcessObservationComplete || len(got.Active) != 0 || called {
			t.Fatalf("observation=%+v called=%t", got, called)
		}
	})

	for _, rows := range [][]procRow{
		{row(10), row(20), row(30)},
		{row(30), row(10), row(20)},
		{row(20), row(30), row(10)},
	} {
		got := observeProviderDirectProcessRowsWithProbe(context.Background(), identity, nil, rows, func(_ context.Context, candidate providerProcessWorkingDirectoryCandidateV1, _ providerDirectoryObjectIdentityV1) providerProcessWorkingDirectoryResultV1 {
			if candidate.PID == 10 {
				return providerProcessWorkingDirectoryResultV1{State: providerProcessWorkingDirectoryEqual}
			}
			return providerProcessWorkingDirectoryResultV1{State: providerProcessWorkingDirectoryDifferent}
		})
		if got.State != providerProcessObservationComplete || len(got.Active) != 1 || got.Active[0].PID != 10 {
			t.Fatalf("permutation observation=%+v", got)
		}
	}

	t.Run("unknown-invalidates-whole-observation", func(t *testing.T) {
		got := observeProviderDirectProcessRowsWithProbe(context.Background(), identity, nil, []procRow{row(10), row(20)}, func(_ context.Context, candidate providerProcessWorkingDirectoryCandidateV1, _ providerDirectoryObjectIdentityV1) providerProcessWorkingDirectoryResultV1 {
			if candidate.PID == 20 {
				return providerProcessWorkingDirectoryResultV1{State: providerProcessWorkingDirectoryUnknown, FailureID: "provider-working-directory-access-unavailable", Err: errors.New("denied")}
			}
			return providerProcessWorkingDirectoryResultV1{State: providerProcessWorkingDirectoryEqual}
		})
		if got.State != providerProcessObservationUnavailable || got.FailureID != "provider-working-directory-access-unavailable" || len(got.Active) != 0 {
			t.Fatalf("observation=%+v", got)
		}
	})

	t.Run("duplicate-snapshot-root-is-one-generation", func(t *testing.T) {
		duplicate := row(10)
		got := observeProviderDirectProcessRowsWithProbe(context.Background(), identity, nil, []procRow{duplicate, duplicate}, providerWorkingDirectoryEqualTestProbe)
		if got.State != providerProcessObservationComplete || len(got.Active) != 1 || got.Active[0].PID != 10 {
			t.Fatalf("observation=%+v", got)
		}
	})
}

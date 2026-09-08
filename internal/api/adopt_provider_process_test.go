package api

import (
	"testing"
	"time"
)

func TestProviderDirectProcessObservationFromRows(t *testing.T) {
	started := time.Unix(100, 0).UTC()
	identity := providerDirectProcessIdentityV1{ExecutablePath: `C:\tools\runtime.exe`, Args: []string{"--serve", "plugin"}}
	root := procRow{pid: 10, ppid: 1, created: started, exePath: identity.ExecutablePath, cmdline: `"C:\tools\runtime.exe" --serve plugin`}
	child := procRow{pid: 11, ppid: 10, created: started.Add(time.Second), exePath: `C:\tools\child.exe`, cmdline: `"C:\tools\child.exe"`}
	prior := providerProcessGenerationV1{PID: 12, ParentPID: 10, RootPID: 10, StartedAt: started.Add(2 * time.Second), ExecutablePath: `C:\tools\old-child.exe`, CommandLine: `"C:\tools\old-child.exe"`}
	for _, tc := range []struct {
		name  string
		id    providerDirectProcessIdentityV1
		prior []providerProcessGenerationV1
		rows  []procRow
		state providerProcessObservationStateV1
		count int
	}{
		{"root-and-descendant", identity, nil, []procRow{root, child}, providerProcessObservationComplete, 2},
		{"reordered-args-not-root", providerDirectProcessIdentityV1{ExecutablePath: identity.ExecutablePath, Args: []string{"plugin", "--serve"}}, nil, []procRow{root}, providerProcessObservationComplete, 0},
		{"pid-reuse-not-prior", identity, []providerProcessGenerationV1{prior}, []procRow{{pid: 12, ppid: 99, created: prior.StartedAt.Add(time.Second), exePath: prior.ExecutablePath, cmdline: prior.CommandLine}}, providerProcessObservationComplete, 0},
		{"reparented-prior-active", identity, []providerProcessGenerationV1{prior}, []procRow{{pid: 12, ppid: 99, created: prior.StartedAt, exePath: prior.ExecutablePath, cmdline: prior.CommandLine}}, providerProcessObservationComplete, 1},
		{"wrapper-relative-rejected", providerDirectProcessIdentityV1{ExecutablePath: "runtime.exe", Args: identity.Args}, nil, []procRow{root}, providerProcessObservationUnavailable, 0},
		{"missing-root-command-evidence", identity, nil, []procRow{{pid: root.pid, ppid: root.ppid, created: root.created, exePath: root.exePath}}, providerProcessObservationUnavailable, 0},
		{"matching-args-missing-root-executable-evidence", identity, nil, []procRow{{pid: root.pid, ppid: root.ppid, created: root.created, cmdline: root.cmdline}}, providerProcessObservationUnavailable, 0},
		{"unrelated-missing-executable-is-not-candidate", identity, nil, []procRow{{pid: root.pid, ppid: root.ppid, created: root.created, cmdline: `"C:\\tools\\unrelated.exe" --other`}}, providerProcessObservationComplete, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := observeProviderDirectProcessRows(tc.id, tc.prior, tc.rows)
			if got.State != tc.state || len(got.Active) != tc.count {
				t.Fatalf("observation=%+v", got)
			}
		})
	}
}

func TestProviderDirectProcessObservationUnavailableSnapshot(t *testing.T) {
	identity := providerDirectProcessIdentityV1{ExecutablePath: `C:\tools\runtime.exe`, Args: []string{"--serve"}}
	got := providerDirectProcessObservationUnavailable(identity, "process-snapshot-unavailable", nil)
	if got.State != providerProcessObservationUnavailable || got.FailureID != "process-snapshot-unavailable" || got.Err == nil {
		t.Fatalf("observation=%+v", got)
	}
}

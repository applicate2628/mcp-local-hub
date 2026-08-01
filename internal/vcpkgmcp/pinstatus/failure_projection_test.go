package pinstatus

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"

	hubprocess "mcp-local-hub/internal/process"
	"mcp-local-hub/internal/vcpkgmcp/publicresult"
)

func TestPinStatusProjection_NMinusOne_N_NPlusOne(t *testing.T) {
	maxPortDir := largestCompleteFailurePortDir(t)
	for _, tc := range []struct {
		name       string
		portDirLen int
		projected  bool
	}{
		{name: "N-1", portDirLen: maxPortDir - 1},
		{name: "N", portDirLen: maxPortDir},
		{name: "N+1", portDirLen: maxPortDir + 1, projected: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			result := failureProjectionResult(tc.portDirLen)
			ordinary, err := json.MarshalIndent(result, "", "  ")
			if err != nil {
				t.Fatal(err)
			}
			got, err := publicresult.MarshalIndent(result)
			if err != nil {
				t.Fatal(err)
			}
			if tc.projected {
				if len(got) > publicresult.MaxEncodedBytes {
					t.Fatalf("projected bytes=%d, limit=%d", len(got), publicresult.MaxEncodedBytes)
				}
				var body struct {
					FailurePorts []failureProjectionPort `json:"failure_ports"`
					Projection   publicresult.Projection `json:"result_projection"`
				}
				if err := json.Unmarshal(got, &body); err != nil {
					t.Fatal(err)
				}
				if len(body.FailurePorts) != 1 || !reflect.DeepEqual(body.FailurePorts[0], expectedFailureProjectionPort(result.Ports[0], 0)) {
					t.Fatalf("failure_ports=%+v, want exact causal tuple", body.FailurePorts)
				}
				assertFailureProjectionOmissions(t, body.Projection, len(result.Ports), len(body.FailurePorts), 1)
				return
			}
			if string(got) != string(ordinary) {
				t.Fatalf("under-budget bytes changed\ngot: %s\nwant: %s", got, ordinary)
			}
		})
	}
}

func TestPinStatusProjection_RetainsOrderedFailureTuples(t *testing.T) {
	ports := make([]PortResult, 4)
	for i := range ports {
		exitCode := 10 + i
		ports[i] = PortResult{
			PortDir: strings.Repeat(string(rune('a'+i)), 80<<10),
			Status:  Status("unknown"),
			Reason:  ReasonRemoteQueryFailed,
			Failure: &PublicFailure{ID: FailureGitExitNonzero, CauseIDs: []FailureID{FailureProcessCleanupTimeout}, ExitCode: &exitCode, Detail: "remote query failed"},
		}
	}
	result := Result{Status: Status("ok"), Ports: ports}
	got, err := publicresult.MarshalIndent(result)
	if err != nil {
		t.Fatal(err)
	}
	var body struct {
		FailurePorts []failureProjectionPort `json:"failure_ports"`
		Projection   publicresult.Projection `json:"result_projection"`
	}
	if err := json.Unmarshal(got, &body); err != nil {
		t.Fatal(err)
	}
	if len(body.FailurePorts) != len(ports) {
		t.Fatalf("failure_ports=%d, want %d", len(body.FailurePorts), len(ports))
	}
	for i, gotPort := range body.FailurePorts {
		if want := expectedFailureProjectionPort(ports[i], i); !reflect.DeepEqual(gotPort, want) {
			t.Fatalf("failure_ports[%d]=%+v, want %+v", i, gotPort, want)
		}
	}
	assertFailureProjectionOmissions(t, body.Projection, len(ports), len(body.FailurePorts), len(ports))
}

func TestPinStatusProjection_ImpossibleCausalCoreFailsClosed(t *testing.T) {
	result := Result{Status: Status("ok"), Ports: []PortResult{{
		Status:  Status("unknown"),
		Reason:  ReasonRemoteQueryFailed,
		Failure: &PublicFailure{ID: FailureRemoteQueryFailed, Detail: strings.Repeat("x", publicresult.MaxEncodedBytes)},
	}}}
	if _, err := publicresult.MarshalIndent(result); !errors.Is(err, publicresult.ErrBudgetInvariant) {
		t.Fatalf("MarshalIndent error=%v, want ErrBudgetInvariant", err)
	}
}

type failureProjectionPort struct {
	PortIndex int            `json:"port_index"`
	Status    Status         `json:"status"`
	Reason    Reason         `json:"reason,omitempty"`
	Failure   *PublicFailure `json:"failure"`
}

func failureProjectionResult(portDirLen int) Result {
	return Result{Status: Status("ok"), Ports: []PortResult{{
		PortDir: strings.Repeat("x", portDirLen),
		Status:  Status("unknown"),
		Reason:  ReasonRemoteQueryFailed,
		Failure: &PublicFailure{ID: FailureRemoteQueryFailed, Detail: "remote query failed"},
	}}}
}

func largestCompleteFailurePortDir(t *testing.T) int {
	t.Helper()
	low, high := 0, publicresult.MaxEncodedBytes
	for low < high {
		middle := low + (high-low+1)/2
		body, err := json.MarshalIndent(failureProjectionResult(middle), "", "  ")
		if err != nil {
			t.Fatal(err)
		}
		if len(body) <= publicresult.MaxEncodedBytes {
			low = middle
		} else {
			high = middle - 1
		}
	}
	return low
}

func expectedFailureProjectionPort(port PortResult, index int) failureProjectionPort {
	return failureProjectionPort{PortIndex: index, Status: port.Status, Reason: port.Reason, Failure: port.Failure}
}

func assertFailureProjectionOmissions(t *testing.T, projection publicresult.Projection, portCount, retained, failures int) {
	t.Helper()
	if projection.Complete || len(projection.Omissions) != 2 {
		t.Fatalf("projection=%+v, want incomplete exact two omissions", projection)
	}
	for _, omission := range projection.Omissions {
		if omission.Reason != publicresult.InternalProjectionLimit {
			t.Fatalf("omission=%+v, want internal projection limit", omission)
		}
		switch omission.Field {
		case "ports":
			if omission.Retained != 0 || omission.Omitted == nil || *omission.Omitted != portCount {
				t.Fatalf("ports omission=%+v, want retained=0 omitted=%d", omission, portCount)
			}
		case "failure_causal_rows":
			if omission.Retained != retained || omission.Omitted == nil || *omission.Omitted != failures-retained {
				t.Fatalf("failure omission=%+v, want retained=%d omitted=%d", omission, retained, failures-retained)
			}
		default:
			t.Fatalf("unexpected omission field %q", omission.Field)
		}
	}
}

func TestRemoteFailureProjectionPreservesExistingTriStateReasons(t *testing.T) {
	dir := newPort(t, "remote-failure", `vcpkg_from_github(REPO a/b REF `+commitA+` SHA512 0)`)
	tests := []struct {
		name       string
		err        error
		wantReason Reason
		wantID     FailureID
		wantDetail string
	}{
		{
			name:       "reference limit",
			err:        ErrRemoteRefLimit,
			wantReason: ReasonRemoteRefLimit,
			wantID:     FailureRemoteParseLimit,
			wantDetail: "remote reference limit reached",
		},
		{
			name:       "timeout",
			err:        context.DeadlineExceeded,
			wantReason: ReasonRemoteQueryTimeout,
			wantID:     FailureRemoteTimeout,
			wantDetail: "remote query timed out",
		},
		{
			name:       "canceled",
			err:        context.Canceled,
			wantReason: ReasonRemoteQueryCanceled,
			wantID:     FailureRemoteCanceled,
			wantDetail: "remote query canceled",
		},
		{
			name:       "unclassified error",
			err:        errors.New("untrusted error text"),
			wantReason: ReasonRemoteQueryFailed,
			wantID:     FailureRemoteQueryFailed,
			wantDetail: "remote query failed",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			res := PinStatus(context.Background(), Args{PortDirs: []string{dir}}, Deps{
				FS:  DefaultFS(),
				Now: fixedNow(),
				RemoteRefs: func(context.Context, approvedRemoteURL) (map[string]string, error) {
					return nil, tc.err
				},
			}).Ports[0]
			if res.Reason != tc.wantReason {
				t.Fatalf("reason=%q, want %q", res.Reason, tc.wantReason)
			}
			if res.Failure == nil {
				t.Fatal("missing public failure")
			}
			if res.Failure.ID != tc.wantID || res.Failure.Detail != tc.wantDetail {
				t.Fatalf("failure=%+v, want id=%q detail=%q", res.Failure, tc.wantID, tc.wantDetail)
			}
			if res.Failure.ExitCode != nil || len(res.Failure.CauseIDs) != 0 {
				t.Fatalf("failure=%+v unexpectedly inferred unavailable causal data", res.Failure)
			}
		})
	}
}

func TestRemoteFailureProjectionLifecycleMatrixIsTypedAndSecretFree(t *testing.T) {
	secret := "projection-secret-must-not-escape"
	exitCode := 23
	tests := []struct {
		name       string
		err        error
		wantReason Reason
		wantID     FailureID
		wantCause  []FailureID
		wantExit   *int
	}{
		{
			name: "containment",
			err: &hubprocess.ContainedRunError{
				Stage: hubprocess.ContainedStageContainment,
				Cause: errors.Join(hubprocess.ErrContainmentUnavailable, errors.New(secret)),
			},
			wantReason: ReasonRemoteQueryFailed,
			wantID:     FailureProcessContainmentUnavailable,
		},
		{
			name: "start",
			err: &hubprocess.ContainedRunError{
				Stage: hubprocess.ContainedStageStart,
				Cause: errors.New(secret),
			},
			wantReason: ReasonRemoteQueryFailed,
			wantID:     FailureRemoteStartFailed,
		},
		{
			name: "parse with cleanup timeout",
			err: &hubprocess.ContainedRunError{
				Stage:        hubprocess.ContainedStageStdout,
				Cause:        errors.Join(ErrRemoteRefLimit, errors.New(secret)),
				CleanupStage: hubprocess.ContainedStageCleanup,
				CleanupCause: errors.Join(hubprocess.ErrCleanupTimeout, errors.New(secret)),
			},
			wantReason: ReasonRemoteRefLimit,
			wantID:     FailureRemoteParseLimit,
			wantCause:  []FailureID{FailureProcessCleanupTimeout},
		},
		{
			name:       "canceled",
			err:        &hubprocess.ContainedRunError{Stage: hubprocess.ContainedStageCleanup, Cause: context.Canceled},
			wantReason: ReasonRemoteQueryCanceled,
			wantID:     FailureRemoteCanceled,
		},
		{
			name:       "timeout",
			err:        &hubprocess.ContainedRunError{Stage: hubprocess.ContainedStageCleanup, Cause: context.DeadlineExceeded},
			wantReason: ReasonRemoteQueryTimeout,
			wantID:     FailureRemoteTimeout,
		},
		{
			name: "cleanup timeout",
			err: &hubprocess.ContainedRunError{
				Stage: hubprocess.ContainedStageCleanup,
				Cause: errors.Join(hubprocess.ErrCleanupTimeout, errors.New(secret)),
			},
			wantReason: ReasonRemoteQueryFailed,
			wantID:     FailureProcessCleanupTimeout,
		},
		{
			name: "git exit",
			err: &hubprocess.ContainedRunError{
				Stage:    hubprocess.ContainedStageExit,
				Cause:    errors.New(secret),
				ExitCode: &exitCode,
			},
			wantReason: ReasonRemoteQueryFailed,
			wantID:     FailureGitExitNonzero,
			wantExit:   &exitCode,
		},
		{
			name:       "read",
			err:        &hubprocess.ContainedRunError{Stage: hubprocess.ContainedStageStdout, Cause: errors.New(secret)},
			wantReason: ReasonRemoteQueryFailed,
			wantID:     FailureRemoteQueryFailed,
		},
		{
			name:       "wait",
			err:        &hubprocess.ContainedRunError{Stage: hubprocess.ContainedStageWait, Cause: errors.New(secret)},
			wantReason: ReasonRemoteQueryFailed,
			wantID:     FailureRemoteQueryFailed,
		},
	}

	dir := newPort(t, "remote-lifecycle", `vcpkg_from_github(REPO a/b REF `+commitA+` SHA512 0)`)
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			result := PinStatus(context.Background(), Args{PortDirs: []string{dir}}, Deps{
				FS:  DefaultFS(),
				Now: fixedNow(),
				RemoteRefs: func(context.Context, approvedRemoteURL) (map[string]string, error) {
					return nil, tc.err
				},
			})
			port := result.Ports[0]
			if port.Reason != tc.wantReason {
				t.Fatalf("reason=%q, want %q", port.Reason, tc.wantReason)
			}
			if port.Failure == nil || port.Failure.ID != tc.wantID {
				t.Fatalf("failure=%+v, want id=%q", port.Failure, tc.wantID)
			}
			if !equalFailureIDs(port.Failure.CauseIDs, tc.wantCause) {
				t.Fatalf("cause_ids=%v, want %v", port.Failure.CauseIDs, tc.wantCause)
			}
			if tc.wantExit == nil {
				if port.Failure.ExitCode != nil {
					t.Fatalf("exit_code=%v, want absent", *port.Failure.ExitCode)
				}
			} else if port.Failure.ExitCode == nil || *port.Failure.ExitCode != *tc.wantExit {
				t.Fatalf("exit_code=%v, want %d", port.Failure.ExitCode, *tc.wantExit)
			}

			whole, err := json.Marshal(result)
			if err != nil {
				t.Fatal(err)
			}
			minimal, err := json.Marshal(result.PublicResultProjection())
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(string(whole), secret) || strings.Contains(string(minimal), secret) {
				t.Fatalf("secret leaked: whole=%s minimal=%s", whole, minimal)
			}
		})
	}
}

func equalFailureIDs(left, right []FailureID) bool {
	if len(left) != len(right) {
		return false
	}
	for i := range left {
		if left[i] != right[i] {
			return false
		}
	}
	return true
}

package cli

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"

	"mcp-local-hub/internal/api"
)

func attachFullTestPlan(state, before, after, receipt string) *api.AttachClientPlan {
	return &api.AttachClientPlan{State: state, ReceiptID: receipt, Server: "fixture", Daemon: "default", Client: "mimocode", EntryKey: "fixture",
		BeforeTransport: before, AfterTransport: after, TargetBytesSHA256: strings.Repeat("a", 64), EntryTokenSHA256: strings.Repeat("b", 64)}
}

func attachFullTestResult(state string, noWrite bool) *api.AttachClientApplyResult {
	return &api.AttachClientApplyResult{State: state, IdempotentNoWrite: noWrite, ReceiptID: "r1", Server: "fixture", Daemon: "default", Client: "mimocode", EntryKey: "fixture",
		BeforeSHA256: strings.Repeat("a", 64), AfterSHA256: strings.Repeat("b", 64)}
}

func TestAttachClientFullRollbackPreviewUsesExactReceiptWithoutMutation(t *testing.T) {
	var gotID string
	mutations := 0
	cmd := newAttachClientCmdWithDeps(attachClientCmdDeps{
		buildRollback: func(_ context.Context, req api.AttachClientRequest, id string) (*api.AttachClientPlan, error) {
			gotID = id
			if req != (api.AttachClientRequest{Server: "fixture", Daemon: "default", Client: "mimocode"}) {
				t.Fatalf("selection=%+v", req)
			}
			return attachFullTestPlan("would-rollback", "http", "direct", id), nil
		},
		applyRollback: func(context.Context, api.AttachClientRequest, string) (*api.AttachClientApplyResult, error) {
			mutations++
			return nil, nil
		},
	})
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetArgs([]string{"fixture", "--daemon", "default", "--client", "mimocode", "--rollback-receipt", "r1"})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	if gotID != "r1" || mutations != 0 || !strings.Contains(out.String(), "operation=rollback state=would-rollback") || !strings.Contains(out.String(), `receipt_id="r1"`) || !strings.Contains(out.String(), "transport=http->direct") {
		t.Fatalf("id=%q mutations=%d output=%q", gotID, mutations, out.String())
	}
}

func TestAttachClientFullApplyReportsMutatorResultNotStalePreview(t *testing.T) {
	for _, tc := range []struct {
		name, operation, planState, apiState, wantState string
		noWrite                                         bool
	}{
		{name: "attach-first", operation: "attach", planState: "would-attach", apiState: "committed", wantState: "committed"},
		{name: "attach-repeat", operation: "attach", planState: "would-attach", apiState: "committed", wantState: "already-committed", noWrite: true},
		{name: "rollback-first", operation: "rollback", planState: "would-rollback", apiState: "rolled-back", wantState: "rolled-back"},
		{name: "rollback-repeat", operation: "rollback", planState: "would-rollback", apiState: "rolled-back", wantState: "already-rolled-back", noWrite: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			applyCalls := 0
			deps := attachClientCmdDeps{
				buildAttach: func(context.Context, api.AttachClientRequest) (*api.AttachClientPlan, error) {
					return attachFullTestPlan(tc.planState, "direct", "http", ""), nil
				},
				buildRollback: func(context.Context, api.AttachClientRequest, string) (*api.AttachClientPlan, error) {
					return attachFullTestPlan(tc.planState, "http", "direct", "r1"), nil
				},
				applyAttach: func(context.Context, *api.AttachClientPlan) (*api.AttachClientApplyResult, error) {
					applyCalls++
					return attachFullTestResult(tc.apiState, tc.noWrite), nil
				},
				applyRollback: func(context.Context, api.AttachClientRequest, string) (*api.AttachClientApplyResult, error) {
					applyCalls++
					return attachFullTestResult(tc.apiState, tc.noWrite), nil
				},
			}
			cmd := newAttachClientCmdWithDeps(deps)
			var out bytes.Buffer
			cmd.SetOut(&out)
			args := []string{"fixture", "--daemon", "default", "--client", "mimocode", "--apply"}
			if tc.operation == "rollback" {
				args = append(args, "--rollback-receipt", "r1")
			}
			cmd.SetArgs(args)
			if err := cmd.Execute(); err != nil {
				t.Fatal(err)
			}
			if applyCalls != 1 || !strings.Contains(out.String(), "operation="+tc.operation+" state="+tc.wantState) || !strings.Contains(out.String(), `receipt_id="r1"`) {
				t.Fatalf("apply calls=%d output=%q", applyCalls, out.String())
			}
		})
	}
}

func TestAttachClientFullFailureHasTypedStageAndNoSummary(t *testing.T) {
	for _, tc := range []struct {
		name string
		deps attachClientCmdDeps
		args []string
		want string
	}{
		{name: "attach-inconsistent-result", deps: attachClientCmdDeps{buildAttach: func(context.Context, api.AttachClientRequest) (*api.AttachClientPlan, error) {
			return attachFullTestPlan("would-attach", "direct", "http", ""), nil
		}, applyAttach: func(context.Context, *api.AttachClientPlan) (*api.AttachClientApplyResult, error) {
			return attachFullTestResult("already-committed", false), nil
		}}, args: []string{"--apply"}, want: "ATTACH_RESULT_INVALID stage=apply"},
		{name: "rollback-empty-plan", deps: attachClientCmdDeps{buildRollback: func(context.Context, api.AttachClientRequest, string) (*api.AttachClientPlan, error) { return nil, nil }}, args: []string{"--rollback-receipt", "r1"}, want: "ATTACH_PLAN_INVALID stage=rollback-plan"},
		{name: "rollback-plan", deps: attachClientCmdDeps{buildRollback: func(context.Context, api.AttachClientRequest, string) (*api.AttachClientPlan, error) {
			return nil, &api.AttachPlanError{Code: "rollback-conflict"}
		}}, args: []string{"--rollback-receipt", "r1"}, want: "ATTACH_ROLLBACK_CONFLICT stage=rollback-plan"},
		{name: "attach-apply", deps: attachClientCmdDeps{buildAttach: func(context.Context, api.AttachClientRequest) (*api.AttachClientPlan, error) {
			return attachFullTestPlan("would-attach", "direct", "http", ""), nil
		}, applyAttach: func(context.Context, *api.AttachClientPlan) (*api.AttachClientApplyResult, error) {
			return nil, errors.New("private-cause-marker")
		}}, args: []string{"--apply"}, want: "ATTACH_APPLY_FAILED stage=apply"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cmd := newAttachClientCmdWithDeps(tc.deps)
			var out bytes.Buffer
			cmd.SetOut(&out)
			cmd.SetArgs(append([]string{"fixture", "--daemon", "default", "--client", "mimocode"}, tc.args...))
			err := cmd.Execute()
			if err == nil || err.Error() != tc.want || out.Len() != 0 {
				t.Fatalf("err=%v output=%q", err, out.String())
			}
		})
	}
}

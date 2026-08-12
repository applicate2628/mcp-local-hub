package daemon

import (
	"context"
	"errors"
	"os/exec"
	"testing"

	"mcp-local-hub/internal/api"
)

type recordingEnrollmentClient struct {
	enrollErr error
	enrolls   []api.HubEnrollmentEnrollV1
	cancels   []api.HubEnrollmentCancelV1
}

type fakeCapabilityPipe struct{ closed bool }

func (p *fakeCapabilityPipe) writeAndClose(v []byte) error {
	if len(v) != 32 {
		return errors.New("wrong length")
	}
	return nil
}
func (p *fakeCapabilityPipe) close() error          { p.closed = true; return nil }
func (p *fakeCapabilityPipe) apply(*exec.Cmd) error { return nil }
func (p *fakeCapabilityPipe) locator() uintptr      { return 1234 }

func fakeLaunchCapabilityOps(_ error) launchCapabilityOps {
	return launchCapabilityOps{
		random32: func(dst *[32]byte) error {
			for i := range dst {
				dst[i] = byte(i + 1)
			}
			return nil
		},
		zero32: func(dst *[32]byte) {
			for i := range dst {
				dst[i] = 0
			}
		},
		openPipe: func() (launchCapabilityPipe, error) { return &fakeCapabilityPipe{}, nil },
	}
}

func (c *recordingEnrollmentClient) Enroll(_ context.Context, req api.HubEnrollmentEnrollV1) (api.HubEnrollmentReceiptV1, error) {
	c.enrolls = append(c.enrolls, req)
	if c.enrollErr != nil {
		return api.HubEnrollmentReceiptV1{}, c.enrollErr
	}
	return api.HubEnrollmentReceiptV1{Version: 1, Correlation: req.Correlation, State: api.HubEnrollmentStateEnrolled, ChannelSettled: true}, nil
}

func (c *recordingEnrollmentClient) Cancel(_ context.Context, req api.HubEnrollmentCancelV1) (api.HubEnrollmentReceiptV1, error) {
	c.cancels = append(c.cancels, req)
	return api.HubEnrollmentReceiptV1{Version: 1, Correlation: req.Correlation, State: api.HubEnrollmentStateCancelled, ChannelSettled: true}, nil
}

func TestLaunchCapabilityLifecycleAllReturns(t *testing.T) {
	tests := []struct {
		name         string
		enrollErr    error
		startErr     error
		wantPrepared bool
		wantEnrolls  int
		wantCancels  int
	}{
		{name: "enroll and start", wantPrepared: true, wantEnrolls: 1},
		{name: "enroll failure cancels and degrades without capability", enrollErr: errors.New("unavailable"), wantEnrolls: 1, wantCancels: 1},
		{name: "start failure cancels", startErr: errors.New("start failed"), wantPrepared: true, wantEnrolls: 1, wantCancels: 1},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			client := &recordingEnrollmentClient{enrollErr: tc.enrollErr}
			ops := fakeLaunchCapabilityOps(tc.startErr)
			prepared, err := prepareLaunchCapability(context.Background(), LaunchCapabilityConfig{
				Task: "cst", Generation: 7, Enrollment: client,
			}, ops)
			if err != nil {
				t.Fatalf("prepare: %v", err)
			}
			if (prepared != nil) != tc.wantPrepared {
				t.Fatalf("prepared=%v want=%v", prepared != nil, tc.wantPrepared)
			}
			if prepared != nil {
				err = prepared.start(func() error { return tc.startErr })
				if !errors.Is(err, tc.startErr) {
					t.Fatalf("start error=%v want=%v", err, tc.startErr)
				}
			}
			if len(client.enrolls) != tc.wantEnrolls || len(client.cancels) != tc.wantCancels {
				t.Fatalf("enrolls/cancels=%d/%d want=%d/%d", len(client.enrolls), len(client.cancels), tc.wantEnrolls, tc.wantCancels)
			}
		})
	}
}

func TestLaunchCapabilityHandleListAndEnvironment(t *testing.T) {
	client := &recordingEnrollmentClient{}
	ops := fakeLaunchCapabilityOps(nil)
	prepared, err := prepareLaunchCapability(context.Background(), LaunchCapabilityConfig{Task: "cst", Generation: 9, Enrollment: client}, ops)
	if err != nil {
		t.Fatal(err)
	}
	if prepared == nil {
		t.Fatal("capability not prepared")
	}
	if got := prepared.handleInventory(); len(got) != 4 || got[0] != "stdin" || got[1] != "stdout" || got[2] != "stderr" || got[3] != "capability-read" {
		t.Fatalf("handle inventory=%v", got)
	}
	if prepared.environmentKey() != LaunchCapabilityHandleEnv || prepared.environmentValue() == "" {
		t.Fatalf("locator=%q=%q", prepared.environmentKey(), prepared.environmentValue())
	}
	for _, req := range client.enrolls {
		if req.CapabilitySHA256 == "" || req.CapabilitySHA256 == prepared.environmentValue() {
			t.Fatalf("digest/locator contract violated: %#v locator=%q", req, prepared.environmentValue())
		}
	}
}

package cli

import (
	"context"
	"errors"
	"testing"
)

// Call-site fixtures receive a capability; they never simulate process custody
// by returning nil from an error-only starter.
type fixtureSupervisorLaunch struct {
	bindErr, abortErr, handoffErr error
	binds, aborts, handoffs       int
	order                         *[]string
}

func (f *fixtureSupervisorLaunch) BindControl(context.Context, SupervisorControlExpectation) (SupervisorControlBinding, error) {
	f.binds++
	if f.order != nil {
		*f.order = append(*f.order, "bind-launch")
	}
	return SupervisorControlBinding{proof: &supervisorControlProof{issuer: f}}, f.bindErr
}
func (f *fixtureSupervisorLaunch) Abort(ctx context.Context) error {
	f.aborts++
	if ctx == nil || ctx.Err() != nil {
		return errors.New("cleanup context unusable")
	}
	if f.order != nil {
		*f.order = append(*f.order, "abort-launch")
	}
	return f.abortErr
}
func (f *fixtureSupervisorLaunch) Handoff(_ context.Context, binding SupervisorControlBinding) error {
	f.handoffs++
	if binding.proof == nil || binding.proof.issuer != f {
		return errors.New("foreign binding")
	}
	if f.order != nil {
		*f.order = append(*f.order, "handoff-launch")
	}
	return f.handoffErr
}

func TestSupervisorLaunchManagedStartRefusesMissingAndSettlesFailure(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	f := &fixtureSupervisorLaunch{bindErr: errors.New("channel absent")}
	err := receiveStartedSupervisor(ctx, f, SupervisorControlExpectation{}, nil)
	if err == nil || f.aborts != 1 || f.handoffs != 0 {
		t.Fatalf("err=%v launch=%+v", err, f)
	}
	if err := receiveStartedSupervisor(context.Background(), nil, SupervisorControlExpectation{}, nil); err == nil {
		t.Fatal("missing creator accepted")
	}
	f = &fixtureSupervisorLaunch{}
	trigger := errors.New("readiness failed")
	err = receiveStartedSupervisor(context.Background(), f, SupervisorControlExpectation{}, func() error { return trigger })
	if !errors.Is(err, trigger) || f.aborts != 1 || f.handoffs != 0 {
		t.Fatalf("err=%v launch=%+v", err, f)
	}
	f = &fixtureSupervisorLaunch{}
	if err := receiveStartedSupervisor(context.Background(), f, SupervisorControlExpectation{}, func() error { return nil }); err != nil || f.aborts != 0 || f.handoffs != 1 {
		t.Fatalf("err=%v launch=%+v", err, f)
	}
}

type fixtureLaunchingUpgradeDeps struct {
	*fakeUpgradeDeps
	launch *fixtureSupervisorLaunch
}

func (f fixtureLaunchingUpgradeDeps) StartSupervisor(string) (SupervisorLaunch, error) {
	return f.launch, nil
}

func TestSupervisorLaunchRestoredPriorSurvivesOuterTrigger(t *testing.T) {
	launch := &fixtureSupervisorLaunch{}
	trigger := errors.New("original promotion failure")
	err := recoverUnpromotedUpgrade(context.Background(), UpgradeOpts{Deps: fixtureLaunchingUpgradeDeps{&fakeUpgradeDeps{}, launch}}, defaultSupervisorLockReleaseTimeout, "", trigger)
	if !errors.Is(err, trigger) || launch.handoffs != 1 || launch.aborts != 0 {
		t.Fatalf("outer error destroyed healthy prior: %v %+v", err, launch)
	}
}

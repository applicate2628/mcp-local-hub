package gui

import (
	"context"
	"errors"
	"testing"
	"time"

	"mcp-local-hub/internal/api"
	"mcp-local-hub/internal/daemonrecovery"
	"mcp-local-hub/internal/process"
)

func TestHubInitialBindFailureEnqueuesTypedRestartAndKeepsRecovering(t *testing.T) {
	s := NewServer(Config{Port: 0})
	s.signalInitialHubBindFailure()

	select {
	case cause := <-s.hubRestartCh:
		if cause != hubListenerRestartCauseInitialBindFailed {
			t.Fatalf("restart cause = %v, want initial-bind-failed", cause)
		}
	default:
		t.Fatal("initial hub bind failure did not enqueue the restart driver")
	}
	if got, action := s.hubHealth.snapshot(); got != HubHealthRecovering || action != "" {
		t.Fatalf("health after initial bind failure = state %q action %q, want recovering", got, action)
	}
}

func TestHubListenerRestartDriverInitialBindFailureRetriesFromNil(t *testing.T) {
	s := NewServer(Config{Port: 0})
	newComp := liveRestartTestComp(3439)
	events := make(chan hubRestartTestEvent, 8)
	var starts, shutdowns int

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		runHubListenerRestartDriver(ctx, s, hubListenerRestartDriverOptions{
			startFn: func(context.Context) (*HubListenerComponents, error) {
				starts++
				return newComp, nil
			},
			shutdownFn: func(context.Context, *HubListenerComponents) { shutdowns++ },
			emitFn: s.hubHealthEmitWrapper(func(level, event string, fields map[string]any) error {
				events <- hubRestartTestEvent{level: level, event: event, fields: fields}
				return nil
			}),
			sleepFn: func(context.Context, time.Duration) bool { return true },
			nowFn:   func() time.Time { return time.Unix(100, 0) },
		})
	}()

	s.signalInitialHubBindFailure()
	waitRestartTestEvent(t, events, "hub-listener-restarted")
	cancel()
	waitRestartDriverDone(t, done)

	if starts != 1 {
		t.Fatalf("start calls = %d, want 1", starts)
	}
	if shutdowns != 0 {
		t.Fatalf("shutdown calls from nil initial component = %d, want 0", shutdowns)
	}
	if got := s.hubMcpComp.Load(); got != newComp {
		t.Fatalf("published component = %#v, want %#v", got, newComp)
	}
	if got, action := s.hubHealth.snapshot(); got != HubHealthHealthy || action != "" {
		t.Fatalf("health after initial-bind retry = state %q action %q, want healthy", got, action)
	}
}

func TestHubInitialBindPortOwnerRotationGateReusesDaemonRecoveryClassifier(t *testing.T) {
	const (
		restartPort = 3439
		ownerPID    = 4242
	)
	descriptor := api.SupervisorDaemon{
		TaskName: `\mcp-local-hub-memory-default`,
		Server:   "memory",
		Daemon:   "default",
		Command:  `C:\Program Files\mcphub\mcphub.exe`,
		Args:     []string{"daemon", "--server", "memory", "--daemon", "default"},
		Port:     restartPort,
	}
	ownIdentity := process.ProcessIdentity{
		PID:            ownerPID,
		Basename:       "mcphub.exe",
		CommandLine:    `"C:\Program Files\mcphub\mcphub.exe" daemon --server memory --daemon default`,
		ExecutablePath: descriptor.Command,
	}

	tests := []struct {
		name              string
		portOwner         func(context.Context, int) (int, bool, error)
		lookupIdentity    func(context.Context, int) (process.ProcessIdentity, error)
		executableMatches bool
		wantRotate        bool
	}{
		{
			name: "no holder",
			portOwner: func(context.Context, int) (int, bool, error) {
				return 0, false, nil
			},
			wantRotate: false,
		},
		{
			name: "verified own stale daemon",
			portOwner: func(context.Context, int) (int, bool, error) {
				return ownerPID, true, nil
			},
			lookupIdentity: func(context.Context, int) (process.ProcessIdentity, error) {
				return ownIdentity, nil
			},
			executableMatches: true,
			wantRotate:        false,
		},
		{
			name: "foreign holder",
			portOwner: func(context.Context, int) (int, bool, error) {
				return ownerPID, true, nil
			},
			lookupIdentity: func(context.Context, int) (process.ProcessIdentity, error) {
				return ownIdentity, nil
			},
			executableMatches: false,
			wantRotate:        true,
		},
		{
			name: "unverifiable identity",
			portOwner: func(context.Context, int) (int, bool, error) {
				return ownerPID, true, nil
			},
			lookupIdentity: func(context.Context, int) (process.ProcessIdentity, error) {
				return process.ProcessIdentity{}, errors.New("identity unavailable")
			},
			executableMatches: true,
			wantRotate:        true,
		},
		{
			name: "unverifiable owner probe",
			portOwner: func(context.Context, int) (int, bool, error) {
				return 0, false, errors.New("owner probe unavailable")
			},
			wantRotate: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			deps := daemonrecovery.Dependencies{
				StateDir: func() (string, error) { return t.TempDir(), nil },
				ReadIntent: func(string) (*api.SupervisorIntentFile, error) {
					return &api.SupervisorIntentFile{Daemons: []api.SupervisorDaemon{descriptor}}, nil
				},
				PortOwner:      tt.portOwner,
				SelfPID:        func() int { return 1 },
				LookupIdentity: tt.lookupIdentity,
				ExecutableMatches: func(int, string) bool {
					return tt.executableMatches
				},
			}

			if got := hubInitialBindPortNeedsInstanceIDRotationWithDeps(context.Background(), restartPort, deps); got != tt.wantRotate {
				t.Fatalf("rotation gate = %v, want %v", got, tt.wantRotate)
			}
		})
	}
}

func TestHubListenerRestartDriverInitialBindForeignOwnerRotatesOnceAndNeedsReconcile(t *testing.T) {
	setupInitialHubPortDependencyTest(t)
	before, err := api.EnsureHubEndpoint(3439, 111)
	if err != nil {
		t.Fatalf("EnsureHubEndpoint before restart: %v", err)
	}

	s := NewServer(Config{Port: 0})
	newComp := liveRestartTestComp(before.Port)
	events := make(chan hubRestartTestEvent, 8)
	var probes, rotations int

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		runHubListenerRestartDriver(ctx, s, hubListenerRestartDriverOptions{
			startFn: func(context.Context) (*HubListenerComponents, error) {
				return newComp, nil
			},
			shutdownFn: func(context.Context, *HubListenerComponents) {},
			emitFn: s.hubHealthEmitWrapper(func(level, event string, fields map[string]any) error {
				events <- hubRestartTestEvent{level: level, event: event, fields: fields}
				return nil
			}),
			sleepFn: func(context.Context, time.Duration) bool { return true },
			nowFn:   func() time.Time { return time.Unix(100, 0) },
			initialBindPortNeedsRotationFn: func(context.Context, int) bool {
				probes++
				return true
			},
			rotateHubInstanceIDFn: func() (api.HubEndpoint, error) {
				rotations++
				return api.RotateHubInstanceID()
			},
		})
	}()

	s.signalInitialHubBindFailure()
	ev := waitRestartTestEvent(t, events, "hub-listener-restart-instance-id-changed")
	cancel()
	waitRestartDriverDone(t, done)

	if probes != 1 {
		t.Fatalf("owner probes = %d, want 1", probes)
	}
	if rotations != 1 {
		t.Fatalf("InstanceID rotations = %d, want 1", rotations)
	}
	after, err := api.LoadHubEndpoint()
	if err != nil {
		t.Fatalf("LoadHubEndpoint after restart: %v", err)
	}
	if after.InstanceID == before.InstanceID {
		t.Fatalf("InstanceID = %q, want rotation from %q", after.InstanceID, before.InstanceID)
	}
	if ev.fields["operator_action"] != hubReconcileOperatorAction {
		t.Fatalf("operator action = %v, want %q", ev.fields["operator_action"], hubReconcileOperatorAction)
	}
	if got, action := s.hubHealth.snapshot(); got != HubHealthNeedsReconcile || action != hubReconcileOperatorAction {
		t.Fatalf("health after adversarial retry = state %q action %q, want needs-reconcile + action", got, action)
	}
}

func TestHubListenerRestartDriverInitialBindOwnOrAbsentOwnerDoesNotRotateOrReconcile(t *testing.T) {
	setupInitialHubPortDependencyTest(t)
	before, err := api.EnsureHubEndpoint(3439, 111)
	if err != nil {
		t.Fatalf("EnsureHubEndpoint before restart: %v", err)
	}

	s := NewServer(Config{Port: 0})
	newComp := liveRestartTestComp(before.Port)
	events := make(chan hubRestartTestEvent, 8)
	var probes, rotations int

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		runHubListenerRestartDriver(ctx, s, hubListenerRestartDriverOptions{
			startFn: func(context.Context) (*HubListenerComponents, error) {
				return newComp, nil
			},
			shutdownFn: func(context.Context, *HubListenerComponents) {},
			emitFn: s.hubHealthEmitWrapper(func(level, event string, fields map[string]any) error {
				events <- hubRestartTestEvent{level: level, event: event, fields: fields}
				return nil
			}),
			sleepFn: func(context.Context, time.Duration) bool { return true },
			nowFn:   func() time.Time { return time.Unix(100, 0) },
			initialBindPortNeedsRotationFn: func(context.Context, int) bool {
				probes++
				return false
			},
			rotateHubInstanceIDFn: func() (api.HubEndpoint, error) {
				rotations++
				return api.HubEndpoint{}, errors.New("unexpected rotation")
			},
		})
	}()

	s.signalInitialHubBindFailure()
	ev := waitRestartTestEvent(t, events, "hub-listener-restarted")
	cancel()
	waitRestartDriverDone(t, done)

	if probes != 1 {
		t.Fatalf("owner probes = %d, want 1", probes)
	}
	if rotations != 0 {
		t.Fatalf("InstanceID rotations = %d, want 0", rotations)
	}
	if ev.fields["instance_id_preserved"] != true {
		t.Fatalf("instance_id_preserved = %v, want true", ev.fields["instance_id_preserved"])
	}
	after, err := api.LoadHubEndpoint()
	if err != nil {
		t.Fatalf("LoadHubEndpoint after restart: %v", err)
	}
	if after.InstanceID != before.InstanceID {
		t.Fatalf("InstanceID = %q, want preserved %q", after.InstanceID, before.InstanceID)
	}
	if got, action := s.hubHealth.snapshot(); got != HubHealthHealthy || action != "" {
		t.Fatalf("health after benign retry = state %q action %q, want healthy without reconcile", got, action)
	}
}

func TestHubListenerRestartDriverInitialBindFailureExhaustionEndsDown(t *testing.T) {
	s := NewServer(Config{Port: 0})
	events := make(chan hubRestartTestEvent, 64)
	var starts, shutdowns int

	done := make(chan struct{})
	go func() {
		defer close(done)
		runHubListenerRestartDriver(context.Background(), s, hubListenerRestartDriverOptions{
			startFn: func(context.Context) (*HubListenerComponents, error) {
				starts++
				return nil, errors.New("permanently unbindable")
			},
			shutdownFn: func(context.Context, *HubListenerComponents) { shutdowns++ },
			emitFn: s.hubHealthEmitWrapper(func(level, event string, fields map[string]any) error {
				events <- hubRestartTestEvent{level: level, event: event, fields: fields}
				return nil
			}),
			sleepFn: func(context.Context, time.Duration) bool { return true },
			nowFn:   func() time.Time { return time.Unix(100, 0) },
		})
	}()

	s.signalInitialHubBindFailure()
	waitRestartTestEvent(t, events, "hub-listener-restart-abandoned")
	waitRestartDriverDone(t, done)

	if starts != hubListenerRestartMaxAttemptsPerWindow {
		t.Fatalf("start calls = %d, want rolling-window cap %d", starts, hubListenerRestartMaxAttemptsPerWindow)
	}
	if shutdowns != 0 {
		t.Fatalf("shutdown calls from permanently nil component = %d, want 0", shutdowns)
	}
	if got, action := s.hubHealth.snapshot(); got != HubHealthDown || action != "" {
		t.Fatalf("health after bounded initial-bind exhaustion = state %q action %q, want down", got, action)
	}
}

func TestHubListenerRestartDriverNilComponentNonInitialCausesStop(t *testing.T) {
	for _, cause := range []hubListenerRestartCause{
		hubListenerRestartCauseUnresponsive,
		hubListenerRestartCause(255),
	} {
		t.Run(cause.String(), func(t *testing.T) {
			s := NewServer(Config{Port: 0})
			var starts, shutdowns int
			outcome := restartHubListenerWithOutcome(context.Background(), s, hubListenerRestartDriverOptions{
				cause: cause,
				startFn: func(context.Context) (*HubListenerComponents, error) {
					starts++
					return liveRestartTestComp(3439), nil
				},
				shutdownFn: func(context.Context, *HubListenerComponents) { shutdowns++ },
				emitFn:     func(string, string, map[string]any) error { return nil },
				sleepFn:    func(context.Context, time.Duration) bool { return true },
				nowFn:      func() time.Time { return time.Unix(100, 0) },
			})
			if outcome != hubListenerRestartStopDriver {
				t.Fatalf("outcome = %v, want stop-driver", outcome)
			}
			if starts != 0 || shutdowns != 0 {
				t.Fatalf("starts=%d shutdowns=%d, want zero for nil non-initial entry", starts, shutdowns)
			}
		})
	}
}

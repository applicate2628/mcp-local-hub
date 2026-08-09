package api

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"testing"
	"time"

	"mcp-local-hub/internal/clients"

	"gopkg.in/yaml.v3"
)

func TestMCPFrontRoutingLeaseBlocksTransitionAcrossOrdinaryMutation(t *testing.T) {
	path := filepath.Join(t.TempDir(), "settings.yaml")
	a := NewAPI()
	resolved := make(chan struct{})
	resumeWrite := make(chan struct{})
	writerDone := make(chan error, 1)
	go func() {
		writerDone <- a.WithClientRoutingAuthorityLeaseIn(context.Background(), path, CanonicalAtAdmission(), func(ClientRoutingTarget) error {
			close(resolved)
			<-resumeWrite
			return nil // deterministic stand-in for mutation plus durable readback
		})
	}()
	<-resolved
	transitionDone := make(chan error, 1)
	go func() {
		transitionDone <- a.TransitionMCPFrontRoutingEpochIn(path,
			MCPFrontRoutingTargetSnapshot{State: MCPFrontRoutingTargetGUI},
			MCPFrontRoutingTargetSnapshot{State: MCPFrontRoutingTargetFrontPreparing, Generation: 1, Port: DefaultMCPFrontPort})
	}()
	select {
	case err := <-transitionDone:
		t.Fatalf("transition interleaved after resolution but before ordinary mutation: %v", err)
	case <-time.After(100 * time.Millisecond):
	}
	close(resumeWrite)
	if err := <-writerDone; err != nil {
		t.Fatal(err)
	}
	if err := <-transitionDone; err != nil {
		t.Fatal(err)
	}
}

func TestWithMCPFrontRouteDaemonTargetLease_ProjectsAdmittedEpoch(t *testing.T) {
	for _, tc := range []struct {
		name    string
		yaml    string
		want    MCPFrontRouteDaemonTarget
		wantErr error
	}{
		{name: "stable gui uses requested port", yaml: "mcp_front.routing_target: gui\nmcp_front.port: \"9444\"\n", want: MCPFrontRouteDaemonTarget{State: MCPFrontRoutingTargetGUI, Port: 9444}},
		{name: "preparing uses admitted port", yaml: "mcp_front.routing_target: front-preparing\nmcp_front.routing_generation: \"7\"\nmcp_front.routing_admitted_port: \"9555\"\nmcp_front.port: \"9666\"\n", want: MCPFrontRouteDaemonTarget{State: MCPFrontRoutingTargetFrontPreparing, Generation: 7, Port: 9555}},
		{name: "stable front ignores requested drift", yaml: "mcp_front.routing_target: front\nmcp_front.routing_generation: \"7\"\nmcp_front.routing_admitted_port: \"9555\"\nmcp_front.port: \"9666\"\n", want: MCPFrontRouteDaemonTarget{State: MCPFrontRoutingTargetFront, Generation: 7, Port: 9555}},
		{name: "restoring uses admitted port", yaml: "mcp_front.routing_target: gui-restoring\nmcp_front.routing_generation: \"7\"\nmcp_front.routing_admitted_port: \"9555\"\nmcp_front.port: \"9666\"\n", want: MCPFrontRouteDaemonTarget{State: MCPFrontRoutingTargetGUIRestoring, Generation: 7, Port: 9555}},
		{name: "invalid fails closed", yaml: "mcp_front.routing_target: invalid\n", wantErr: ErrMCPFrontTargetInvalid},
		{name: "unbound fails closed", yaml: "mcp_front.routing_target: front\nmcp_front.routing_generation: \"7\"\n", wantErr: ErrMCPFrontRoutingPortUnbound},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "settings.yaml")
			if tc.yaml != "" {
				seedRoutingSettings(t, path, tc.yaml)
			}
			called := false
			var got MCPFrontRouteDaemonTarget
			err := NewAPI().WithMCPFrontRouteDaemonTargetLeaseIn(context.Background(), path, func(target MCPFrontRouteDaemonTarget) error {
				called = true
				got = target
				return nil
			})
			if tc.wantErr != nil {
				if !errors.Is(err, tc.wantErr) || called {
					t.Fatalf("err=%T %v called=%v, want %v without callback", err, err, called, tc.wantErr)
				}
				return
			}
			if err != nil || !called || got != tc.want {
				t.Fatalf("target=%+v called=%v err=%v, want %+v", got, called, err, tc.want)
			}
		})
	}
}

func TestWithMCPFrontRouteDaemonTargetLease_BlocksEpochTransitionUntilPersist(t *testing.T) {
	path := filepath.Join(t.TempDir(), "settings.yaml")
	a := NewAPI()
	insidePersist := make(chan struct{})
	releasePersist := make(chan struct{})
	seedDone := make(chan error, 1)
	go func() {
		seedDone <- a.WithMCPFrontRouteDaemonTargetLeaseIn(context.Background(), path, func(target MCPFrontRouteDaemonTarget) error {
			if target.State != MCPFrontRoutingTargetGUI {
				return fmt.Errorf("initial target=%+v, want GUI", target)
			}
			close(insidePersist)
			<-releasePersist
			return nil
		})
	}()
	<-insidePersist
	transitionDone := make(chan error, 1)
	go func() {
		transitionDone <- a.TransitionMCPFrontRoutingEpochIn(path,
			MCPFrontRoutingTargetSnapshot{State: MCPFrontRoutingTargetGUI},
			MCPFrontRoutingTargetSnapshot{State: MCPFrontRoutingTargetFrontPreparing, Generation: 1, Port: DefaultMCPFrontPort})
	}()
	select {
	case err := <-transitionDone:
		t.Fatalf("epoch transition interleaved before descriptor persist completed: %v", err)
	case <-time.After(100 * time.Millisecond):
	}
	close(releasePersist)
	if err := <-seedDone; err != nil {
		t.Fatal(err)
	}
	if err := <-transitionDone; err != nil {
		t.Fatal(err)
	}
}

func TestMCPFrontRoutingLeaseReleasesOnFailureCancellationAndRetry(t *testing.T) {
	path := filepath.Join(t.TempDir(), "settings.yaml")
	a := NewAPI()
	sentinel := errors.New("synthetic ordinary mutation failure")
	if err := a.WithClientRoutingAuthorityLeaseIn(context.Background(), path, CanonicalAtAdmission(), func(ClientRoutingTarget) error {
		return sentinel
	}); !errors.Is(err, sentinel) {
		t.Fatalf("callback failure=%v, want sentinel", err)
	}
	preparing := MCPFrontRoutingTargetSnapshot{State: MCPFrontRoutingTargetFrontPreparing, Generation: 1, Port: DefaultMCPFrontPort}
	if err := a.TransitionMCPFrontRoutingEpochIn(path, MCPFrontRoutingTargetSnapshot{State: MCPFrontRoutingTargetGUI}, preparing); err != nil {
		t.Fatalf("failure leaked shared lease: %v", err)
	}

	holderReady := make(chan struct{})
	releaseHolder := make(chan struct{})
	holderDone := make(chan error, 1)
	go func() {
		holderDone <- withMCPFrontRoutingFileLease(context.Background(), path, true, func() error {
			close(holderReady)
			<-releaseHolder
			return nil
		})
	}()
	<-holderReady
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := a.WithClientRoutingAuthorityLeaseIn(ctx, path, CanonicalAtAdmission(), func(ClientRoutingTarget) error {
		t.Fatal("cancelled waiter entered mutation callback")
		return nil
	}); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled acquisition=%v, want context.Canceled", err)
	}
	close(releaseHolder)
	if err := <-holderDone; err != nil {
		t.Fatal(err)
	}
	if err := a.TransitionMCPFrontRoutingEpochIn(path, preparing, MCPFrontRoutingTargetSnapshot{State: MCPFrontRoutingTargetFront, Generation: 1, Port: DefaultMCPFrontPort}); err != nil {
		t.Fatalf("settle after cancellation: %v", err)
	}
	retried := false
	if err := a.WithClientRoutingAuthorityLeaseIn(context.Background(), path, CanonicalAtAdmission(), func(ClientRoutingTarget) error {
		retried = true
		return nil
	}); err != nil || !retried {
		t.Fatalf("retry after cancellation: entered=%v err=%v", retried, err)
	}
}

func TestMCPFrontRoutingLeaseRejectsStaleRequestedTarget(t *testing.T) {
	path := filepath.Join(t.TempDir(), "settings.yaml")
	a := NewAPI()
	stale := ClientRoutingTarget{Mode: MCPFrontRoutingTargetGUI, Port: 9125}
	preparing := MCPFrontRoutingTargetSnapshot{State: MCPFrontRoutingTargetFrontPreparing, Generation: 3, Port: DefaultMCPFrontPort}
	if err := a.TransitionMCPFrontRoutingEpochIn(path, MCPFrontRoutingTargetSnapshot{State: MCPFrontRoutingTargetGUI}, preparing); err != nil {
		t.Fatal(err)
	}
	if err := a.TransitionMCPFrontRoutingEpochIn(path, preparing, MCPFrontRoutingTargetSnapshot{State: MCPFrontRoutingTargetFront, Generation: 3, Port: DefaultMCPFrontPort}); err != nil {
		t.Fatal(err)
	}
	called := false
	err := a.WithClientRoutingAuthorityLeaseIn(context.Background(), path, ExactTarget(stale), func(ClientRoutingTarget) error {
		called = true
		return nil
	})
	if !errors.Is(err, ErrMCPFrontTargetConflict) || called {
		t.Fatalf("stale requested target err=%T %v called=%v", err, err, called)
	}
}

func TestMCPFrontRoutingAuthorityKeepsLegacyEndpointOutsideStableGUIFence(t *testing.T) {
	path := filepath.Join(t.TempDir(), "settings.yaml")
	a := NewAPI()
	legacyPort := 7777
	canonical := ClientRoutingTarget{}
	if err := a.WithClientRoutingAuthorityLeaseIn(context.Background(), path, StableGUICompatibility(), func(target ClientRoutingTarget) error {
		canonical = target
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if canonical.Port != 9125 || legacyPort != 7777 {
		t.Fatalf("canonical port=%d legacy port=%d, want independent 9125 and 7777", canonical.Port, legacyPort)
	}
	preparing := MCPFrontRoutingTargetSnapshot{State: MCPFrontRoutingTargetFrontPreparing, Generation: 4, Port: DefaultMCPFrontPort}
	if err := a.TransitionMCPFrontRoutingEpochIn(path, MCPFrontRoutingTargetSnapshot{State: MCPFrontRoutingTargetGUI}, preparing); err != nil {
		t.Fatal(err)
	}
	if err := a.TransitionMCPFrontRoutingEpochIn(path, preparing, MCPFrontRoutingTargetSnapshot{State: MCPFrontRoutingTargetFront, Generation: 4, Port: DefaultMCPFrontPort}); err != nil {
		t.Fatal(err)
	}
	called := false
	err := a.WithClientRoutingAuthorityLeaseIn(context.Background(), path, StableGUICompatibility(), func(ClientRoutingTarget) error {
		called = true
		return nil
	})
	if !errors.Is(err, ErrMCPFrontTargetConflict) || called {
		t.Fatalf("legacy request after front settle: called=%v err=%T %v", called, err, err)
	}
}

func TestMCPFrontTransitionBlocksOrdinaryWriters(t *testing.T) {
	t.Setenv("LOCALAPPDATA", t.TempDir())
	seedRoutingSettings(t, SettingsPath(), "mcp_front.routing_target: front-preparing\nmcp_front.routing_generation: \"41\"\nmcp_front.routing_admitted_port: \"9137\"\n")
	a := NewAPI()
	if _, err := a.PlanLSPRouterClientEntries(LSPClientRouterOpts{Languages: []string{"go"}, Clients: map[string]clients.Client{}}); !errors.Is(err, ErrMCPFrontTransitionActive) {
		t.Fatalf("LSP writer error=%T %v, want transition-active", err, err)
	}
	if err := a.Install(InstallOpts{Server: "serena", DryRun: true}); !errors.Is(err, ErrMCPFrontTransitionActive) {
		t.Fatalf("install writer error=%T %v, want transition-active", err, err)
	}
	transition := ClientRoutingTarget{Mode: MCPFrontRoutingTargetFrontPreparing, Port: DefaultMCPFrontPort, Generation: 41}
	if _, err := ReconcileSerenaClientsToRouter(context.Background(), SerenaReconcileOpts{
		RoutingTarget: &transition,
		Ping: func(context.Context, int) error {
			t.Fatal("transitioning Serena target reached liveness probe")
			return nil
		},
		Clients: map[string]clients.Client{},
	}); !errors.Is(err, ErrMCPFrontTransitionActive) {
		t.Fatalf("Serena writer error=%T %v, want transition-active", err, err)
	}
}

func TestAllClientWritersConsumeCanonicalRoutingTarget(t *testing.T) {
	t.Setenv("LOCALAPPDATA", t.TempDir())
	seedRoutingSettings(t, SettingsPath(), "mcp_front.routing_target: front\nmcp_front.routing_generation: \"17\"\nmcp_front.routing_admitted_port: \"9666\"\nmcp_front.port: \"9777\"\n")
	a := NewAPI()
	lspPlan, err := a.PlanLSPRouterClientEntries(LSPClientRouterOpts{Languages: []string{"go"}, Clients: map[string]clients.Client{}})
	if err != nil || lspPlan.Port != 9666 || lspPlan.opts.RoutingTarget == nil || lspPlan.opts.RoutingTarget.Generation != 17 {
		t.Fatalf("LSP canonical target plan=%+v err=%v", lspPlan, err)
	}
	installOpts := InstallOpts{}
	installAuthority, err := a.prepareInstallClientRoutingDecision(&installOpts)
	if err != nil || installOpts.RoutingTarget == nil || *installOpts.RoutingTarget != (ClientRoutingTarget{Mode: MCPFrontRoutingTargetFront, Port: 9666, Generation: 17}) || installAuthority.kind != clientRoutingAuthorityExact || installAuthority.exact != *installOpts.RoutingTarget {
		t.Fatalf("install canonical opts=%+v authority=%+v err=%v", installOpts, installAuthority, err)
	}
	target, err := a.ResolveClientRoutingTarget()
	if err != nil {
		t.Fatalf("resolve Serena composition target: %v", err)
	}
	seenPort := 0
	port, err := resolveSerenaReconcilePort(context.Background(), SerenaReconcileOpts{
		RoutingTarget: &target,
		Ping: func(_ context.Context, port int) error {
			seenPort = port
			return nil
		},
	})
	if err != nil || port != 9666 || seenPort != 9666 {
		t.Fatalf("Serena canonical target port=%d seen=%d err=%v", port, seenPort, err)
	}
}

func TestPrepareInstallClientRoutingDecision_BoundGUIWinsOnlyInStableGUI(t *testing.T) {
	t.Run("stable GUI retains the explicitly bound listener", func(t *testing.T) {
		t.Setenv("LOCALAPPDATA", t.TempDir())
		opts := InstallOpts{GUIPort: 19425}
		authority, err := NewAPI().prepareInstallClientRoutingDecision(&opts)
		if err != nil {
			t.Fatalf("prepare stable GUI: %v", err)
		}
		if authority.kind != clientRoutingAuthorityStableGUICompatibility || opts.GUIPort != 19425 || opts.RoutingTarget != nil {
			t.Fatalf("stable GUI opts=%+v authority=%+v", opts, authority)
		}
	})

	t.Run("settled front replaces the GUI transport hint", func(t *testing.T) {
		t.Setenv("LOCALAPPDATA", t.TempDir())
		seedRoutingSettings(t, SettingsPath(), "mcp_front.routing_target: front\nmcp_front.routing_generation: \"9\"\nmcp_front.routing_admitted_port: \"9666\"\nmcp_front.port: \"9777\"\n")
		opts := InstallOpts{GUIPort: 19425}
		authority, err := NewAPI().prepareInstallClientRoutingDecision(&opts)
		if err != nil {
			t.Fatalf("prepare settled front: %v", err)
		}
		want := ClientRoutingTarget{Mode: MCPFrontRoutingTargetFront, Port: 9666, Generation: 9}
		if authority.kind != clientRoutingAuthorityExact || authority.exact != want || opts.RoutingTarget == nil || *opts.RoutingTarget != want || opts.GUIPort != want.Port {
			t.Fatalf("settled front opts=%+v authority=%+v, want target %+v", opts, authority, want)
		}
	})

	t.Run("transition fails before a client write can be admitted", func(t *testing.T) {
		t.Setenv("LOCALAPPDATA", t.TempDir())
		seedRoutingSettings(t, SettingsPath(), "mcp_front.routing_target: front-preparing\nmcp_front.routing_generation: \"9\"\nmcp_front.routing_admitted_port: \"9666\"\n")
		opts := InstallOpts{GUIPort: 19425}
		if _, err := NewAPI().prepareInstallClientRoutingDecision(&opts); !errors.Is(err, ErrMCPFrontTransitionActive) {
			t.Fatalf("prepare transition error=%T %v, want transition-active", err, err)
		}
	})
}

func TestResolveClientRoutingTargetIn(t *testing.T) {
	tests := []struct {
		name       string
		yaml       string
		want       ClientRoutingTarget
		wantErr    error
		transition bool
	}{
		{name: "missing_is_legacy_gui", want: ClientRoutingTarget{Mode: MCPFrontRoutingTargetGUI, Port: 9125}},
		{name: "stable_gui_custom_port", yaml: "mcp_front.routing_target: gui\ngui_server.port: \"9444\"\n", want: ClientRoutingTarget{Mode: MCPFrontRoutingTargetGUI, Port: 9444}},
		{name: "stable_front_ignores_requested_port", yaml: "mcp_front.routing_target: front\nmcp_front.routing_generation: \"7\"\nmcp_front.routing_admitted_port: \"9555\"\nmcp_front.port: \"9666\"\n", want: ClientRoutingTarget{Mode: MCPFrontRoutingTargetFront, Port: 9555, Generation: 7}},
		{name: "front_preparing_blocks", yaml: "mcp_front.routing_target: front-preparing\nmcp_front.routing_generation: \"7\"\nmcp_front.routing_admitted_port: \"9555\"\n", wantErr: ErrMCPFrontTransitionActive, transition: true},
		{name: "gui_restoring_blocks", yaml: "mcp_front.routing_target: gui-restoring\nmcp_front.routing_generation: \"7\"\nmcp_front.routing_admitted_port: \"9555\"\n", wantErr: ErrMCPFrontTransitionActive, transition: true},
		{name: "invalid_target_fails_closed", yaml: "mcp_front.routing_target: guessed\n", wantErr: ErrMCPFrontTargetInvalid},
		{name: "front_without_generation_fails_closed", yaml: "mcp_front.routing_target: front\n", wantErr: ErrMCPFrontTargetInvalid},
		{name: "front_without_admitted_port_fails_closed", yaml: "mcp_front.routing_target: front\nmcp_front.routing_generation: \"3\"\n", wantErr: ErrMCPFrontRoutingPortUnbound},
		{name: "invalid_admitted_port_fails_closed", yaml: "mcp_front.routing_target: front\nmcp_front.routing_generation: \"3\"\nmcp_front.routing_admitted_port: nope\n", wantErr: ErrMCPFrontTargetInvalid},
		{name: "orphan_generation_fails_closed", yaml: "mcp_front.routing_generation: \"9\"\n", wantErr: ErrMCPFrontTargetInvalid},
		{name: "orphan_admitted_port_fails_closed", yaml: "mcp_front.routing_admitted_port: \"9555\"\n", wantErr: ErrMCPFrontTargetInvalid},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "settings.yaml")
			if test.yaml != "" {
				seedRoutingSettings(t, path, test.yaml)
			}
			got, err := NewAPI().ResolveClientRoutingTargetIn(path)
			if test.wantErr != nil {
				if !errors.Is(err, test.wantErr) {
					t.Fatalf("error=%T %v, want errors.Is(%v)", err, err, test.wantErr)
				}
				if test.transition {
					var detail *MCPFrontTransitionActiveError
					if !errors.As(err, &detail) || detail.Generation != 7 {
						t.Fatalf("transition detail=%+v err=%v", detail, err)
					}
				}
				return
			}
			if err != nil || got != test.want {
				t.Fatalf("target=%+v err=%v, want %+v", got, err, test.want)
			}
		})
	}
}

func TestTransitionMCPFrontRoutingTargetIn_LegalLifecycleAndReadOnlyRegistry(t *testing.T) {
	path := filepath.Join(t.TempDir(), "settings.yaml")
	a := NewAPI()
	if err := a.SettingsSetIn(path, MCPFrontRoutingTargetSettingKey, string(MCPFrontRoutingTargetFront)); err == nil {
		t.Fatal("generic settings mutation accepted the transaction-owned routing target")
	}
	gui := MCPFrontRoutingTargetSnapshot{State: MCPFrontRoutingTargetGUI}
	preparing := MCPFrontRoutingTargetSnapshot{State: MCPFrontRoutingTargetFrontPreparing, Generation: 11, Port: DefaultMCPFrontPort}
	front := MCPFrontRoutingTargetSnapshot{State: MCPFrontRoutingTargetFront, Generation: 11, Port: DefaultMCPFrontPort}
	restoring := MCPFrontRoutingTargetSnapshot{State: MCPFrontRoutingTargetGUIRestoring, Generation: 11, Port: DefaultMCPFrontPort}
	if err := a.TransitionMCPFrontRoutingEpochIn(path, gui, preparing); err != nil {
		t.Fatalf("gui -> front-preparing: %v", err)
	}
	if err := a.TransitionMCPFrontRoutingEpochIn(path, preparing, front); err != nil {
		t.Fatalf("front-preparing -> front: %v", err)
	}
	target, err := a.ResolveClientRoutingTargetIn(path)
	if err != nil || target != (ClientRoutingTarget{Mode: MCPFrontRoutingTargetFront, Port: DefaultMCPFrontPort, Generation: 11}) {
		t.Fatalf("settled front target=%+v err=%v", target, err)
	}
	if err := a.TransitionMCPFrontRoutingEpochIn(path, front, restoring); err != nil {
		t.Fatalf("front -> gui-restoring: %v", err)
	}
	if err := a.TransitionMCPFrontRoutingEpochIn(path, restoring, gui); err != nil {
		t.Fatalf("gui-restoring -> gui: %v", err)
	}
	snapshot, err := a.MCPFrontRoutingTargetSnapshotIn(path)
	if err != nil || snapshot != (MCPFrontRoutingTargetSnapshot{State: MCPFrontRoutingTargetGUI}) {
		t.Fatalf("restored snapshot=%+v err=%v", snapshot, err)
	}
}

func TestMCPFrontTargetCASRejectsConcurrentGeneration(t *testing.T) {
	path := filepath.Join(t.TempDir(), "settings.yaml")
	a := NewAPI()
	type result struct {
		generation int
		err        error
	}
	start := make(chan struct{})
	results := make(chan result, 2)
	var workers sync.WaitGroup
	for _, generation := range []int{21, 22} {
		generation := generation
		workers.Add(1)
		go func() {
			defer workers.Done()
			<-start
			results <- result{generation: generation, err: a.TransitionMCPFrontRoutingEpochIn(path,
				MCPFrontRoutingTargetSnapshot{State: MCPFrontRoutingTargetGUI},
				MCPFrontRoutingTargetSnapshot{State: MCPFrontRoutingTargetFrontPreparing, Generation: generation, Port: 9100 + generation})}
		}()
	}
	close(start)
	workers.Wait()
	close(results)

	successes := 0
	winner := 0
	conflicts := 0
	for result := range results {
		if result.err == nil {
			successes++
			winner = result.generation
			continue
		}
		if !errors.Is(result.err, ErrMCPFrontTargetConflict) {
			t.Fatalf("loser error=%T %v, want target conflict", result.err, result.err)
		}
		conflicts++
	}
	if successes != 1 || conflicts != 1 {
		t.Fatalf("successes=%d conflicts=%d, want one each", successes, conflicts)
	}
	snapshot, err := a.MCPFrontRoutingTargetSnapshotIn(path)
	if err != nil || snapshot != (MCPFrontRoutingTargetSnapshot{State: MCPFrontRoutingTargetFrontPreparing, Generation: winner, Port: 9100 + winner}) {
		t.Fatalf("snapshot=%+v err=%v, want winning generation %d", snapshot, err, winner)
	}
	other := 21
	if winner == other {
		other = 22
	}
	if err := a.TransitionMCPFrontRoutingEpochIn(path,
		MCPFrontRoutingTargetSnapshot{State: MCPFrontRoutingTargetFrontPreparing, Generation: other, Port: 9100 + other},
		MCPFrontRoutingTargetSnapshot{State: MCPFrontRoutingTargetFront, Generation: other, Port: 9100 + other}); !errors.Is(err, ErrMCPFrontTargetConflict) {
		t.Fatalf("wrong generation settled transition: %T %v", err, err)
	}
}

func TestMCPFrontRoutingEpoch_StableFrontIgnoresRequestedPortUntilAdmission(t *testing.T) {
	path := filepath.Join(t.TempDir(), "settings.yaml")
	seedRoutingSettings(t, path, "mcp_front.routing_target: front\nmcp_front.routing_generation: \"7\"\nmcp_front.routing_admitted_port: \"9555\"\nmcp_front.port: \"9666\"\n")
	a := NewAPI()
	before, err := a.ResolveClientRoutingTargetIn(path)
	if err != nil || before.Port != 9555 {
		t.Fatalf("before admission target=%+v err=%v", before, err)
	}
	if err := a.SettingsSetIn(path, MCPFrontPortSettingKey, "9777"); err != nil {
		t.Fatal(err)
	}
	stillOld, err := a.ResolveClientRoutingTargetIn(path)
	if err != nil || stillOld.Port != 9555 {
		t.Fatalf("requested setting silently changed routing authority: target=%+v err=%v", stillOld, err)
	}
	oldEpoch := MCPFrontRoutingTargetSnapshot{State: MCPFrontRoutingTargetFront, Generation: 7, Port: 9555}
	newPreparing := MCPFrontRoutingTargetSnapshot{State: MCPFrontRoutingTargetFrontPreparing, Generation: 8, Port: 9777}
	if err := a.TransitionMCPFrontRoutingEpochIn(path, oldEpoch, newPreparing); err != nil {
		t.Fatalf("admit rebase: %v", err)
	}
	if _, err := a.ResolveClientRoutingTargetIn(path); !errors.Is(err, ErrMCPFrontTransitionActive) {
		t.Fatalf("ordinary writer after epoch commit err=%T %v, want transition active", err, err)
	}
	if err := a.TransitionMCPFrontRoutingEpochIn(path, newPreparing, MCPFrontRoutingTargetSnapshot{State: MCPFrontRoutingTargetFront, Generation: 8, Port: 9777}); err != nil {
		t.Fatal(err)
	}
	after, err := a.ResolveClientRoutingTargetIn(path)
	if err != nil || after.Port != 9777 || after.Generation != 8 {
		t.Fatalf("settled target=%+v err=%v", after, err)
	}
}

func TestMCPFrontRoutingEpoch_MissingPortRequiresExactMigrationBinding(t *testing.T) {
	path := filepath.Join(t.TempDir(), "settings.yaml")
	seedRoutingSettings(t, path, "mcp_front.routing_target: front\nmcp_front.routing_generation: \"7\"\nmcp_front.port: \"9999\"\n")
	a := NewAPI()
	if _, err := a.ResolveClientRoutingTargetIn(path); !errors.Is(err, ErrMCPFrontRoutingPortUnbound) {
		t.Fatalf("ordinary resolve err=%T %v, want port unbound", err, err)
	}
	unbound, err := a.MCPFrontRoutingTargetSnapshotForMigrationIn(path)
	if err != nil || unbound != (MCPFrontRoutingTargetSnapshot{State: MCPFrontRoutingTargetFront, Generation: 7}) {
		t.Fatalf("migration snapshot=%+v err=%v", unbound, err)
	}
	bound := MCPFrontRoutingTargetSnapshot{State: MCPFrontRoutingTargetFront, Generation: 7, Port: 9555}
	if err := a.TransitionMCPFrontRoutingEpochIn(path, unbound, bound); err != nil {
		t.Fatalf("bind admitted port: %v", err)
	}
	target, err := a.ResolveClientRoutingTargetIn(path)
	if err != nil || target.Port != 9555 {
		t.Fatalf("bound target=%+v err=%v", target, err)
	}
}

func TestMCPFrontRoutingEpoch_RejectsGenerationGapAndSameGenerationPortDrift(t *testing.T) {
	path := filepath.Join(t.TempDir(), "settings.yaml")
	seedRoutingSettings(t, path, "mcp_front.routing_target: front\nmcp_front.routing_generation: \"7\"\nmcp_front.routing_admitted_port: \"9555\"\n")
	a := NewAPI()
	old := MCPFrontRoutingTargetSnapshot{State: MCPFrontRoutingTargetFront, Generation: 7, Port: 9555}
	for _, next := range []MCPFrontRoutingTargetSnapshot{
		{State: MCPFrontRoutingTargetFrontPreparing, Generation: 7, Port: 9666},
		{State: MCPFrontRoutingTargetFrontPreparing, Generation: 9, Port: 9666},
	} {
		if err := a.TransitionMCPFrontRoutingEpochIn(path, old, next); !errors.Is(err, ErrMCPFrontTargetInvalid) {
			t.Fatalf("transition %+v err=%T %v, want invalid", next, err, err)
		}
	}
	snapshot, err := a.MCPFrontRoutingTargetSnapshotIn(path)
	if err != nil || snapshot != old {
		t.Fatalf("illegal transition changed epoch: %+v err=%v", snapshot, err)
	}
}

func TestMCPFrontRoutingEpoch_EmitsOneCommittedSettlementEventAndNoneForRejectedNPlusOne(t *testing.T) {
	stateDir := hubMcpStateTestHelper(t)
	path := filepath.Join(t.TempDir(), "settings.yaml")
	a := NewAPI()
	gui := MCPFrontRoutingTargetSnapshot{State: MCPFrontRoutingTargetGUI}
	preparing := MCPFrontRoutingTargetSnapshot{State: MCPFrontRoutingTargetFrontPreparing, Generation: 1, Port: DefaultMCPFrontPort}
	front := MCPFrontRoutingTargetSnapshot{State: MCPFrontRoutingTargetFront, Generation: 1, Port: DefaultMCPFrontPort}
	crossPortPreparing := MCPFrontRoutingTargetSnapshot{State: MCPFrontRoutingTargetFrontPreparing, Generation: 2, Port: DefaultMCPFrontPort + 1}
	if err := a.TransitionMCPFrontRoutingEpochIn(path, gui, preparing); err != nil {
		t.Fatalf("initial admission: %v", err)
	}
	if err := a.TransitionMCPFrontRoutingEpochIn(path, preparing, front); err != nil {
		t.Fatalf("settle initial generation: %v", err)
	}
	before, err := os.ReadFile(filepath.Join(stateDir, hubMcpLogFileLeaf))
	if err != nil {
		t.Fatalf("read pre-admission event log: %v", err)
	}
	if err := a.TransitionMCPFrontRoutingEpochIn(path, front, crossPortPreparing); err != nil {
		t.Fatalf("cross-port admission: %v", err)
	}
	if err := a.TransitionMCPFrontRoutingEpochIn(path, crossPortPreparing, MCPFrontRoutingTargetSnapshot{State: MCPFrontRoutingTargetFrontPreparing, Generation: 3, Port: crossPortPreparing.Port}); !errors.Is(err, ErrMCPFrontTargetInvalid) {
		t.Fatalf("same-port N+1 transition error=%T %v, want invalid", err, err)
	}
	logBytes, err := os.ReadFile(filepath.Join(stateDir, hubMcpLogFileLeaf))
	if err != nil {
		t.Fatalf("read committed-event log: %v", err)
	}
	newEvents := logBytes[len(before):]
	if got := bytes.Count(newEvents, []byte(`"event":"mcp-front-routing-target-settled"`)); got != 1 {
		t.Fatalf("cross-port committed settlement events=%d, want exactly one; events=%s", got, newEvents)
	}
	if !bytes.Contains(newEvents, []byte(`"old_generation":1`)) ||
		!bytes.Contains(newEvents, []byte(`"new_generation":2`)) ||
		!bytes.Contains(newEvents, []byte(`"admitted_port":`+strconv.Itoa(crossPortPreparing.Port))) {
		t.Fatalf("cross-port committed event lacks exact epoch fields: events=%s", newEvents)
	}
}

func TestSettingsListInRejectsCorruptReadOnlyRoutingTarget(t *testing.T) {
	path := filepath.Join(t.TempDir(), "settings.yaml")
	seedRoutingSettings(t, path, "mcp_front.routing_target: corrupt\n")
	if _, err := NewAPI().SettingsListIn(path); err == nil {
		t.Fatal("SettingsListIn silently replaced corrupt transaction-owned state with its default")
	}
}

func seedRoutingSettings(t *testing.T, path, body string) {
	t.Helper()
	values := map[string]string{}
	if err := yaml.Unmarshal([]byte(body), &values); err != nil {
		t.Fatalf("decode seed settings: %v", err)
	}
	if err := mutateRawSettingsMapLocked(path, func(raw map[string]string) error {
		for key, value := range values {
			raw[key] = value
		}
		return nil
	}); err != nil {
		t.Fatalf("seed hardened settings: %v", err)
	}
}

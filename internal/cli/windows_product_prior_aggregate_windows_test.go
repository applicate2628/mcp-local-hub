package cli

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/windows"

	"mcp-local-hub/internal/api"
	"mcp-local-hub/internal/api/apitest"
	"mcp-local-hub/internal/binaryadmission"
	"mcp-local-hub/internal/process"
)

func TestPriorServingAggregateMatchesTrackedRoute(t *testing.T) {
	path := filepath.Join(t.TempDir(), "mcphub.exe")
	owner := api.SupervisorLockOwner{PID: 19, StartedAt: time.Now().UTC().Format(time.RFC3339Nano)}
	d := api.BuildBuiltinRouteDaemon(path, 54321)
	row := api.DaemonStatus{Server: d.Server, Daemon: d.Daemon, TaskName: d.TaskName, CommandInternal: path,
		ArgsInternal: d.Args, StartedAtInternal: owner.StartedAt, State: "Running", Port: d.Port, PID: owner.PID}
	argv := `"` + path + `" route --port 54321`
	if err := matchServingRoute(row, path, owner, argv); err != nil {
		t.Fatal(err)
	}
	for _, change := range []func(*api.DaemonStatus){
		func(r *api.DaemonStatus) { r.PID++ },
		func(r *api.DaemonStatus) { r.StartedAtInternal = time.Now().Add(-time.Hour).Format(time.RFC3339Nano) },
		func(r *api.DaemonStatus) { r.CommandInternal = path + ".different" },
		func(r *api.DaemonStatus) { r.ArgsInternal = []string{"route", "--port", "54322"} },
		func(r *api.DaemonStatus) { r.TaskName += "-foreign" },
	} {
		changed := row
		change(&changed)
		if err := matchServingRoute(changed, path, owner, argv); err == nil {
			t.Fatal("other tracked Route identity accepted")
		}
	}
	if err := matchServingRoute(row, path, owner, `"`+path+`" tray`); err == nil {
		t.Fatal("another live role accepted as Route")
	}
}

func TestServingAggregateReapPinsOriginalOperations(t *testing.T) {
	for _, scenario := range []string{"changed-owner-after-quiesce", "wrong-native-peer-before-quiesce"} {
		t.Run(scenario, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), 25*time.Second)
			defer cancel()
			deadline, _ := ctx.Deadline()
			stateDir := apitest.HardenedTempDir(t)
			child := startBoundStatusChildAt(t, ctx, deadline, 0, stateDir, true)
			owner := api.SupervisorLockOwner{PID: child.peer.PID, StartedAt: child.peer.CreationTime}
			held, err := process.HoldPIDForObservation(owner.PID)
			if err != nil {
				t.Fatal(err)
			}
			defer func() {
				if err := held.Close(); err != nil {
					t.Error(err)
				}
			}()
			p := &priorAggregateProcesses{supervisor: held, owner: owner, peer: child.peer}
			deps := buildV5UpgradeDeps(child.peer.ImagePath, stateDir)
			deps.originalContext = ctx
			r := &servingAggregateReap{v5UpgradeDeps: deps, stateDir: stateDir, live: p}
			if scenario == "wrong-native-peer-before-quiesce" {
				p.peer.SessionID++
			}
			resp, first := r.QuiesceTimers(ctx, deps.pipePath, 5000)
			marker := filepath.Join(stateDir, "aggregate-quiesce-observed")
			if scenario == "changed-owner-after-quiesce" {
				if first != nil || !resp.OK || !resp.Final {
					t.Fatalf("original quiesce was not received: %+v %v", resp, first)
				}
				if _, err := os.Stat(marker); err != nil {
					t.Fatal(err)
				}
				replacement, err := api.ObserveSupervisorProcessIdentityV1(os.Getpid())
				if err != nil {
					t.Fatal(err)
				}
				if err := api.WriteStateFileAtomic(filepath.Join(stateDir, "supervisor.lock.owner.json"), api.SupervisorLockOwner{PID: replacement.PID, StartedAt: replacement.CreationTime}); err != nil {
					t.Fatal(err)
				}
			} else if !errors.Is(first, api.ErrSupervisorControlPeerIdentity) {
				t.Fatalf("expected quiesce to reject the wrong native peer: %v", first)
			} else if _, err := os.Stat(marker); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("wrong native peer received a write: %v", err)
			}
			if _, err := r.ExitGraceful(ctx, deps.pipePath, 5000); err == nil {
				t.Fatal("exit accepted after original owner/peer changed")
			}
			if err := r.ForceKillSupervisor(deps.pipePath); err == nil {
				t.Fatal("force accepted after original owner/peer changed")
			}
			if wait, err := held.Wait(); err != nil || wait.Result != uint32(windows.WAIT_TIMEOUT) {
				t.Fatalf("original child was affected: %+v %v", wait, err)
			}
			if _, err := os.Stat(filepath.Join(stateDir, "aggregate-exit-observed")); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("exit request reached wrong generation: %v", err)
			}
		})
	}
}

// Uses the real V2 file bank and private Windows namespace without executing
// its Product. The process that owns the test socket has no Product admission.
func TestPriorAggregateNativeReservationAndVacancy(t *testing.T) {
	input, snapshot, _ := v2TxnFixtureInput(t)
	ctx := t.Context()
	profile := apitest.HardenedTempDir(t)
	state := apitest.HardenedDir(t, filepath.Join(profile, "mcp-local-hub"))
	target := apitest.HardenedDir(t, filepath.Join(profile, "target"))
	t.Setenv("LOCALAPPDATA", profile)
	t.Setenv("MCPHUB_GUI_TEST_PIDPORT_DIR", "")
	t.Cleanup(api.SetDaemonStateRootForTest(state))
	dst, err := binaryadmission.CanonicalEntryTargetsV2(filepath.Join(target, binaryadmission.WindowsCanonicalEntryRelativePathV1))
	if err != nil {
		t.Fatal(err)
	}
	originals := v2TxnSeedPrior(t, input, dst, snapshot)
	source, err := binaryadmission.AcquireWindowsCanonicalEntrySetV2(ctx, input, snapshot)
	if source != nil {
		t.Cleanup(func() {
			if err := source.Close(); err != nil {
				t.Error(err)
			}
		})
	}
	if err != nil {
		t.Fatal(err)
	}
	carrier, err := stageCurrentEntryBundleForUpgrade(ctx, source, dst.CanonicalEntryPath)
	if carrier != nil {
		t.Cleanup(func() {
			if err := carrier.Close(); err != nil {
				t.Error(err)
			}
		})
	}
	if err != nil {
		t.Fatal(err)
	}
	prior, err := carrier.acquirePrior(ctx)
	if err != nil {
		t.Fatal(err)
	}
	runtime, err := prior.RuntimeProduct(ctx)
	if err != nil || runtime == nil {
		t.Fatal("original prior runtime required", err)
	}
	ns, err := api.OpenWindowsProductStateNamespace(ctx, state)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := ns.Close(); err != nil {
			t.Error(err)
		}
	})
	foreign, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := foreign.Close(); err != nil && !errors.Is(err, net.ErrClosed) {
			t.Error(err)
		}
	})
	_, portText, err := net.SplitHostPort(foreign.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	port, err := strconv.Atoi(portText)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := api.EnsureHubEndpoint(port, os.Getpid()); err != nil {
		t.Fatal(err)
	}
	reserve := func() (*productAggregateQualification, error) {
		lease, q, err := reserveAbsentPriorAggregate(ctx, state, dst.ProductPath, runtime, ns)
		if lease != nil {
			if release := lease.ReleaseErr(); release != nil {
				t.Fatal(release)
			}
		}
		return q, err
	}
	if _, err := reserve(); err == nil {
		t.Fatal("occupied test socket qualified as a serving prior")
	}
	if err := foreign.Close(); err != nil {
		t.Fatal(err)
	}
	q, err := reserve()
	if err != nil || q == nil || q.serving != nil {
		t.Fatal("originally vacant native reservation failed", err)
	}
	if err := q.beforePromotion(ctx); err == nil || !strings.Contains(err.Error(), "no durable Product quiescence") {
		t.Fatal("promotion accepted a reservation before durable quiescence", err)
	}
	// The Product.Run boundary separately receives the original quiescence
	// readback. Here keep that prerequisite fixed to exercise the real H guard.
	q.prepared = &windowsProductPreparedTxn{journal: windowsProductRecoveryJournalV3{Quiescence: &windowsProductRecoveryQuiescenceV2{}}}
	occupied, err := net.Listen("tcp4", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := occupied.Close(); err != nil && !errors.Is(err, net.ErrClosed) {
			t.Error(err)
		}
	})
	if err := q.beforePromotion(ctx); err == nil || !strings.Contains(err.Error(), "socket vacancy unconfirmed") || v2TxnFixtureIdentity(t, dst.ProductPath) != originals[dst.ProductPath] {
		t.Fatal("post-quiescence occupied endpoint allowed Product promotion", err)
	}
	if err := occupied.Close(); err != nil {
		t.Fatal(err)
	}
	if err := q.beforePromotion(ctx); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(state, "hub-mcp.endpoint.json")
	raw := mustReadFile(t, path)
	if err := os.WriteFile(path, append(raw, '\n'), 0600); err != nil {
		t.Fatal(err)
	}
	if err := q.beforePromotion(ctx); err == nil || !strings.Contains(err.Error(), "original Hub endpoint changed before Product promotion") {
		t.Fatal("post-settlement guard accepted an in-place endpoint rewrite", err)
	}
	if err := os.WriteFile(path, raw, 0600); err != nil {
		t.Fatal(err)
	}
	if err := q.beforePromotion(ctx); err != nil {
		t.Fatal("restored exact endpoint rejected", err)
	}
	token := filepath.Join(state, "hub-mcp-control.token")
	if err := api.SecureWriteClientConfig(token, []byte(strings.Repeat("b", 64)+"\n")); err != nil {
		t.Fatal(err)
	}
	if _, err := reserve(); err == nil {
		t.Fatal("stale reload object qualified with a vacant port")
	}
	if err := q.beforePromotion(ctx); err == nil || v2TxnFixtureIdentity(t, dst.ProductPath) != originals[dst.ProductPath] {
		t.Fatal("post-settlement check accepted a newly occupied reload leaf or Product changed", err)
	}
	if _, err := os.Stat(token); err != nil {
		t.Fatal("vacancy check disturbed the foreign reload object", err)
	}

	// Retain a real child and the actual Product file bank for these negative
	// adapter cases; the child is not presented as an admitted Product process.
	t.Run("reap_adapter_original_boundaries", func(t *testing.T) {
		deadline, ok := t.Deadline()
		if !ok {
			t.Fatal("original test deadline required for retained child")
		}
		bounded, cancel := context.WithDeadline(t.Context(), deadline)
		defer cancel()
		stateDir := apitest.HardenedTempDir(t)
		child := startBoundStatusChildAt(t, bounded, deadline, 0, stateDir, true)
		owner := api.SupervisorLockOwner{PID: child.peer.PID, StartedAt: child.peer.CreationTime}
		held, err := process.HoldPIDForObservation(owner.PID)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			if err := held.Close(); err != nil {
				t.Error(err)
			}
		})
		live := &priorAggregateProcesses{supervisor: held, owner: owner, peer: child.peer}
		if err := live.checkSupervisor(bounded, stateDir); err != nil {
			t.Fatal("original child proof unavailable", err)
		}
		if err := runtime.ValidateOriginal(bounded); err != nil {
			t.Fatal("original Product bank unavailable", err)
		}
		other, err := prior.RuntimeProduct(bounded)
		if err != nil || other == nil || other == runtime {
			t.Fatal("separately issued prior view unavailable", err)
		}
		deps := buildV5UpgradeDeps(runtime.Artifact().Path, stateDir)
		deps.originalRuntime, deps.originalContext = runtime, bounded
		reap := &servingAggregateReap{v5UpgradeDeps: deps, stateDir: stateDir, live: live}
		observed := &windowsProductQuiescenceObservationV1{Supervisor: windowsProductSupervisorSettlementV1{
			State:      windowsProductSupervisorGracefullySettled,
			Generation: &windowsProductSupervisorGenerationV1{PID: owner.PID, StartedAt: owner.StartedAt, IPCRequestVersion: 1},
		}}
		for _, mismatch := range []struct {
			name, state string
			prior       *binaryadmission.WindowsCanonicalEntryPriorRuntimeV1
		}{
			{"prior_view", stateDir, other},
			{"namespace", apitest.HardenedTempDir(t), runtime},
		} {
			if err := reap.verifyOriginalPriorReap(bounded, mismatch.state, mismatch.prior, observed); err == nil || !strings.Contains(err.Error(), "serving prior missing its original reap observation") {
				t.Fatalf("%s reached original process admission: %v", mismatch.name, err)
			}
		}
		observed.Supervisor.Generation.PID++
		if err := reap.verifyOriginalPriorReap(bounded, stateDir, runtime, observed); !errors.Is(err, api.ErrSupervisorControlPeerIdentity) {
			t.Fatalf("different observed generation admitted: %v", err)
		}
		if wait, err := held.Wait(); err != nil || wait.Result != uint32(windows.WAIT_TIMEOUT) {
			t.Fatalf("refusal affected original child: %+v %v", wait, err)
		}
		for _, marker := range []string{"aggregate-quiesce-observed", "aggregate-exit-observed"} {
			if _, err := os.Stat(filepath.Join(stateDir, marker)); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("refused adapter performed lifecycle operation: %s %v", marker, err)
			}
		}
	})

}

type priorAggregateObservedClose struct {
	process.HeldPIDObservation
	closes int
	joined error
}

func (h *priorAggregateObservedClose) Close() error {
	h.closes++
	return errors.Join(h.HeldPIDObservation.Close(), h.joined)
}

func TestPriorAggregateReceivesOriginalProcessExitAndClose(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	deadline, _ := ctx.Deadline()
	a := startBoundStatusChild(t, ctx, deadline, 0)
	b := startBoundStatusChild(t, ctx, deadline, 0)
	hold := func(peer api.SupervisorProcessIdentityV1) *priorAggregateObservedClose {
		h, err := process.HoldPIDForObservation(peer.PID)
		if err != nil {
			t.Fatal(err)
		}
		if err := h.VerifyIdentity(process.PIDIdentityProof{PID: peer.PID, StartedAt: peer.CreationTime, ExecutablePath: peer.ImagePath, StartTolerance: time.Nanosecond}); err != nil {
			t.Fatal(errors.Join(err, h.Close()))
		}
		return &priorAggregateObservedClose{HeldPIDObservation: h}
	}
	supervisor, route := hold(a.peer), hold(b.peer)
	p := &priorAggregateProcesses{supervisor: supervisor, route: route}
	t.Cleanup(func() {
		if !p.closed {
			if err := p.close(); err != nil {
				t.Error(err)
			}
		}
	})
	dead, stop := context.WithCancel(ctx)
	stop()
	if err := p.receiveExit(dead); !errors.Is(err, context.Canceled) || supervisor.closes != 0 || route.closes != 0 {
		t.Fatalf("canceled settlement accepted or released still-live handles: %v", err)
	}
	if err := a.input(); err != nil {
		t.Fatal(err)
	}
	short, done := context.WithTimeout(ctx, 10*time.Millisecond)
	err := p.receiveExit(short)
	done()
	if !errors.Is(err, context.DeadlineExceeded) || supervisor.closes != 0 || route.closes != 0 {
		t.Fatalf("live Route was not retained while waiting for both exits: %v", err)
	}
	if err := b.input(); err != nil {
		t.Fatal(err)
	}
	closeFault := errors.New("fixture late close")
	route.joined = closeFault
	if err := p.receiveExit(ctx); !errors.Is(err, closeFault) || !errors.Is(err, ErrWindowsProductRecoveryPending) {
		t.Fatal("late native close failed to block replacement", err)
	}
	if err := p.close(); !errors.Is(err, closeFault) || route.closes != 1 || supervisor.closes != 1 {
		t.Fatal("cleanup repeated or lost an original close", err)
	}
}

func v2TxnAssertAggregateBeforePromotion(t *testing.T, txn *WindowsProductTxn, receipt *windowsProductQuiescenceReceiptV1, path string, priorIDs map[string]binaryadmission.WindowsCanonicalEntryFileIdentityV2) {
	t.Helper()
	failure := errors.New("fixture saved aggregate failed to settle")
	tasks := txn.Opts.Tasks.(*v2TxnFixtureTasks)
	checkOriginals := func() {
		t.Helper()
		for member, original := range priorIDs {
			if v2TxnFixtureIdentity(t, member) != original {
				t.Fatalf("aggregate veto displaced original member %s", filepath.Base(member))
			}
		}
	}
	enters := 0
	txn.beforePromotion = func(context.Context) error {
		enters++
		j, err := v2TxnReadFixtureJournal(path)
		if err != nil || j.Phase != WindowsProductRecoveryPrepared || j.Quiescence == nil ||
			j.EntryBundle == nil || len(j.EntryBundle.Moves) != 0 || tasks.rewrites != 0 {
			t.Fatal("guard not at durable quiescence before Product/Task mutation", err)
		}
		checkOriginals()
		return failure
	}
	if err := txn.Prepare(); err != nil {
		t.Fatal(err)
	}
	txn.Opts.ReplacementAuthority.Quiescence = receipt
	_, err := txn.Run(txn.Opts.Context)
	if !errors.Is(err, failure) || enters != 1 || tasks.rewrites != 0 || tasks.restores != 1 || tasks.discards != 1 {
		t.Fatalf("aggregate veto did not settle through the actual V2 rollback, entered=%d, err=%v", enters, err)
	}
	checkOriginals()
	if err := v2TxnFixtureAbsent(path); err != nil {
		t.Fatal(err)
	}
}

func TestServingPriorDecisionRefusesChangedQualificationProvenance(t *testing.T) {
	ctx := t.Context()
	profile := apitest.HardenedTempDir(t)
	state := apitest.HardenedDir(t, filepath.Join(profile, "mcp-local-hub"))
	t.Cleanup(api.SetDaemonStateRootForTest(state))
	t.Setenv("LOCALAPPDATA", profile)
	t.Setenv("MCPHUB_GUI_TEST_PIDPORT_DIR", "")
	if err := api.WriteStateFileBytesAtomic(api.SettingsPath(), []byte("gui_server.hub_endpoint_enabled: true\n")); err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	if err := ln.Close(); err != nil {
		t.Fatal(err)
	}
	ep, err := api.EnsureHubEndpoint(port, os.Getpid())
	if err != nil {
		t.Fatal(err)
	}
	priorID, nextID := strings.Repeat("a", 32), strings.Repeat("b", 32)
	body, err := json.Marshal(struct {
		api.HubEndpoint
		TransactionID string `json:"route_qualification_transaction_id"`
	}{ep, priorID})
	if err != nil {
		t.Fatal(err)
	}
	if err := api.SecureWriteClientConfig(filepath.Join(state, "hub-mcp.endpoint.json"), body); err != nil {
		t.Fatal(err)
	}
	ns, err := api.OpenWindowsProductStateNamespace(ctx, state)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := ns.Close(); err != nil {
			t.Error(err)
		}
	})
	owner, err := api.AcquireSupervisorLock(filepath.Join(state, "supervisor.lock"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(owner.Release)
	product := filepath.Join(profile, "target", "mcphub.exe")
	q := &productAggregateQualification{stateDir: state, namespace: ns, priorPath: product, guiPath: filepath.Join(state, "gui.pidport"),
		serving: &windowsProductPriorServingAggregateV1{PriorTransactionID: priorID}}
	err = ns.WithHubRouteState(ctx, func(h *api.HubRouteState) error {
		current, err := h.ReadEndpoint()
		if err != nil {
			return err
		}
		q.endpoint = current
		q.serving.EndpointIdentity, q.serving.EndpointRawSHA256 = current.OriginalIdentity(), current.OriginalSHA256()
		q.priorSelection = api.HubRouteQualification{TransactionID: priorID, SuccessorSHA256: strings.Repeat("1", 64),
			ScopeSHA256: strings.Repeat("2", 64), EndpointAuthoritySHA256: current.AuthoritySHA256(), EndpointProvenanceSHA256: current.ProvenanceSHA256()}
		q.next = q.priorSelection
		q.next.TransactionID = nextID
		return h.StageQualification("", current, q.priorSelection)
	})
	if err != nil {
		t.Fatal(err)
	}
	changed := q.priorSelection
	changed.EndpointProvenanceSHA256 = strings.Repeat("3", 64)
	body, err = json.Marshal(map[string]any{"schema": "hub-mcp-route-qualification-v1", "qualified": []api.HubRouteQualification{changed}})
	if err != nil {
		t.Fatal(err)
	}
	if err := api.SecureWriteClientConfig(filepath.Join(state, "hub-mcp-route-qualification.json"), body); err != nil {
		t.Fatal(err)
	}
	q.priorReceipt = productPairFileSnapshot{path: filepath.Join(state, UpgradeReceiptSchemaV3+".json"), present: true, body: []byte("received private receipt")}
	if err := api.WriteStateFileBytesAtomic(q.priorReceipt.path, q.priorReceipt.body); err != nil {
		t.Fatal(err)
	}
	launch := &fixtureSupervisorLaunch{}
	p := &windowsProductPreparedTxn{successorLaunch: launch}
	p.journal.TransactionID, p.journal.Phase, p.journal.Product.Target = nextID, WindowsProductRecoverySuccessorStarting, product
	p.staged.SHA256 = strings.Repeat("4", 64)
	p.deps.ReadReceipt = os.ReadFile
	p.deps.WaitSupervisorReady = func(context.Context, string, binaryadmission.WindowsArtifact) error {
		return nil
	}
	p.successorBinding.proof = &supervisorControlProof{issuer: launch, owner: owner.Owner(), expected: supervisorExpectation(state, product, p.staged.SHA256),
		validateCreator: func() error { return nil }, originalOutcome: func() (bool, error) { return false, nil }}
	q.prepared = p
	writes := 0
	write := func() error { writes++; return nil }
	err = q.beforeDecision(ctx, write)
	if err == nil || !strings.Contains(err.Error(), "serving aggregate qualification changed") || writes != 0 {
		t.Fatalf("provenance-only drift reached writer or wrong gate: writes=%d err=%v", writes, err)
	}
	// The same bytes and endpoint witness still permit the legacy vacant arm.
	q.serving = nil
	if err := q.beforeDecision(ctx, write); err != nil || writes != 1 {
		t.Fatalf("originally vacant qualification changed: writes=%d err=%v", writes, err)
	}
}

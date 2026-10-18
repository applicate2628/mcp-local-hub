//go:build windows

package cli

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"mcp-local-hub/internal/daemon"
	"mcp-local-hub/internal/process"
)

// Reuse the TestMain-qualified native helpers. Their selected-test fast path
// creates no CLI package state root and no live client/supervisor configuration.
func stdioCLITestHost(t *testing.T, exit bool) *daemon.StdioHost {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	selector := "^TestStaleExitReleaseHelper$"
	env := map[string]string{staleExitHelperSentinelEnv: "1", staleExitHelperReleaseEnv: filepath.Join(t.TempDir(), "release"), staleExitCustodyFixtureEnv: ""}
	if exit {
		selector = "^TestFormatChildExit_RealProcessShowsExitCode$"
		env = map[string]string{"MCPHUB_TEST_CHILD_EXIT_CODE": "42", staleExitCustodyFixtureEnv: ""}
	}
	h, err := daemon.NewStdioHost(daemon.HostConfig{Command: exe, Args: []string{"-test.run=" + selector}, Env: env})
	if err != nil {
		t.Fatal(err)
	}
	if err := h.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := h.Stop(); err != nil {
			t.Error(err)
		}
	})
	return h
}

func TestStdioCLIReceivesBindFailureAndHostSettlement(t *testing.T) {
	occupied, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer occupied.Close()
	h := stdioCLITestHost(t, false)
	err = serveStdioHost(context.Background(), h, &http.Server{Addr: occupied.Addr().String(), Handler: h.HTTPHandler()})
	if err == nil || !strings.Contains(err.Error(), "http server") {
		t.Fatalf("bind failure vanished: %v", err)
	}
	r, stopErr := h.StopObserved(context.Background())
	if stopErr != nil || !r.Complete() {
		t.Fatalf("bind failure lost started child cleanup: %+v %v", r, stopErr)
	}
}

func TestStdioCLIReceivesCanceledLifetimeWithHeldDescendant(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	var closeMembers func() error
	defer func() {
		// Defers run before testing.T cleanups on Fatal. Receive this borrow
		// before canceling even when a membership assertion cannot complete.
		if closeMembers != nil {
			if err := closeMembers(); err != nil {
				t.Errorf("original member observation cleanup: %v", err)
			}
		}
		cancel()
	}()
	h, fixture := stdioCLICustodyHost(t, ctx, true)
	sentinel, sentinelFixture := stdioCLICustodyHost(t, context.Background(), false)
	listenerEntered := make(chan net.Listener, 1)
	handlerEntered := make(chan struct{}, 1)
	handler := h.HTTPHandler()
	srv := &http.Server{
		Addr:              "127.0.0.1:0",
		ReadHeaderTimeout: time.Second,
		BaseContext:       func(listener net.Listener) context.Context { listenerEntered <- listener; return context.Background() },
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/fixture/serve-entered" {
				handlerEntered <- struct{}{}
				w.WriteHeader(http.StatusNoContent)
				return
			}
			handler.ServeHTTP(w, r)
		}),
	}
	served := make(chan error, 1)
	go func() { served <- serveStdioHost(ctx, h, srv) }()
	received := false
	t.Cleanup(func() {
		cancel()
		if !received {
			select {
			case err := <-served:
				if err != nil {
					t.Errorf("CLI fixture cleanup: %v", err)
				}
			case <-time.After(7 * time.Second):
				t.Error("CLI fixture Serve result not received")
			}
		}
		if err := srv.Close(); err != nil {
			t.Errorf("same fixture server close: %v", err)
		}
	})
	var listener net.Listener
	select {
	case listener = <-listenerEntered:
	case err := <-served:
		received = true
		t.Fatalf("CLI returned before actual Serve entry: %v", err)
	case <-time.After(5 * time.Second):
		t.Fatal("actual Serve entry absent")
	}
	// The callback alone is not an accept-loop receipt. A complete real HTTP
	// exchange proves that the exact listener's Serve worker reached the handler.
	transport := &http.Transport{DisableKeepAlives: true}
	t.Cleanup(transport.CloseIdleConnections)
	client := &http.Client{Timeout: 5 * time.Second, Transport: transport}
	probe, err := client.Get("http://" + listener.Addr().String() + "/fixture/serve-entered")
	if err != nil {
		t.Fatal(err)
	}
	status := probe.StatusCode
	if err := probe.Body.Close(); err != nil {
		t.Fatal(err)
	}
	if status != http.StatusNoContent {
		t.Fatalf("Serve probe status=%d", status)
	}
	select {
	case <-handlerEntered:
	default:
		t.Fatal("HTTP response not bound to fixture handler")
	}
	sentinelBefore := stdioCLICustodyLive(t, sentinel, sentinelFixture.Nonce+":sentinel-before", false)
	before := stdioCLICustodyLive(t, h, fixture.Nonce+":target-before", true)
	if _, err := os.Stat(filepath.Join(fixture.Root, "descendant.release")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("descendant barrier was released before cancellation")
	}
	observationCtx, observationCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer observationCancel()
	source, err := h.OriginalMemberSource(observationCtx)
	if err != nil {
		t.Fatalf("target original member source: %v", err)
	}
	sentinelSource, err := sentinel.OriginalMemberSource(observationCtx)
	if err != nil {
		t.Fatalf("sentinel original member source: %v", err)
	}
	observation, observeErr := h.ObserveOriginalMembers(observationCtx)
	if observation == nil {
		t.Fatalf("target original member observation unavailable: %v", observeErr)
	}
	closeObservation := func() error {
		closed, closeErr := observation.CloseObserved()
		if closed == nil {
			return errors.Join(closeErr, errors.New("original member close observation missing"))
		}
		return errors.Join(closeErr, closed.Validate(source))
	}
	closeMembers = closeObservation
	cohort := observation.Snapshot()
	evidence, evidenceErr := json.Marshal(struct {
		Root, Descendant, Sentinel process.PublisherNativeIncarnationV1
		Members                    process.OriginalJobMemberSnapshotV1
	}{before.RootNative, *before.ChildNative, sentinelBefore.RootNative, cohort})
	if evidenceErr != nil {
		t.Fatal(evidenceErr)
	}
	t.Logf("C5_ORIGINAL_MEMBER_EVIDENCE %s", evidence)
	if observeErr != nil {
		t.Fatalf("target original member acquisition incomplete: %v", observeErr)
	}
	if err := observation.ValidateSource(observationCtx, source); err != nil {
		t.Fatalf("same original member source validation: %v", err)
	}
	if err := observation.ValidateSource(observationCtx, sentinelSource); err == nil {
		t.Fatal("sentinel source validated target observation")
	}
	second, secondErr := h.ObserveOriginalMembers(observationCtx)
	if second != nil {
		closed, closeErr := second.CloseObserved()
		if closed != nil {
			closeErr = errors.Join(closeErr, closed.Validate(source))
		}
		t.Fatalf("second active observation acquired: %v", errors.Join(secondErr, closeErr))
	}
	if secondErr == nil {
		t.Fatal("second active observation lost original busy refusal")
	}
	if err := observation.MatchOwnerPID(observationCtx, uint32(before.ChildPID)); err != nil {
		t.Fatalf("declared descendant is not an original live member: %v", err)
	}
	if !cohort.Complete || cohort.PrimaryError != nil || len(cohort.CleanupErrors) != 0 ||
		cohort.PreTotal != cohort.PostTotal || cohort.PreActive != cohort.PostActive || cohort.PreActive != uint32(len(cohort.Rows)) {
		t.Fatalf("full original member/accounting coverage unavailable: %+v", cohort)
	}
	members := make(map[uint32]process.OriginalJobMemberRowV1, len(cohort.Rows))
	for _, row := range cohort.Rows {
		if row.PID == 0 || row.NativeBirthFILETIME == 0 || row.ImagePathSHA256 == "" || row.PrincipalSIDSHA256 == "" ||
			!row.MembershipDefined || !row.IsMember || !row.LiveDefined || !row.Live {
			t.Fatalf("incomplete original member row: %+v", row)
		}
		if _, duplicate := members[row.PID]; duplicate {
			t.Fatal("original member row duplicated")
		}
		if row.PID == sentinelBefore.RootNative.PID {
			t.Fatal("independent sentinel is inside target original Job")
		}
		members[row.PID] = row
		if row.PID != before.RootNative.PID && row.PID != before.ChildNative.PID {
			// Preserve the actual identity as an additional original member; its
			// role is not guessed or classified as a benign OS helper.
			t.Logf("C5_ADDITIONAL_ORIGINAL_MEMBER %+v", row)
		}
	}
	for _, named := range []process.PublisherNativeIncarnationV1{before.RootNative, *before.ChildNative} {
		row, found := members[named.PID]
		if !found || row.NativeBirthFILETIME != named.NativeBirthFILETIME || row.ImagePathSHA256 != named.NativeImagePathSHA256 || row.PrincipalSIDSHA256 != named.PrincipalSIDSHA256 {
			t.Fatal("fixture native identity differs from its original member row")
		}
	}
	if err := closeObservation(); err != nil {
		t.Fatalf("original member borrowed handles not closed before cancellation: %v", err)
	}
	closed, closeErr := observation.CloseObserved()
	if closeErr != nil || closed == nil {
		t.Fatalf("cached original member close unavailable: %v", closeErr)
	}
	closedEvidence, err := json.Marshal(closed.Snapshot())
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("C5_ORIGINAL_MEMBER_CLOSE_BEFORE_CANCEL %s", closedEvidence)
	// Both native actors have just answered through the owned protocol channel
	// while the child still holds both inherited pipes. This same context owns
	// the host lifetime and the CLI select; no pre-Serve cancellation shortcut.
	cancel()
	select {
	case err := <-served:
		received = true
		if err != nil {
			t.Fatalf("held-descendant lifetime cancellation: %v", err)
		}
	case <-time.After(7 * time.Second):
		t.Fatal("host/Shutdown/Serve cancellation receiving timed out")
	}
	settlement, err := h.StopObserved(context.Background())
	if err != nil || !settlement.Complete() || !settlement.RootWait.Known || settlement.OriginalIdentity.PID != before.RootPID {
		t.Fatalf("root/native/pipe/worker settlement missing: %+v %v", settlement, err)
	}
	native := settlement.NativeSettlement.Snapshot()
	if !native.MembersExited || !native.AccountingKnown || native.InitialTotal != cohort.PreTotal || native.FinalActive != 0 || !native.HandlesClosed || len(native.CapturedMemberPIDs) != len(members) {
		t.Fatalf("held descendant did not settle in original Job: %+v", native)
	}
	seen := make(map[uint32]bool, len(native.CapturedMemberPIDs))
	for _, pid := range native.CapturedMemberPIDs {
		if _, observed := members[pid]; !observed || seen[pid] {
			t.Fatal("terminal member set differs from original before-cancel cohort")
		}
		seen[pid] = true
	}
	terminalCtx, terminalCancel := context.WithTimeout(context.Background(), time.Second)
	defer terminalCancel()
	if source, err := h.OriginalMemberSource(terminalCtx); source != nil || err == nil {
		t.Fatal("terminal host issued a fresh original source")
	}
	if observation, err := h.ObserveOriginalMembers(terminalCtx); observation != nil || err == nil {
		t.Fatal("terminal host issued a fresh original observation")
	}
	tcp, ok := listener.(*net.TCPListener)
	if !ok {
		t.Fatal("fixture listener has an unexpected concrete owner")
	}
	if err := tcp.SetDeadline(time.Now().Add(time.Second)); !errors.Is(err, net.ErrClosed) {
		t.Fatalf("same listener not positively closed: %v", err)
	}
	sentinelAfter := stdioCLICustodyLive(t, sentinel, sentinelFixture.Nonce+":sentinel-after", false)
	if sentinelAfter.RootPID != sentinelBefore.RootPID || sentinelAfter.RootNative != sentinelBefore.RootNative {
		t.Fatal("foreign sentinel original identity changed")
	}
	select {
	case <-sentinel.ChildExited():
		t.Fatal("foreign sentinel retired during target cancellation")
	default:
	}
	if _, err := h.StopObserved(context.Background()); err != nil {
		t.Fatalf("cached cancellation outcome changed: %v", err)
	}
	t.Log("C5 actual Serve and genuine fixture identities received; full original member cohort and query-handle closes before cancel; same lifetime canceled; original Job/root/native/pipes/listener/Serve/Shutdown received; same-identity foreign sentinel answered afterward")
}

func stdioCLICustodyHost(t *testing.T, ctx context.Context, descendant bool) (*daemon.StdioHost, staleExitCustodyFixture) {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	fixture := staleExitCustodyFixture{Version: 1, Role: "root", Root: root, Nonce: filepath.Base(root), Descendant: descendant}
	raw, err := json.Marshal(fixture)
	if err != nil {
		t.Fatal(err)
	}
	h, err := daemon.NewStdioHost(daemon.HostConfig{
		Command: exe, Args: []string{"-test.run=^TestStaleExitReleaseHelper$", "-test.count=1", "-test.timeout=15s"},
		Env: map[string]string{staleExitHelperSentinelEnv: "1", staleExitHelperReleaseEnv: filepath.Join(root, "root.release"), staleExitCustodyFixtureEnv: string(raw)},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := h.Start(ctx); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := h.Stop(); err != nil {
			t.Error(err)
		}
	})
	return h, fixture
}

func stdioCLICustodyLive(t *testing.T, h *daemon.StdioHost, nonce string, held bool) stdioCLICustodyReply {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	request := staleExitCustodyRequest{JSONRPC: "2.0", ID: json.RawMessage("1"), Method: "fixture/liveness"}
	request.Params.Nonce = nonce
	raw, err := json.Marshal(request)
	if err != nil {
		t.Fatal(err)
	}
	raw, err = h.SendRPC(ctx, raw)
	if err != nil {
		t.Fatalf("fresh custody liveness receiving: %v", err)
	}
	var response struct {
		JSONRPC string               `json:"jsonrpc"`
		ID      int                  `json:"id"`
		Result  stdioCLICustodyReply `json:"result"`
	}
	if err := json.Unmarshal(raw, &response); err != nil {
		t.Fatal(err)
	}
	identity, wait, native := h.NativeExit()
	// SendRPC returns the host's positive internal ID; its pending map owns
	// correlation. The fresh nonce binds this fixture response across calls.
	if response.JSONRPC != "2.0" || response.ID <= 0 || response.Result.Nonce != nonce ||
		!native || !identity.Known || wait.Known || response.Result.RootPID != identity.PID ||
		response.Result.RootNative.PID != uint32(identity.PID) || response.Result.RootNative.NativeBirthFILETIME != identity.NativeBirthFILETIME ||
		response.Result.RootNative.NativeImagePathSHA256 == "" || response.Result.RootNative.PrincipalSIDSHA256 == "" || response.Result.RootNative.NativeCaptureSHA256 == "" ||
		response.Result.Held != held || held && (response.Result.ChildPID <= 0 || response.Result.ChildPID == identity.PID) ||
		held && (response.Result.ChildNative == nil || response.Result.ChildNative.PID != uint32(response.Result.ChildPID) || response.Result.ChildNative.NativeBirthFILETIME == 0 || response.Result.ChildNative.NativeImagePathSHA256 == "" || response.Result.ChildNative.PrincipalSIDSHA256 == "" || response.Result.ChildNative.NativeCaptureSHA256 == "") ||
		!held && (response.Result.ChildPID != 0 || response.Result.ChildNative != nil) {
		t.Fatalf("fresh same-owner liveness reply not qualified: %+v", response)
	}
	return response.Result
}

func TestStdioCLIReceivesOriginalNonzeroExitAndListener(t *testing.T) {
	h := stdioCLITestHost(t, true)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	err := serveStdioHost(ctx, h, &http.Server{Addr: "127.0.0.1:0", Handler: h.HTTPHandler()})
	if err == nil || !strings.Contains(err.Error(), "exited unexpectedly") || !strings.Contains(err.Error(), "exit_code=42") {
		t.Fatalf("native exit diagnostics vanished: %v", err)
	}
	var retained *stdioListenerError
	if !errors.As(err, &retained) || retained.owner.receiveError != nil || retained.owner.shutdownError != nil {
		t.Fatalf("joint Serve/Shutdown result not received: %v", err)
	}
	select {
	case <-retained.owner.serveDone:
	default:
		t.Fatal("CLI returned before Serve receipt")
	}
}

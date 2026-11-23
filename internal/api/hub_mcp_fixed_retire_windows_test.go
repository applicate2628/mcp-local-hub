package api

import (
	"errors"
	"net"
	"os"
	"testing"

	"golang.org/x/sys/windows"
)

func TestHubRouteFixedBindPendingRetirement(t *testing.T) {
	dir := isolateStateDir(t)
	if _, err := EnsureHubTokens([]string{"claude-code"}); err != nil {
		t.Fatal(err)
	}
	probe, err := NewListenerWithSOExclusiveContext(t.Context(), "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	address := probe.Addr().String()
	port := probe.Addr().(*net.TCPAddr).Port
	if err := probe.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := EnsureHubEndpoint(port, os.Getpid()); err != nil {
		t.Fatal(err)
	}
	identity, err := ObserveHubRouteStateDirectory(t.Context(), dir)
	if err != nil {
		t.Fatal(err)
	}
	fixture := hubRouteFixture{dir: dir, id: identity}
	failedStart := errors.New("fixture refused endpoint before rename")
	var bind *HubMcpBindResult
	var originalEndpoint, ownedReload hubRouteLeaf
	var blocker windows.Handle
	releaseBlocker := func() {
		if blocker != 0 {
			if err := windows.CloseHandle(blocker); err != nil {
				t.Error("close fixture-owned blocker:", err)
			}
			blocker = 0
		}
	}
	defer releaseBlocker()
	fixture.with(t, func(h *HubRouteState) error {
		endpoint, err := h.ReadEndpoint()
		if err != nil {
			return err
		}
		originalEndpoint = endpoint.leaf
		row := testHubRouteQualification(endpoint, "a")
		if err := h.StageQualification("", endpoint, row); err != nil {
			return err
		}
		renames := 0
		h.core.native.beforeRename = func() error {
			renames++
			if renames != 2 {
				return nil
			}
			ownedReload, err = h.core.native.read(hubMcpControlTokenFileLeaf, hubRouteReloadLimit)
			if err != nil {
				return err
			}
			blocker, err = ntOpenRelativeWithShareAccess(h.core.native.parent, hubMcpControlTokenFileLeaf,
				windows.GENERIC_READ, windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE)
			return errors.Join(failedStart, err)
		}
		bind, err = h.BindExistingHubMcpListener(endpoint, row)
		if !errors.Is(err, failedStart) || !fixedBindSharingViolation(err) || blocker == 0 || !ownedReload.present {
			return errors.Join(errors.New("fixture did not reach original reload removal refusal"), err)
		}
		return nil
	})
	readBackRetained := func() {
		fixture.with(t, func(h *HubRouteState) error {
			endpoint, err := h.ReadEndpoint()
			if err != nil {
				return err
			}
			reload, err := h.core.native.read(hubMcpControlTokenFileLeaf, hubRouteReloadLimit)
			if err == nil && (!endpoint.leaf.matches(originalEndpoint) || !reload.matches(ownedReload)) {
				return errors.New("refusal changed original reload ownership or endpoint")
			}
			return err
		})
	}
	readBackRetained()
	vacant, err := NewListenerWithSOExclusiveContext(t.Context(), address)
	if err != nil {
		t.Fatalf("failed bind left original socket active: %v", err)
	}
	if err := vacant.Close(); err != nil {
		t.Fatal(err)
	}
	if bind == nil || bind.Listener != nil || bind.Reload == nil || bind.Reload.controlTok.Load() != nil {
		t.Fatal("failed start lost cleanup authority or offered an active effect")
	}
	if err := bind.CloseUnserved(t.Context()); !fixedBindSharingViolation(err) {
		t.Fatalf("explicit retirement failed to report the original native obstruction: %v", err)
	}
	readBackRetained()
	releaseBlocker()
	if err := bind.CloseUnserved(t.Context()); err != nil {
		t.Fatal("original owner did not retire:", err)
	}
	fixture.with(t, func(h *HubRouteState) error { return h.CheckReloadTokenAbsent() })
}

func fixedBindSharingViolation(err error) bool {
	return errors.Is(err, windows.ERROR_SHARING_VIOLATION) || errors.Is(err, windows.STATUS_SHARING_VIOLATION)
}

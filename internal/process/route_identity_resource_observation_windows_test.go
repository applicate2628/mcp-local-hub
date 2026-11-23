//go:build windows

package process

import (
	"errors"
	"testing"

	"golang.org/x/sys/windows"
)

func TestObservedDeniedOriginalSIDRetainsPartialTokenClose(t *testing.T) {
	denied := errors.New("fixture token query denied")
	closeErr := errors.New("fixture partial token close failed")
	var calls int
	value, observation, err := observeProcessHandleOwnerSIDWith(3, func(_ windows.Handle, _ uint32, token *windows.Token) error {
		*token = 7
		return denied
	}, func(token windows.Token) error {
		calls++
		if token != 7 {
			t.Fatalf("closed a substitute token: %v", token)
		}
		return closeErr
	})
	if value != "" || !errors.Is(err, denied) || !errors.Is(err, closeErr) || calls != 1 ||
		observation == nil || observation.TokenClose.native != 7 || !observation.TokenClose.Returned ||
		observation.ResourcesFinalized() {
		t.Fatalf("denied phase lost partial first close: %v", err)
	}
	peer := &WindowsIdentityObservation{finalized: true, ProcessClose: closeWindowsIdentityHandle(8, func() error { return closeErr }),
		QueryError: &WindowsIdentityQueryError{Query: WindowsIdentityOpenToken, Cause: denied}}
	if peer.ResourcesFinalized() || !peer.ProcessClose.Returned || !errors.Is(peer.ProcessClose.Err, closeErr) {
		t.Fatal("peer query failed to retain its original process close outcome")
	}
}

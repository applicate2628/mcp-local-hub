package clients

import (
	"errors"
	"reflect"
	"testing"
)

func TestConfigWriteCaptureRetainsFirstPartialHandleClose(t *testing.T) {
	owner := &ConfigWriteCaptureTransaction{}
	closeErr := errors.New("fixture first native close failed")
	var order []uintptr
	ancestor := &configWriteCaptureResource{native: 1, closeFn: func() error {
		order = append(order, 1)
		return nil
	}}
	partial := &configWriteCaptureResource{native: 2, closeFn: func() error {
		order = append(order, 2)
		return closeErr
	}}
	owner.register(ancestor)
	owner.register(partial)
	// Qualification failed before wrapping the partial handle. Its first
	// actual close still belongs to capture finalization.
	if err := partial.transferFile(nil); err == nil {
		t.Fatal("missing file wrapper transferred original native ownership")
	}
	observed, err := owner.CloseObserved()
	if !errors.Is(err, closeErr) || observed == nil || observed.ValidateTransaction(owner) == nil ||
		len(observed.resources) != 2 || !reflect.DeepEqual(order, []uintptr{2, 1}) {
		t.Fatalf("partial original close evidence was lost: %v", err)
	}
	first := observed.resources[0]
	if first.native != 2 || !first.attempted || !first.returned || !errors.Is(first.err, closeErr) {
		t.Fatalf("partial original native outcome was not retained: %+v", first)
	}
	again, err := owner.CloseObserved()
	if again != observed || !errors.Is(err, closeErr) || len(order) != 2 {
		t.Fatal("capture retried or replaced its first native close")
	}
}

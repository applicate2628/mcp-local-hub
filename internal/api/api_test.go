package api

import "testing"

func TestNewAPIReturnsWorkingInstance(t *testing.T) {
	a := NewAPI()
	if a == nil {
		t.Fatal("NewAPI returned nil")
	}
	if a.state == nil {
		t.Error("state is nil")
	}
	if a.state.Daemons == nil {
		t.Error("Daemons map is nil")
	}
	if a.bus == nil {
		t.Error("bus is nil")
	}
}

package api

import "testing"

// PART 2d: a handler panic must be OBSERVED (so the otherwise-silent
// loop-goroutine death becomes attributable) and then RE-RAISED (so the
// death stays loud and no half-applied state-machine transition is
// silently continued).

func TestEventLoop_Dispatch_HandlerPanic_ObservedThenReraised(t *testing.T) {
	loop := NewEventLoop(16)
	var gotRecovered any
	var gotEvent LoopEvent
	observed := false
	loop.SetPanicHandler(func(r any, e LoopEvent) {
		observed = true
		gotRecovered = r
		gotEvent = e
	})
	loop.RegisterHandler(func(LoopEvent) { panic("boom") })

	ev := LoopEvent{TaskName: "task-1"}
	func() {
		defer func() {
			r := recover()
			if r == nil {
				t.Fatal("dispatch must RE-RAISE the handler panic, but it was swallowed")
			}
			if r != "boom" {
				t.Fatalf("re-raised wrong panic value: %v", r)
			}
		}()
		loop.dispatch(ev)
	}()

	if !observed {
		t.Fatal("panic observer was not invoked")
	}
	if gotRecovered != "boom" {
		t.Fatalf("observer got wrong recovered value: %v", gotRecovered)
	}
	if gotEvent.TaskName != "task-1" {
		t.Fatalf("observer got wrong event: %+v", gotEvent)
	}
}

func TestEventLoop_Dispatch_HandlerPanic_NoObserver_PropagatesUnchanged(t *testing.T) {
	// With no panic handler set, behavior must be identical to a bare
	// handler loop: the panic propagates unchanged.
	loop := NewEventLoop(16)
	loop.RegisterHandler(func(LoopEvent) { panic("raw") })
	func() {
		defer func() {
			if r := recover(); r != "raw" {
				t.Fatalf("expected raw panic to propagate, got %v", r)
			}
		}()
		loop.dispatch(LoopEvent{})
	}()
}

func TestEventLoop_Dispatch_NoPanic_RunsAllHandlers_ObserverSilent(t *testing.T) {
	loop := NewEventLoop(16)
	loop.SetPanicHandler(func(any, LoopEvent) {
		t.Fatal("panic observer must NOT fire when no handler panics")
	})
	calls := 0
	loop.RegisterHandler(func(LoopEvent) { calls++ })
	loop.RegisterHandler(func(LoopEvent) { calls++ })
	loop.dispatch(LoopEvent{})
	if calls != 2 {
		t.Fatalf("want both handlers invoked (2), got %d", calls)
	}
}

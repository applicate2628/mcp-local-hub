package api

// EventBus is a simple in-process SSE broker. Populated in Task 22 of
// Phase 3B; this stub exists so the api package compiles before the bus is
// implemented. Methods intentionally no-op for now.
type EventBus struct{}

func newEventBus() *EventBus { return &EventBus{} }

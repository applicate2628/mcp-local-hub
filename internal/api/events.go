package api

// EventBus is a VESTIGIAL no-op placeholder. The live in-process SSE bus is
// internal/gui/events.go Broadcaster (NewBroadcaster) — that is where event
// types, Publish, persistence, and the /api/events SSE stream actually live.
// This empty struct was reserved for a "Task 22" api-package bus that never
// landed there; the capability shipped in the gui package instead.
//
// DO NOT add event logic, types, or a Publish method here. Any future event
// work (e.g. install-progress/scan-result SSE types) MUST extend the gui
// Broadcaster, not this stub — wiring events into this dead bus would publish
// to nothing. Kept only because api.API still allocates a bus field header
// (api.go) that an api_test.go assertion pins as non-nil; safe because it
// holds no goroutine or background resource (the basis for the shared-API-handle
// safety note on the API type).
type EventBus struct{}

func newEventBus() *EventBus { return &EventBus{} }

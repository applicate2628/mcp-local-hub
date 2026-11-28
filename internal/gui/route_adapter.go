package gui

import "context"

// RunSerenaBackendLossEventSubscriber is §3.x signal #1: the event-driven
// backend-loss trigger. It subscribes to the GUI event bus and, on each serena
// `daemon-backend-lost` event, wakes ReconcileSerenaBackendLossViaIPC instead
// of duplicating the reconcile owner's teardown decisions.
func (s *Server) RunSerenaBackendLossEventSubscriber(ctx context.Context) {
	if s == nil {
		return
	}
	bus := s.Broadcaster()
	if bus == nil {
		return
	}
	// Subscribe is best-effort: if the bounded subscriber channel drops an
	// event, the 30s IPC reconcile fallback remains the correctness floor.
	s.consumeSerenaBackendLossEvents(ctx, bus.Subscribe(ctx))
}

func (s *Server) consumeSerenaBackendLossEvents(ctx context.Context, ch <-chan Event) {
	_ = ctx
	for ev := range ch {
		if ev.Type != "daemon-backend-lost" {
			continue
		}
		if srv, _ := ev.Body["server"].(string); srv != "serena" {
			continue
		}
		s.TriggerSerenaBackendLossReconcile()
	}
}

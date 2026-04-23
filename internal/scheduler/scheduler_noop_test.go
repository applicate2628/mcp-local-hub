package scheduler

import (
	"errors"
	"testing"
)

func TestNew_WithoutEnvReturnsPlatformImpl(t *testing.T) {
	t.Setenv(e2eSchedulerEnv, "")
	s, err := New()
	// On Linux/Darwin, newPlatformScheduler() errors out ("not
	// implemented") — (nil, err). Either way, default path must
	// NOT silently return the noop.
	if err == nil {
		if _, ok := s.(*noopScheduler); ok {
			t.Fatalf("default path must not return noopScheduler")
		}
	}
}

func TestNew_WithE2EEnvReturnsNoop(t *testing.T) {
	t.Setenv(e2eSchedulerEnv, "none")
	s, err := New()
	if err != nil {
		t.Fatalf("noop path must not error: %v", err)
	}
	if _, ok := s.(*noopScheduler); !ok {
		t.Fatalf("MCPHUB_E2E_SCHEDULER=none must return *noopScheduler, got %T", s)
	}
	tasks, err := s.List("mcp-local-hub-")
	if err != nil {
		t.Fatalf("noop List must not error: %v", err)
	}
	if len(tasks) != 0 {
		t.Fatalf("noop List must return empty, got %d entries", len(tasks))
	}
}

func TestNoopScheduler_StatusReturnsNotFound(t *testing.T) {
	var s Scheduler = &noopScheduler{}
	_, err := s.Status("anything")
	if !errors.Is(err, ErrTaskNotFound) {
		t.Fatalf("noop Status must return ErrTaskNotFound, got %v", err)
	}
}

func TestNoopScheduler_CreateRunDeleteAreNoOps(t *testing.T) {
	var s Scheduler = &noopScheduler{}
	if err := s.Create(TaskSpec{Name: "x"}); err != nil {
		t.Errorf("Create: %v", err)
	}
	if err := s.Run("x"); err != nil {
		t.Errorf("Run: %v", err)
	}
	if err := s.Stop("x"); err != nil {
		t.Errorf("Stop: %v", err)
	}
	if err := s.Delete("x"); err != nil {
		t.Errorf("Delete: %v", err)
	}
	if err := s.ImportXML("x", []byte("<Task/>")); err != nil {
		t.Errorf("ImportXML: %v", err)
	}
}

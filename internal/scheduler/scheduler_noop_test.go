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

// TestNoopScheduler_MutationsAreRejected locks in the "hard-fail on
// mutation" contract added after Codex GitHub R1 flagged the original
// silent-success behavior. If MCPHUB_E2E_SCHEDULER=none leaks into a
// production shell, install/restart flows that call scheduler.New()
// must error out loudly instead of reporting phantom success.
func TestNoopScheduler_MutationsAreRejected(t *testing.T) {
	var s Scheduler = &noopScheduler{}
	mutations := []struct {
		name string
		call func() error
	}{
		{"Create", func() error { return s.Create(TaskSpec{Name: "x"}) }},
		{"Delete", func() error { return s.Delete("x") }},
		{"Run", func() error { return s.Run("x") }},
		{"Stop", func() error { return s.Stop("x") }},
		{"ImportXML", func() error { return s.ImportXML("x", []byte("<Task/>")) }},
	}
	for _, m := range mutations {
		err := m.call()
		if err == nil {
			t.Errorf("%s: expected error, got nil (silent success is the regression Codex flagged)", m.name)
			continue
		}
		if !errors.Is(err, errNoopSchedulerMutation) {
			t.Errorf("%s: expected errNoopSchedulerMutation, got %v", m.name, err)
		}
	}
}

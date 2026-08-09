package process

import (
	"context"
	"io"
	"os/exec"
	"sync"
	"testing"
	"time"
)

type r29DeferredReapChild struct {
	*scriptedContainedChild
	mu    sync.Mutex
	order []string
}

func (c *r29DeferredReapChild) terminate(ms uint32) error {
	c.mu.Lock()
	c.order = append(c.order, "terminate")
	c.mu.Unlock()
	return c.scriptedContainedChild.terminate(ms)
}

func (c *r29DeferredReapChild) reapBy(time.Time) (containedWaitResult, bool) {
	c.mu.Lock()
	c.order = append(c.order, "reap")
	c.mu.Unlock()
	return containedWaitResult{exitCode: 0, exited: true}, true
}

func TestR29ContainedCleanupTerminatesGroupBeforeDeferredReap(t *testing.T) {
	child := &r29DeferredReapChild{scriptedContainedChild: newScriptedContainedChild(containedWaitResult{})}
	harness := &containedDependencyHarness{child: child.scriptedContainedChild}
	deps := harness.dependencies()
	deps.newChild = func(*exec.Cmd) (containedChild, error) { return child, nil }
	err := runContainedStreamWithDependencies(
		context.Background(),
		exec.Command("contained-test-helper"),
		ContainedStreamOptions{CleanupTimeout: time.Second, Stderr: io.Discard},
		drainContainedReader,
		deps,
	)
	if err != nil {
		t.Fatalf("run=%v", err)
	}
	child.mu.Lock()
	defer child.mu.Unlock()
	if len(child.order) != 2 || child.order[0] != "terminate" || child.order[1] != "reap" {
		t.Fatalf("cleanup order=%v, want terminate then reap", child.order)
	}
}

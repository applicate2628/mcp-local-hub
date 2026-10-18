package sonnet

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

func TestOutputSettlementDoesNotHoldServerMutex(t *testing.T) {
	root := t.TempDir()
	s := NewServer(testConfig(root, &fakeDriver{}))
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	unblock := func() { once.Do(func() { close(release) }) }
	s.beforeOutput = func() { close(entered); <-release }
	defer func() { unblock(); _ = s.Close(context.Background()) }()
	started, err := s.Start(context.Background(), StartRequest{
		ProjectRoot: writeProject(t), ProjectFile: "sample.son", TimeoutSeconds: 60,
	})
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("worker did not enter output settlement")
	}
	statusDone := make(chan error, 1)
	go func() {
		status, err := s.Status(started.RunID)
		if err == nil && terminal(status.State) {
			err = errors.New("terminal state exposed before output settlement")
		}
		statusDone <- err
	}()
	select {
	case err := <-statusDone:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("Status blocked behind output settlement")
	}
	cancelDone := make(chan error, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
		defer cancel()
		_, err := s.Cancel(ctx, started.RunID, "during-output", true)
		cancelDone <- err
	}()
	select {
	case err := <-cancelDone:
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("Cancel = %v, want bounded wait", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Cancel could not reach its context-aware wait")
	}
	closeDone := make(chan error, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
		defer cancel()
		closeDone <- s.Close(ctx)
	}()
	select {
	case err := <-closeDone:
		if !errors.Is(err, ErrShutdownIncomplete) {
			t.Fatalf("Close = %v, want incomplete shutdown", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Close blocked behind output settlement")
	}
	next := NewServer(testConfig(root, &fakeDriver{}))
	if !errors.Is(next.initErr, ErrRunStoreOwned) {
		_ = next.Close(context.Background())
		t.Fatalf("store lease released before worker settled: %v", next.initErr)
	}
	unblock()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := s.Close(ctx); err != nil {
		t.Fatal(err)
	}
	status, err := s.Status(started.RunID)
	if err != nil || status.State != StateForceCancelled {
		t.Fatalf("settled status=%+v err=%v", status, err)
	}
}

package sonnet

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestStartRejectsPostCopyHandoffAfterClose(t *testing.T) {
	for _, cleanupFails := range []bool{false, true} {
		name := "cleanup_succeeds"
		if cleanupFails {
			name = "cleanup_failure_preserves_both_causes"
		}
		t.Run(name, func(t *testing.T) {
			projectRoot := writeProject(t)
			storeRoot := t.TempDir()
			driver := &fakeDriver{}
			s := NewServer(testConfig(storeRoot, driver))
			defer s.Close(context.Background())
			arrived := make(chan struct{})
			release := make(chan struct{})
			finished := make(chan struct{})
			var releaseOnce sync.Once
			unblock := func() { releaseOnce.Do(func() { close(release) }) }
			s.afterCopy = func() {
				close(arrived)
				<-release
			}
			cleanupErr := errors.New("injected cleanup failure")
			var cleanupPath string
			s.removeWork = func(path string) error {
				cleanupPath = path
				if cleanupFails {
					return cleanupErr
				}
				return os.RemoveAll(path)
			}
			results := make(chan concurrentStartResult, 1)
			go func() {
				defer close(finished)
				response, err := s.Start(context.Background(), StartRequest{
					ProjectRoot: projectRoot, ProjectFile: "sample.son",
					TimeoutSeconds: 10, IdempotencyKey: "late-start",
				})
				results <- concurrentStartResult{response: response, err: err}
			}()
			defer func() {
				unblock()
				select {
				case <-finished:
				case <-time.After(5 * time.Second):
					t.Error("Start did not finish after releasing the copy barrier")
				}
			}()
			select {
			case <-arrived:
			case <-time.After(5 * time.Second):
				t.Fatal("Start did not reach the post-copy barrier")
			}
			if got := countRunWorkspaces(t, storeRoot); got != 1 {
				t.Fatalf("copied workspaces=%d, want one", got)
			}
			closeCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			if err := s.Close(closeCtx); err != nil {
				t.Fatal(err)
			}
			// Prove another owner can acquire the store before the late handoff.
			next := NewServer(testConfig(storeRoot, &fakeDriver{}))
			defer next.Close(context.Background())
			if next.initErr != nil {
				t.Fatalf("replacement owner failed: %v", next.initErr)
			}
			unblock()
			var result concurrentStartResult
			select {
			case result = <-results:
			case <-time.After(5 * time.Second):
				t.Fatal("late Start did not return")
			}
			if result.err == nil || !strings.Contains(result.err.Error(), "SONNET_RUN_OWNER_CLOSED") {
				t.Fatalf("late Start error=%v, want closed-owner refusal", result.err)
			}
			if result.response != (StartResponse{}) {
				t.Fatalf("refused Start returned a receipt: %+v", result.response)
			}
			s.mu.Lock()
			runs, keys, workers, generation := len(s.runs), len(s.keys), s.workers, s.next
			s.mu.Unlock()
			if runs != 0 || keys != 0 || workers != 0 || generation != 0 {
				t.Fatalf("late admission changed state: runs=%d keys=%d workers=%d next=%d", runs, keys, workers, generation)
			}
			driver.mu.Lock()
			calls := driver.calls
			driver.mu.Unlock()
			if calls != 0 {
				t.Fatalf("closed owner invoked driver %d times", calls)
			}
			if cleanupPath == "" || !within(filepath.Join(storeRoot, "runs"), cleanupPath) {
				t.Fatalf("cleanup did not target the copied workspace: %q", cleanupPath)
			}
			if cleanupFails {
				if !errors.Is(result.err, cleanupErr) || !strings.Contains(result.err.Error(), "SONNET_WORKSPACE_CLEANUP_FAILED") {
					t.Fatalf("cleanup failure was lost: %v", result.err)
				}
				if _, err := os.Stat(cleanupPath); err != nil {
					t.Fatalf("failed cleanup workspace unavailable: %v", err)
				}
				for _, leaf := range []string{"control", activeMarkerLeaf} {
					if _, err := os.Stat(filepath.Join(cleanupPath, leaf)); !os.IsNotExist(err) {
						t.Fatalf("refused Start created %q: %v", leaf, err)
					}
				}
			} else {
				assertOwnedWorkspaceRemoved(t, storeRoot, cleanupPath)
			}
			if body, err := os.ReadFile(filepath.Join(projectRoot, "sample.son")); err != nil || string(body) != admittedTestSON {
				t.Fatalf("source changed: body=%q err=%v", body, err)
			}
		})
	}
}

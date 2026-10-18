package sonnet

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestIdempotencyRequestBindingSurvivesRestart(t *testing.T) {
	root := writeProject(t)
	storeRoot := t.TempDir()
	driver := &fakeDriver{}
	s := NewServer(testConfig(storeRoot, driver))
	defer func() { _ = s.Close(context.Background()) }()
	req := StartRequest{ProjectRoot: root, ProjectFile: "sample.son", TimeoutSeconds: 10, IdempotencyKey: "bound"}
	first, err := s.Start(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	waitTerminal(t, s, first.RunID)
	for _, restart := range []bool{false, true} {
		if restart {
			if err := s.Close(context.Background()); err != nil {
				t.Fatal(err)
			}
			s = NewServer(testConfig(storeRoot, driver))
			if s.initErr != nil {
				t.Fatal(s.initErr)
			}
		}
		equivalent := req
		equivalent.ProjectRoot = root + string(filepath.Separator) + "."
		equivalent.ProjectFile = "." + string(filepath.Separator) + "sample.son"
		if runtime.GOOS == "windows" {
			equivalent.ProjectRoot = strings.ToUpper(equivalent.ProjectRoot)
			equivalent.ProjectFile = strings.ToUpper(equivalent.ProjectFile)
		}
		replay, err := s.Start(context.Background(), equivalent)
		if err != nil || replay.RunID != first.RunID || replay.RequestDisposition != "replayed" {
			t.Fatalf("restart=%v replay=%+v err=%v", restart, replay, err)
		}
		for _, field := range []string{"root", "file", "timeout"} {
			changed := req
			switch field {
			case "root":
				changed.ProjectRoot = filepath.Join(root, "other")
			case "file":
				changed.ProjectFile = "other.son"
			case "timeout":
				changed.TimeoutSeconds++
			}
			if _, err := s.Start(context.Background(), changed); !errors.Is(err, ErrIdempotencyConflict) {
				t.Fatalf("restart=%v field=%s err=%v", restart, field, err)
			}
		}
	}
	driver.mu.Lock()
	calls := driver.calls
	driver.mu.Unlock()
	if calls != 1 || countRunWorkspaces(t, storeRoot) != 1 {
		t.Fatalf("replay/conflict created work: driver calls=%d", calls)
	}
	// Old persisted receipts cannot safely authorize replay of unknown arguments.
	s.mu.Lock()
	s.runs[first.RunID].RequestHash = ""
	s.mu.Unlock()
	if _, err := s.Start(context.Background(), req); !errors.Is(err, ErrIdempotencyConflict) {
		t.Fatalf("legacy digestless replay=%v", err)
	}
}

func TestConcurrentIdempotencyConflictCleansLosingCopy(t *testing.T) {
	root := writeProject(t)
	storeRoot := t.TempDir()
	driver := &fakeDriver{}
	s := NewServer(testConfig(storeRoot, driver))
	defer s.Close(context.Background())
	arrived := make(chan struct{}, 2)
	release := make(chan struct{})
	s.afterCopy = func() { arrived <- struct{}{}; <-release }
	results := make(chan concurrentStartResult, 2)
	for _, timeout := range []int{10, 11} {
		go func(timeout int) {
			response, err := s.Start(context.Background(), StartRequest{
				ProjectRoot: root, ProjectFile: "sample.son", TimeoutSeconds: timeout, IdempotencyKey: "collision",
			})
			results <- concurrentStartResult{response: response, err: err}
		}(timeout)
	}
	for range 2 {
		select {
		case <-arrived:
		case <-time.After(5 * time.Second):
			close(release)
			t.Fatal("concurrent Starts did not reach the copy barrier")
		}
	}
	close(release)
	successes, conflicts := 0, 0
	for range 2 {
		select {
		case result := <-results:
			if result.err == nil {
				successes++
				waitTerminal(t, s, result.response.RunID)
			} else if errors.Is(result.err, ErrIdempotencyConflict) {
				conflicts++
			} else {
				t.Fatalf("unexpected Start error: %v", result.err)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("concurrent Start did not finish")
		}
	}
	if successes != 1 || conflicts != 1 || countRunWorkspaces(t, storeRoot) != 1 {
		t.Fatalf("successes=%d conflicts=%d; want one retained workspace", successes, conflicts)
	}
	driver.mu.Lock()
	calls := driver.calls
	driver.mu.Unlock()
	if calls != 1 {
		t.Fatalf("driver calls=%d", calls)
	}
}

func TestTimedOutFlagPersistsWithTerminalState(t *testing.T) {
	root := writeProject(t)
	storeRoot := t.TempDir()
	driver := &fakeDriver{done: make(chan struct{})}
	s := NewServer(testConfig(storeRoot, driver))
	defer s.Close(context.Background())
	started, err := s.Start(context.Background(), StartRequest{ProjectRoot: root, ProjectFile: "sample.son", TimeoutSeconds: 1})
	if err != nil {
		t.Fatal(err)
	}
	eventually(t, 5*time.Second, func() bool {
		status, err := s.Status(started.RunID)
		return err == nil && status.State == StateTimedOut
	})
	if err := s.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	restarted := NewServer(testConfig(storeRoot, &fakeDriver{}))
	defer restarted.Close(context.Background())
	for _, owner := range []*Server{s, restarted} {
		status, err := owner.Status(started.RunID)
		if err != nil || status.State != StateTimedOut || !status.TimedOut || status.FailureID != "SONNET_RUN_TIMED_OUT" {
			t.Fatalf("timeout status=%+v err=%v", status, err)
		}
	}
	if body, err := os.ReadFile(filepath.Join(root, "sample.son")); err != nil || string(body) != admittedTestSON {
		t.Fatalf("source changed: %q %v", body, err)
	}
}

func TestRequestHashPathCaseFollowsPlatformIdentity(t *testing.T) {
	req := StartRequest{ProjectRoot: filepath.Join(t.TempDir(), "MixedCase"), ProjectFile: "SubDir/Project.son", TimeoutSeconds: 10}
	original, err := startRequestHash(req)
	if err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{"root", "file"} {
		changed := req
		if field == "root" {
			changed.ProjectRoot = strings.ToUpper(changed.ProjectRoot)
		} else {
			changed.ProjectFile = strings.ToUpper(changed.ProjectFile)
		}
		got, err := startRequestHash(changed)
		if err != nil {
			t.Fatal(err)
		}
		if (got == original) != (runtime.GOOS == "windows") {
			t.Fatalf("%s case identity on %s: equal=%v", field, runtime.GOOS, got == original)
		}
	}
}

func TestReplayDoesNotRequireProductDiscovery(t *testing.T) {
	for _, discoveryFailure := range []string{"missing", "ambiguous", "unavailable"} {
		t.Run(discoveryFailure, func(t *testing.T) {
			root, storeRoot := writeProject(t), t.TempDir()
			driver := &fakeDriver{}
			cfg := testConfig(storeRoot, driver)
			workingLocator := cfg.Locator
			discoveryCalls := 0
			failDiscovery := false
			cfg.Locator = func() (Product, error) {
				discoveryCalls++
				if !failDiscovery {
					return workingLocator()
				}
				switch discoveryFailure {
				case "missing":
					return Product{}, errors.New("SONNET_PRODUCT_NOT_FOUND")
				case "ambiguous":
					return Product{}, errors.New("SONNET_PRODUCT_AMBIGUOUS")
				default:
					return Product{}, nil
				}
			}
			s := NewServer(cfg)
			defer func() { _ = s.Close(context.Background()) }()
			req := StartRequest{ProjectRoot: root, ProjectFile: "sample.son", TimeoutSeconds: 10, IdempotencyKey: "durable"}
			first, err := s.Start(context.Background(), req)
			if err != nil {
				t.Fatal(err)
			}
			waitTerminal(t, s, first.RunID)
			failDiscovery = true
			for _, restart := range []bool{false, true} {
				if restart {
					if err := s.Close(context.Background()); err != nil {
						t.Fatal(err)
					}
					s = NewServer(cfg)
					if s.initErr != nil {
						t.Fatal(s.initErr)
					}
				}
				before := discoveryCalls
				replay, err := s.Start(context.Background(), req)
				if err != nil || replay.RunID != first.RunID || replay.RequestDisposition != "replayed" {
					t.Fatalf("restart=%v replay=%+v err=%v", restart, replay, err)
				}
				conflict := req
				conflict.TimeoutSeconds++
				if _, err := s.Start(context.Background(), conflict); !errors.Is(err, ErrIdempotencyConflict) {
					t.Fatalf("restart=%v conflict=%v", restart, err)
				}
				if discoveryCalls != before {
					t.Fatal("replay/conflict attempted product discovery")
				}
				fresh := req
				fresh.IdempotencyKey = "new"
				if _, err := s.Start(context.Background(), fresh); err == nil {
					t.Fatal("new request bypassed unavailable product")
				}
				if discoveryCalls != before+1 {
					t.Fatal("new request did not attempt product discovery exactly once")
				}
			}
			driver.mu.Lock()
			calls := driver.calls
			driver.mu.Unlock()
			if calls != 1 || countRunWorkspaces(t, storeRoot) != 1 {
				t.Fatalf("replay/failure created work: driver calls=%d", calls)
			}
		})
	}
}

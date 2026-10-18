package api

import (
	"context"
	"errors"
	"testing"
)

func TestCodexProjectConfigOwnerReadContextReportsSettlement(t *testing.T) {
	bodyCalls, releaseCalls := 0, 0
	releaseErr := errors.New("synthetic read release error")
	owner := &CodexProjectConfigMutationOwner{
		lockPath: "synthetic-unused-leaf",
		acquire:  func(string) (func() error, error) { t.Fatal("context read used blocking acquisition"); return nil, nil },
		tryAcquire: func(string) (func() error, bool, error) {
			return func() error { releaseCalls++; return releaseErr }, true, nil
		},
	}
	observed, err := owner.RunReadContext(context.Background(), func() error { bodyCalls++; return nil })
	if err == nil || !errors.Is(err, releaseErr) || bodyCalls != 1 || releaseCalls != 1 || !observed.BodyEntered || !observed.ReleaseDefined || observed.ReleaseSucceeded == nil || *observed.ReleaseSucceeded || !errors.Is(observed.CleanupError, releaseErr) {
		t.Fatal("read body/release outcome was not received")
	}
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	observed, err = owner.RunReadContext(canceled, func() error { bodyCalls++; return nil })
	if !errors.Is(err, context.Canceled) || observed.BodyEntered || bodyCalls != 1 || releaseCalls != 1 {
		t.Fatal("canceled read entered its body or lost refusal")
	}
}

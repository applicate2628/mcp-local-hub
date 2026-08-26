package apitest

import (
	"errors"
	"os"
	"testing"
)

func TestRemoveTestMainRootWith_RejectsSilentResidue(t *testing.T) {
	removeCalls := 0
	err := removeTestMainRootWith(
		"test-root",
		3,
		func(string) error { removeCalls++; return nil },
		func(string) (os.FileInfo, error) { return nil, nil },
		func() {},
	)
	if err == nil {
		t.Fatal("cleanup succeeded while exact test root remained")
	}
	if removeCalls != 3 {
		t.Fatalf("remove calls = %d, want bounded 3 attempts", removeCalls)
	}
}

func TestRemoveTestMainRootWith_RetriesTransientResidueAndVerifiesAbsence(t *testing.T) {
	removeCalls := 0
	statCalls := 0
	err := removeTestMainRootWith(
		"test-root",
		3,
		func(string) error { removeCalls++; return nil },
		func(string) (os.FileInfo, error) {
			statCalls++
			if statCalls == 1 {
				return nil, nil
			}
			return nil, os.ErrNotExist
		},
		func() {},
	)
	if err != nil {
		t.Fatalf("cleanup error = %v", err)
	}
	if removeCalls != 2 {
		t.Fatalf("remove calls = %d, want 2", removeCalls)
	}
}

func TestRemoveTestMainRootWith_ReturnsRemovalCause(t *testing.T) {
	want := errors.New("locked")
	err := removeTestMainRootWith(
		"test-root",
		1,
		func(string) error { return want },
		func(string) (os.FileInfo, error) { return nil, nil },
		func() {},
	)
	if !errors.Is(err, want) {
		t.Fatalf("cleanup error = %v, want cause %v", err, want)
	}
}

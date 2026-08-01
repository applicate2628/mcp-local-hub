package api

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"path/filepath"
	"strings"
)

// StateFileReadErrorCategory is a stable machine-readable outcome from the
// retained-handle state reader. Callers must branch on this category rather
// than on error prose, which may change without changing the safety result.
type StateFileReadErrorCategory string

const (
	StateFileReadErrorInvalidInput     StateFileReadErrorCategory = "invalid-input"
	StateFileReadErrorCanceled         StateFileReadErrorCategory = "canceled"
	StateFileReadErrorTooLarge         StateFileReadErrorCategory = "too-large"
	StateFileReadErrorChecksumMismatch StateFileReadErrorCategory = "checksum-mismatch"
	StateFileReadErrorUnsafeObjectOrIO StateFileReadErrorCategory = "unsafe-object-or-io"
)

// StateFileReadError carries the reader's safety decision and retains the
// underlying operating-system failure for errors.Is/errors.As diagnostics.
type StateFileReadError struct {
	Category  StateFileReadErrorCategory
	Operation string
	Component string
	Cause     error
}

func (e *StateFileReadError) Error() string {
	if e.Component != "" {
		return fmt.Sprintf("state file read %s during %s for component %q", e.Category, e.Operation, e.Component)
	}
	return fmt.Sprintf("state file read %s during %s", e.Category, e.Operation)
}

func (e *StateFileReadError) Unwrap() error {
	return e.Cause
}

func newStateFileReadError(category StateFileReadErrorCategory, operation, component string, cause error) *StateFileReadError {
	return &StateFileReadError{Category: category, Operation: operation, Component: component, Cause: cause}
}

func validateStateReadBeneathRootInput(root string, relativeComponents []string, expectedSHA256 string) *StateFileReadError {
	if !filepath.IsAbs(root) {
		return newStateFileReadError(StateFileReadErrorInvalidInput, "validate", "", fmt.Errorf("state root must be absolute"))
	}
	if len(relativeComponents) == 0 {
		return newStateFileReadError(StateFileReadErrorInvalidInput, "validate", "", fmt.Errorf("state path has no relative components"))
	}
	if len(expectedSHA256) != sha256.Size*2 {
		return newStateFileReadError(StateFileReadErrorInvalidInput, "validate", "", fmt.Errorf("state file checksum is not a SHA-256 digest"))
	}
	if _, err := hex.DecodeString(expectedSHA256); err != nil {
		return newStateFileReadError(StateFileReadErrorInvalidInput, "validate", "", err)
	}
	for _, component := range relativeComponents {
		if component == "" || component == "." || component == ".." || filepath.IsAbs(component) ||
			filepath.Base(component) != component || strings.ContainsAny(component, `/\\`) {
			return newStateFileReadError(StateFileReadErrorInvalidInput, "validate", component, nil)
		}
	}
	return nil
}

func minStateReadCapacity(size int64) int {
	if size <= 0 {
		return 0
	}
	if size > maxStateFileBytes {
		return maxStateFileBytes
	}
	return int(size)
}

func stateReadChecksumMatches(bytes []byte, expectedSHA256 string) bool {
	sum := sha256.Sum256(bytes)
	return strings.EqualFold(hex.EncodeToString(sum[:]), expectedSHA256)
}

const stateReadChunkSize = 4096

// stateReadRequestLimit asks for one byte beyond the remaining safe capacity.
// That sentinel byte proves a file that grows after the initial stat exceeds
// the cap before append can allocate beyond the bounded buffer.
func stateReadRequestLimit(bufferLength, chunkLength int) int {
	remaining := maxStateFileBytes - bufferLength
	if remaining < 0 || chunkLength <= 0 {
		return 0
	}
	limit := remaining + 1
	if limit < chunkLength {
		return limit
	}
	return chunkLength
}

type stateReadBeneathRootStepEvent string

const (
	stateReadBeneathRootBeforeComponentOpen stateReadBeneathRootStepEvent = "before-component-open"
	stateReadBeneathRootBeforeRead          stateReadBeneathRootStepEvent = "before-read"
)

type stateReadBeneathRootStep struct {
	Event          stateReadBeneathRootStepEvent
	ComponentIndex int
	Component      string
	Requested      int
}

type stateReadBeneathRootStepFunc func(stateReadBeneathRootStep) error

func invokeStateReadBeneathRootStep(step stateReadBeneathRootStepFunc, event stateReadBeneathRootStep) error {
	if step == nil {
		return nil
	}
	if err := step(event); err != nil {
		return newStateFileReadError(StateFileReadErrorUnsafeObjectOrIO, string(event.Event), event.Component, err)
	}
	return nil
}

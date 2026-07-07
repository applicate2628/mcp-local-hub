package process

import (
	"fmt"
	"path/filepath"
	"time"
)

const pidIdentityStartTolerance = 2 * time.Second

func parseExpectedStartedAt(s string) (time.Time, error) {
	if s == "" {
		return time.Time{}, fmt.Errorf("%w: missing started_at", ErrProcessIdentityMismatch)
	}
	t, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		return time.Time{}, fmt.Errorf("%w: parse started_at %q: %v", ErrProcessIdentityMismatch, s, err)
	}
	return t.UTC(), nil
}

func startTimesMatch(recorded, observed time.Time) bool {
	return startTimesMatchWithin(recorded, observed, pidIdentityStartTolerance)
}

func startTimesMatchWithin(recorded, observed time.Time, tolerance time.Duration) bool {
	delta := recorded.Sub(observed)
	if delta < 0 {
		delta = -delta
	}
	return delta <= tolerance
}

func pidIdentityStartToleranceFor(proof PIDIdentityProof) time.Duration {
	if proof.StartTolerance > 0 {
		return proof.StartTolerance
	}
	return pidIdentityStartTolerance
}

func normalizeExpectedExecutablePath(path string) (string, error) {
	if path == "" {
		return "", fmt.Errorf("%w: missing executable path", ErrProcessIdentityMismatch)
	}
	if abs, err := filepath.Abs(path); err == nil {
		path = abs
	}
	if resolved, err := filepath.EvalSymlinks(path); err == nil {
		path = resolved
	}
	return filepath.Clean(path), nil
}

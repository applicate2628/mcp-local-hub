// Package api — RetryPolicy timing surface. Memo D9.
package api

import (
	"fmt"
	"time"
)

// RetryPolicy is the timing/budget surface. Implementations are stateless.
type RetryPolicy interface {
	// Backoff returns the delay to wait BEFORE attempt+1 after a failed
	// attempt. attempt is 1-indexed (Backoff(1) is the first wait).
	Backoff(attempt int) time.Duration
	// MaxAttempts is the total attempt budget. Backoff is consulted up
	// to MaxAttempts-1 times.
	MaxAttempts() int
}

// PolicyFromString returns the policy named by s. Memo D9 names:
// "none" | "linear" | "exponential". Unknown returns error.
func PolicyFromString(s string) (RetryPolicy, error) {
	switch s {
	case "none":
		return nonePolicy{}, nil
	case "linear":
		return linearPolicy{step: 2 * time.Second, max: 3}, nil
	case "exponential":
		return exponentialPolicy{base: time.Second, cap: 5 * time.Minute, max: 5}, nil
	default:
		return nil, fmt.Errorf("unknown retry policy %q (must be none|linear|exponential)", s)
	}
}

type nonePolicy struct{}

func (nonePolicy) Backoff(attempt int) time.Duration { return 0 }
func (nonePolicy) MaxAttempts() int                  { return 1 }

type linearPolicy struct {
	step time.Duration
	max  int
}

func (p linearPolicy) Backoff(attempt int) time.Duration { return p.step * time.Duration(attempt) }
func (p linearPolicy) MaxAttempts() int                  { return p.max }

type exponentialPolicy struct {
	base time.Duration
	cap  time.Duration
	max  int
}

func (p exponentialPolicy) Backoff(attempt int) time.Duration {
	d := p.base
	for i := 1; i < attempt; i++ {
		d *= 2
		if d > p.cap {
			return p.cap
		}
	}
	return d
}

func (p exponentialPolicy) MaxAttempts() int { return p.max }

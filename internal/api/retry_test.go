package api

import (
	"errors"
	"io/fs"
	"os"
	"testing"
	"time"
)

func TestPolicyFromString_KnownStrings(t *testing.T) {
	cases := []string{"none", "linear", "exponential"}
	for _, name := range cases {
		p, err := PolicyFromString(name)
		if err != nil {
			t.Errorf("PolicyFromString(%q): %v", name, err)
			continue
		}
		if p == nil {
			t.Errorf("PolicyFromString(%q) returned nil policy", name)
		}
	}
}

func TestPolicyFromString_UnknownReturnsError(t *testing.T) {
	_, err := PolicyFromString("custom")
	if err == nil {
		t.Error("expected error for unknown policy name")
	}
}

func TestNonePolicy_BackoffAndMaxAttempts(t *testing.T) {
	p, _ := PolicyFromString("none")
	if p.MaxAttempts() != 1 {
		t.Errorf("none MaxAttempts = %d, want 1 (no retries means 1 attempt)", p.MaxAttempts())
	}
	if p.Backoff(1) != 0 {
		t.Errorf("none Backoff(1) = %v, want 0", p.Backoff(1))
	}
}

func TestLinearPolicy_BackoffSequence(t *testing.T) {
	p, _ := PolicyFromString("linear")
	if p.MaxAttempts() < 3 {
		t.Errorf("linear MaxAttempts = %d, want at least 3", p.MaxAttempts())
	}
	if p.Backoff(1) <= 0 {
		t.Errorf("linear Backoff(1) = %v, want > 0", p.Backoff(1))
	}
	if p.Backoff(2) != 2*p.Backoff(1) {
		t.Errorf("linear Backoff(2) = %v, want 2*Backoff(1)=%v", p.Backoff(2), 2*p.Backoff(1))
	}
	if p.Backoff(3) != 3*p.Backoff(1) {
		t.Errorf("linear Backoff(3) = %v, want 3*Backoff(1)", p.Backoff(3))
	}
}

func TestExponentialPolicy_BackoffSequence(t *testing.T) {
	p, _ := PolicyFromString("exponential")
	if p.MaxAttempts() < 5 {
		t.Errorf("exponential MaxAttempts = %d, want at least 5", p.MaxAttempts())
	}
	if p.Backoff(1) <= 0 {
		t.Error("exponential Backoff(1) must be > 0")
	}
	if p.Backoff(2) != 2*p.Backoff(1) {
		t.Errorf("exponential Backoff(2) = %v, want 2*Backoff(1)", p.Backoff(2))
	}
	if p.Backoff(3) != 4*p.Backoff(1) {
		t.Errorf("exponential Backoff(3) = %v, want 4*Backoff(1)", p.Backoff(3))
	}
	if p.Backoff(20) > 5*time.Minute {
		t.Errorf("exponential Backoff(20) = %v, must be capped at 5min", p.Backoff(20))
	}
}

func TestIsRetryableError_NilDefensive(t *testing.T) {
	if IsRetryableError(nil) {
		t.Error("IsRetryableError(nil) = true, want false (defensive degenerate)")
	}
}

func TestIsRetryableError_NonRetryableClasses(t *testing.T) {
	cases := []error{
		ErrBinaryNotFound,
		ErrPermissionDenied,
		ErrBadConfig,
		ErrUnrecoverableLockState,
		fs.ErrPermission,
		os.ErrNotExist,
	}
	for _, e := range cases {
		if IsRetryableError(e) {
			t.Errorf("IsRetryableError(%v) = true, want false", e)
		}
	}
}

func TestIsRetryableError_RetryableClasses(t *testing.T) {
	cases := []error{
		errors.New("port already in use"),
		errors.New("temporary EAGAIN"),
		errors.New("scheduler service hiccup"),
	}
	for _, e := range cases {
		if !IsRetryableError(e) {
			t.Errorf("IsRetryableError(%v) = false, want true", e)
		}
	}
}

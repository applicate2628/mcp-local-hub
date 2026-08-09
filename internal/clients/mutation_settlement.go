package clients

import "errors"

// ClientMutationSettlement is the single clients-owned interpretation of an
// entry mutation result. Transaction owners use it to decide whether their
// existing compensator remains armed; they do not infer applied state from an
// undifferentiated error chain.
type ClientMutationSettlement uint8

const (
	ClientMutationNeedsCompensation ClientMutationSettlement = iota
	ClientMutationApplied
	ClientMutationAppliedReleaseUnconfirmed
)

// appliedReleaseUnconfirmedError is deliberately private: only
// ClassifyClientMutation exposes the fact that the body applied. The error
// remains compatible with ErrConfigLockReleaseUnconfirmed and its cause.
type appliedReleaseUnconfirmedError struct {
	err error
}

func (e *appliedReleaseUnconfirmedError) Error() string { return e.err.Error() }
func (e *appliedReleaseUnconfirmedError) Unwrap() error { return e.err }

// ClassifyClientMutation converts a lockingClient entry-mutation result into
// its one settlement disposition. A normal body error stays conservative even
// when an adapter could have written before returning it.
func ClassifyClientMutation(err error) ClientMutationSettlement {
	if err == nil {
		return ClientMutationApplied
	}
	var appliedRelease *appliedReleaseUnconfirmedError
	if errors.As(err, &appliedRelease) {
		return ClientMutationAppliedReleaseUnconfirmed
	}
	return ClientMutationNeedsCompensation
}

func withConfigMutationLock(configPath string, fn func() error) error {
	execution, err := withConfigLockExecution(configPath, fn)
	if execution.bodyApplied() && execution.releaseErr != nil {
		return &appliedReleaseUnconfirmedError{err: err}
	}
	return err
}

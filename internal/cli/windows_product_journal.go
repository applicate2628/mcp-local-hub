package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strconv"
	"sync"

	"mcp-local-hub/internal/api"
)

// Initial operator-selected encoded journal budget; the lazy resolver may
// override it. This is not a whole-state cap or a peak-memory guarantee.
const defaultWindowsProductJournalMaxBytes int64 = 32 << 20

// WindowsProductJournalAllowance is one operation's immutable encoded-byte
// admission. Its zero value grants no journal allocation or publication.
type WindowsProductJournalAllowance struct {
	maxEncodedBytes int64
}

func NewWindowsProductJournalAllowance(maxEncodedBytes int64) (WindowsProductJournalAllowance, error) {
	if err := api.ValidateStateFileReadCapacity(maxEncodedBytes); err != nil {
		return WindowsProductJournalAllowance{}, fmt.Errorf("Product journal allowance: %w", err)
	}
	return WindowsProductJournalAllowance{maxEncodedBytes: maxEncodedBytes}, nil
}

func resolveWindowsProductJournalAllowance() (WindowsProductJournalAllowance, error) {
	const name = "MCPHUB_PRODUCT_JOURNAL_MAX_BYTES"
	raw, present := os.LookupEnv(name)
	if !present {
		return NewWindowsProductJournalAllowance(defaultWindowsProductJournalMaxBytes)
	}
	if raw == "" {
		return WindowsProductJournalAllowance{}, fmt.Errorf("%s must be a positive decimal byte capacity when supplied", name)
	}
	for _, digit := range raw {
		if digit < '0' || digit > '9' {
			return WindowsProductJournalAllowance{}, fmt.Errorf("%s must be a positive decimal byte capacity", name)
		}
	}
	capacity, err := strconv.ParseInt(raw, 10, 64)
	if err != nil {
		return WindowsProductJournalAllowance{}, fmt.Errorf("%s: %w", name, err)
	}
	return NewWindowsProductJournalAllowance(capacity)
}

func admitWindowsProductRecoveryJournalBytes(ctx context.Context, allowance WindowsProductJournalAllowance, raw []byte) error {
	if ctx == nil {
		return errors.New("Product journal context required")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := api.ValidateStateFileReadCapacity(allowance.maxEncodedBytes); err != nil {
		return fmt.Errorf("Product journal allowance: %w", err)
	}
	if len(raw) == 0 {
		return errors.New("encoded Product journal is empty")
	}
	if int64(len(raw)) > allowance.maxEncodedBytes {
		return fmt.Errorf("encoded Product journal size %d exceeds admitted capacity %d", len(raw), allowance.maxEncodedBytes)
	}
	return nil
}

func readWindowsProductRecoveryJournal(ctx context.Context, path string, allowance WindowsProductJournalAllowance) ([]byte, *api.StateFileReadObservation, error) {
	if ctx == nil {
		return nil, nil, errors.New("Product journal context required")
	}
	if err := ctx.Err(); err != nil {
		return nil, nil, err
	}
	if err := api.ValidateStateFileReadCapacity(allowance.maxEncodedBytes); err != nil {
		return nil, nil, fmt.Errorf("Product journal allowance: %w", err)
	}
	return api.ReadStateFileInodeAnchoredObserved(ctx, path, allowance.maxEncodedBytes)
}

type windowsProductJournalReadOutcome struct {
	phase     string
	path      string
	allowance WindowsProductJournalAllowance
	original  *api.StateFileReadObservation
	readError error
}

// Read evidence belongs to the existing serialized operation. Concurrent reader
// callbacks may append results; they cannot replace earlier actual outcomes.
// The API reader finalizes its own resources before returning its observation.
type windowsProductJournalReadEvidence struct {
	mu       sync.Mutex
	outcomes []windowsProductJournalReadOutcome
}

func (e *windowsProductJournalReadEvidence) retain(outcome windowsProductJournalReadOutcome) {
	if e == nil {
		return
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	e.outcomes = append(e.outcomes, outcome)
}

func (e *windowsProductJournalReadEvidence) observations() []*api.StateFileReadObservation {
	if e == nil {
		return nil
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	result := make([]*api.StateFileReadObservation, 0, len(e.outcomes))
	for _, outcome := range e.outcomes {
		if outcome.original != nil {
			result = append(result, outcome.original)
		}
	}
	return result
}

type windowsProductJournalError struct {
	cause    error
	evidence *windowsProductJournalReadEvidence
}

func (e *windowsProductJournalError) Error() string { return e.cause.Error() }
func (e *windowsProductJournalError) Unwrap() error { return e.cause }
func (e *windowsProductJournalError) JournalReadObservations() []*api.StateFileReadObservation {
	return e.evidence.observations()
}

func withWindowsProductJournalEvidence(err error, evidence *windowsProductJournalReadEvidence) error {
	if err == nil || evidence == nil {
		return err
	}
	return &windowsProductJournalError{cause: err, evidence: evidence}
}

type windowsProductJournalBinding struct {
	ctx       context.Context
	allowance WindowsProductJournalAllowance
	evidence  *windowsProductJournalReadEvidence
	phase     func() string
}

func (b windowsProductJournalBinding) reader(path string) ([]byte, error) {
	return b.read(b.ctx, path)
}

// readerContext keeps the original operation bound and adds the read-only
// caller's cancellation and deadline to the same observed reader.
func (b windowsProductJournalBinding) readerContext(ctx context.Context, path string) ([]byte, error) {
	if b.ctx == nil || ctx == nil {
		return nil, errors.New("Product journal context required")
	}

	var readCtx context.Context
	var cancel context.CancelFunc
	if deadline, ok := b.ctx.Deadline(); ok {
		readCtx, cancel = context.WithDeadline(ctx, deadline)
	} else {
		readCtx, cancel = context.WithCancel(ctx)
	}
	defer cancel()
	stop := context.AfterFunc(b.ctx, cancel)
	defer stop()
	if b.ctx.Err() != nil {
		cancel()
	}

	raw, err := b.read(readCtx, path)
	for _, contextErr := range []error{readCtx.Err(), b.ctx.Err()} {
		if contextErr != nil {
			raw = nil
			if !errors.Is(err, contextErr) {
				err = errors.Join(err, contextErr)
			}
		}
	}
	return raw, err
}

func (b windowsProductJournalBinding) read(ctx context.Context, path string) ([]byte, error) {
	if b.evidence == nil {
		return nil, errors.New("Product journal operation evidence owner required")
	}
	phase := "unbound-journal-read"
	if b.phase != nil {
		phase = b.phase()
	}
	raw, original, err := readWindowsProductRecoveryJournal(ctx, path, b.allowance)
	b.evidence.retain(windowsProductJournalReadOutcome{phase: phase, path: path, allowance: b.allowance, original: original, readError: err})
	return raw, err
}

func validateWindowsProductJournalOperation(ctx context.Context, allowance WindowsProductJournalAllowance) error {
	if ctx == nil {
		return errors.New("Product journal context required")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	return api.ValidateStateFileReadCapacity(allowance.maxEncodedBytes)
}

func (t *WindowsProductTxn) journalBinding() windowsProductJournalBinding {
	if t == nil {
		return windowsProductJournalBinding{}
	}
	if t.journalReadEvidence == nil {
		t.journalReadEvidence = t.Opts.journalReadEvidence
		if t.journalReadEvidence == nil {
			t.journalReadEvidence = &windowsProductJournalReadEvidence{}
		}
	}
	binding := windowsProductJournalBinding{ctx: t.Opts.Context, allowance: t.Opts.JournalAllowance, evidence: t.journalReadEvidence}
	if t.prepared != nil {
		binding.ctx = t.prepared.journalContext
		binding.allowance = t.prepared.journalAllowance
	}
	binding.phase = func() string {
		if t.prepared != nil {
			return string(t.prepared.journal.Phase)
		}
		return "prepare"
	}
	return binding
}

func (t *WindowsProductTxn) JournalReadObservations() []*api.StateFileReadObservation {
	if t == nil {
		return nil
	}
	return t.journalReadEvidence.observations()
}

func (r WindowsProductCommitted) JournalReadObservations() []*api.StateFileReadObservation {
	return r.journalReadEvidence.observations()
}

// Joined not-exist plus any nonabsence failure is not clean absence.
func windowsProductJournalCleanAbsence(err error) bool {
	if err == nil {
		return false
	}
	if joined, ok := err.(interface{ Unwrap() []error }); ok {
		children := joined.Unwrap()
		if len(children) == 0 {
			return false
		}
		for _, child := range children {
			if !windowsProductJournalCleanAbsence(child) {
				return false
			}
		}
		return true
	}
	if wrapped, ok := err.(interface{ Unwrap() error }); ok {
		return windowsProductJournalCleanAbsence(wrapped.Unwrap())
	}
	return errors.Is(err, os.ErrNotExist)
}

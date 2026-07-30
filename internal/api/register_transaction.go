package api

import (
	"errors"
	"fmt"
	"io"
)

type registrationTransactionState string

const (
	registrationTransactionOpen       registrationTransactionState = "open"
	registrationTransactionCommitted  registrationTransactionState = "committed"
	registrationTransactionRolledBack registrationTransactionState = "rolled-back"
)

type compensation func() error
type finalizer func() error
type afterCommitObserver func() error

type namedCompensation struct {
	label  string
	undo   compensation
	active bool
}

type namedFinalizer struct {
	label  string
	close  finalizer
	active bool
}

type namedAfterCommitObserver struct {
	label   string
	observe afterCommitObserver
}

type transactionMark struct {
	steps     int
	resources int
	observers int
}

type finalizerToken int

type registrationTransactionOutcome struct {
	State       registrationTransactionState
	Err         error
	ObserverErr error
}

func (o registrationTransactionOutcome) Committed() bool {
	return o.State == registrationTransactionCommitted && o.Err == nil
}

type registrationTransaction struct {
	state     registrationTransactionState
	steps     []namedCompensation
	resources []namedFinalizer
	observers []namedAfterCommitObserver
}

func newRegistrationTransaction() *registrationTransaction {
	return &registrationTransaction{state: registrationTransactionOpen}
}

func (t *registrationTransaction) Mark() transactionMark {
	if t == nil {
		return transactionMark{}
	}
	return transactionMark{
		steps:     len(t.steps),
		resources: len(t.resources),
		observers: len(t.observers),
	}
}

func (t *registrationTransaction) AddCompensation(label string, undo compensation) {
	if t == nil || t.state != registrationTransactionOpen || undo == nil {
		return
	}
	t.steps = append(t.steps, namedCompensation{label: label, undo: undo, active: true})
}

func (t *registrationTransaction) AddFinalizer(label string, close finalizer) finalizerToken {
	if t == nil || t.state != registrationTransactionOpen || close == nil {
		return finalizerToken(-1)
	}
	t.resources = append(t.resources, namedFinalizer{label: label, close: close, active: true})
	return finalizerToken(len(t.resources) - 1)
}

func (t *registrationTransaction) AddAfterCommit(label string, observe afterCommitObserver) {
	if t == nil || t.state != registrationTransactionOpen || observe == nil {
		return
	}
	t.observers = append(t.observers, namedAfterCommitObserver{label: label, observe: observe})
}

func (t *registrationTransaction) AddSuccessOutput(label string, w io.Writer, format string, args ...any) {
	if w == nil {
		return
	}
	capturedArgs := append([]any(nil), args...)
	t.AddAfterCommit(label, func() error {
		_, err := fmt.Fprintf(w, format, capturedArgs...)
		return err
	})
}

func (t *registrationTransaction) Release(token finalizerToken) error {
	if t == nil {
		return nil
	}
	index := int(token)
	if index < 0 || index >= len(t.resources) {
		return fmt.Errorf("registration transaction: invalid finalizer token %d", index)
	}
	resource := &t.resources[index]
	if !resource.active {
		return nil
	}
	resource.active = false
	return labeledTransactionError("finalizer", resource.label, resource.close())
}

func (t *registrationTransaction) ReleaseSince(mark transactionMark) error {
	if t == nil {
		return nil
	}
	if mark.resources < 0 || mark.resources > len(t.resources) {
		return fmt.Errorf("registration transaction: invalid resource mark %d", mark.resources)
	}
	var joined error
	for i := len(t.resources) - 1; i >= mark.resources; i-- {
		resource := &t.resources[i]
		if !resource.active {
			continue
		}
		resource.active = false
		joined = errors.Join(joined, labeledTransactionError("finalizer", resource.label, resource.close()))
	}
	t.resources = t.resources[:mark.resources]
	return joined
}

func (t *registrationTransaction) RollbackTo(mark transactionMark) error {
	if t == nil {
		return nil
	}
	if t.state != registrationTransactionOpen {
		return fmt.Errorf("registration transaction: rollback-to after %s settlement", t.state)
	}
	if mark.steps < 0 || mark.steps > len(t.steps) ||
		mark.resources < 0 || mark.resources > len(t.resources) ||
		mark.observers < 0 || mark.observers > len(t.observers) {
		return fmt.Errorf(
			"registration transaction: invalid rollback mark steps=%d resources=%d observers=%d",
			mark.steps, mark.resources, mark.observers,
		)
	}
	var joined error
	for i := len(t.steps) - 1; i >= mark.steps; i-- {
		step := &t.steps[i]
		if !step.active {
			continue
		}
		step.active = false
		joined = errors.Join(joined, labeledTransactionError("compensation", step.label, step.undo()))
	}
	t.steps = t.steps[:mark.steps]
	joined = errors.Join(joined, t.ReleaseSince(mark))
	t.observers = t.observers[:mark.observers]
	return joined
}

func (t *registrationTransaction) Fail(cause error) registrationTransactionOutcome {
	if t == nil {
		return registrationTransactionOutcome{State: registrationTransactionRolledBack, Err: cause}
	}
	if t.state != registrationTransactionOpen {
		return registrationTransactionOutcome{
			State: t.state,
			Err:   errors.Join(cause, fmt.Errorf("registration transaction already settled as %s", t.state)),
		}
	}
	rollbackErr := t.RollbackTo(transactionMark{})
	t.state = registrationTransactionRolledBack
	return registrationTransactionOutcome{State: t.state, Err: errors.Join(cause, rollbackErr)}
}

func (t *registrationTransaction) Commit() registrationTransactionOutcome {
	if t == nil {
		return registrationTransactionOutcome{
			State: registrationTransactionRolledBack,
			Err:   errors.New("registration transaction is nil"),
		}
	}
	if t.state != registrationTransactionOpen {
		return registrationTransactionOutcome{
			State: t.state,
			Err:   fmt.Errorf("registration transaction already settled as %s", t.state),
		}
	}
	finalizerErr := t.ReleaseSince(transactionMark{})
	if finalizerErr != nil {
		rollbackErr := t.RollbackTo(transactionMark{})
		t.state = registrationTransactionRolledBack
		return registrationTransactionOutcome{State: t.state, Err: errors.Join(finalizerErr, rollbackErr)}
	}
	observers := append([]namedAfterCommitObserver(nil), t.observers...)
	for i := range t.steps {
		t.steps[i].active = false
	}
	t.steps = nil
	t.resources = nil
	t.observers = nil
	t.state = registrationTransactionCommitted
	var observerErr error
	for _, observer := range observers {
		observerErr = errors.Join(
			observerErr,
			labeledTransactionError("after-commit observer", observer.label, observer.observe()),
		)
	}
	return registrationTransactionOutcome{State: t.state, ObserverErr: observerErr}
}

func labeledTransactionError(kind, label string, err error) error {
	if err == nil {
		return nil
	}
	if label == "" {
		label = "unnamed"
	}
	return fmt.Errorf("%s %s: %w", kind, label, err)
}

func restoreRegistryRowForRollback(
	registryPath, workspaceKey, language string,
	prior WorkspaceEntry,
	hadPrior bool,
) (err error) {
	return restoreRegistryRowWithStore(
		NewRegistry(registryPath),
		workspaceKey,
		language,
		prior,
		hadPrior,
	)
}

func removeLSPRegistryRow(registryPath, workspaceKey, language string) error {
	return restoreRegistryRowForRollback(
		registryPath,
		workspaceKey,
		language,
		WorkspaceEntry{},
		false,
	)
}

type registryRollbackStore interface {
	TryLockWithRelease() (func() error, bool, error)
	Load() error
	PutLSP(WorkspaceEntry) error
	Remove(string, string)
	Save() error
}

func restoreRegistryRowWithStore(
	reg registryRollbackStore,
	workspaceKey, language string,
	prior WorkspaceEntry,
	hadPrior bool,
) (err error) {
	release, locked, lockErr := reg.TryLockWithRelease()
	if lockErr != nil {
		return fmt.Errorf("try-lock registry for rollback: %w", lockErr)
	}
	if !locked {
		return fmt.Errorf("try-lock registry for rollback: lock is busy")
	}
	defer func() {
		err = errors.Join(err, labeledTransactionError("finalizer", "release rollback registry lock", release()))
	}()
	if err := reg.Load(); err != nil {
		return fmt.Errorf("load registry for rollback: %w", err)
	}
	if hadPrior {
		if err := reg.PutLSP(prior); err != nil {
			return fmt.Errorf("restore prior registry row: %w", err)
		}
	} else {
		reg.Remove(workspaceKey, language)
	}
	if err := reg.Save(); err != nil {
		return fmt.Errorf("save registry rollback: %w", err)
	}
	return nil
}

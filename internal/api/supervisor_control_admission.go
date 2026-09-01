package api

import (
	"context"
	"errors"
	"fmt"
	"sync"
)

// SupervisorControlAdmission is the shared pre-mutation compatibility owner.
// GUI, tray and CLI delegate through API methods; the platform composition
// root may register the one legacy-takeover implementation.
type SupervisorControlAdmission func(context.Context) error

var (
	supervisorControlAdmissionMu sync.RWMutex
	supervisorControlAdmissionFn SupervisorControlAdmission = defaultSupervisorControlAdmission
)

// RegisterSupervisorControlAdmission registers the process-wide platform
// handoff and returns a restore closure for isolated tests.
func RegisterSupervisorControlAdmission(fn SupervisorControlAdmission) func() {
	if fn == nil {
		fn = defaultSupervisorControlAdmission
	}
	supervisorControlAdmissionMu.Lock()
	prior := supervisorControlAdmissionFn
	supervisorControlAdmissionFn = fn
	supervisorControlAdmissionMu.Unlock()
	return func() {
		supervisorControlAdmissionMu.Lock()
		supervisorControlAdmissionFn = prior
		supervisorControlAdmissionMu.Unlock()
	}
}

func ensureSupervisorControlAdmission(ctx context.Context) error {
	supervisorControlAdmissionMu.RLock()
	fn := supervisorControlAdmissionFn
	supervisorControlAdmissionMu.RUnlock()
	if fn == nil {
		return errors.New("supervisor control admission is not configured")
	}
	return fn(ctx)
}

func defaultSupervisorControlAdmission(ctx context.Context) error {
	_, err := ProbeSupervisorControlCapabilitiesSnapshot(ctx)
	if err == nil || errors.Is(err, ErrSupervisorIPCUnavailable) {
		return nil
	}
	if errors.Is(err, ErrSupervisorCapabilityLegacy) {
		return fmt.Errorf("legacy supervisor control protocol requires replacement before mutation: %w", err)
	}
	return fmt.Errorf("supervisor control compatibility before mutation: %w", err)
}

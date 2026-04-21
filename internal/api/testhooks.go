// Package api — test hooks exported for cross-package integration tests.
//
// The Register/Unregister paths consult package-scoped overrides for the
// scheduler backend, client adapter set, and registry file location. In
// unit tests inside internal/api those overrides are assigned directly;
// cross-package tests (e.g. internal/e2e) cannot reach unexported names,
// so the install-time factory hooks are surfaced as typed public helpers
// here. Production callers never invoke these.
package api

import (
	"mcp-local-hub/internal/clients"
	"mcp-local-hub/internal/scheduler"
)

// TestSchedulerIface matches the subset of scheduler.Scheduler that the
// Register/Unregister paths use. Cross-package fakes implement it to
// replace the real scheduler backend.
type TestSchedulerIface interface {
	Create(spec scheduler.TaskSpec) error
	Delete(name string) error
	Run(name string) error
	ExportXML(name string) ([]byte, error)
	ImportXML(name string, xml []byte) error
}

// TestClientIface matches the subset of clients.Client that the register
// path consumes. Cross-package fakes implement it.
type TestClientIface interface {
	Exists() bool
	AddEntry(clients.MCPEntry) error
	RemoveEntry(name string) error
	GetEntry(name string) (*clients.MCPEntry, error)
}

// InstallTestHooks replaces the Register/Unregister factories with fakes
// for cross-package tests. Every argument is mandatory except
// registryPathOverride (use "" to keep the default). Returns a restore
// function that resets every hook to the production default.
//
// Intended for internal/e2e-style integration tests; production code must
// never call this.
func InstallTestHooks(newScheduler func() (TestSchedulerIface, error),
	clientSet func() map[string]TestClientIface,
	registryPathOverride string,
) (restore func()) {
	origSch := testSchedulerFactory
	origClients := testClientFactory
	origRegPath := testRegistryPathOverride

	testSchedulerFactory = func() (testScheduler, error) {
		s, err := newScheduler()
		if err != nil {
			return nil, err
		}
		return testSchedulerShim{s}, nil
	}
	testClientFactory = func() map[string]registerClient {
		out := map[string]registerClient{}
		for name, c := range clientSet() {
			out[name] = testClientShim{c}
		}
		return out
	}
	testRegistryPathOverride = registryPathOverride

	return func() {
		testSchedulerFactory = origSch
		testClientFactory = origClients
		testRegistryPathOverride = origRegPath
	}
}

// testSchedulerShim adapts a caller-supplied TestSchedulerIface to the
// package-private testScheduler interface.
type testSchedulerShim struct{ s TestSchedulerIface }

func (a testSchedulerShim) Create(spec scheduler.TaskSpec) error    { return a.s.Create(spec) }
func (a testSchedulerShim) Delete(name string) error                { return a.s.Delete(name) }
func (a testSchedulerShim) Run(name string) error                   { return a.s.Run(name) }
func (a testSchedulerShim) ExportXML(name string) ([]byte, error)   { return a.s.ExportXML(name) }
func (a testSchedulerShim) ImportXML(name string, xml []byte) error { return a.s.ImportXML(name, xml) }

// testClientShim adapts a caller-supplied TestClientIface to the
// package-private registerClient interface.
type testClientShim struct{ c TestClientIface }

func (a testClientShim) Exists() bool                                    { return a.c.Exists() }
func (a testClientShim) AddEntry(e clients.MCPEntry) error               { return a.c.AddEntry(e) }
func (a testClientShim) RemoveEntry(name string) error                   { return a.c.RemoveEntry(name) }
func (a testClientShim) GetEntry(name string) (*clients.MCPEntry, error) { return a.c.GetEntry(name) }

package scheduler

import "errors"

// noopScheduler is a test-only Scheduler that returns empty results for
// read operations (List/Status/ExportXML) and REJECTS every mutation
// (Create/Delete/Run/Stop/ImportXML) with errNoopSchedulerMutation.
// Activated via the MCPHUB_E2E_SCHEDULER=none env var from the Playwright
// E2E fixture so tests run against deterministic empty scheduler state
// regardless of the developer's real Task Scheduler contents.
//
// Why mutations hard-fail: reads need to succeed so empty-state UI tests
// can render (Dashboard empty cards, Logs no-daemons notice). But if
// MCPHUB_E2E_SCHEDULER=none leaks into a production shell by accident,
// install/restart/upgrade flows that call scheduler.New() would
// silently report success while making no changes — a particularly bad
// silent-failure mode. Hard error on mutation makes accidental
// activation crash loudly at the first install call, not after weeks
// of phantom success.
//
// Never construct this outside tests — New() is the only production-
// callable constructor, and it only returns noopScheduler when the
// env seam is explicit.
type noopScheduler struct{}

// errNoopSchedulerMutation is returned by every mutation method when
// the noop scheduler is active. Intentionally a concrete error so
// install/restart callers see a predictable failure signature rather
// than a spurious success.
var errNoopSchedulerMutation = errors.New("scheduler: noop test-seam active (MCPHUB_E2E_SCHEDULER=none); mutations rejected")

func (*noopScheduler) Create(TaskSpec) error             { return errNoopSchedulerMutation }
func (*noopScheduler) Delete(string) error               { return errNoopSchedulerMutation }
func (*noopScheduler) Run(string) error                  { return errNoopSchedulerMutation }
func (*noopScheduler) Stop(string) error                 { return errNoopSchedulerMutation }
func (*noopScheduler) Status(string) (TaskStatus, error) { return TaskStatus{}, ErrTaskNotFound }
func (*noopScheduler) List(string) ([]TaskStatus, error) { return nil, nil }
func (*noopScheduler) ExportXML(string) ([]byte, error)  { return nil, ErrTaskNotFound }
func (*noopScheduler) ImportXML(string, []byte) error    { return errNoopSchedulerMutation }

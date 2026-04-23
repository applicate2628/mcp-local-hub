package scheduler

// noopScheduler is a test-only Scheduler that records no tasks and returns
// empty results for List/Status. Activated via the MCPHUB_E2E_SCHEDULER=none
// env var from the Playwright E2E fixture so tests run against deterministic
// empty scheduler state regardless of the developer's real Task Scheduler
// contents. Never construct this outside tests — New() is the only
// production-callable constructor, and it only returns noopScheduler when
// the env seam is explicit.
type noopScheduler struct{}

func (*noopScheduler) Create(TaskSpec) error                  { return nil }
func (*noopScheduler) Delete(string) error                    { return nil }
func (*noopScheduler) Run(string) error                       { return nil }
func (*noopScheduler) Stop(string) error                      { return nil }
func (*noopScheduler) Status(string) (TaskStatus, error)      { return TaskStatus{}, ErrTaskNotFound }
func (*noopScheduler) List(string) ([]TaskStatus, error)      { return nil, nil }
func (*noopScheduler) ExportXML(string) ([]byte, error)       { return nil, ErrTaskNotFound }
func (*noopScheduler) ImportXML(string, []byte) error         { return nil }

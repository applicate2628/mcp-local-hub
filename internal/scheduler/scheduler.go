package scheduler

import "errors"

// TaskSpec describes a scheduled task the hub wants the OS to manage.
// Scheduler backends translate this into Windows Task Scheduler, systemd user units,
// or launchd agents.
type TaskSpec struct {
	// Name is a unique identifier. Used for create/delete/run operations.
	// Convention: "mcp-local-hub-<server>-<daemon>" for daemon tasks, "mcp-local-hub-refresh" for weekly refresh.
	Name string

	// Description is a human-readable summary shown by the OS scheduler UI.
	Description string

	// Command is the program to run. Typically the absolute path to the `mcp` binary.
	Command string

	// Args are passed verbatim to the command. Typically: ["daemon", "--server", "serena", "--daemon", "claude"].
	Args []string

	// WorkingDir is the process's cwd at launch. Usually the repo root.
	WorkingDir string

	// Env is added to the process environment at launch.
	Env map[string]string

	// Trigger determines when the task fires. Only one of LogonTrigger or WeeklyTrigger is used.
	LogonTrigger bool
	// WeeklyTrigger fires every week on the named day at the given time.
	// DayOfWeek uses Go's time.Weekday (Sunday=0 .. Saturday=6). HourLocal+MinuteLocal are 24h local time.
	WeeklyTrigger *WeeklyTrigger

	// RestartOnFailure enables automatic retry. The backend configures a fixed policy:
	// retry every 60 seconds, max 3 attempts (per spec §3.3.1).
	RestartOnFailure bool
}

type WeeklyTrigger struct {
	DayOfWeek   int // 0=Sunday .. 6=Saturday
	HourLocal   int
	MinuteLocal int
}

// TaskStatus summarizes what the OS scheduler currently thinks of a task.
type TaskStatus struct {
	Name       string
	State      string // "Ready", "Running", "Disabled", "Unknown"
	LastResult int    // exit code of last run, or -1 if never run
	NextRun    string // human-readable, backend-specific
}

// ErrTaskNotFound is returned by ExportXML and other lookup APIs when
// the named task does not exist. Separate sentinel so callers can
// distinguish absent-but-expected tasks from schtasks communication
// failures.
var ErrTaskNotFound = errors.New("scheduler: task not found")

// Scheduler is the OS-abstracted interface for managing mcp-local-hub daemon tasks.
// Implementations live in scheduler_<os>.go files selected by build tags.
type Scheduler interface {
	// Create registers a new task. If a task with the same name already exists,
	// Create returns an error — callers must Delete first for idempotence.
	Create(spec TaskSpec) error

	// Delete removes a task by name. Returns nil if the task does not exist.
	Delete(name string) error

	// Run triggers an immediate one-off execution of a task.
	Run(name string) error

	// Stop terminates a currently-running task. No-op if not running.
	Stop(name string) error

	// Status reports the current state of a task.
	Status(name string) (TaskStatus, error)

	// List returns all tasks whose Name starts with prefix (e.g., "mcp-local-hub-").
	List(prefix string) ([]TaskStatus, error)

	// ExportXML returns the raw Task Scheduler XML for a task by name.
	// Used by install's rollback path to snapshot an existing task before
	// replacing it, so a failed mid-sequence install can restore the
	// prior task instead of leaving nothing. Platforms without native
	// equivalents (Linux, macOS) return an error; callers guard on that
	// and treat the case as "no prior spec to preserve".
	ExportXML(name string) ([]byte, error)

	// ImportXML re-creates a task from raw Task Scheduler XML. Counterpart
	// of ExportXML; used for rollback restoration.
	ImportXML(name string, xml []byte) error
}

// New returns the platform-appropriate Scheduler implementation for the current OS.
// Defined per-OS in scheduler_<os>.go.
func New() (Scheduler, error) {
	return newPlatformScheduler()
}

package gui

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"

	"github.com/gofrs/flock"
)

// ErrSingleInstanceBusy is returned by AcquireSingleInstance when another
// mcphub gui process already holds the lock. Callers should read the
// pidport file, probe the incumbent's /api/ping, and POST
// /api/activate-window before giving up.
var ErrSingleInstanceBusy = errors.New("another mcphub gui is already running")

// SingleInstanceLock represents the acquired single-instance ownership.
// Release must be called on shutdown (or by an errdefer immediately after
// acquisition) to free the lock file and remove the pidport record.
type SingleInstanceLock struct {
	pidport string
	fl      *flock.Flock
}

// AcquireSingleInstance tries to become the sole mcphub gui process for
// this user. On success it writes a pidport record at PidportPath() and
// returns a lock the caller must Release on shutdown.
//
// The lock is a flock-managed adjacent .lock file — the same pattern
// workspace-registry uses elsewhere in the codebase. It is NOT a Windows
// named kernel mutex; portability across Linux/macOS was favored over
// the tiny-but-theoretical advantage of kernel-level serialization on
// Windows alone.
func AcquireSingleInstance(port int) (*SingleInstanceLock, error) {
	p, err := PidportPath()
	if err != nil {
		return nil, err
	}
	return acquireSingleInstanceAt(p, port)
}

// acquireSingleInstanceAt is the injectable form used by tests.
func acquireSingleInstanceAt(pidportPath string, port int) (*SingleInstanceLock, error) {
	fl := flock.New(pidportPath + ".lock")
	ok, err := fl.TryLock()
	if err != nil {
		return nil, fmt.Errorf("flock %s: %w", pidportPath+".lock", err)
	}
	if !ok {
		return nil, ErrSingleInstanceBusy
	}
	if err := os.WriteFile(pidportPath, []byte(formatPidport(os.Getpid(), port)), 0o600); err != nil {
		_ = fl.Unlock()
		return nil, fmt.Errorf("write pidport: %w", err)
	}
	return &SingleInstanceLock{pidport: pidportPath, fl: fl}, nil
}

// Release removes the pidport record and releases the lock. Idempotent.
func (l *SingleInstanceLock) Release() {
	if l == nil || l.fl == nil {
		return
	}
	// Order matters: unlock the flock FIRST so a racing second instance
	// can acquire ownership immediately. Removing the pidport before
	// unlock leaves a window where the second instance sees
	// ErrSingleInstanceBusy but ReadPidport fails (file gone), causing
	// false-negative "running but unreachable" failures during normal
	// shutdown. The reverse window (flock free, stale pidport file) is
	// safe because acquireSingleInstanceAt always overwrites pidport
	// via os.WriteFile after TryLock succeeds.
	_ = l.fl.Unlock()
	_ = os.Remove(l.pidport)
	l.fl = nil
}

// ReadPidport reads "<PID> <PORT>\n" format. Returns (0,0,err) on parse
// failure or missing file. Second-instance callers use it to probe the
// incumbent.
func ReadPidport(path string) (pid, port int, err error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return 0, 0, err
	}
	parts := strings.Fields(string(b))
	if len(parts) != 2 {
		return 0, 0, fmt.Errorf("malformed pidport %q", string(b))
	}
	pid, err = strconv.Atoi(parts[0])
	if err != nil {
		return 0, 0, fmt.Errorf("parse pid: %w", err)
	}
	port, err = strconv.Atoi(parts[1])
	if err != nil {
		return 0, 0, fmt.Errorf("parse port: %w", err)
	}
	return pid, port, nil
}

func formatPidport(pid, port int) string {
	return fmt.Sprintf("%d %d\n", pid, port)
}

// AcquireSingleInstanceAt is the exported form of acquireSingleInstanceAt
// so callers outside the gui package (cli) can share the same path.
func AcquireSingleInstanceAt(pidportPath string, port int) (*SingleInstanceLock, error) {
	return acquireSingleInstanceAt(pidportPath, port)
}

// RewritePidportPort overwrites the pidport file with the current PID and
// the supplied port. Used by the CLI after Server.Start resolves an
// OS-assigned port (--port 0): the lock was acquired before bind with
// the originally requested port, but second-instance handshake probes
// need the actual bound port. The caller must hold the single-instance
// lock — the flock on *.lock gates ownership, the pidport file is
// ownership metadata the lock holder freely updates.
func RewritePidportPort(pidportPath string, port int) error {
	return os.WriteFile(pidportPath, []byte(formatPidport(os.Getpid(), port)), 0o600)
}

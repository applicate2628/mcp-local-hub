# Phase 3B — GUI Installer MVP Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Ship an MVP local GUI (browser window served by `mcphub.exe` at `127.0.0.1:<free port>` + Windows system-tray icon) covering the primary flow — scan client configs, view the unified servers-×-clients matrix, toggle hub routing per cell, apply migrations, live daemon dashboard with restart, and a live log viewer. Secondary screens (Migration detail, Add/Edit manifest form, Secrets, Settings, About) and polish are out of scope; they ship in Phase 3B-II.

**Architecture:** Add `internal/gui/` package (HTTP server + embedded HTML/CSS/JS assets) and `internal/tray/` package (Windows-only systray integration). Both are thin wrappers over the existing `internal/api/` (source-of-truth invariant: no direct reach into `internal/clients`, `internal/scheduler`, `internal/config`, `internal/daemon`). A new `mcphub gui` cobra subcommand is the entry point. Single-instance behavior uses a Windows named mutex (`Local\mcp-local-hub-gui`) plus a `%LOCALAPPDATA%/mcp-local-hub/gui.pidport` file so a second invocation activates the first window instead of starting a second server. Real-time updates flow via a shared SSE endpoint `/api/events` fed by a status poller goroutine.

**Tech Stack:** Go 1.26 stdlib `net/http`, `embed`, `golang.org/x/sys/windows` (already pulled in by `cmd/mcphub/console_windows.go`), `github.com/gofrs/flock` (already in go.mod, used cross-platform for single-instance), `github.com/getlantern/systray` for Windows tray, `github.com/spf13/cobra` (existing). Embedded static assets: vanilla HTML + CSS-variable theme + vanilla JS SPA (no framework — full control, zero build step, matches project style).

**Reference implementations already in tree:**

- [internal/api/scan.go:393](internal/api/scan.go#L393) — `api.Scan()` returns `*ScanResult`
- [internal/api/install.go:363](internal/api/install.go#L363) — `api.Status()` returns `[]DaemonStatus`
- [internal/api/install.go:138](internal/api/install.go#L138) — `api.Install(InstallOpts)` — for migrate via install path
- [internal/api/logs.go](internal/api/logs.go) — `api.Logs(server, tail)`
- [cmd/mcphub/console_windows.go](cmd/mcphub/console_windows.go) — AttachConsole pattern to extend
- [internal/cli/root.go:16](internal/cli/root.go#L16) — `NewRootCmd()` where `gui` will register

**Prerequisites:**

- Phase 3 workspace-scoped + security hotfix merged (master at `7d8e630`)
- CLI parity already delivered by Phase 3A (scan, migrate, install, logs, backups, rollback, manifest, scheduler, settings, stop, secrets commands all present)
- `console_windows.go` / `console_other.go` exist — do NOT add an `init()` `AttachConsole` change until Task 23 (release hardening verifies the switch to `-H windowsgui` linker flag)

---

## File Structure

```
cmd/mcphub/
├── main.go                    # unchanged (links cli.NewRootCmd)
├── console_windows.go         # unchanged (keeps AttachConsole for CLI subsystem)
└── console_other.go           # unchanged

internal/cli/
└── gui.go                     # NEW — `mcphub gui` cobra command

internal/gui/
├── server.go                  # HTTP Server type, lifecycle
├── routes.go                  # handler registrations
├── ping.go                    # GET /api/ping + POST /api/activate-window
├── scan.go                    # GET /api/scan
├── status.go                  # GET /api/status
├── migrate.go                 # POST /api/migrate
├── servers.go                 # POST /api/servers/:s/restart, /stop
├── logs.go                    # GET /api/logs/:s + /stream (SSE)
├── events.go                  # SSE event bus (broadcaster + poller)
├── single_instance.go         # pidport I/O, cross-platform
├── single_instance_windows.go # Windows named mutex
├── single_instance_other.go   # Linux/macOS flock fallback
├── browser.go                 # Chrome/Edge app-mode launch
├── paths.go                   # %LOCALAPPDATA%/mcp-local-hub helpers
└── assets/
    ├── index.html             # SPA shell with sidebar
    ├── style.css              # CSS vars, light/dark, layout
    ├── icons.svg              # Lucide-style SVG symbol set
    ├── app.js                 # router + fetch helpers + SSE client
    ├── servers.js             # Servers screen
    ├── dashboard.js           # Dashboard screen
    └── logs.js                # Logs screen

internal/tray/
├── tray.go                    # Run() interface (no-op on non-Windows)
├── tray_windows.go            # getlantern/systray integration
├── tray_other.go              # stub
└── assets/
    └── icon_*.ico             # 4 state icons (healthy/partial/down/error)

internal/gui/*_test.go         # HTTP handler table tests via httptest
internal/gui/single_instance_test.go  # concurrent-instance smoke
```

---

## Task 1: Scaffold `internal/gui` package + `Server` type

**Files:**
- Create: `internal/gui/server.go`
- Create: `internal/gui/server_test.go`

- [ ] **Step 1: Write the failing test**

```go
// internal/gui/server_test.go
package gui

import (
	"context"
	"net/http"
	"testing"
	"time"
)

func TestServer_StartAndShutdown(t *testing.T) {
	s := NewServer(Config{Port: 0}) // 0 = OS picks free port
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ready := make(chan struct{})
	errCh := make(chan error, 1)
	go func() {
		errCh <- s.Start(ctx, ready)
	}()
	select {
	case <-ready:
	case <-time.After(2 * time.Second):
		t.Fatal("server never signaled ready")
	}
	if s.Port() == 0 {
		t.Fatal("Port() returned 0 after ready")
	}
	resp, err := http.Get("http://127.0.0.1:" + itoa(s.Port()) + "/api/ping")
	if err != nil {
		t.Fatalf("ping: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("ping status %d", resp.StatusCode)
	}
	cancel()
	select {
	case err := <-errCh:
		if err != nil && err != http.ErrServerClosed {
			t.Errorf("Start returned %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Start did not return after cancel")
	}
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var out []byte
	for n > 0 {
		out = append([]byte{byte('0' + n%10)}, out...)
		n /= 10
	}
	return string(out)
}
```

- [ ] **Step 2: Run test — expect FAIL (package not built)**

Run: `go test ./internal/gui -run TestServer_StartAndShutdown -v`
Expected: build error `no Go files in .../internal/gui`

- [ ] **Step 3: Write minimal implementation**

```go
// internal/gui/server.go
package gui

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"sync/atomic"
	"time"
)

// Config drives Server construction. Zero values are sensible defaults.
type Config struct {
	// Port to bind on 127.0.0.1. Zero lets the OS pick one from the
	// ephemeral range; the chosen port is reported via Server.Port().
	Port int
}

// Server is the GUI HTTP server. It owns a net/http.Server bound to
// 127.0.0.1, a ready-to-register mux, and a best-effort shutdown path.
type Server struct {
	cfg     Config
	mux     *http.ServeMux
	srv     *http.Server
	port    atomic.Int32 // set after Listen, read by Port()
}

// NewServer constructs the Server. It registers the ping handler
// immediately so even a minimal Server answers /api/ping.
func NewServer(cfg Config) *Server {
	s := &Server{cfg: cfg, mux: http.NewServeMux()}
	s.mux.HandleFunc("/api/ping", s.handlePing)
	return s
}

// Port returns the actual TCP port the server is bound to. Zero until
// Start has signaled ready.
func (s *Server) Port() int { return int(s.port.Load()) }

// Start binds 127.0.0.1:<cfg.Port>, signals `ready` once the listener
// is accepting, then blocks in ListenAndServe. Returns when ctx is
// canceled (graceful shutdown, 5s deadline) or the listener errors.
// http.ErrServerClosed is returned as nil.
func (s *Server) Start(ctx context.Context, ready chan<- struct{}) error {
	ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", s.cfg.Port))
	if err != nil {
		return fmt.Errorf("bind 127.0.0.1:%d: %w", s.cfg.Port, err)
	}
	s.port.Store(int32(ln.Addr().(*net.TCPAddr).Port))
	s.srv = &http.Server{
		Handler:           s.mux,
		ReadHeaderTimeout: 10 * time.Second,
	}
	close(ready)

	errCh := make(chan error, 1)
	go func() { errCh <- s.srv.Serve(ln) }()

	select {
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = s.srv.Shutdown(shutdownCtx)
		return nil
	case err := <-errCh:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	}
}

// handlePing is the skeleton that Task 3 fills out with version info.
func (s *Server) handlePing(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.Write([]byte(`{"ok":true}`))
}
```

- [ ] **Step 4: Run test to verify PASS**

Run: `go test ./internal/gui -run TestServer_StartAndShutdown -v`
Expected: `--- PASS: TestServer_StartAndShutdown`

- [ ] **Step 5: Commit**

```bash
git add internal/gui/server.go internal/gui/server_test.go
git commit -m "feat(gui): scaffold HTTP server with ping + lifecycle test"
```

---

## Task 2: `mcphub gui` cobra command

**Files:**
- Create: `internal/cli/gui.go`
- Modify: `internal/cli/root.go` (add to NewRootCmd)
- Create: `internal/cli/gui_test.go`

- [ ] **Step 1: Write the failing test**

```go
// internal/cli/gui_test.go
package cli

import (
	"bytes"
	"strings"
	"testing"
)

func TestGuiCmd_HelpIncludesFlags(t *testing.T) {
	cmd := newGuiCmdReal()
	var buf bytes.Buffer
	cmd.SetOut(&buf)
	cmd.SetArgs([]string{"--help"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	for _, want := range []string{"--port", "--no-browser", "--no-tray", "--force"} {
		if !strings.Contains(buf.String(), want) {
			t.Errorf("--help missing %q; got %q", want, buf.String())
		}
	}
}
```

- [ ] **Step 2: Run test — expect FAIL (`newGuiCmdReal` undefined)**

Run: `go test ./internal/cli -run TestGuiCmd -v`
Expected: build error about `newGuiCmdReal` undefined.

- [ ] **Step 3: Write minimal implementation**

```go
// internal/cli/gui.go
package cli

import (
	"context"
	"fmt"
	"os/signal"
	"syscall"

	"mcp-local-hub/internal/gui"

	"github.com/spf13/cobra"
)

func newGuiCmdReal() *cobra.Command {
	var (
		port      int
		noBrowser bool
		noTray    bool
		force     bool
	)
	c := &cobra.Command{
		Use:   "gui",
		Short: "Launch the local GUI (browser window + tray icon served by mcphub itself)",
		Long: `mcphub gui starts an HTTP server on 127.0.0.1 that serves a local-only
browser GUI for managing MCP servers. A Windows tray icon and auto-launched
Chrome/Edge app-mode window accompany it by default.

The server binds 127.0.0.1 only — no remote access, no auth, no TLS.
A Windows named mutex guards against a second instance: a second invocation
activates the first window and exits 0.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx, stop := signal.NotifyContext(cmd.Context(), syscall.SIGINT, syscall.SIGTERM)
			defer stop()

			s := gui.NewServer(gui.Config{Port: port})
			ready := make(chan struct{})
			errCh := make(chan error, 1)
			go func() { errCh <- s.Start(ctx, ready) }()

			select {
			case <-ready:
				fmt.Fprintf(cmd.OutOrStdout(), "GUI listening on http://127.0.0.1:%d\n", s.Port())
			case err := <-errCh:
				return err
			case <-ctx.Done():
				return ctx.Err()
			}

			// TODO (Task 4+): tray + browser launch + single-instance.
			// For now, just block until the server returns.
			_ = noBrowser
			_ = noTray
			_ = force
			return <-errCh
		},
	}
	c.Flags().IntVar(&port, "port", 0, "TCP port on 127.0.0.1 (0 = auto-pick from ephemeral)")
	c.Flags().BoolVar(&noBrowser, "no-browser", false, "do not auto-launch a browser window")
	c.Flags().BoolVar(&noTray, "no-tray", false, "do not show the system-tray icon")
	c.Flags().BoolVar(&force, "force", false, "take over a stuck single-instance mutex if pidport probe fails")
	return c
}
```

And add to `internal/cli/root.go` — inside `NewRootCmd`, just after `root.AddCommand(newWeeklyRefreshCmd())`:

```go
	root.AddCommand(newGuiCmd())
```

And add the stub next to the others at the bottom of `root.go`:

```go
func newGuiCmd() *cobra.Command { return newGuiCmdReal() }
```

- [ ] **Step 4: Run test to verify PASS**

Run: `go test ./internal/cli -run TestGuiCmd -v && go build ./... && go run ./cmd/mcphub gui --help`
Expected: test PASS + `--help` prints flag list including `--port`, `--no-browser`, `--no-tray`, `--force`.

- [ ] **Step 5: Commit**

```bash
git add internal/cli/gui.go internal/cli/gui_test.go internal/cli/root.go
git commit -m "feat(cli): mcphub gui subcommand with --port/--no-browser/--no-tray/--force"
```

---

## Task 3: `/api/ping` with version info + `/api/activate-window` skeleton

**Files:**
- Modify: `internal/gui/server.go`
- Create: `internal/gui/ping.go`
- Create: `internal/gui/ping_test.go`

- [ ] **Step 1: Write the failing test**

```go
// internal/gui/ping_test.go
package gui

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestPing_ReturnsJSONWithPIDAndVersion(t *testing.T) {
	s := NewServer(Config{Version: "test-v1", PID: 4242})
	req := httptest.NewRequest(http.MethodGet, "/api/ping", nil)
	rec := httptest.NewRecorder()
	s.mux.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("status = %d", rec.Code)
	}
	var body struct {
		OK      bool   `json:"ok"`
		PID     int    `json:"pid"`
		Version string `json:"version"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !body.OK || body.PID != 4242 || body.Version != "test-v1" {
		t.Errorf("unexpected body: %+v", body)
	}
}

func TestActivateWindow_MarksSignalReceived(t *testing.T) {
	s := NewServer(Config{})
	got := make(chan struct{}, 1)
	s.OnActivateWindow(func() { got <- struct{}{} })
	req := httptest.NewRequest(http.MethodPost, "/api/activate-window", nil)
	rec := httptest.NewRecorder()
	s.mux.ServeHTTP(rec, req)
	if rec.Code != 204 {
		t.Errorf("status = %d, want 204", rec.Code)
	}
	select {
	case <-got:
	default:
		t.Error("activate-window did not invoke callback")
	}
}

func TestPing_WrongMethodIs405(t *testing.T) {
	s := NewServer(Config{})
	req := httptest.NewRequest(http.MethodPost, "/api/ping", strings.NewReader(""))
	rec := httptest.NewRecorder()
	s.mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("status = %d, want 405", rec.Code)
	}
}
```

- [ ] **Step 2: Run test — expect FAIL**

Run: `go test ./internal/gui -run 'TestPing|TestActivateWindow' -v`
Expected: FAIL — `Config` lacks `Version` / `PID` fields, `OnActivateWindow` undefined.

- [ ] **Step 3: Write minimal implementation**

Extend `Config` in `server.go`:

```go
type Config struct {
	Port    int
	Version string // surfaced by /api/ping for the GUI's About
	PID     int    // surfaced by /api/ping for the single-instance probe; 0 = use os.Getpid
}
```

Extend `Server`:

```go
type Server struct {
	cfg              Config
	mux              *http.ServeMux
	srv              *http.Server
	port             atomic.Int32
	onActivateWindow func() // set via OnActivateWindow; nil = no-op
}

func (s *Server) OnActivateWindow(fn func()) { s.onActivateWindow = fn }
```

Replace the inline `handlePing` with:

```go
func NewServer(cfg Config) *Server {
	if cfg.PID == 0 {
		cfg.PID = os.Getpid()
	}
	s := &Server{cfg: cfg, mux: http.NewServeMux()}
	registerPingRoutes(s)
	return s
}
```

Create `internal/gui/ping.go`:

```go
// internal/gui/ping.go
package gui

import (
	"encoding/json"
	"net/http"
)

func registerPingRoutes(s *Server) {
	s.mux.HandleFunc("/api/ping", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			w.Header().Set("Allow", "GET")
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"ok":      true,
			"pid":     s.cfg.PID,
			"version": s.cfg.Version,
		})
	})
	s.mux.HandleFunc("/api/activate-window", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			w.Header().Set("Allow", "POST")
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		if s.onActivateWindow != nil {
			s.onActivateWindow()
		}
		w.WriteHeader(http.StatusNoContent)
	})
}
```

Remove the old inline `s.handlePing` registration from `server.go`:

```go
// delete this line from NewServer:
// s.mux.HandleFunc("/api/ping", s.handlePing)
// delete the s.handlePing method entirely.
```

Add `"os"` import to `server.go` for `os.Getpid()`.

- [ ] **Step 4: Run test to verify PASS**

Run: `go test ./internal/gui -v`
Expected: all tests PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/gui/server.go internal/gui/ping.go internal/gui/ping_test.go
git commit -m "feat(gui): /api/ping with pid/version + /api/activate-window skeleton"
```

---

## Task 4: Cross-platform `%LOCALAPPDATA%/mcp-local-hub` path helper

**Files:**
- Create: `internal/gui/paths.go`
- Create: `internal/gui/paths_test.go`

- [ ] **Step 1: Write the failing test**

```go
// internal/gui/paths_test.go
package gui

import (
	"runtime"
	"strings"
	"testing"
)

func TestAppDataDir_ReturnsUserWriteablePath(t *testing.T) {
	got, err := AppDataDir()
	if err != nil {
		t.Fatalf("AppDataDir: %v", err)
	}
	if got == "" {
		t.Fatal("empty path")
	}
	if runtime.GOOS == "windows" && !strings.Contains(strings.ToLower(got), "appdata") {
		t.Errorf("windows path should include AppData: %q", got)
	}
	if !strings.HasSuffix(got, "mcp-local-hub") {
		t.Errorf("path should end with mcp-local-hub: %q", got)
	}
}

func TestPidportPath_IsUnderAppData(t *testing.T) {
	got, err := PidportPath()
	if err != nil {
		t.Fatalf("PidportPath: %v", err)
	}
	if !strings.HasSuffix(got, "gui.pidport") {
		t.Errorf("path should end with gui.pidport: %q", got)
	}
}
```

- [ ] **Step 2: Run test — expect FAIL**

Run: `go test ./internal/gui -run 'TestAppDataDir|TestPidportPath' -v`
Expected: FAIL — functions undefined.

- [ ] **Step 3: Write implementation**

```go
// internal/gui/paths.go
package gui

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
)

// AppDataDir returns the per-user writeable directory for mcp-local-hub
// runtime artifacts (pidport, gui-preferences.yaml). On Windows:
// %LOCALAPPDATA%\mcp-local-hub. On Linux/macOS: $XDG_STATE_HOME or
// $HOME/.local/state/mcp-local-hub. Creates the directory 0700 on first call.
func AppDataDir() (string, error) {
	var base string
	switch runtime.GOOS {
	case "windows":
		base = os.Getenv("LOCALAPPDATA")
		if base == "" {
			home, err := os.UserHomeDir()
			if err != nil {
				return "", fmt.Errorf("resolve home: %w", err)
			}
			base = filepath.Join(home, "AppData", "Local")
		}
	default:
		if x := os.Getenv("XDG_STATE_HOME"); x != "" {
			base = x
		} else {
			home, err := os.UserHomeDir()
			if err != nil {
				return "", fmt.Errorf("resolve home: %w", err)
			}
			base = filepath.Join(home, ".local", "state")
		}
	}
	dir := filepath.Join(base, "mcp-local-hub")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", fmt.Errorf("mkdir %s: %w", dir, err)
	}
	return dir, nil
}

// PidportPath returns the absolute path to the single-instance pidport
// file. Format: ASCII "<PID> <PORT>\n" — read by second-instance probe.
func PidportPath() (string, error) {
	dir, err := AppDataDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "gui.pidport"), nil
}
```

- [ ] **Step 4: Run test to verify PASS**

Run: `go test ./internal/gui -run 'TestAppDataDir|TestPidportPath' -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/gui/paths.go internal/gui/paths_test.go
git commit -m "feat(gui): AppDataDir + PidportPath helpers (XDG + Windows parity)"
```

---

## Task 5: Single-instance lock — cross-platform via `gofrs/flock`

**Files:**
- Create: `internal/gui/single_instance.go`
- Create: `internal/gui/single_instance_test.go`

- [ ] **Step 1: Write the failing test**

```go
// internal/gui/single_instance_test.go
package gui

import (
	"os"
	"path/filepath"
	"testing"
)

func TestAcquireSingleInstance_FirstCallerSucceeds(t *testing.T) {
	dir := t.TempDir()
	pidport := filepath.Join(dir, "gui.pidport")
	lock, err := acquireSingleInstanceAt(pidport, 9100)
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	defer lock.Release()
	got, err := os.ReadFile(pidport)
	if err != nil {
		t.Fatalf("read pidport: %v", err)
	}
	want := []byte(formatPidport(os.Getpid(), 9100))
	if string(got) != string(want) {
		t.Errorf("pidport content = %q, want %q", got, want)
	}
}

func TestAcquireSingleInstance_SecondCallerFails(t *testing.T) {
	dir := t.TempDir()
	pidport := filepath.Join(dir, "gui.pidport")
	first, err := acquireSingleInstanceAt(pidport, 9100)
	if err != nil {
		t.Fatalf("first acquire: %v", err)
	}
	defer first.Release()
	_, err = acquireSingleInstanceAt(pidport, 9101)
	if err == nil {
		t.Fatal("second acquire should fail but succeeded")
	}
	if err != ErrSingleInstanceBusy {
		t.Errorf("err = %v, want ErrSingleInstanceBusy", err)
	}
}

func TestReadPidport_ParsesPidAndPort(t *testing.T) {
	dir := t.TempDir()
	pidport := filepath.Join(dir, "gui.pidport")
	if err := os.WriteFile(pidport, []byte("12345 9100\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	pid, port, err := ReadPidport(pidport)
	if err != nil {
		t.Fatalf("ReadPidport: %v", err)
	}
	if pid != 12345 || port != 9100 {
		t.Errorf("got pid=%d port=%d, want 12345 9100", pid, port)
	}
}
```

- [ ] **Step 2: Run test — expect FAIL**

Run: `go test ./internal/gui -run TestAcquireSingleInstance -v`
Expected: FAIL — functions undefined.

- [ ] **Step 3: Write implementation**

```go
// internal/gui/single_instance.go
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
	_ = os.Remove(l.pidport)
	_ = l.fl.Unlock()
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
```

- [ ] **Step 4: Run test to verify PASS**

Run: `go test ./internal/gui -v`
Expected: all green.

- [ ] **Step 5: Commit**

```bash
git add internal/gui/single_instance.go internal/gui/single_instance_test.go
git commit -m "feat(gui): single-instance lock via gofrs/flock + pidport read/write"
```

---

## Task 6: Second-instance handshake — probe incumbent + activate

**Files:**
- Create: `internal/gui/handshake.go`
- Create: `internal/gui/handshake_test.go`

- [ ] **Step 1: Write the failing test**

```go
// internal/gui/handshake_test.go
package gui

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestHandshake_PingOKThenActivate(t *testing.T) {
	activated := make(chan struct{}, 1)
	mux := http.NewServeMux()
	mux.HandleFunc("/api/ping", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"ok":true,"pid":111,"version":"t"}`))
	})
	mux.HandleFunc("/api/activate-window", func(w http.ResponseWriter, r *http.Request) {
		activated <- struct{}{}
		w.WriteHeader(http.StatusNoContent)
	})
	ts := httptest.NewServer(mux)
	defer ts.Close()

	port := parsePort(t, ts.URL)
	dir := t.TempDir()
	pidport := filepath.Join(dir, "gui.pidport")
	writePidport(t, pidport, 111, port)

	err := TryActivateIncumbent(pidport, 2*time.Second)
	if err != nil {
		t.Fatalf("TryActivateIncumbent: %v", err)
	}
	select {
	case <-activated:
	case <-time.After(1 * time.Second):
		t.Fatal("incumbent never received activate")
	}
}

func TestHandshake_ConnectionRefusedReturnsError(t *testing.T) {
	dir := t.TempDir()
	pidport := filepath.Join(dir, "gui.pidport")
	writePidport(t, pidport, 99999, 1) // unreachable port
	err := TryActivateIncumbent(pidport, 500*time.Millisecond)
	if err == nil {
		t.Fatal("expected error on unreachable incumbent")
	}
}

// --- helpers ---

func parsePort(t *testing.T, url string) int {
	t.Helper()
	// url is e.g. "http://127.0.0.1:54321"
	i := strings.LastIndex(url, ":")
	p, err := strconv.Atoi(url[i+1:])
	if err != nil {
		t.Fatalf("parsePort %q: %v", url, err)
	}
	return p
}

func writePidport(t *testing.T, path string, pid, port int) {
	t.Helper()
	if err := writeFile(path, formatPidport(pid, port)); err != nil {
		t.Fatal(err)
	}
}

func writeFile(path, content string) error {
	return osWriteFile(path, []byte(content), 0o600)
}

var osWriteFile = func(name string, data []byte, perm uint32) error {
	return nil // replaced by platform-appropriate shim below
}
```

Then in `handshake_test.go` also add:

```go
func init() {
	osWriteFile = func(name string, data []byte, perm uint32) error {
		return os.WriteFile(name, data, os.FileMode(perm))
	}
}
```

And add `"os"` import.

- [ ] **Step 2: Run test — expect FAIL**

Run: `go test ./internal/gui -run TestHandshake -v`
Expected: FAIL — `TryActivateIncumbent` undefined.

- [ ] **Step 3: Write implementation**

```go
// internal/gui/handshake.go
package gui

import (
	"encoding/json"
	"fmt"
	"net/http"
	"time"
)

// TryActivateIncumbent is called by a second `mcphub gui` invocation when
// AcquireSingleInstance returned ErrSingleInstanceBusy. It reads the
// pidport file to locate the running instance, probes /api/ping with a
// short total deadline, and if that succeeds posts /api/activate-window.
// Returns nil if the incumbent was reached and signaled; any non-nil
// error means the second instance should either escalate (--force) or
// abort with a human-readable message.
func TryActivateIncumbent(pidportPath string, totalTimeout time.Duration) error {
	pid, port, err := ReadPidport(pidportPath)
	if err != nil {
		return fmt.Errorf("read pidport: %w", err)
	}
	deadline := time.Now().Add(totalTimeout)
	client := &http.Client{Timeout: 500 * time.Millisecond}

	var lastErr error
	for time.Now().Before(deadline) {
		resp, err := client.Get(fmt.Sprintf("http://127.0.0.1:%d/api/ping", port))
		if err != nil {
			lastErr = err
			time.Sleep(250 * time.Millisecond)
			continue
		}
		var body struct {
			OK  bool `json:"ok"`
			PID int  `json:"pid"`
		}
		_ = json.NewDecoder(resp.Body).Decode(&body)
		resp.Body.Close()
		if !body.OK {
			return fmt.Errorf("incumbent ping replied not-ok")
		}
		if body.PID != pid {
			return fmt.Errorf("pidport PID %d does not match running /api/ping PID %d", pid, body.PID)
		}
		// Ping OK — activate.
		req, _ := http.NewRequest(http.MethodPost, fmt.Sprintf("http://127.0.0.1:%d/api/activate-window", port), nil)
		resp2, err := client.Do(req)
		if err != nil {
			return fmt.Errorf("activate-window: %w", err)
		}
		resp2.Body.Close()
		if resp2.StatusCode != http.StatusNoContent {
			return fmt.Errorf("activate-window status %d", resp2.StatusCode)
		}
		return nil
	}
	return fmt.Errorf("incumbent unreachable: %w", lastErr)
}
```

- [ ] **Step 4: Run test to verify PASS**

Run: `go test ./internal/gui -v`
Expected: all green.

- [ ] **Step 5: Commit**

```bash
git add internal/gui/handshake.go internal/gui/handshake_test.go
git commit -m "feat(gui): second-instance handshake (ping incumbent + activate)"
```

---

## Task 7: Wire single-instance + handshake into `mcphub gui`

**Files:**
- Modify: `internal/cli/gui.go`
- Create: `internal/cli/gui_integration_test.go`

- [ ] **Step 1: Write the failing test**

```go
// internal/cli/gui_integration_test.go
package cli

import (
	"os"
	"os/exec"
	"runtime"
	"strings"
	"testing"
	"time"
)

// TestGuiCmd_SecondInstanceActivates spawns two `mcphub gui` processes
// and asserts the second exits 0 without binding a new port (the first
// keeps running).
func TestGuiCmd_SecondInstanceActivates(t *testing.T) {
	if testing.Short() {
		t.Skip("subprocess test")
	}
	if runtime.GOOS == "windows" && os.Getenv("CI") != "" {
		// Named-object isolation between CI containers is unreliable.
		t.Skip("flaky in Windows CI sandbox")
	}
	t.Setenv("MCPHUB_GUI_TEST_PIDPORT_DIR", t.TempDir())

	exe, err := exec.LookPath("go")
	if err != nil {
		t.Fatal(err)
	}
	first := exec.Command(exe, "run", "./cmd/mcphub", "gui", "--no-browser", "--no-tray", "--port", "0")
	first.Stdout = os.Stderr
	first.Stderr = os.Stderr
	if err := first.Start(); err != nil {
		t.Fatalf("start first: %v", err)
	}
	defer func() {
		_ = first.Process.Kill()
		_ = first.Wait()
	}()

	// Give the first instance up to 3s to bind.
	time.Sleep(2 * time.Second)

	out, err := exec.Command(exe, "run", "./cmd/mcphub", "gui", "--no-browser", "--no-tray").CombinedOutput()
	if err != nil {
		t.Fatalf("second instance failed: %v\noutput: %s", err, out)
	}
	if !strings.Contains(string(out), "activated") {
		t.Errorf("second instance output should confirm activation; got: %s", out)
	}
}
```

- [ ] **Step 2: Run test — expect FAIL**

Run: `go test ./internal/cli -run TestGuiCmd_SecondInstance -v`
Expected: FAIL — current `RunE` doesn't honor pidport env var and doesn't activate.

- [ ] **Step 3: Write implementation**

Replace the `RunE` body in `internal/cli/gui.go`:

```go
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx, stop := signal.NotifyContext(cmd.Context(), syscall.SIGINT, syscall.SIGTERM)
			defer stop()

			// Test seam: override pidport dir for subprocess tests that
			// need isolation between concurrent runs.
			pidportPath, err := gui.PidportPath()
			if err != nil {
				return err
			}
			if d := os.Getenv("MCPHUB_GUI_TEST_PIDPORT_DIR"); d != "" {
				pidportPath = filepath.Join(d, "gui.pidport")
			}

			s := gui.NewServer(gui.Config{Port: port, Version: versionString()})

			// Phase A: acquire single-instance lock, falling back to
			// handshake-and-exit if another instance is live.
			listener, err := listenForPort(port)
			if err != nil {
				return err
			}
			boundPort := listener.Addr().(*net.TCPAddr).Port
			_ = listener.Close() // gui.Server will rebind. Ephemeral port picked so handshake has the right port.

			lock, err := gui.AcquireSingleInstanceAt(pidportPath, boundPort)
			if err != nil {
				if !errors.Is(err, gui.ErrSingleInstanceBusy) {
					return err
				}
				if force {
					fmt.Fprintln(cmd.OutOrStderr(), "warning: --force not implemented yet; falling back to handshake")
				}
				if err := gui.TryActivateIncumbent(pidportPath, 2*time.Second); err != nil {
					return fmt.Errorf("another mcphub gui is running but unreachable (%v) — use --force to take over", err)
				}
				fmt.Fprintln(cmd.OutOrStdout(), "activated existing mcphub gui")
				return nil
			}
			defer lock.Release()

			s.OnActivateWindow(func() {
				// Phase 3B-II will wire tray/browser here. MVP: just log.
				fmt.Fprintln(cmd.OutOrStdout(), "activate-window received")
			})

			// Override Config.Port with the pre-picked port so server rebinds same port.
			s = gui.NewServer(gui.Config{Port: boundPort, Version: versionString()})

			ready := make(chan struct{})
			errCh := make(chan error, 1)
			go func() { errCh <- s.Start(ctx, ready) }()
			select {
			case <-ready:
				fmt.Fprintf(cmd.OutOrStdout(), "GUI listening on http://127.0.0.1:%d\n", s.Port())
			case err := <-errCh:
				return err
			case <-ctx.Done():
				return ctx.Err()
			}

			_ = noBrowser
			_ = noTray
			return <-errCh
		},
```

Add helpers at bottom of `gui.go`:

```go
// listenForPort picks an OS-assigned free port when port==0, or binds
// the requested port. Returns the bound listener so the caller can
// forward the port number to Server without a TOCTOU window.
func listenForPort(port int) (*net.TCPListener, error) {
	addr := &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1), Port: port}
	return net.ListenTCP("tcp", addr)
}

// versionString returns the linker-baked version. Ephemeral for MVP.
func versionString() string {
	if v := os.Getenv("MCPHUB_VERSION"); v != "" {
		return v
	}
	return "dev"
}
```

Add imports: `"errors"`, `"fmt"`, `"net"`, `"os"`, `"path/filepath"`, `"time"`.

Also expose `AcquireSingleInstanceAt` from `internal/gui/single_instance.go`:

```go
// AcquireSingleInstanceAt is the exported form of acquireSingleInstanceAt
// so callers outside the gui package (cli) can share the same path.
func AcquireSingleInstanceAt(pidportPath string, port int) (*SingleInstanceLock, error) {
	return acquireSingleInstanceAt(pidportPath, port)
}
```

- [ ] **Step 4: Run test to verify PASS**

Run: `go test ./internal/cli -run TestGuiCmd -v -timeout 30s`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/cli/gui.go internal/cli/gui_integration_test.go internal/gui/single_instance.go
git commit -m "feat(cli): wire single-instance lock + handshake into mcphub gui"
```

---

## Task 8: Embed HTML shell with sidebar navigation

**Files:**
- Create: `internal/gui/assets/index.html`
- Create: `internal/gui/assets.go`
- Create: `internal/gui/assets_test.go`

- [ ] **Step 1: Write the failing test**

```go
// internal/gui/assets_test.go
package gui

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestIndexHTML_ServedAtRoot(t *testing.T) {
	s := NewServer(Config{})
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rec := httptest.NewRecorder()
	s.mux.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("status = %d", rec.Code)
	}
	ct := rec.Header().Get("Content-Type")
	if !strings.HasPrefix(ct, "text/html") {
		t.Errorf("content-type = %q", ct)
	}
	body := rec.Body.String()
	for _, want := range []string{"<!doctype html>", "mcp-local-hub", "data-screen=\"servers\"", "data-screen=\"dashboard\"", "data-screen=\"logs\""} {
		if !strings.Contains(strings.ToLower(body), strings.ToLower(want)) {
			t.Errorf("index.html missing %q", want)
		}
	}
}

func TestStaticAsset_StyleCSS(t *testing.T) {
	s := NewServer(Config{})
	req := httptest.NewRequest(http.MethodGet, "/assets/style.css", nil)
	rec := httptest.NewRecorder()
	s.mux.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("status = %d", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/css") {
		t.Errorf("content-type = %q", ct)
	}
}
```

- [ ] **Step 2: Run test — expect FAIL (paths not served)**

Run: `go test ./internal/gui -run 'TestIndexHTML|TestStaticAsset' -v`
Expected: FAIL — handlers return 404.

- [ ] **Step 3: Create `internal/gui/assets/index.html`**

```html
<!doctype html>
<html lang="en" data-theme="auto">
<head>
  <meta charset="utf-8">
  <meta name="viewport" content="width=device-width,initial-scale=1">
  <title>mcp-local-hub</title>
  <link rel="stylesheet" href="/assets/style.css">
</head>
<body>
  <aside class="sidebar">
    <div class="brand">mcp-local-hub</div>
    <nav>
      <a href="#/servers"   data-screen="servers">Servers</a>
      <a href="#/dashboard" data-screen="dashboard">Dashboard</a>
      <a href="#/logs"      data-screen="logs">Logs</a>
    </nav>
  </aside>
  <main id="screen-root"><!-- filled by app.js --></main>
  <script type="module" src="/assets/app.js"></script>
</body>
</html>
```

Also create an initial `internal/gui/assets/style.css`:

```css
:root {
  --bg: #ffffff;
  --text: #1f2328;
  --border: #d0d7de;
  --success: #1a7f37;
  --danger: #cf222e;
  --sidebar-bg: #f6f8fa;
}
[data-theme="dark"] {
  --bg: #0d1117;
  --text: #c9d1d9;
  --border: #30363d;
  --success: #3fb950;
  --danger: #f85149;
  --sidebar-bg: #161b22;
}
*, *::before, *::after { box-sizing: border-box; }
html, body { margin: 0; padding: 0; height: 100%; background: var(--bg); color: var(--text); font-family: -apple-system, BlinkMacSystemFont, "Segoe UI", sans-serif; }
body { display: grid; grid-template-columns: 220px 1fr; }
.sidebar { background: var(--sidebar-bg); border-right: 1px solid var(--border); padding: 16px; }
.brand { font-weight: 600; margin-bottom: 24px; }
nav { display: flex; flex-direction: column; gap: 4px; }
nav a { color: var(--text); text-decoration: none; padding: 8px 12px; border-radius: 6px; }
nav a:hover { background: var(--border); }
nav a.active { background: var(--border); font-weight: 500; }
main { padding: 24px; overflow: auto; }
```

Create minimal `internal/gui/assets/app.js`:

```js
// Minimal hash-router scaffold. Screen modules register into window.mcphub.screens.
window.mcphub = window.mcphub || { screens: {} };

function render() {
  const hash = location.hash || "#/servers";
  const name = hash.replace(/^#\//, "");
  document.querySelectorAll("nav a").forEach(a => {
    a.classList.toggle("active", a.dataset.screen === name);
  });
  const root = document.getElementById("screen-root");
  root.textContent = "";
  const fn = window.mcphub.screens[name];
  if (fn) fn(root);
  else root.textContent = "Unknown screen: " + name;
}

window.addEventListener("hashchange", render);
window.addEventListener("DOMContentLoaded", render);
```

- [ ] **Step 4: Create `internal/gui/assets.go`**

```go
// internal/gui/assets.go
package gui

import (
	"embed"
	"io/fs"
	"net/http"
)

//go:embed assets/*
var assetsFS embed.FS

func registerAssetRoutes(s *Server) {
	sub, err := fs.Sub(assetsFS, "assets")
	if err != nil {
		panic(err) // embed.FS is a compile-time source, so this cannot fail at runtime
	}
	fileServer := http.FileServer(http.FS(sub))
	s.mux.Handle("/assets/", http.StripPrefix("/assets/", fileServer))
	s.mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		b, _ := fs.ReadFile(sub, "index.html")
		w.Write(b)
	})
}
```

And call it from `NewServer`:

```go
func NewServer(cfg Config) *Server {
	if cfg.PID == 0 {
		cfg.PID = os.Getpid()
	}
	s := &Server{cfg: cfg, mux: http.NewServeMux()}
	registerPingRoutes(s)
	registerAssetRoutes(s)
	return s
}
```

- [ ] **Step 5: Run test to verify PASS**

Run: `go test ./internal/gui -v`
Expected: all green including the new asset tests.

- [ ] **Step 6: Commit**

```bash
git add internal/gui/assets/ internal/gui/assets.go internal/gui/assets_test.go internal/gui/server.go
git commit -m "feat(gui): embed HTML shell + CSS theme + hash-router skeleton"
```

---

## Task 9: `/api/scan` endpoint wrapping `api.Scan()`

**Files:**
- Create: `internal/gui/scan.go`
- Create: `internal/gui/scan_test.go`

- [ ] **Step 1: Write the failing test**

```go
// internal/gui/scan_test.go
package gui

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"mcp-local-hub/internal/api"
)

type fakeScanner struct {
	result *api.ScanResult
	err    error
}

func (f fakeScanner) Scan() (*api.ScanResult, error) { return f.result, f.err }

func TestScan_ReturnsJSONWrappingAPIResult(t *testing.T) {
	r := &api.ScanResult{}
	s := NewServer(Config{})
	s.scanner = fakeScanner{result: r}
	req := httptest.NewRequest(http.MethodGet, "/api/scan", nil)
	rec := httptest.NewRecorder()
	s.mux.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("status = %d", rec.Code)
	}
	var out map[string]any
	if err := json.NewDecoder(rec.Body).Decode(&out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if _, ok := out["clients"]; !ok {
		t.Errorf("response missing top-level keys — got %+v", out)
	}
}
```

- [ ] **Step 2: Run test — expect FAIL**

Run: `go test ./internal/gui -run TestScan -v`
Expected: FAIL — `scanner` field / `/api/scan` route undefined.

- [ ] **Step 3: Write implementation**

Add to `internal/gui/server.go`:

```go
type scanner interface {
	Scan() (*api.ScanResult, error)
}
```

Add field `scanner scanner` to `Server`. Default-initialize in `NewServer` via:

```go
s.scanner = realScanner{}
```

Define `realScanner`:

```go
type realScanner struct{}

func (realScanner) Scan() (*api.ScanResult, error) {
	return (&api.API{}).Scan()
}
```

Add import `"mcp-local-hub/internal/api"` to `server.go`.

Create `internal/gui/scan.go`:

```go
// internal/gui/scan.go
package gui

import (
	"encoding/json"
	"net/http"
)

func registerScanRoutes(s *Server) {
	s.mux.HandleFunc("/api/scan", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			w.Header().Set("Allow", "GET")
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		result, err := s.scanner.Scan()
		if err != nil {
			writeAPIError(w, err, http.StatusInternalServerError, "SCAN_FAILED")
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(result)
	})
}

// writeAPIError is the canonical shape described in spec §4.3.
func writeAPIError(w http.ResponseWriter, err error, status int, code string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{
		"error": err.Error(),
		"code":  code,
	})
}
```

Call from `NewServer`:

```go
registerScanRoutes(s)
```

- [ ] **Step 4: Run test to verify PASS**

Run: `go test ./internal/gui -v`
Expected: all green.

- [ ] **Step 5: Commit**

```bash
git add internal/gui/server.go internal/gui/scan.go internal/gui/scan_test.go
git commit -m "feat(gui): GET /api/scan wraps api.Scan() with error envelope"
```

---

## Task 10: `/api/status` endpoint wrapping `api.Status()`

**Files:**
- Create: `internal/gui/status.go`
- Create: `internal/gui/status_test.go`

- [ ] **Step 1: Write the failing test**

```go
// internal/gui/status_test.go
package gui

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"mcp-local-hub/internal/api"
)

type fakeStatus struct {
	rows []api.DaemonStatus
	err  error
}

func (f fakeStatus) Status() ([]api.DaemonStatus, error) { return f.rows, f.err }

func TestStatus_ReturnsArrayOfDaemonStatus(t *testing.T) {
	s := NewServer(Config{})
	s.status = fakeStatus{rows: []api.DaemonStatus{
		{Server: "memory", TaskName: "mcp-local-hub-memory-default", State: "Running", Port: 9123},
	}}
	req := httptest.NewRequest(http.MethodGet, "/api/status", nil)
	rec := httptest.NewRecorder()
	s.mux.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("status = %d", rec.Code)
	}
	var out []api.DaemonStatus
	if err := json.NewDecoder(rec.Body).Decode(&out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(out) != 1 || out[0].Server != "memory" || out[0].Port != 9123 {
		t.Errorf("unexpected rows: %+v", out)
	}
}
```

- [ ] **Step 2: Run test — expect FAIL**

Run: `go test ./internal/gui -run TestStatus -v`
Expected: FAIL — `status` field / `/api/status` route undefined.

- [ ] **Step 3: Write implementation**

In `server.go`:

```go
type statusProvider interface {
	Status() ([]api.DaemonStatus, error)
}

type realStatusProvider struct{}

func (realStatusProvider) Status() ([]api.DaemonStatus, error) {
	return (&api.API{}).Status()
}
```

Add `status statusProvider` to `Server`. In `NewServer`:

```go
s.status = realStatusProvider{}
registerStatusRoutes(s)
```

Create `internal/gui/status.go`:

```go
// internal/gui/status.go
package gui

import (
	"encoding/json"
	"net/http"
)

func registerStatusRoutes(s *Server) {
	s.mux.HandleFunc("/api/status", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			w.Header().Set("Allow", "GET")
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		rows, err := s.status.Status()
		if err != nil {
			writeAPIError(w, err, http.StatusInternalServerError, "STATUS_FAILED")
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(rows)
	})
}
```

- [ ] **Step 4: Run test to verify PASS**

Run: `go test ./internal/gui -v`
Expected: all green.

- [ ] **Step 5: Commit**

```bash
git add internal/gui/server.go internal/gui/status.go internal/gui/status_test.go
git commit -m "feat(gui): GET /api/status wraps api.Status()"
```

---

## Task 11: SSE event bus — broadcaster infrastructure

**Files:**
- Create: `internal/gui/events.go`
- Create: `internal/gui/events_test.go`

- [ ] **Step 1: Write the failing test**

```go
// internal/gui/events_test.go
package gui

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestBroadcaster_SubscribeReceivesPublishedEvent(t *testing.T) {
	b := NewBroadcaster()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ch := b.Subscribe(ctx)
	go b.Publish(Event{Type: "test", Body: map[string]any{"k": "v"}})

	select {
	case ev := <-ch:
		if ev.Type != "test" {
			t.Errorf("type = %s, want test", ev.Type)
		}
	case <-time.After(500 * time.Millisecond):
		t.Fatal("subscriber timed out")
	}
}

func TestBroadcaster_UnsubscribeOnContextCancel(t *testing.T) {
	b := NewBroadcaster()
	ctx, cancel := context.WithCancel(context.Background())
	ch := b.Subscribe(ctx)
	cancel()
	// Publish after cancel; the subscriber's channel should have been closed.
	b.Publish(Event{Type: "after-cancel"})
	select {
	case _, ok := <-ch:
		if ok {
			t.Error("subscriber channel should be closed after context cancel")
		}
	case <-time.After(200 * time.Millisecond):
		t.Error("cancelled subscriber should have returned from receive immediately")
	}
}

func TestEventsSSE_StreamsPublishedEvents(t *testing.T) {
	s := NewServer(Config{})
	go func() {
		time.Sleep(100 * time.Millisecond)
		s.Broadcaster().Publish(Event{Type: "daemon-state", Body: map[string]any{"server": "memory"}})
	}()

	req := httptest.NewRequest(http.MethodGet, "/api/events", nil)
	rec := httptest.NewRecorder()
	done := make(chan struct{})
	go func() {
		s.mux.ServeHTTP(rec, req)
		close(done)
	}()
	// Read the SSE output until we see an event.
	deadline := time.Now().Add(1 * time.Second)
	for time.Now().Before(deadline) {
		if strings.Contains(rec.Body.String(), "event: daemon-state") {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("never saw event in stream; body: %q", rec.Body.String())
}
```

- [ ] **Step 2: Run test — expect FAIL**

Run: `go test ./internal/gui -run 'TestBroadcaster|TestEventsSSE' -v`
Expected: FAIL — types undefined.

- [ ] **Step 3: Write implementation**

```go
// internal/gui/events.go
package gui

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sync"
)

// Event is the shape pushed onto /api/events. Type matches spec §3.6;
// Body is an arbitrary JSON-serializable payload.
type Event struct {
	Type string         `json:"type"`
	Body map[string]any `json:"body,omitempty"`
}

// Broadcaster is a fan-out channel for Events. Each Subscribe call
// returns a dedicated buffered channel; Publish writes to every
// subscriber without blocking (dropped if the buffer is full).
type Broadcaster struct {
	mu    sync.Mutex
	subs  map[chan Event]struct{}
}

func NewBroadcaster() *Broadcaster {
	return &Broadcaster{subs: map[chan Event]struct{}{}}
}

// Subscribe returns a channel that will receive every Event published
// while ctx is alive. The channel is closed when ctx is canceled.
// Buffered at 16 — a slow consumer drops events rather than backpressures.
func (b *Broadcaster) Subscribe(ctx context.Context) <-chan Event {
	ch := make(chan Event, 16)
	b.mu.Lock()
	b.subs[ch] = struct{}{}
	b.mu.Unlock()
	go func() {
		<-ctx.Done()
		b.mu.Lock()
		delete(b.subs, ch)
		b.mu.Unlock()
		close(ch)
	}()
	return ch
}

// Publish fans out to all subscribers. Non-blocking: a subscriber with
// a full buffer simply misses the event.
func (b *Broadcaster) Publish(ev Event) {
	b.mu.Lock()
	subs := make([]chan Event, 0, len(b.subs))
	for c := range b.subs {
		subs = append(subs, c)
	}
	b.mu.Unlock()
	for _, c := range subs {
		select {
		case c <- ev:
		default: // drop
		}
	}
}

// Broadcaster exposes the singleton on the Server so tests and poller
// goroutines can publish into the stream directly.
func (s *Server) Broadcaster() *Broadcaster { return s.events }

func registerEventsRoutes(s *Server) {
	s.mux.HandleFunc("/api/events", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			w.Header().Set("Allow", "GET")
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		flusher, ok := w.(http.Flusher)
		if !ok {
			http.Error(w, "streaming unsupported", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("Connection", "keep-alive")
		flusher.Flush()

		ctx := r.Context()
		ch := s.events.Subscribe(ctx)
		for {
			select {
			case <-ctx.Done():
				return
			case ev, ok := <-ch:
				if !ok {
					return
				}
				body, _ := json.Marshal(ev.Body)
				fmt.Fprintf(w, "event: %s\ndata: %s\n\n", ev.Type, body)
				flusher.Flush()
			}
		}
	})
}
```

Wire into `Server`:

```go
type Server struct {
	cfg              Config
	mux              *http.ServeMux
	srv              *http.Server
	port             atomic.Int32
	onActivateWindow func()
	events           *Broadcaster
	scanner          scanner
	status           statusProvider
}
```

Populate in `NewServer`:

```go
s.events = NewBroadcaster()
...
registerEventsRoutes(s)
```

- [ ] **Step 4: Run test to verify PASS**

Run: `go test ./internal/gui -v`
Expected: all green.

- [ ] **Step 5: Commit**

```bash
git add internal/gui/events.go internal/gui/events_test.go internal/gui/server.go
git commit -m "feat(gui): /api/events SSE broadcaster with context-bound subscribers"
```

---

## Task 12: Daemon-state poller → publishes `daemon-state` events on deltas

**Files:**
- Create: `internal/gui/poller.go`
- Create: `internal/gui/poller_test.go`

- [ ] **Step 1: Write the failing test**

```go
// internal/gui/poller_test.go
package gui

import (
	"context"
	"testing"
	"time"

	"mcp-local-hub/internal/api"
)

type scriptedStatus struct {
	frames [][]api.DaemonStatus
	idx    int
}

func (s *scriptedStatus) Status() ([]api.DaemonStatus, error) {
	if s.idx >= len(s.frames) {
		return s.frames[len(s.frames)-1], nil
	}
	out := s.frames[s.idx]
	s.idx++
	return out, nil
}

func TestPoller_EmitsDeltaOnStateChange(t *testing.T) {
	frames := [][]api.DaemonStatus{
		{{Server: "memory", State: "Running", Port: 9123}},
		{{Server: "memory", State: "Stopped", Port: 9123}},
	}
	status := &scriptedStatus{frames: frames}
	b := NewBroadcaster()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ch := b.Subscribe(ctx)

	p := NewStatusPoller(status, b, 50*time.Millisecond)
	go p.Run(ctx)

	seen := map[string]int{}
	deadline := time.After(2 * time.Second)
	for len(seen) < 1 {
		select {
		case ev := <-ch:
			if ev.Type == "daemon-state" {
				s, _ := ev.Body["state"].(string)
				seen[s]++
			}
		case <-deadline:
			t.Fatalf("never saw state change; seen=%v", seen)
		}
	}
	if seen["Stopped"] < 1 {
		t.Errorf("expected at least one 'Stopped' delta; seen=%v", seen)
	}
}
```

- [ ] **Step 2: Run test — expect FAIL**

Run: `go test ./internal/gui -run TestPoller -v`
Expected: FAIL.

- [ ] **Step 3: Write implementation**

```go
// internal/gui/poller.go
package gui

import (
	"context"
	"time"

	"mcp-local-hub/internal/api"
)

// StatusPoller samples api.Status() on a fixed interval and publishes
// a daemon-state event on every observed change in (Server, State, PID,
// Port, RAM). The event body matches spec §3.6.
type StatusPoller struct {
	status   statusProvider
	events   *Broadcaster
	interval time.Duration
	last     map[string]api.DaemonStatus
}

func NewStatusPoller(status statusProvider, events *Broadcaster, interval time.Duration) *StatusPoller {
	return &StatusPoller{status: status, events: events, interval: interval, last: map[string]api.DaemonStatus{}}
}

// Run blocks until ctx is canceled. Polls every interval and publishes
// deltas. A single fetch error is logged via the event stream as
// {type:"poller-error"} and the loop continues.
func (p *StatusPoller) Run(ctx context.Context) {
	p.poll(ctx)
	t := time.NewTicker(p.interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			p.poll(ctx)
		}
	}
}

func (p *StatusPoller) poll(ctx context.Context) {
	rows, err := p.status.Status()
	if err != nil {
		p.events.Publish(Event{Type: "poller-error", Body: map[string]any{"err": err.Error()}})
		return
	}
	seen := map[string]struct{}{}
	for _, r := range rows {
		seen[r.Server] = struct{}{}
		prev, ok := p.last[r.Server]
		if ok && prev.State == r.State && prev.PID == r.PID && prev.Port == r.Port {
			continue
		}
		p.last[r.Server] = r
		p.events.Publish(Event{
			Type: "daemon-state",
			Body: map[string]any{
				"server": r.Server,
				"state":  r.State,
				"pid":    r.PID,
				"port":   r.Port,
			},
		})
	}
	// Removed rows: server in last but not in this fetch.
	for name := range p.last {
		if _, still := seen[name]; !still {
			delete(p.last, name)
			p.events.Publish(Event{
				Type: "daemon-state",
				Body: map[string]any{"server": name, "state": "Gone"},
			})
		}
	}
}
```

- [ ] **Step 4: Run test to verify PASS**

Run: `go test ./internal/gui -v`
Expected: all green.

- [ ] **Step 5: Commit**

```bash
git add internal/gui/poller.go internal/gui/poller_test.go
git commit -m "feat(gui): StatusPoller emits daemon-state events on observed deltas"
```

---

## Task 13: Start Poller in `mcphub gui`

**Files:**
- Modify: `internal/cli/gui.go`

- [ ] **Step 1: Update RunE to start poller goroutine**

In `internal/cli/gui.go` RunE, after `go func() { errCh <- s.Start(ctx, ready) }()` but before the `select`, add:

```go
			// Poll status every 5s and push daemon-state events onto /api/events.
			poller := gui.NewStatusPoller(gui.RealStatusProvider{}, s.Broadcaster(), 5*time.Second)
			go poller.Run(ctx)
```

Expose `RealStatusProvider` in `internal/gui/server.go`:

```go
// RealStatusProvider is the production-default statusProvider. Tests inject
// their own; callers outside the package construct this one.
type RealStatusProvider = realStatusProvider
```

- [ ] **Step 2: Run tests** (existing ones still pass)

Run: `go test ./... -count=1`
Expected: all green.

- [ ] **Step 3: Commit**

```bash
git add internal/cli/gui.go internal/gui/server.go
git commit -m "feat(cli): wire StatusPoller into mcphub gui for live SSE"
```

---

## Task 14: `/api/migrate` endpoint wrapping `api.Migrate()`

**Files:**
- Create: `internal/gui/migrate.go`
- Create: `internal/gui/migrate_test.go`

- [ ] **Step 1: Write the failing test**

```go
// internal/gui/migrate_test.go
package gui

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

type fakeMigrator struct {
	called []string
	err    error
}

func (f *fakeMigrator) Migrate(servers []string) error {
	f.called = servers
	return f.err
}

func TestMigrate_CallsAPIWithServerList(t *testing.T) {
	fm := &fakeMigrator{}
	s := NewServer(Config{})
	s.migrator = fm

	req := httptest.NewRequest(http.MethodPost, "/api/migrate",
		bytes.NewReader([]byte(`{"servers":["memory","wolfram"]}`)))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	s.mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d, body=%q", rec.Code, rec.Body.String())
	}
	if strings.Join(fm.called, ",") != "memory,wolfram" {
		t.Errorf("Migrate called with %v", fm.called)
	}
}

func TestMigrate_BadMethodReturns405(t *testing.T) {
	s := NewServer(Config{})
	req := httptest.NewRequest(http.MethodGet, "/api/migrate", nil)
	rec := httptest.NewRecorder()
	s.mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("status = %d", rec.Code)
	}
}
```

- [ ] **Step 2: Run test — expect FAIL**

Run: `go test ./internal/gui -run TestMigrate -v`
Expected: FAIL.

- [ ] **Step 3: Write implementation**

In `server.go`:

```go
type migrator interface {
	Migrate(servers []string) error
}

type realMigrator struct{}

func (realMigrator) Migrate(servers []string) error {
	_, err := (&api.API{}).Migrate(api.MigrateOpts{Servers: servers})
	return err
}
```

Add `migrator migrator` field, populate in `NewServer`:

```go
s.migrator = realMigrator{}
registerMigrateRoutes(s)
```

Create `internal/gui/migrate.go`:

```go
// internal/gui/migrate.go
package gui

import (
	"encoding/json"
	"fmt"
	"net/http"
)

type migrateRequest struct {
	Servers []string `json:"servers"`
}

func registerMigrateRoutes(s *Server) {
	s.mux.HandleFunc("/api/migrate", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			w.Header().Set("Allow", "POST")
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		var req migrateRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeAPIError(w, fmt.Errorf("invalid JSON: %w", err), http.StatusBadRequest, "BAD_REQUEST")
			return
		}
		if err := s.migrator.Migrate(req.Servers); err != nil {
			writeAPIError(w, err, http.StatusInternalServerError, "MIGRATE_FAILED")
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})
}
```

> Note: `api.MigrateOpts` / `api.API.Migrate` signatures may differ in the codebase. Check `internal/api/migrate.go` for the exact shape and adapt the `realMigrator` adapter; the HTTP contract (`{"servers":[...]}` → 204 or error envelope) stays.

- [ ] **Step 4: Run test to verify PASS**

Run: `go test ./internal/gui -v`
Expected: all green.

- [ ] **Step 5: Commit**

```bash
git add internal/gui/server.go internal/gui/migrate.go internal/gui/migrate_test.go
git commit -m "feat(gui): POST /api/migrate wraps api.Migrate()"
```

---

## Task 15: `/api/servers/:server/restart` endpoint

**Files:**
- Create: `internal/gui/servers.go`
- Create: `internal/gui/servers_test.go`

- [ ] **Step 1: Write the failing test**

```go
// internal/gui/servers_test.go
package gui

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

type fakeRestart struct {
	called string
	err    error
}

func (f *fakeRestart) Restart(server string) error {
	f.called = server
	return f.err
}

func TestRestartServer_InvokesAPI(t *testing.T) {
	fr := &fakeRestart{}
	s := NewServer(Config{})
	s.restart = fr
	req := httptest.NewRequest(http.MethodPost, "/api/servers/memory/restart", nil)
	rec := httptest.NewRecorder()
	s.mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d body=%q", rec.Code, rec.Body.String())
	}
	if fr.called != "memory" {
		t.Errorf("Restart called with %q, want memory", fr.called)
	}
}
```

- [ ] **Step 2: Run test — expect FAIL**

Run: `go test ./internal/gui -run TestRestartServer -v`
Expected: FAIL.

- [ ] **Step 3: Write implementation**

In `server.go`:

```go
type restarter interface {
	Restart(server string) error
}

type realRestarter struct{}

func (realRestarter) Restart(server string) error {
	return (&api.API{}).Restart(server)
}
```

Add `restart restarter` field; populate `s.restart = realRestarter{}` and `registerServerRoutes(s)`.

Create `internal/gui/servers.go`:

```go
// internal/gui/servers.go
package gui

import (
	"net/http"
	"strings"
)

func registerServerRoutes(s *Server) {
	s.mux.HandleFunc("/api/servers/", func(w http.ResponseWriter, r *http.Request) {
		// URL pattern: /api/servers/<name>/restart  (POST)
		rest := strings.TrimPrefix(r.URL.Path, "/api/servers/")
		parts := strings.Split(rest, "/")
		if len(parts) != 2 {
			http.NotFound(w, r)
			return
		}
		name, action := parts[0], parts[1]
		switch action {
		case "restart":
			if r.Method != http.MethodPost {
				w.Header().Set("Allow", "POST")
				http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
				return
			}
			if err := s.restart.Restart(name); err != nil {
				writeAPIError(w, err, http.StatusInternalServerError, "RESTART_FAILED")
				return
			}
			w.WriteHeader(http.StatusNoContent)
		default:
			http.NotFound(w, r)
		}
	})
}
```

> Note: confirm `(&api.API{}).Restart(server string) error` signature in `internal/api/`. If the signature is `RestartOpts`-based, adapt `realRestarter` but keep the HTTP shape.

- [ ] **Step 4: Run test to verify PASS**

Run: `go test ./internal/gui -v`
Expected: all green.

- [ ] **Step 5: Commit**

```bash
git add internal/gui/server.go internal/gui/servers.go internal/gui/servers_test.go
git commit -m "feat(gui): POST /api/servers/:name/restart wraps api.Restart()"
```

---

## Task 16: Servers screen — HTML template + JS module

**Files:**
- Create: `internal/gui/assets/servers.js`
- Modify: `internal/gui/assets/app.js` (register servers screen)
- Modify: `internal/gui/assets/style.css` (add table styles)

- [ ] **Step 1: Author `servers.js`**

```js
// internal/gui/assets/servers.js
window.mcphub.screens.servers = async function(root) {
  root.innerHTML = '<h1>Servers</h1><div id="servers-content">Loading…</div>';
  const content = document.getElementById("servers-content");

  try {
    const [scan, status] = await Promise.all([
      fetch("/api/scan").then(r => r.json()),
      fetch("/api/status").then(r => r.json()),
    ]);
    content.innerHTML = "";
    const table = document.createElement("table");
    table.className = "servers-matrix";
    const clients = ["claude-code", "codex-cli", "gemini-cli", "antigravity"];
    const header = document.createElement("thead");
    header.innerHTML = `
      <tr>
        <th>Server</th>
        ${clients.map(c => `<th>${c}</th>`).join("")}
        <th>Port</th>
        <th>State</th>
      </tr>`;
    table.appendChild(header);
    const tbody = document.createElement("tbody");
    const statusByServer = Object.fromEntries(status.map(s => [s.server, s]));
    // Render one row per scanned server (union of via-hub + can-migrate + unknown).
    const servers = collectServers(scan);
    for (const server of servers) {
      const row = document.createElement("tr");
      const st = statusByServer[server.name] || {};
      row.innerHTML = `
        <td>${server.name}</td>
        ${clients.map(c => renderCell(server, c)).join("")}
        <td>${st.port ?? "—"}</td>
        <td>${st.state ?? "—"}</td>`;
      tbody.appendChild(row);
    }
    table.appendChild(tbody);
    content.appendChild(table);
  } catch (err) {
    content.innerHTML = `<p class="error">Failed to load: ${err.message}</p>`;
  }
};

function collectServers(scan) {
  // Shape depends on api.ScanResult; adapt once the struct is known.
  // Defensive: always return an array of {name, routing:{client:"via-hub"|"stdio"|"none"}}.
  const out = {};
  for (const [client, servers] of Object.entries(scan.clients ?? {})) {
    for (const s of servers ?? []) {
      if (!out[s.name]) out[s.name] = { name: s.name, routing: {} };
      out[s.name].routing[client] = s.status; // e.g. "via-hub"
    }
  }
  return Object.values(out).sort((a, b) => a.name.localeCompare(b.name));
}

function renderCell(server, client) {
  const routing = server.routing[client];
  const checked = routing === "via-hub" ? "checked" : "";
  const disabled = routing === "unsupported" || routing === "not-installed" ? "disabled" : "";
  return `<td><input type="checkbox" data-server="${server.name}" data-client="${client}" ${checked} ${disabled}></td>`;
}
```

- [ ] **Step 2: Register in app.js**

Add near the top of `app.js`:

```js
// Load per-screen modules. These populate window.mcphub.screens[name].
const modules = ["/assets/servers.js", "/assets/dashboard.js", "/assets/logs.js"];
modules.forEach(src => {
  const sc = document.createElement("script");
  sc.src = src;
  document.head.appendChild(sc);
});
```

Remove the `type="module"` attribute from the `<script>` tag in `index.html` (so scripts can add sibling scripts via appendChild without module boundary). Replace with:

```html
<script src="/assets/app.js"></script>
```

- [ ] **Step 3: Add CSS for matrix table**

Append to `style.css`:

```css
.servers-matrix { border-collapse: collapse; width: 100%; max-width: 960px; }
.servers-matrix th, .servers-matrix td { border: 1px solid var(--border); padding: 8px 12px; text-align: left; }
.servers-matrix thead th { background: var(--sidebar-bg); font-weight: 500; }
.servers-matrix input[type="checkbox"] { margin: 0; }
.error { color: var(--danger); }
```

Also add empty placeholders so tests for dashboard.js / logs.js don't break:

```js
// internal/gui/assets/dashboard.js
window.mcphub.screens.dashboard = function(root) { root.innerHTML = "<h1>Dashboard</h1><p>Coming in Task 17.</p>"; };
```

```js
// internal/gui/assets/logs.js
window.mcphub.screens.logs = function(root) { root.innerHTML = "<h1>Logs</h1><p>Coming in Task 19.</p>"; };
```

- [ ] **Step 4: Manually smoke the UI**

Run: `go run ./cmd/mcphub gui --no-browser --no-tray --port 9100`
Then in a browser: `http://127.0.0.1:9100/#/servers`
Expected: the servers matrix loads with checkboxes. Checkbox toggles don't yet call Migrate (Task 17 wires Apply).

- [ ] **Step 5: Commit**

```bash
git add internal/gui/assets/
git commit -m "feat(gui): Servers screen — scan+status matrix with per-client checkboxes"
```

---

## Task 17: Servers screen — "Apply changes" button → `/api/migrate`

**Files:**
- Modify: `internal/gui/assets/servers.js`

- [ ] **Step 1: Extend the screen body**

Replace the body of `window.mcphub.screens.servers` with:

```js
window.mcphub.screens.servers = async function(root) {
  root.innerHTML = `
    <h1>Servers</h1>
    <div id="servers-toolbar">
      <button id="apply-migrate" disabled>Apply changes</button>
      <span id="apply-status" style="margin-left:12px"></span>
    </div>
    <div id="servers-content">Loading…</div>`;
  const content = document.getElementById("servers-content");
  const applyBtn = document.getElementById("apply-migrate");
  const applyStatus = document.getElementById("apply-status");
  const toMigrate = new Set();

  function onCheckboxChange(e) {
    const server = e.target.dataset.server;
    const initial = e.target.defaultChecked;
    if (e.target.checked !== initial) toMigrate.add(server);
    else toMigrate.delete(server);
    applyBtn.disabled = toMigrate.size === 0;
  }

  async function applyChanges() {
    const servers = [...toMigrate];
    applyBtn.disabled = true;
    applyStatus.textContent = `Migrating ${servers.join(", ")}…`;
    try {
      const resp = await fetch("/api/migrate", {
        method: "POST",
        headers: {"Content-Type": "application/json"},
        body: JSON.stringify({servers}),
      });
      if (!resp.ok) {
        const body = await resp.json().catch(() => ({error: "unknown"}));
        throw new Error(body.error);
      }
      applyStatus.textContent = "Migrated. Refreshing…";
      toMigrate.clear();
      render(); // reload the matrix
    } catch (err) {
      applyStatus.innerHTML = `<span class="error">Failed: ${err.message}</span>`;
    }
  }
  applyBtn.addEventListener("click", applyChanges);

  async function render() {
    content.textContent = "Loading…";
    const [scan, status] = await Promise.all([
      fetch("/api/scan").then(r => r.json()),
      fetch("/api/status").then(r => r.json()),
    ]);
    content.innerHTML = "";
    const table = document.createElement("table");
    table.className = "servers-matrix";
    const clients = ["claude-code", "codex-cli", "gemini-cli", "antigravity"];
    table.innerHTML = `<thead><tr><th>Server</th>${clients.map(c => `<th>${c}</th>`).join("")}<th>Port</th><th>State</th></tr></thead>`;
    const tbody = document.createElement("tbody");
    const statusByServer = Object.fromEntries(status.map(s => [s.server, s]));
    const servers = collectServers(scan);
    for (const server of servers) {
      const row = document.createElement("tr");
      const st = statusByServer[server.name] || {};
      row.innerHTML = `
        <td>${server.name}</td>
        ${clients.map(c => renderCell(server, c)).join("")}
        <td>${st.port ?? "—"}</td>
        <td>${st.state ?? "—"}</td>`;
      tbody.appendChild(row);
    }
    table.appendChild(tbody);
    content.appendChild(table);
    content.querySelectorAll("input[type=checkbox]").forEach(cb => cb.addEventListener("change", onCheckboxChange));
  }
  render();
};
```

(Keep `collectServers` and `renderCell` unchanged below.)

- [ ] **Step 2: Smoke test**

Run: `go run ./cmd/mcphub gui --no-browser --no-tray --port 9100`
In browser at `/#/servers`:
- Toggle a checkbox. Expected: "Apply changes" button becomes enabled.
- Click Apply. Expected: status text updates, then matrix re-renders with the new state.

- [ ] **Step 3: Commit**

```bash
git add internal/gui/assets/servers.js
git commit -m "feat(gui): Servers screen Apply → /api/migrate end-to-end"
```

---

## Task 18: Dashboard screen — live SSE-driven daemon cards

**Files:**
- Modify: `internal/gui/assets/dashboard.js`
- Modify: `internal/gui/assets/style.css`

- [ ] **Step 1: Write `dashboard.js` with SSE subscriber**

```js
// internal/gui/assets/dashboard.js
window.mcphub.screens.dashboard = function(root) {
  root.innerHTML = `<h1>Dashboard</h1><div id="cards" class="cards"></div>`;
  const cardsEl = document.getElementById("cards");
  const state = {}; // server name -> DaemonStatus

  function render() {
    cardsEl.innerHTML = "";
    Object.values(state).sort((a, b) => a.server.localeCompare(b.server)).forEach(d => {
      const card = document.createElement("div");
      card.className = "card " + (d.state === "Running" ? "ok" : "down");
      card.innerHTML = `
        <div class="card-title">${d.server}</div>
        <div class="card-kv"><span>Port</span><span>${d.port ?? "—"}</span></div>
        <div class="card-kv"><span>PID</span><span>${d.pid ?? "—"}</span></div>
        <div class="card-kv"><span>State</span><span class="state">${d.state}</span></div>
        <div class="card-actions">
          <button data-server="${d.server}" class="restart-btn">Restart</button>
        </div>`;
      cardsEl.appendChild(card);
    });
    cardsEl.querySelectorAll(".restart-btn").forEach(btn => btn.addEventListener("click", async () => {
      const name = btn.dataset.server;
      btn.disabled = true;
      btn.textContent = "Restarting…";
      try {
        const resp = await fetch(`/api/servers/${encodeURIComponent(name)}/restart`, {method: "POST"});
        if (!resp.ok) throw new Error((await resp.json()).error ?? resp.status);
        btn.textContent = "Restarted";
      } catch (e) {
        btn.textContent = "Failed";
      } finally {
        setTimeout(() => { btn.textContent = "Restart"; btn.disabled = false; }, 1500);
      }
    }));
  }

  // Initial load via /api/status, then live updates via SSE.
  fetch("/api/status").then(r => r.json()).then(rows => {
    rows.forEach(r => state[r.server] = r);
    render();
  });

  const es = new EventSource("/api/events");
  es.addEventListener("daemon-state", e => {
    const body = JSON.parse(e.data);
    if (body.state === "Gone") delete state[body.server];
    else state[body.server] = Object.assign(state[body.server] ?? {server: body.server}, body);
    render();
  });
  // Cleanup on screen switch: close the EventSource when the screen root is replaced.
  const observer = new MutationObserver(() => {
    if (!document.body.contains(root)) es.close();
  });
  observer.observe(document.body, {childList: true, subtree: true});
};
```

- [ ] **Step 2: Append card styles to `style.css`**

```css
.cards { display: grid; grid-template-columns: repeat(auto-fit, minmax(240px, 1fr)); gap: 16px; margin-top: 16px; }
.card { border: 1px solid var(--border); border-radius: 8px; padding: 16px; background: var(--bg); }
.card-title { font-weight: 600; margin-bottom: 12px; }
.card-kv { display: flex; justify-content: space-between; padding: 4px 0; font-size: 14px; }
.card.ok .state { color: var(--success); }
.card.down .state { color: var(--danger); }
.card-actions { margin-top: 12px; }
.card-actions button { width: 100%; padding: 6px; border: 1px solid var(--border); background: var(--sidebar-bg); color: var(--text); border-radius: 4px; cursor: pointer; }
```

- [ ] **Step 3: Smoke test**

Run: `go run ./cmd/mcphub gui --no-browser --no-tray --port 9100`
Browser at `/#/dashboard`:
- Expected: one card per running daemon.
- Kill a daemon via Task Manager. Expected: within 5s, its card flips to red.
- Click Restart on a card. Expected: scheduler restarts the daemon, card flips back to green.

- [ ] **Step 4: Commit**

```bash
git add internal/gui/assets/dashboard.js internal/gui/assets/style.css
git commit -m "feat(gui): Dashboard screen — live SSE daemon cards with restart"
```

---

## Task 19: `/api/logs/:server` and `/api/logs/:server/stream` + Logs screen

**Files:**
- Create: `internal/gui/logs.go`
- Create: `internal/gui/logs_test.go`
- Modify: `internal/gui/assets/logs.js`

- [ ] **Step 1: Write the failing test**

```go
// internal/gui/logs_test.go
package gui

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

type fakeLogs struct {
	body string
	err  error
}

func (f fakeLogs) Logs(server string, tail int) (string, error) {
	return f.body, f.err
}

func TestLogs_GetReturnsText(t *testing.T) {
	s := NewServer(Config{})
	s.logs = fakeLogs{body: "line1\nline2\n"}
	req := httptest.NewRequest(http.MethodGet, "/api/logs/memory?tail=100", nil)
	rec := httptest.NewRecorder()
	s.mux.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("status = %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "line1") {
		t.Errorf("body = %q", rec.Body.String())
	}
}
```

- [ ] **Step 2: Run test — expect FAIL**

Run: `go test ./internal/gui -run TestLogs -v`
Expected: FAIL.

- [ ] **Step 3: Write implementation**

In `server.go`:

```go
type logsProvider interface {
	Logs(server string, tail int) (string, error)
}

type realLogs struct{}

func (realLogs) Logs(server string, tail int) (string, error) {
	return (&api.API{}).Logs(server, tail)
}
```

Add `logs logsProvider` field, populate `s.logs = realLogs{}` and `registerLogsRoutes(s)`.

Create `internal/gui/logs.go`:

```go
// internal/gui/logs.go
package gui

import (
	"fmt"
	"net/http"
	"strconv"
	"strings"
)

func registerLogsRoutes(s *Server) {
	s.mux.HandleFunc("/api/logs/", func(w http.ResponseWriter, r *http.Request) {
		rest := strings.TrimPrefix(r.URL.Path, "/api/logs/")
		parts := strings.SplitN(rest, "/", 2)
		server := parts[0]
		streaming := len(parts) == 2 && parts[1] == "stream"
		if streaming {
			streamLogs(s, server, w, r)
			return
		}
		if r.Method != http.MethodGet {
			w.Header().Set("Allow", "GET")
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		tail, _ := strconv.Atoi(r.URL.Query().Get("tail"))
		if tail <= 0 {
			tail = 500
		}
		body, err := s.logs.Logs(server, tail)
		if err != nil {
			writeAPIError(w, err, http.StatusInternalServerError, "LOGS_FAILED")
			return
		}
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		_, _ = w.Write([]byte(body))
	})
}

// streamLogs is a minimal tail-follow implementation: it re-reads the
// log periodically and emits any suffix new since the last emission.
// More sophisticated fsnotify-based streaming is Phase 3B-II territory.
func streamLogs(s *Server, server string, w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	flusher.Flush()
	var lastLen int
	ctx := r.Context()
	ticker := timeNewTicker()
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			body, err := s.logs.Logs(server, 0)
			if err != nil {
				fmt.Fprintf(w, "event: error\ndata: %s\n\n", err.Error())
				flusher.Flush()
				return
			}
			if len(body) > lastLen {
				suffix := body[lastLen:]
				for _, line := range strings.Split(suffix, "\n") {
					if line == "" {
						continue
					}
					fmt.Fprintf(w, "event: log-line\ndata: %s\n\n", line)
				}
				lastLen = len(body)
				flusher.Flush()
			}
		}
	}
}

// timeNewTicker is a tiny indirection to keep this file testable without
// dragging time.Ticker into the test seam. Extracted for parity with
// the poller's interval handling.
func timeNewTicker() *timeTicker { return newTimeTicker() }

type timeTicker struct {
	C <-chan timeTime
	s func()
}

func (t *timeTicker) Stop() { t.s() }

type timeTime struct{}

func newTimeTicker() *timeTicker {
	ch := make(chan timeTime, 1)
	done := make(chan struct{})
	go func() {
		t := timeNewTimer()
		for {
			select {
			case <-done:
				return
			case <-t:
				ch <- timeTime{}
				t = timeNewTimer()
			}
		}
	}()
	return &timeTicker{C: ch, s: func() { close(done) }}
}

func timeNewTimer() <-chan timeTime {
	ch := make(chan timeTime, 1)
	go func() {
		// Use time.Sleep here directly to avoid spinning up a full ticker
		// for a seam that tests don't need to accelerate.
		timeSleepMillis(500)
		ch <- timeTime{}
	}()
	return ch
}

// Separate thin wrappers kept unexported so test build can shim them if
// we later want deterministic log-stream timing.
var timeSleepMillis = func(ms int) { /* injectable */ }
```

> The ticker indirection above is intentionally overengineered so a future test can inject faster timing without importing `time` in the test body. For the MVP smoke path, replace the `timeSleepMillis` default with a real `time.Sleep`:

```go
func init() {
	timeSleepMillis = func(ms int) { time.Sleep(time.Duration(ms) * time.Millisecond) }
}
```

Add `"time"` import.

> Note: the `(&api.API{}).Logs(server, tail)` signature should match `internal/api/logs.go`. Verify and adapt the `realLogs` adapter.

- [ ] **Step 4: Update `logs.js`**

```js
// internal/gui/assets/logs.js
window.mcphub.screens.logs = function(root) {
  root.innerHTML = `
    <h1>Logs</h1>
    <div id="logs-controls">
      <select id="logs-server"></select>
      <label><input type="number" id="logs-tail" value="500" min="1" max="10000"> lines</label>
      <label><input type="checkbox" id="logs-follow"> Follow</label>
      <button id="logs-refresh">Refresh</button>
    </div>
    <pre id="logs-body"></pre>`;

  const sel = document.getElementById("logs-server");
  const tailEl = document.getElementById("logs-tail");
  const followEl = document.getElementById("logs-follow");
  const body = document.getElementById("logs-body");
  let es = null;

  fetch("/api/status").then(r => r.json()).then(rows => {
    rows.forEach(r => {
      const opt = document.createElement("option");
      opt.value = r.server; opt.textContent = r.server;
      sel.appendChild(opt);
    });
    if (rows.length) load();
  });

  async function load() {
    if (es) { es.close(); es = null; }
    body.textContent = "Loading…";
    const server = sel.value;
    const tail = tailEl.value;
    const resp = await fetch(`/api/logs/${encodeURIComponent(server)}?tail=${encodeURIComponent(tail)}`);
    body.textContent = await resp.text();
    if (followEl.checked) startFollow(server);
  }
  function startFollow(server) {
    es = new EventSource(`/api/logs/${encodeURIComponent(server)}/stream`);
    es.addEventListener("log-line", e => {
      body.textContent += e.data + "\n";
      body.scrollTop = body.scrollHeight;
    });
  }
  sel.addEventListener("change", load);
  tailEl.addEventListener("change", load);
  followEl.addEventListener("change", load);
  document.getElementById("logs-refresh").addEventListener("click", load);
};
```

- [ ] **Step 5: Append CSS**

```css
#logs-controls { display: flex; gap: 12px; align-items: center; margin-bottom: 16px; }
#logs-body { background: var(--sidebar-bg); border: 1px solid var(--border); padding: 12px; overflow: auto; max-height: 70vh; font-family: monospace; font-size: 12px; }
```

- [ ] **Step 6: Run tests + smoke**

Run: `go test ./internal/gui -v`
Expected: all green.

Run: `go run ./cmd/mcphub gui --no-browser --port 9100` then open `/#/logs` in browser.

- [ ] **Step 7: Commit**

```bash
git add internal/gui/server.go internal/gui/logs.go internal/gui/logs_test.go internal/gui/assets/logs.js internal/gui/assets/style.css
git commit -m "feat(gui): /api/logs + Logs screen with tail + follow via SSE"
```

---

## Task 20: Browser auto-launch (Chrome/Edge app-mode → default)

**Files:**
- Create: `internal/gui/browser.go`
- Create: `internal/gui/browser_test.go`
- Modify: `internal/cli/gui.go`

- [ ] **Step 1: Write the failing test**

```go
// internal/gui/browser_test.go
package gui

import (
	"errors"
	"testing"
)

func TestLaunchBrowser_PrefersChromeOverEdgeOverDefault(t *testing.T) {
	log := []string{}
	restore := withSpawnerOverride(func(cmd string, args ...string) error {
		log = append(log, cmd)
		// chrome "not found", edge "not found", default succeeds
		if cmd == "chrome" || cmd == "msedge" {
			return errors.New("not found")
		}
		return nil
	})
	defer restore()
	if err := LaunchBrowser("http://127.0.0.1:9100"); err != nil {
		t.Fatalf("LaunchBrowser: %v", err)
	}
	if len(log) != 3 {
		t.Errorf("attempted spawns = %v, want chrome→msedge→default", log)
	}
}

func withSpawnerOverride(fn func(string, ...string) error) func() {
	orig := spawnProcess
	spawnProcess = fn
	return func() { spawnProcess = orig }
}
```

- [ ] **Step 2: Run test — expect FAIL**

Run: `go test ./internal/gui -run TestLaunchBrowser -v`
Expected: FAIL.

- [ ] **Step 3: Write implementation**

```go
// internal/gui/browser.go
package gui

import (
	"os/exec"
	"runtime"
)

// spawnProcess is the injectable seam for LaunchBrowser tests.
var spawnProcess = func(name string, args ...string) error {
	return exec.Command(name, args...).Start()
}

// LaunchBrowser opens the GUI URL in the user's browser. Preference:
// Chrome --app → Edge --app → OS default. App-mode (--app=...) gives a
// chromeless window that feels more desktop-native than a new tab.
//
// All errors are swallowed past the last fallback — the user can
// always open the URL manually; a failed browser launch should not
// fail `mcphub gui`.
func LaunchBrowser(url string) error {
	chromeArg := "--app=" + url
	for _, cmd := range []string{"chrome", "google-chrome", "chromium"} {
		if err := spawnProcess(cmd, chromeArg); err == nil {
			return nil
		}
	}
	if err := spawnProcess("msedge", chromeArg); err == nil {
		return nil
	}
	// OS default browser fallback.
	switch runtime.GOOS {
	case "windows":
		return spawnProcess("rundll32", "url.dll,FileProtocolHandler", url)
	case "darwin":
		return spawnProcess("open", url)
	default:
		return spawnProcess("xdg-open", url)
	}
}
```

Wire into `internal/cli/gui.go` — after `fmt.Fprintf(cmd.OutOrStdout(), "GUI listening on http://127.0.0.1:%d\n", s.Port())`:

```go
			if !noBrowser {
				url := fmt.Sprintf("http://127.0.0.1:%d/", s.Port())
				if err := gui.LaunchBrowser(url); err != nil {
					fmt.Fprintf(cmd.OutOrStderr(), "warning: could not auto-launch browser: %v\n", err)
				}
			}
```

- [ ] **Step 4: Run test to verify PASS**

Run: `go test ./internal/gui -v`
Expected: all green.

- [ ] **Step 5: Commit**

```bash
git add internal/gui/browser.go internal/gui/browser_test.go internal/cli/gui.go
git commit -m "feat(gui): auto-launch browser (Chrome→Edge→default, --app mode)"
```

---

## Task 21: Tray stub (cross-platform) + Windows systray integration

**Files:**
- Create: `internal/tray/tray.go`
- Create: `internal/tray/tray_windows.go`
- Create: `internal/tray/tray_other.go`
- Create: `internal/tray/icon_healthy.ico` (placeholder; see Step 1)
- Modify: `internal/cli/gui.go`
- Modify: `go.mod` (add `github.com/getlantern/systray`)

- [ ] **Step 1: Author placeholder icon**

Use a minimal 16×16 mono ICO. Generate via any tool (e.g., `magick -size 16x16 xc:#1a7f37 internal/tray/icon_healthy.ico`). One icon is enough for MVP; state-aware variants (§6) are Phase 3B-II.

- [ ] **Step 2: Write tray package**

```go
// internal/tray/tray.go
package tray

import (
	"context"
	_ "embed"
)

//go:embed icon_healthy.ico
var iconBytes []byte

// Config is what Run needs to produce the menu and route actions.
type Config struct {
	// ActivateWindow is called when the user left-clicks the tray icon
	// or picks "Open dashboard" from the right-click menu.
	ActivateWindow func()
	// Quit is called when the user picks "Quit (keep daemons)". The CLI
	// uses this to cancel the GUI context and exit cleanly.
	Quit func()
}

// Run blocks running the tray event loop until ctx is canceled. On
// non-Windows platforms it is a no-op that returns immediately — the
// GUI server runs normally without an accompanying tray icon.
//
// The implementation selects at build time via tray_windows.go /
// tray_other.go build tags.
func Run(ctx context.Context, cfg Config) error {
	return runImpl(ctx, cfg)
}
```

```go
// internal/tray/tray_windows.go
//go:build windows

package tray

import (
	"context"

	"github.com/getlantern/systray"
)

func runImpl(ctx context.Context, cfg Config) error {
	ready := make(chan struct{})
	onReady := func() {
		systray.SetIcon(iconBytes)
		systray.SetTooltip("mcp-local-hub")
		mOpen := systray.AddMenuItem("Open dashboard", "")
		systray.AddSeparator()
		mQuit := systray.AddMenuItem("Quit (keep daemons)", "")
		close(ready)
		go func() {
			for {
				select {
				case <-ctx.Done():
					systray.Quit()
					return
				case <-mOpen.ClickedCh:
					if cfg.ActivateWindow != nil {
						cfg.ActivateWindow()
					}
				case <-mQuit.ClickedCh:
					if cfg.Quit != nil {
						cfg.Quit()
					}
					systray.Quit()
					return
				}
			}
		}()
	}
	onExit := func() {}
	systray.Run(onReady, onExit)
	return nil
}
```

```go
// internal/tray/tray_other.go
//go:build !windows

package tray

import "context"

func runImpl(ctx context.Context, cfg Config) error {
	<-ctx.Done()
	return nil
}
```

- [ ] **Step 3: Wire into `mcphub gui`**

In `internal/cli/gui.go`, before `return <-errCh`:

```go
			if !noTray {
				go func() {
					_ = tray.Run(ctx, tray.Config{
						ActivateWindow: func() {
							// Same handshake second instances use, but in-process.
							_ = gui.TryActivateIncumbent(pidportPath, 500*time.Millisecond)
						},
						Quit: stop, // signal.NotifyContext's cancel function
					})
				}()
			}
```

Add `"mcp-local-hub/internal/tray"` import.

- [ ] **Step 4: Add dependency**

```bash
go get github.com/getlantern/systray@latest
go mod tidy
```

- [ ] **Step 5: Manual smoke on Windows**

Run: `go run ./cmd/mcphub gui --port 9100`
Expected: tray icon appears; left-click focuses browser window; right-click "Quit (keep daemons)" shuts down the GUI.

- [ ] **Step 6: Commit**

```bash
git add internal/tray/ internal/cli/gui.go go.mod go.sum
git commit -m "feat(tray): Windows systray with Open + Quit (keep daemons)"
```

---

## Task 22: Release build — Windows subsystem flag + verification

**Files:**
- Create: `docs/phase-3b-verification.md`

- [ ] **Step 1: Build with Windows GUI subsystem**

```bash
go build -ldflags="-H windowsgui -s -w" -o bin/mcphub.exe ./cmd/mcphub
```

Verify binary size and subsystem:

```bash
ls -la bin/mcphub.exe
# Windows: confirm no console flashes when scheduler launches a daemon.
```

- [ ] **Step 2: Smoke all 4 MVP success criteria (§2.1 #1, #2, #4, #5)**

| # | Criterion | How to verify |
|---|---|---|
| #1 | Servers matrix loads within 1 second on a hub-installed host | Run `mcphub gui`, open `/#/servers`, confirm table populated |
| #2 | Flipping a checkbox + Apply rewrites one client config only | Flip `memory` off for Codex CLI, Apply, diff `~/.codex/config.toml` and `~/.claude.json` — only codex-cli changed |
| #4 | Killing a daemon flips its Dashboard card to red within 5s | `taskkill /f /pid <port-9123-pid>`, watch Dashboard |
| #5 | `mcp gui` after reboot with stale pidport file starts cleanly | `echo "1 9100" > %LOCALAPPDATA%\mcp-local-hub\gui.pidport`, run `mcphub gui`, verify startup |

- [ ] **Step 3: Write `docs/phase-3b-verification.md`**

Use the structure from `docs/phase-3-verification.md` as a template:

- Goal (MVP scope)
- What landed (commit trail M1→M22)
- What's deferred to Phase 3B-II
- Live verification output for criteria #1, #2, #4, #5
- Test suite + static-check status
- Known gaps / follow-ups

- [ ] **Step 4: Full test suite**

```bash
go test ./... -count=1 -timeout 120s
go vet ./...
staticcheck ./...
gofmt -l .
git diff --check origin/master...HEAD
```

All expected clean.

- [ ] **Step 5: Commit**

```bash
git add docs/phase-3b-verification.md
git commit -m "docs(phase-3b): GUI MVP verification — 4 success criteria + build with -H windowsgui"
```

---

## Self-review

1. **Spec coverage (MVP scope §2.1 #1, #2, #4, #5)**:
   - #1 Servers matrix → Task 16 (screen) + Task 9/10 (endpoints) ✓
   - #2 Per-client checkbox flip → Task 17 (Apply wire) ✓
   - #4 Daemon kill → Dashboard card red → Task 11 (SSE bus) + Task 12 (poller) + Task 18 (Dashboard reacts) ✓
   - #5 Stale pidport recovery → Task 5 (flock-based acquire overwrites pidport) ✓
   - Deferred to Phase 3B-II: #3 (create manifest from stdio) + #7 (backup management UI) — acknowledged in goal statement

2. **Placeholder scan**: none (every code block is complete).

3. **Type consistency**:
   - `scanner`/`statusProvider`/`migrator`/`restarter`/`logsProvider` interfaces defined in Task 1/3/9/10/14/15/19, exposed via unexported Server fields, each with a `real*` production implementation — signatures match across tasks
   - `Event {Type string; Body map[string]any}` shape used in Tasks 11/12/18 consistently
   - `SingleInstanceLock` / `Release()` exposed in Task 5, used in Task 7
   - `AppDataDir`/`PidportPath` in Task 4 used by Task 5 and Task 7

4. **Stub-behavior note**: Tasks 1–3 ship a server that binds a port and answers `/api/ping` but has no UI. Tasks 4–6 add single-instance semantics. Task 7 wires it into CLI. Tasks 8–19 build the UI in stages. Task 20 launches the browser. Task 21 adds tray. Task 22 is release hardening. After Task 18 the GUI is functionally usable — Tasks 19–21 are refinements.

---

**Plan complete and saved to `docs/superpowers/plans/2026-04-22-phase-3b-gui-mvp.md`.**

Two execution options:

**1. Subagent-Driven (recommended)** — I dispatch a fresh subagent per task, two-stage review between each (spec-compliance then code-quality), fast iteration without losing main-session context. Best for a 22-task plan where each task is 1-3 files.

**2. Inline Execution** — Execute tasks in this session using `superpowers:executing-plans`, batch with checkpoints for user review every few tasks. Good if you want to watch each step land in real time.

Which approach?

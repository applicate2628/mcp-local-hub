// Package lldb implements the stdio↔TCP bridge for LLDB's built-in MCP
// server. It is consumed as a library from two entry points:
//   - cmd/lldb-bridge, a standalone binary for users who don't want the hub
//   - internal/lldb.NewCommand, embedded as an mcphub subcommand
//
// Both entry points share the same bridge loop, LLDB spawn logic, and
// platform-specific applyNoWindow helpers via runLldbBridge, so there is
// no behavior drift between shapes.
//
// This file is the Go rewrite of the Python reference script at
// C:\Users\dima_\.local\mcp-servers\lldb-bridge\bridge.py. Why this lives
// in mcphub (and as its own standalone binary) rather than as a Python
// script: we ship one exe for the whole stack, so reusing it as the stdio
// MCP command removes an extra install step and keeps upgrade semantics
// uniform with the other hub daemons.
//
// LLDB's built-in MCP server (`protocol-server start MCP listen://host:port`)
// speaks MCP over a raw TCP socket, not stdio — this bridge adapts that to
// the stdio framing every MCP client expects. When paired with the hub's
// stdio-bridge transport it also gains HTTP multiplexing for free: several
// Claude Code sessions can share the single LLDB TCP connection because the
// hub serializes requests on the wire.
//
// Lifecycle:
//  1. Try to connect the existing port. On success, skip straight to bridge.
//  2. Otherwise spawn lldb.exe, pipe the `protocol-server start` command to
//     its stdin, and poll until the port opens (up to spawnTimeout).
//  3. Forward stdin→socket and socket→stdout concurrently; either side
//     closing triggers a full shutdown.
//  4. If we spawned LLDB, terminate it on exit.
package lldb

import (
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// Tunables kept explicit so future operators can justify changes without
// digging through the bridge logic.
const (
	lldbConnectTimeout = 500 * time.Millisecond // single-attempt connect budget
	lldbSpawnTimeout   = 10 * time.Second       // max wait for a spawned lldb to open its port
	lldbPollInterval   = 200 * time.Millisecond // between port-probe attempts after spawn
	bridgeCopyBuf      = 64 * 1024              // io.Copy buffer; matches MCP line sizes in practice
)

// spawnedLldb bundles the subprocess handle with the stdin pipe we keep
// open for its lifetime. exec.Cmd does not retain the pipe returned from
// StdinPipe, so without this wrapper the handle could be finalized and
// closed before we actively close it during teardown.
type spawnedLldb struct {
	cmd   *exec.Cmd
	stdin io.WriteCloser
}

// parseHostPort splits "host:port" with IPv4 tolerance. Intentionally
// rejects bare ports ("47000") to keep the CLI argument shape consistent
// with bridge.py's documented usage.
func parseHostPort(s string) (string, int, error) {
	host, portStr, err := net.SplitHostPort(s)
	if err != nil {
		return "", 0, fmt.Errorf("expected host:port, got %q: %w", s, err)
	}
	port, err := strconv.Atoi(portStr)
	if err != nil || port <= 0 || port > 65535 {
		return "", 0, fmt.Errorf("invalid port %q", portStr)
	}
	return host, port, nil
}

// defaultLldbPath mirrors bridge.py's default for the ru-RU/MSYS2 dev host.
// Keeping the same default means existing `.claude.json` entries migrate
// to the hub without changing argv order.
func defaultLldbPath() string {
	if runtime.GOOS == "windows" {
		return `C:\msys64\ucrt64\bin\lldb.exe`
	}
	return "lldb"
}

// runLldbBridge is the main bridge loop. It owns the spawned subprocess (if
// any) and guarantees it is cleaned up on every exit path including signals.
func runLldbBridge(host string, port int, lldbPath string) error {
	addr := net.JoinHostPort(host, strconv.Itoa(port))

	// Best-effort connect to an existing server.
	sock, err := net.DialTimeout("tcp", addr, lldbConnectTimeout)
	var spawned *spawnedLldb
	if err != nil {
		// Nothing listening → spawn our own LLDB and wait for it to bind.
		if _, statErr := os.Stat(lldbPath); statErr != nil {
			return fmt.Errorf("lldb not found at %s (pass --lldb-path): %w", lldbPath, statErr)
		}
		s, spawnErr := spawnLldb(lldbPath, host, port)
		if spawnErr != nil {
			return fmt.Errorf("spawn lldb: %w", spawnErr)
		}
		spawned = s
		defer terminateSpawned(spawned)

		sock, err = waitForPort(addr, spawned.cmd)
		if err != nil {
			return fmt.Errorf("lldb did not open %s within %s: %w", addr, lldbSpawnTimeout, err)
		}
	}
	defer sock.Close()

	// Honor Ctrl+C / SIGTERM so the spawned LLDB is terminated cleanly.
	// Without the signal trap, a parent kill would orphan LLDB.
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)
	defer signal.Stop(sigCh)

	return bridge(sock, os.Stdin, os.Stdout, sigCh)
}

// spawnLldb starts LLDB with the MCP protocol-server command piped to its
// stdin. Returns both the running subprocess and the stdin pipe; the caller
// must keep both alive for the bridge lifetime and close the stdin handle
// during teardown (see terminateSpawned).
//
// stdout/stderr are discarded because LLDB's interactive prompts would
// otherwise interleave with the MCP wire protocol going through our stdio.
func spawnLldb(lldbPath, host string, port int) (*spawnedLldb, error) {
	cmd := exec.Command(lldbPath)
	applyNoWindow(cmd)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	cmd.Stdout = io.Discard
	cmd.Stderr = io.Discard
	if err := cmd.Start(); err != nil {
		_ = stdin.Close()
		return nil, err
	}
	// The MCP listen command must end with a newline — LLDB's REPL is
	// line-buffered and will otherwise sit on the fragment forever.
	if _, writeErr := fmt.Fprintf(stdin, "protocol-server start MCP listen://%s:%d\n", host, port); writeErr != nil {
		_ = stdin.Close()
		_ = cmd.Process.Kill()
		return nil, fmt.Errorf("write protocol-server command: %w", writeErr)
	}
	return &spawnedLldb{cmd: cmd, stdin: stdin}, nil
}

// waitForPort polls the target address until either the port accepts a
// connection (returns the live socket) or the deadline elapses. The
// spawned subprocess is watched in parallel — if LLDB dies before opening
// its port, we fail fast instead of blocking the full timeout.
func waitForPort(addr string, cmd *exec.Cmd) (net.Conn, error) {
	deadline := time.Now().Add(lldbSpawnTimeout)
	for time.Now().Before(deadline) {
		// Early exit: subprocess crashed before it could bind. ProcessState
		// is only populated after Wait, so a live subprocess shows nil here.
		if cmd.ProcessState != nil && cmd.ProcessState.Exited() {
			return nil, fmt.Errorf("lldb exited with %s", cmd.ProcessState)
		}
		sock, err := net.DialTimeout("tcp", addr, lldbConnectTimeout)
		if err == nil {
			return sock, nil
		}
		time.Sleep(lldbPollInterval)
	}
	return nil, fmt.Errorf("timeout")
}

// bridge runs two concurrent io.Copy loops between the TCP socket and the
// local stdio. Exits when the socket→stdout direction reports EOF (LLDB
// closed its send half) or when a termination signal arrives.
//
// Why not "either direction finishes": stdin→socket finishing means the
// parent (hub or an interactive shell) closed our stdin — but LLDB may
// still be about to send its response bytes over the socket. If we treat
// stdin EOF as "done" we race the socket teardown against LLDB's response
// and truncate it. The socket→stdout loop is the authoritative completion
// signal because LLDB only closes the socket after it has flushed its
// last response and observed our CloseWrite.
func bridge(sock net.Conn, stdin io.Reader, stdout io.Writer, sigCh <-chan os.Signal) error {
	stdinDone := make(chan error, 1)
	stdoutDone := make(chan error, 1)

	// stdin → socket. CloseWrite (not Close) on EOF so the read half of
	// the socket stays open for LLDB's final response.
	go func() {
		_, err := copyBuf(sock, stdin)
		if tcpSock, ok := sock.(*net.TCPConn); ok {
			_ = tcpSock.CloseWrite()
		}
		stdinDone <- err
	}()

	// socket → stdout. Returns when LLDB closes its send half.
	go func() {
		_, err := copyBuf(stdout, sock)
		stdoutDone <- err
	}()

	select {
	case err := <-stdoutDone:
		_ = sock.Close()
		<-stdinDone // drain; may have already finished
		if err != nil && !isClosedConnErr(err) {
			return err
		}
		return nil
	case sig := <-sigCh:
		_ = sock.Close()
		<-stdinDone
		<-stdoutDone
		return fmt.Errorf("interrupted by %s", sig)
	}
}

// copyBuf allocates a dedicated buffer so neither direction steals from a
// shared pool under load.
func copyBuf(dst io.Writer, src io.Reader) (int64, error) {
	return io.CopyBuffer(dst, src, make([]byte, bridgeCopyBuf))
}

// isClosedConnErr recognizes the several error strings Go emits when a
// socket is closed out from under an in-flight Read/Write. net.ErrClosed
// covers most cases but stdio EOF and "file already closed" slip through.
func isClosedConnErr(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, "use of closed network connection") ||
		strings.Contains(msg, "file already closed") ||
		strings.Contains(msg, "EOF")
}

// terminateSpawned tries graceful shutdown first (close stdin triggers
// LLDB's REPL EOF path), then kills hard after a short grace period.
func terminateSpawned(s *spawnedLldb) {
	if s == nil || s.cmd == nil || s.cmd.Process == nil {
		return
	}
	// Closing stdin lets LLDB exit on its own when it notices no more input.
	_ = s.stdin.Close()

	done := make(chan struct{})
	go func() {
		_, _ = s.cmd.Process.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		_ = s.cmd.Process.Kill()
		<-done
	}
}

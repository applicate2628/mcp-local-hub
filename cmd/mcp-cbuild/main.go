// Command mcp-cbuild is a self-contained stdio MCP (Model Context Protocol)
// server that drives CMake (configure/build/test/workflow/clean) and vcpkg
// (install/list/manifest/search) for native C/C++ projects and returns
// structured build diagnostics (file:line:col:severity) parsed from the
// underlying tool output.
//
// It is a SECOND binary in the mcp-local-hub module (alongside cmd/mcphub) and
// imports no mcphub internal packages; the hub manages it as an ordinary stdio
// daemon.
//
// Transport rule (hard): stdout carries ONLY JSON-RPC frames. Every log line
// goes to stderr — main wires log.SetOutput(os.Stderr) before serving so a
// stray log write can never corrupt the protocol stream.
package main

import (
	"context"
	"flag"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"

	"mcp-local-hub/cmd/mcp-cbuild/internal/cbuild"
	"mcp-local-hub/cmd/mcp-cbuild/internal/mcp"
)

// version is the server version advertised in the MCP initialize handshake.
const version = "0.1.0"

func shutdownSignals() []os.Signal {
	return []os.Signal{os.Interrupt, syscall.SIGTERM}
}

func main() {
	workDir := flag.String("w", "", "default working directory for tool calls that omit working_dir (absolute; defaults to the process working directory)")
	flag.Parse()

	// stdout is reserved for JSON-RPC frames; route all logging to stderr.
	log.SetOutput(os.Stderr)
	log.SetFlags(log.LstdFlags | log.Lmsgprefix)
	log.SetPrefix("mcp-cbuild: ")

	defaultDir := *workDir
	if defaultDir != "" {
		if abs, err := filepath.Abs(defaultDir); err == nil {
			defaultDir = abs
		} else {
			log.Printf("warning: could not resolve -w %q to an absolute path: %v", *workDir, err)
		}
	}

	srv := mcp.NewServer("mcp-cbuild", version, cbuild.Tools(defaultDir))

	// SIGINT/Ctrl+C and SIGTERM trigger the same clean shutdown; stdin EOF (the
	// hub closing the pipe) also ends Serve.
	ctx, stop := signal.NotifyContext(context.Background(), shutdownSignals()...)
	defer stop()

	if err := srv.Serve(ctx, os.Stdin, os.Stdout); err != nil {
		log.Fatalf("serve: %v", err)
	}
}

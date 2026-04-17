package cli

import (
	"net"
	"strings"
	"testing"

	"mcp-local-hub/internal/config"
)

// TestPreflight_RespectsDaemonFilter ensures --daemon filter keeps Preflight
// from checking ports of unrelated daemons that may legitimately be occupied
// by a previous partial install.
//
// Setup: two daemons pointing at the SAME occupied port. With filter="second",
// the first daemon must be skipped and the error must reference only "second".
// With no filter, the first daemon is checked first and the error references
// "first".
func TestPreflight_RespectsDaemonFilter(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()
	occupiedPort := ln.Addr().(*net.TCPAddr).Port

	m := &config.ServerManifest{
		Name:      "testsrv",
		Kind:      config.KindGlobal,
		Transport: config.TransportNativeHTTP,
		Command:   "go", // on PATH whenever `go test` runs
		Daemons: []config.DaemonSpec{
			{Name: "first", Port: occupiedPort},
			{Name: "second", Port: occupiedPort},
		},
	}

	// Filter="second" — "first" must be skipped; error should mention only "second".
	err = Preflight(m, "second")
	if err == nil {
		t.Fatal("Preflight(m, 'second') = nil, want error (port occupied)")
	}
	if !strings.Contains(err.Error(), "second") {
		t.Errorf("error should reference 'second' daemon: %v", err)
	}
	if strings.Contains(err.Error(), "first") {
		t.Errorf("error should NOT reference filtered-out 'first' daemon: %v", err)
	}

	// No filter — "first" is checked first, must be in the message.
	err = Preflight(m, "")
	if err == nil {
		t.Fatal("Preflight(m, '') = nil, want error")
	}
	if !strings.Contains(err.Error(), "first") {
		t.Errorf("unfiltered error should reference 'first' daemon (iteration order): %v", err)
	}
}

// TestPreflight_UnknownCommand ensures the command check runs regardless of filter.
func TestPreflight_UnknownCommand(t *testing.T) {
	m := &config.ServerManifest{
		Name:    "testsrv",
		Command: "this-binary-definitely-does-not-exist-mcp-local-hub",
		Daemons: []config.DaemonSpec{{Name: "x", Port: 1}},
	}
	if err := Preflight(m, "x"); err == nil {
		t.Error("expected error for missing command")
	}
}

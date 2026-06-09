//go:build linux

package api

import (
	"net"
	"os"
	"testing"
)

func TestLoopbackPortOwnerPIDLinuxResolvesListeningProcess(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen loopback: %v", err)
	}
	defer ln.Close()
	port := ln.Addr().(*net.TCPAddr).Port

	pid, ok, err := loopbackPortOwnerPID(port)
	if err != nil {
		t.Fatalf("loopbackPortOwnerPID(%d): %v", port, err)
	}
	if !ok {
		t.Fatalf("loopbackPortOwnerPID(%d) ok=false, want true", port)
	}
	if pid != os.Getpid() {
		t.Fatalf("loopbackPortOwnerPID(%d) pid=%d, want current pid %d", port, pid, os.Getpid())
	}
}

func TestLoopbackTCPListenInodeFromProcNetRequiresLoopbackListen(t *testing.T) {
	path := t.TempDir() + "/tcp"
	content := `  sl  local_address rem_address   st tx_queue rx_queue tr tm->when retrnsmt   uid  timeout inode
   0: 0100007F:238B 00000000:0000 0A 00000000:00000000 00:00000000 00000000 1000 0 12345 1 0000000000000000 100 0 0 10 0
   1: 0200007F:238B 00000000:0000 0A 00000000:00000000 00:00000000 00000000 1000 0 99999 1 0000000000000000 100 0 0 10 0
   2: 0100007F:238C 00000000:0000 01 00000000:00000000 00:00000000 00000000 1000 0 77777 1 0000000000000000 100 0 0 10 0
`
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write proc fixture: %v", err)
	}
	inode, ok, err := loopbackTCPListenInodeFromProcNet(path, 9099)
	if err != nil {
		t.Fatalf("loopbackTCPListenInodeFromProcNet: %v", err)
	}
	if !ok || inode != "12345" {
		t.Fatalf("inode=%q ok=%v, want 12345 true", inode, ok)
	}
}

func TestLoopbackTCPListenInodeFromProcNetMissingTableIsNotError(t *testing.T) {
	// A missing /proc/net table (e.g. /proc/net/tcp6 on an IPv6-disabled host)
	// must be treated as "no listener of that family", NOT a probe error, so the
	// caller's IPv4 unbound result stands and a wedged daemon is classified
	// port_unbound rather than port_owner_unverified (Codex bot #271 P2).
	inode, ok, err := loopbackTCPListenInodeFromProcNet(t.TempDir()+"/does-not-exist", 9099)
	if err != nil {
		t.Fatalf("missing table returned a probe error: %v", err)
	}
	if ok || inode != "" {
		t.Fatalf("missing table inode=%q ok=%v, want \"\" false", inode, ok)
	}
}

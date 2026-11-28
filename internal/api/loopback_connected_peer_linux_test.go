//go:build linux

package api

import (
	"context"
	"encoding/binary"
	"fmt"
	"net/netip"
	"strings"
	"testing"
)

func TestGUIQuitEstablishedOwnerTable(t *testing.T) {
	local := netip.MustParseAddrPort("127.0.0.1:45000")
	remote := netip.MustParseAddrPort("127.0.0.1:45001")
	addr := binary.NativeEndian.Uint32([]byte{127, 0, 0, 1})
	row := fmt.Sprintf("0: %08X:%04X %08X:%04X 01 00000000:00000000 00:00000000 00000000 1000 0 12345\n", addr, remote.Port(), addr, local.Port())
	if inode, err := linuxConnectedPeerInode(context.Background(), strings.NewReader(row), local, remote); err != nil || inode != "12345" {
		t.Fatalf("exact established tuple: inode=%q err=%v", inode, err)
	}
	if _, err := linuxConnectedPeerInode(context.Background(), strings.NewReader(row+row), local, remote); err == nil {
		t.Fatal("ambiguous tuple accepted")
	}
	if _, err := linuxConnectedPeerInode(context.Background(), strings.NewReader(row), remote, local); err == nil {
		t.Fatal("wrong tuple direction accepted")
	}
}

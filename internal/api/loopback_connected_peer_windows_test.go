//go:build windows

package api

import (
	"encoding/binary"
	"net/netip"
	"testing"
)

func TestGUIQuitEstablishedOwnerTable(t *testing.T) {
	local := netip.MustParseAddrPort("127.0.0.1:45000")
	remote := netip.MustParseAddrPort("127.0.0.1:45001")
	table := make([]byte, 28)
	binary.LittleEndian.PutUint32(table, 1)
	row := table[4:]
	binary.LittleEndian.PutUint32(row, 5)
	copy(row[4:8], []byte{127, 0, 0, 1})
	binary.BigEndian.PutUint16(row[8:10], remote.Port())
	copy(row[12:16], []byte{127, 0, 0, 1})
	binary.BigEndian.PutUint16(row[16:18], local.Port())
	binary.LittleEndian.PutUint32(row[20:24], 123)
	if err := verifyWindowsConnectedPeerTable(table, local, remote, 123); err != nil {
		t.Fatal(err)
	}
	if err := verifyWindowsConnectedPeerTable(table[:27], local, remote, 123); err == nil {
		t.Fatal("incomplete owner snapshot accepted")
	}
	if err := verifyWindowsConnectedPeerTable(table, remote, local, 123); err == nil {
		t.Fatal("wrong tuple direction accepted")
	}
}

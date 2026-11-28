//go:build windows

package api

import (
	"context"
	"encoding/binary"
	"errors"
	"net/netip"
	"runtime"
	"unsafe"

	"golang.org/x/sys/windows"
)

var getQuitExtendedTCPTable = windows.NewLazySystemDLL("iphlpapi.dll").NewProc("GetExtendedTcpTable")

func verifyLoopbackConnectedPeer(ctx context.Context, local, remote netip.AddrPort, pid int) error {
	if uint64(pid) > uint64(^uint32(0)) {
		return errors.New("invalid Windows peer PID")
	}
	if err := getQuitExtendedTCPTable.Find(); err != nil {
		return err
	}
	const ownerPIDConnections = 4
	const sizeLimit = 16 << 20
	var required uint32
	var buf []byte
	for range 4 {
		if err := ctx.Err(); err != nil {
			return err
		}
		var p uintptr
		if len(buf) > 0 {
			p = uintptr(unsafe.Pointer(&buf[0]))
		}
		status, _, _ := getQuitExtendedTCPTable.Call(p, uintptr(unsafe.Pointer(&required)), 0, windows.AF_INET, ownerPIDConnections, 0)
		runtime.KeepAlive(buf)
		switch uint32(status) {
		case 0:
			if required > uint32(len(buf)) {
				return errors.New("unbounded Windows TCP owner table")
			}
			return verifyWindowsConnectedPeerTable(buf[:required], local, remote, uint32(pid))
		case uint32(windows.ERROR_INSUFFICIENT_BUFFER):
			if required < 4 || required > sizeLimit {
				return errors.New("unbounded Windows TCP owner table")
			}
			buf = make([]byte, required)
		default:
			return windows.Errno(status)
		}
	}
	return errors.New("Windows TCP owner table did not stabilize")
}

func verifyWindowsConnectedPeerTable(buf []byte, local, remote netip.AddrPort, pid uint32) error {
	const rowSize = 24 // documented MIB_TCPROW_OWNER_PID: six DWORD fields
	const established = 5
	if len(buf) < 4 || uint64(binary.LittleEndian.Uint32(buf)) > uint64((len(buf)-4)/rowSize) {
		return errors.New("invalid Windows TCP owner table")
	}
	count := int(binary.LittleEndian.Uint32(buf))
	la, ra := local.Addr().As4(), remote.Addr().As4()
	var owner uint32
	matches := 0
	for i := 0; i < count; i++ {
		row := buf[4+i*rowSize : 4+(i+1)*rowSize]
		if binary.LittleEndian.Uint32(row) != established ||
			[4]byte(row[4:8]) != ra || binary.BigEndian.Uint16(row[8:10]) != remote.Port() ||
			[4]byte(row[12:16]) != la || binary.BigEndian.Uint16(row[16:18]) != local.Port() {
			continue
		}
		matches++
		owner = binary.LittleEndian.Uint32(row[20:24])
	}
	if matches == 0 {
		return errLoopbackPeerNotPublished
	}
	if matches != 1 || owner != pid {
		return errors.New("connected loopback peer is not the retained owner")
	}
	return nil
}

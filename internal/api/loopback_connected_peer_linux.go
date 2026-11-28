//go:build linux

package api

import (
	"bufio"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net/netip"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

func verifyLoopbackConnectedPeer(ctx context.Context, local, remote netip.AddrPort, pid int) error {
	readInode := func() (string, error) {
		f, err := os.Open("/proc/net/tcp")
		if err != nil {
			return "", err
		}
		defer f.Close()
		return linuxConnectedPeerInode(ctx, f, local, remote)
	}
	inode, err := readInode()
	if err != nil {
		return err
	}
	owned, denied, err := scanProcFDDirLinks(ctx, filepath.Join("/proc", strconv.Itoa(pid), "fd"), func(link string) bool {
		return link == "socket:["+inode+"]"
	})
	if err != nil {
		return err
	}
	if denied {
		return errors.New("connected GUI owner socket unavailable")
	}
	if !owned {
		return errLoopbackPeerNotPublished
	}
	checked, err := readInode()
	if err != nil {
		return err
	}
	if checked != inode {
		return errors.New("established loopback peer identity changed")
	}
	return nil
}

func linuxConnectedPeerInode(ctx context.Context, r io.Reader, local, remote netip.AddrPort) (string, error) {
	scan := bufio.NewScanner(r)
	// Linux proc renders the exact IPv4 loopback address in native byte order.
	// The existing listener probe uses the same supported little-endian table.
	addr := fmt.Sprintf("%08X", binary.NativeEndian.Uint32([]byte{127, 0, 0, 1}))
	destination := fmt.Sprintf("%s:%04X", addr, remote.Port())
	source := fmt.Sprintf("%s:%04X", addr, local.Port())
	var result string
	matches := 0
	for scan.Scan() {
		if err := ctx.Err(); err != nil {
			return "", err
		}
		f := strings.Fields(scan.Text())
		if len(f) < 4 || f[1] != destination || f[2] != source || f[3] != "01" {
			continue
		}
		matches++
		if len(f) < 10 {
			return "", errors.New("invalid established loopback row")
		}
		n, err := strconv.ParseUint(f[9], 10, 64)
		if err != nil {
			return "", errors.New("invalid established loopback inode")
		}
		if n > 0 {
			result = f[9]
		}
	}
	if err := scan.Err(); err != nil {
		return "", err
	}
	if matches > 1 {
		return "", errors.New("ambiguous established loopback peer")
	}
	if result == "" {
		return "", errLoopbackPeerNotPublished
	}
	return result, nil
}

package gui

import (
	"context"
	"errors"
	"mcp-local-hub/internal/api"
	"net"
)

// VerifyGUIQuitConnectedPeer relates the actual TCP connection back to the
// original process observation. It does not acquire a new control target.
func VerifyGUIQuitConnectedPeer(ctx context.Context, path string, record GUIOwnerRecord, observed *ProcessIdentity, conn net.Conn) (err error) {
	if observed == nil || observed.Handle == 0 || !observed.Alive || observed.StartTime.IsZero() || !observed.StartTime.Equal(record.StartTime) || conn == nil {
		return ErrGUIQuitGeneration
	}
	l, lok := conn.LocalAddr().(*net.TCPAddr)
	r, rok := conn.RemoteAddr().(*net.TCPAddr)
	if !lok || !rok || r.Port != record.Port {
		return ErrGUIQuitGeneration
	}
	if err := api.VerifyLoopbackConnectedPeer(ctx, l.AddrPort(), r.AddrPort(), record.PID); err != nil {
		return err
	}
	actual, e := retainedProcessID(record.PID)
	defer func() { err = errors.Join(err, actual.Close()) }()
	if e != nil {
		return e
	}
	if !actual.Alive || actual.Denied || actual.Indeterminate || actual.Handle == 0 || !actual.StartTime.Equal(observed.StartTime) || actual.ImagePath != observed.ImagePath {
		return ErrGUIQuitGeneration
	}
	current, e := ReadGUIOwnerRecord(path)
	if e != nil {
		return e
	}
	if current != record {
		return ErrGUIQuitGeneration
	}
	return ctx.Err()
}

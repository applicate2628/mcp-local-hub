package gdb

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"strings"
	"testing"
	"time"
)

func TestCommandReadsTrailingPromptWhileWriting(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	input, debuggerInput := net.Pipe()
	output, debuggerOutput := net.Pipe()
	defer input.Close()
	defer output.Close()
	s := newSession(input, output, func() error { return errors.New("unexpected force") })

	peer := make(chan error, 1)
	go func() {
		// The preceding *stopped was already received. An unbuffered pipe
		// makes the debugger wait until its trailing prompt has been read.
		if _, err := io.WriteString(debuggerOutput, "(gdb) \n"); err != nil {
			peer <- err
			return
		}
		command, err := bufio.NewReader(debuggerInput).ReadString('\n')
		if err == nil && command != "info inferiors\n" {
			err = errors.New("unexpected metadata request")
		}
		if err == nil {
			_, err = io.WriteString(debuggerOutput, "~\"Num Description\\n\"\n^done\n")
		}
		peer <- err
	}()

	got, err := s.Command(ctx, "info inferiors")
	_ = debuggerInput.Close()
	_ = debuggerOutput.Close()
	peerErr := <-peer
	if err != nil || peerErr != nil || !strings.Contains(got, "Num Description") {
		t.Fatalf("metadata=%q; command error=%v; debugger peer error=%v", got, err, peerErr)
	}
}

func TestCommandToolReportsOnlyElapsedMilestonesAfterReply(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	input, peerInput := net.Pipe()
	output, peerOutput := net.Pipe()
	defer input.Close()
	defer output.Close()
	defer peerInput.Close()
	defer peerOutput.Close()
	if err := peerInput.SetReadDeadline(time.Now().Add(3 * time.Second)); err != nil {
		t.Fatal(err)
	}
	if err := peerOutput.SetWriteDeadline(time.Now().Add(3 * time.Second)); err != nil {
		t.Fatal(err)
	}
	s := newSession(input, output, func() error { return errors.New("unexpected force") })
	var log bytes.Buffer
	gs := newServer()
	gs.sessions["private-fixture-session"] = s
	gs.commandTimings = &log
	req := newRequest(t, map[string]any{"session_id": "private-fixture-session", "command": "private-fixture-command"})
	done := make(chan error, 1)
	go func() {
		res, err := gs.commandTool(ctx, req)
		if err == nil && (res == nil || res.IsError) {
			err = errors.New("command did not return a successful tool result")
		}
		done <- err
	}()
	command, err := bufio.NewReader(peerInput).ReadString('\n')
	if err != nil || command != "private-fixture-command\n" {
		t.Fatalf("command write = %q, %v", command, err)
	}
	select {
	case err := <-done:
		t.Fatalf("command returned before its reply: %v", err)
	default:
	}
	if _, err := io.WriteString(peerOutput, "~\"private-fixture-output\\n\"\n^done\n"); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}

	var acquired, wrote, replied, total int64
	line := log.String()
	n, err := fmt.Sscanf(line, "gdb-command-timing acquired_us=%d write_done_us=%d reply_done_us=%d total_us=%d", &acquired, &wrote, &replied, &total)
	if err != nil || n != 4 || acquired < 0 || wrote < acquired || replied < acquired || total < wrote || total < replied ||
		line != fmt.Sprintf("gdb-command-timing acquired_us=%d write_done_us=%d reply_done_us=%d total_us=%d\n", acquired, wrote, replied, total) {
		t.Fatalf("unexpected timing-only record: %q (scan: %v)", line, err)
	}
}

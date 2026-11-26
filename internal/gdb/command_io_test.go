package gdb

import (
	"bufio"
	"context"
	"errors"
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

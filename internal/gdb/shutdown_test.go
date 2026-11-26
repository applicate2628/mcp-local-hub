package gdb

import (
	"bufio"
	"context"
	"errors"
	"io"
	"net"
	"os"
	"testing"
	"time"
)

func TestTerminateDrainsExitReplyBeforeWait(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	stdin, inputPeer := net.Pipe()
	stdout, outputPeer := net.Pipe()
	defer stdin.Close()
	defer stdout.Close()

	ungraceful := errors.New("unused forced shutdown")
	s := newSession(stdin, stdout, func() error { return ungraceful })
	// net.Pipe has no buffering. The simulated debugger cannot exit until
	// the owner reads its exit response, just like the native Windows case.
	exit := make(chan error, 1)
	finished := make(chan struct{})
	peerDone := make(chan struct{})
	go func() {
		defer close(finished)
		s.reap(func() (*os.ProcessState, error) {
			select {
			case err := <-exit:
				return &os.ProcessState{}, err
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		})
	}()
	defer func() {
		cancel()
		_ = inputPeer.Close()
		_ = outputPeer.Close()
		<-finished
		<-peerDone
	}()
	go func() {
		defer close(peerDone)
		defer inputPeer.Close()
		defer outputPeer.Close()
		line, err := bufio.NewReader(inputPeer).ReadString('\n')
		if err == nil && line != "-gdb-exit\n" {
			err = errors.New("missing MI exit request")
		}
		if err == nil {
			_, err = io.WriteString(outputPeer, "^exit\n")
		}
		exit <- err
	}()

	if err := s.terminate(ctx, time.Second); err != nil {
		t.Fatal(err)
	}
	receipt, err := s.awaitSettlement(ctx)
	if err != nil || receipt.err() != nil || s.forceTried {
		t.Fatalf("natural settlement required: receipt=%v observation=%v forced=%v", receipt.err(), err, s.forceTried)
	}
}

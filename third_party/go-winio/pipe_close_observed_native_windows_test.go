//go:build windows

package winio

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"runtime"
	"testing"
	"time"

	"golang.org/x/sys/windows"
)

func TestCloseObservedAcceptedPrivatePipe(t *testing.T) {
	const wait = 5 * time.Second
	var nonce [16]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		t.Fatal(err)
	}
	name := fmt.Sprintf("\\\\.\\pipe\\winio-close-receipt-%x", nonce)
	user, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		t.Fatal(err)
	}
	listener, err := ListenPipe(name, &PipeConfig{
		SecurityDescriptor: "D:P(A;;GA;;;" + user.User.Sid.String() + ")",
		MessageMode:        false,
	})
	if err != nil {
		t.Fatal(err)
	}
	closeListener := closePrivatePipeFixtureOnce(listener, wait)

	type acceptOutcome struct {
		conn net.Conn
		err  error
	}
	accepted := make(chan acceptOutcome, 1)
	go func() {
		conn, err := listener.Accept()
		accepted <- acceptOutcome{conn, err}
	}()

	var receivedAccept bool
	var closeClient, closeServer func() error
	var readerDone chan struct{}
	t.Cleanup(func() {
		if closeServer != nil {
			if err := closeServer(); err != nil {
				t.Errorf("close accepted connection: %v", err)
			}
		}
		if readerDone != nil {
			select {
			case <-readerDone:
			case <-time.After(wait):
				t.Error("accepted reader did not settle during cleanup")
			}
		}
		if closeClient != nil {
			if err := closeClient(); err != nil {
				t.Errorf("close fixture client: %v", err)
			}
		}
		if err := closeListener(); err != nil {
			t.Errorf("close fixture listener: %v", err)
		}
		if !receivedAccept {
			select {
			case outcome := <-accepted:
				if outcome.conn != nil {
					if err := closePrivatePipeFixtureOnce(outcome.conn, wait)(); err != nil {
						t.Errorf("close late accepted connection: %v", err)
					}
				}
			case <-time.After(wait):
				t.Error("fixture accept did not settle during cleanup")
			}
		}
	})

	ctx, cancel := context.WithTimeout(context.Background(), wait)
	defer cancel()
	client, err := DialPipeContext(ctx, name)
	if err != nil {
		t.Fatalf("dial fixture pipe: %v", err)
	}
	closeClient = closePrivatePipeFixtureOnce(client, wait)

	var outcome acceptOutcome
	select {
	case outcome = <-accepted:
		receivedAccept = true
	case <-ctx.Done():
		t.Fatalf("accept fixture connection: %v", ctx.Err())
	}
	if outcome.conn != nil {
		closeServer = closePrivatePipeFixtureOnce(outcome.conn, wait)
	}
	if outcome.err != nil {
		t.Fatalf("accept fixture connection: %v", outcome.err)
	}
	server, serverIsBytePipe := outcome.conn.(*win32Pipe)
	clientPipe, clientIsBytePipe := client.(*win32Pipe)
	if !serverIsBytePipe || !clientIsBytePipe || server == clientPipe || server.win32File == clientPipe.win32File {
		t.Fatalf("expected distinct byte-mode owners, got accepted %T and client %T", outcome.conn, client)
	}

	pid := uint32(os.Getpid())
	var peer uint32
	if err := windows.GetNamedPipeClientProcessId(server.handle, &peer); err != nil || peer != pid {
		t.Fatalf("accepted peer = %d, %v, want fixture process %d", peer, err, pid)
	}
	peer = 0
	if err := windows.GetNamedPipeServerProcessId(clientPipe.handle, &peer); err != nil || peer != pid {
		t.Fatalf("client peer = %d, %v, want fixture process %d", peer, err, pid)
	}
	if err := closeListener(); err != nil {
		t.Fatalf("close listener after first accept: %v", err)
	}

	// Use the same registration, overlapped read, and completion path as
	// win32File.Read. Keeping submission here lets the test prove the exact
	// accepted owner's read was pending before its legacy Close starts.
	owner := server.win32File
	op, err := owner.prepareIO()
	if err != nil {
		t.Fatalf("register accepted owner's read: %v", err)
	}
	buf := make([]byte, 1)
	var transferred uint32
	submitted := windows.ReadFile(owner.handle, buf, &transferred, &op.o)
	if submitted != windows.ERROR_IO_PENDING {
		owner.wg.Done()
		t.Fatalf("submit accepted owner's read: got %v, want ERROR_IO_PENDING", submitted)
	}
	readerDone = make(chan struct{})
	var readBytes int
	var readErr error
	go func() {
		defer close(readerDone)
		defer owner.wg.Done()
		readBytes, readErr = owner.asyncIO(op, &owner.readDeadline, transferred, submitted)
		runtime.KeepAlive(buf)
	}()

	if err := closeServer(); err != nil {
		t.Fatalf("legacy Close on accepted connection: %v", err)
	}
	waitForTestClose(t, readerDone, "original accepted pipe read")
	if readBytes != 0 || !errors.Is(readErr, ErrFileClosed) {
		t.Fatalf("original read = (%d, %v), want (0, ErrFileClosed)", readBytes, readErr)
	}
	original := owner.closeReceipt
	if original == nil {
		t.Fatal("legacy close did not publish an original receipt")
	}
	receipt, err := server.CloseObserved(ctx)
	if err != nil || receipt != original {
		t.Fatalf("observed close = (%p, %v), want original %p with no wait error", receipt, err, original)
	}
	if nativeErr, observed := receipt.NativeResult(); !observed || nativeErr != nil {
		t.Fatalf("native close = (%v, %t), want (nil, true)", nativeErr, observed)
	}
	t.Log("private byte-mode pipe; both peers bound to fixture process; native read pending before legacy Close; original I/O joined; same native close receipt completed")
}

// Each fixture closer owns one native Close and publishes its terminal error
// before closing done. Test cleanup can rejoin the original attempt.
func closePrivatePipeFixtureOnce(closer io.Closer, wait time.Duration) func() error {
	var done chan struct{}
	var closeErr error
	return func() error {
		if done == nil {
			done = make(chan struct{})
			go func() {
				defer close(done)
				closeErr = closer.Close()
			}()
		}
		select {
		case <-done:
			return closeErr
		case <-time.After(wait):
			return errors.New("fixture Close did not settle")
		}
	}
}

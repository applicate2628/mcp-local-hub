package gdb

import (
	"context"
	"errors"
	"io"
	"os"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

type interruptedRead struct {
	testReadCloser
	setDeadline func(time.Time) error
}

func (r interruptedRead) SetReadDeadline(deadline time.Time) error {
	return r.setDeadline(deadline)
}

type interruptedWrite struct {
	nopWriteCloser
	setDeadline func(time.Time) error
}

func (w interruptedWrite) SetWriteDeadline(deadline time.Time) error {
	return w.setDeadline(deadline)
}

func wakeOnExpiredDeadline(wake func()) func(time.Time) error {
	return func(deadline time.Time) error {
		if !deadline.After(time.Now()) {
			wake()
		}
		return nil
	}
}

func TestRunCancellationInterruptsMI(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		serverTransport, clientTransport := mcp.NewInMemoryTransports()
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		gs := newTestServer(t, "")
		beginRead, releaseRead := make(chan struct{}), make(chan struct{})
		release := sync.OnceFunc(func() { close(releaseRead) })
		defer release()
		reader := interruptedRead{testReadCloser{Reader: testReaderFunc(func([]byte) (int, error) {
			close(beginRead)
			<-releaseRead
			return 0, os.ErrDeadlineExceeded
		})}, wakeOnExpiredDeadline(release)}
		s := newSession(nopWriteCloser{io.Discard}, reader, func() error {
			t.Error("Run cancellation forced a pending reader")
			return errors.New("unexpected force")
		})
		go s.reap(func() (*os.ProcessState, error) {
			<-s.closing
			return new(os.ProcessState), nil
		})
		id := gs.trackSession(s)
		runDone := make(chan error, 1)
		go func() { runDone <- gs.run(ctx, serverTransport) }()
		client, err := mcp.NewClient(&mcp.Implementation{Name: "gdb-fixture", Version: "1"}, nil).Connect(context.Background(), clientTransport, nil)
		if err != nil {
			t.Fatal(err)
		}
		type callOutcome struct {
			result *mcp.CallToolResult
			err    error
		}
		callDone := make(chan callOutcome, 1)
		go func() {
			res, err := client.CallTool(context.Background(), &mcp.CallToolParams{
				Name: "gdb_command", Arguments: map[string]any{"session_id": id, "command": "continue"},
			})
			callDone <- callOutcome{res, err}
		}()
		<-beginRead
		cancel()
		synctest.Wait()
		select {
		case err := <-runDone:
			if !errors.Is(err, context.Canceled) {
				t.Errorf("Run result: %v", err)
			}
		default:
			t.Error("SDK Run cancellation is blocked by the active MI read")
			release()
			<-runDone
		}
		// Run closes the transport as well as joining handlers. The SDK may
		// retire the caller before the tool result can be delivered.
		outcome := <-callDone
		if outcome.result != nil {
			if !outcome.result.IsError || !strings.Contains(contentText(t, outcome.result), "context canceled") {
				t.Errorf("interrupted MI call result: %+v", outcome.result)
			}
		} else if !errors.Is(outcome.err, mcp.ErrConnectionClosed) && !errors.Is(outcome.err, io.EOF) {
			t.Errorf("interrupted MI transport outcome: %v", outcome.err)
		}

		if gs.lookup(id) != nil {
			t.Error("Run returned before exact session settlement")
		}
		if err := client.Close(); err != nil {
			t.Error(err)
		}
	})
}

func TestProcessWaitRetainsReaderUntilItsDeadline(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		beginRead, releaseRead, releaseWait := make(chan struct{}), make(chan struct{}), make(chan struct{})
		release := sync.OnceFunc(func() { close(releaseRead) })
		defer release()
		initial := strings.NewReader("~\"before interruption\\n\"\n")
		reader := interruptedRead{testReadCloser{Reader: testReaderFunc(func(p []byte) (int, error) {
			if initial.Len() != 0 {
				return initial.Read(p)
			}
			close(beginRead)
			<-releaseRead
			return copy(p, "29^exit"), os.ErrDeadlineExceeded
		})}, func(deadline time.Time) error {
			if deadline.After(time.Now()) {
				expected, _ := ctx.Deadline()
				if !deadline.Equal(expected) {
					t.Errorf("initial read deadline = %s, want %s", deadline, expected)
				}
				return nil
			}
			release()
			return nil
		}}
		s := newSession(nopWriteCloser{io.Discard}, reader, nil)
		go s.reap(func() (*os.ProcessState, error) {
			<-releaseWait
			return new(os.ProcessState), nil
		})
		commandDone := make(chan struct{})
		go func() {
			defer close(commandDone)
			out, err := s.Command(ctx, "continue")
			if out != "before interruption\n29^exit" || !errors.Is(err, os.ErrDeadlineExceeded) {
				t.Errorf("partial terminal-looking fragment lost its I/O error: %q, %v", out, err)
			}
		}()
		<-beginRead
		close(releaseWait)
		<-s.processDone
		select {
		case <-s.done:
			t.Error("process Wait closed pipes while the MI reader was active")
		default:
		}
		cancel()
		<-commandDone
		<-s.done
	})
}

func TestProcessWaitPublishesWhileGracefulWriteIsBlocked(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		beginWrite, releaseWrite, releaseWait := make(chan struct{}), make(chan struct{}), make(chan struct{})
		release := sync.OnceFunc(func() { close(releaseWrite) })
		defer release()
		writer := interruptedWrite{nopWriteCloser{testWriterFunc(func([]byte) (int, error) {
			close(beginWrite)
			<-releaseWrite
			return 0, os.ErrDeadlineExceeded
		})}, wakeOnExpiredDeadline(release)}
		s := newSession(writer, testReadCloser{Reader: strings.NewReader("")}, func() error {
			t.Error("termination forced after original Wait")
			return errors.New("unexpected force")
		})
		go s.reap(func() (*os.ProcessState, error) {
			<-releaseWait
			return new(os.ProcessState), nil
		})
		result := make(chan error, 1)
		go func() { result <- s.terminate(context.Background(), terminateGrace) }()
		<-beginWrite
		close(releaseWait)
		synctest.Wait()
		if !s.hasWaitResult() {
			t.Error("blocked graceful write prevents publication of original Wait")
		}
		select {
		case <-s.done:
			t.Error("write endpoint closed while graceful write was active")
		default:
		}
		if err := <-result; err != nil {
			t.Errorf("confirmed cleanup reported an error: %v", err)
		}
		<-s.done
		if !errors.Is(s.exitWriteErr, os.ErrDeadlineExceeded) {
			t.Errorf("graceful write error was lost: %v", s.exitWriteErr)
		}
	})
}

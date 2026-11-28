package cli

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

type guiQuitFixtureConn struct {
	net.Conn
	wrote  atomic.Int64
	closed atomic.Bool
}

func (c *guiQuitFixtureConn) Write(p []byte) (int, error) {
	c.wrote.Add(int64(len(p)))
	return c.Conn.Write(p)
}

func (c *guiQuitFixtureConn) Close() error {
	c.closed.Store(true)
	return c.Conn.Close()
}

func TestGUIQuitTransportRequiresVerifiedSameConnectionBeforeWriting(t *testing.T) {
	errUnverified := errors.New("native connection ownership unavailable")
	for _, scenario := range []string{"refused", "canceled", "qualified"} {
		t.Run(scenario, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			raw, server := net.Pipe()
			conn := &guiQuitFixtureConn{Conn: raw}
			defer conn.Close()
			defer server.Close()
			received := make(chan string, 1)
			go func() {
				defer server.Close()
				req, err := http.ReadRequest(bufio.NewReader(server))
				if err != nil {
					received <- ""
					return
				}
				b, err := io.ReadAll(req.Body)
				req.Body.Close()
				if err != nil {
					received <- ""
					return
				}
				received <- req.Method + " " + req.URL.Path + " " + string(b)
				fmt.Fprint(server, "HTTP/1.1 202 Accepted\r\nContent-Length: 2\r\nConnection: close\r\n\r\nok")
			}()
			calls := make(chan net.Conn, 1)
			tr, closeConnection, err := newGUIQuitTransport(ctx, "127.0.0.1:49152",
				func(context.Context, string, string) (net.Conn, error) { return conn, nil },
				func(_ context.Context, got net.Conn) error {
					if conn.wrote.Load() != 0 {
						return errors.New("request bytes preceded peer verification")
					}
					calls <- got
					switch scenario {
					case "refused":
						return errUnverified
					case "canceled":
						cancel()
					}
					return nil
				})
			if tr != nil {
				defer tr.CloseIdleConnections()
				defer closeConnection()
			}
			if err != nil {
				if scenario == "qualified" {
					t.Fatal(err)
				}
				want := errUnverified
				if scenario == "canceled" {
					want = context.Canceled
				}
				if !errors.Is(err, want) || conn.wrote.Load() != 0 || !conn.closed.Load() {
					t.Fatalf("verification must close without writes: err=%v, bytes=%d, closed=%t", err, conn.wrote.Load(), conn.closed.Load())
				}
				return
			}
			if scenario != "qualified" {
				t.Fatal("unverified connection was returned")
			}
			req, err := http.NewRequestWithContext(context.WithoutCancel(ctx), http.MethodPost, "http://127.0.0.1:49152/api/gui/quit", strings.NewReader("fixture"))
			if err != nil {
				t.Fatal(err)
			}
			res, err := tr.RoundTrip(req)
			if res != nil {
				defer res.Body.Close()
			}
			if err != nil {
				t.Fatal(err)
			}
			if res.StatusCode != 202 || <-received != "POST /api/gui/quit fixture" {
				t.Fatal("qualified mock did not receive the ordinary request")
			}
			select {
			case verified := <-calls:
				if verified != conn {
					t.Fatal("different connection verified")
				}
			default:
				t.Fatal("qualified connection was never verified")
			}
		})
	}
}

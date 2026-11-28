package daemon

import (
	"bufio"
	"errors"
	"fmt"
	"io"
)

type stdioStderrMode uint8

const (
	stdioStderrShared stdioStderrMode = iota
	stdioStderrPrivate
)

type stdioStderrObservation struct {
	bytes int64
	eof   bool
	err   error
}

// Select the sink before starting the reader. A private stream never enters
// the log/terminal path and is drained independent of line length or encoding.
func (h *StdioHost) stderrReader(src io.Reader) func() stdioStderrObservation {
	if h.stderrMode == stdioStderrPrivate {
		return func() stdioStderrObservation {
			size, err := io.Copy(io.Discard, src)
			return stdioStderrObservation{bytes: size, eof: err == nil, err: err}
		}
	}
	sink := h.openStderrSink()
	return func() stdioStderrObservation {
		scanner := bufio.NewScanner(src)
		scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
		var writeErr error
		for scanner.Scan() {
			if _, err := fmt.Fprintf(sink, "[subproc stderr] %s\n", scanner.Bytes()); err != nil {
				writeErr = errors.Join(writeErr, err)
			}
		}
		return stdioStderrObservation{eof: scanner.Err() == nil, err: errors.Join(writeErr, scanner.Err())}
	}
}

func (s stdioStderrObservation) logPrivate() {
	fmt.Fprintf(daemonDiagWriter(), "stdio mode=private stage=stderr bytes=%d eof=%t read_failed=%t\n", s.bytes, s.eof, s.err != nil)
}

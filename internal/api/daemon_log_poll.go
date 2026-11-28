package api

import (
	"io"
	"os"
)

// DaemonLogPoll reports the next byte offset and any newly observed log text.
// A lower offset means the file shrank and its current contents were skipped.
type DaemonLogPoll struct {
	Offset int64
	Suffix string
}

// PollDaemonLog reads the appended bytes of a daemon log using one file handle.
// On prime it positions the cursor at the current end without returning text.
// An absent log leaves the cursor unchanged.
func PollDaemonLog(server, daemon string, offset int64, prime bool) (DaemonLogPoll, error) {
	if offset < 0 {
		return DaemonLogPoll{}, os.ErrInvalid
	}
	path := logFilePath(defaultLogDir(), server, daemon)
	file, err := openDaemonLog(path)
	if os.IsNotExist(err) {
		return DaemonLogPoll{Offset: offset}, nil
	}
	if err != nil {
		return DaemonLogPoll{}, err
	}
	defer file.Close() // The snapshot reader also reports open/read errors only.
	return pollOpenedDaemonLog(file, LogPlaceholderFor(server, daemon), offset, prime)
}

type daemonLogReader interface {
	Name() string
	Stat() (os.FileInfo, error)
	Read([]byte) (int, error)
	ReadAt([]byte, int64) (int, error)
}

func pollOpenedDaemonLog(file daemonLogReader, placeholder string, offset int64, prime bool) (DaemonLogPoll, error) {
	info, err := file.Stat()
	if err != nil {
		return DaemonLogPoll{}, err
	}
	if !info.Mode().IsRegular() {
		if info.IsDir() {
			// Read the opened directory to get the same native read error
			// as LogsGetFrom, which is shown verbatim in the error event.
			var probe [1]byte
			if _, err := file.Read(probe[:]); err != nil && err != io.EOF {
				return DaemonLogPoll{}, err
			}
		}
		return DaemonLogPoll{}, &os.PathError{Op: "read", Path: file.Name(), Err: os.ErrInvalid}
	}

	end := info.Size()
	if end == int64(len(placeholder)) {
		return pollDaemonLogSentinel(file, placeholder, offset, prime)
	}
	if prime || end <= offset {
		return DaemonLogPoll{Offset: end}, nil
	}

	data := make([]byte, end-offset)
	n, err := file.ReadAt(data, offset)
	if err != nil && err != io.EOF {
		return DaemonLogPoll{}, err
	}
	if n > 0 {
		return DaemonLogPoll{Offset: offset + int64(n), Suffix: string(data[:n])}, nil
	}

	// A concurrent truncate can leave us at EOF without reading anything.
	// Restat the same file and only move the cursor back on a proved shrink.
	info, err = file.Stat()
	if err != nil {
		return DaemonLogPoll{}, err
	}
	end = info.Size()
	if end >= offset {
		return DaemonLogPoll{Offset: offset}, nil
	}
	if end == int64(len(placeholder)) {
		return pollDaemonLogSentinel(file, placeholder, offset, false)
	}
	return DaemonLogPoll{Offset: end}, nil
}

// The absent-log text was also a sentinel when found verbatim in a real file.
// Only a file of this exact sampled length needs a bounded prefix read.
func pollDaemonLogSentinel(file daemonLogReader, placeholder string, offset int64, prime bool) (DaemonLogPoll, error) {
	data := make([]byte, len(placeholder))
	n, err := file.ReadAt(data, 0)
	if err != nil && err != io.EOF {
		return DaemonLogPoll{}, err
	}
	if n == 0 {
		info, err := file.Stat()
		if err != nil {
			return DaemonLogPoll{}, err
		}
		end := info.Size()
		if end < offset && end != int64(len(placeholder)) {
			return DaemonLogPoll{Offset: end}, nil
		}
		return DaemonLogPoll{Offset: offset}, nil
	}
	if string(data[:n]) == placeholder {
		return DaemonLogPoll{Offset: offset}, nil
	}
	poll := DaemonLogPoll{Offset: int64(n)}
	if !prime && poll.Offset > offset {
		poll.Suffix = string(data[offset:n])
	}
	return poll, nil
}

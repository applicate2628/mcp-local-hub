package main

import "io"

type checkedWriter struct {
	destination io.Writer
	err         error
}

func (w *checkedWriter) Write(p []byte) (int, error) {
	if w == nil {
		return 0, io.ErrClosedPipe
	}
	if w.err != nil {
		return 0, w.err
	}
	if w.destination == nil {
		w.err = io.ErrClosedPipe
		return 0, w.err
	}
	n, err := w.destination.Write(p)
	if err == nil && n != len(p) {
		err = io.ErrShortWrite
	}
	if err != nil {
		w.err = err
	}
	return n, err
}

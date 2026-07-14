package main

import (
	"syscall"
	"testing"
)

func TestShutdownSignalsIncludeSIGTERM(t *testing.T) {
	for _, sig := range shutdownSignals() {
		if sig == syscall.SIGTERM {
			return
		}
	}
	t.Fatal("shutdown signal set does not include SIGTERM")
}

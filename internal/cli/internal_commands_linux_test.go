//go:build linux

package cli

import (
	"bytes"
	"strconv"
	"strings"
	"syscall"
	"testing"

	processinternal "mcp-local-hub/internal/process"
)

func TestLinuxProcfsClassifierHelperCommandIsHiddenStrictAndBounded(t *testing.T) {
	for _, args := range [][]string{
		nil,
		{""},
		{"0"},
		{"-1"},
		{"+1"},
		{"01"},
		{" 1"},
		{"1 "},
		{"one"},
		{"1", "2"},
	} {
		command := newLinuxProcfsClassifierHelperCmd()
		if err := command.Args(command, args); err == nil {
			t.Fatalf("args=%q accepted", args)
		}
	}

	command := newLinuxProcfsClassifierHelperCmd()
	if !command.Hidden || command.IsAvailableCommand() {
		t.Fatalf("hidden/available=%v/%v, want true/false", command.Hidden, command.IsAvailableCommand())
	}
	var output bytes.Buffer
	command.SetOut(&output)
	command.SetArgs([]string{strconv.Itoa(syscall.Getpgrp())})
	if err := command.Execute(); err != nil {
		t.Fatal(err)
	}
	frame := output.String()
	if len(frame) > 128 || !strings.HasPrefix(frame, "mcphub-linux-procfs-v1:") || strings.Count(frame, "\n") != 1 {
		t.Fatalf("frame length/content=%d/%q", len(frame), frame)
	}
	payload := strings.TrimSuffix(strings.TrimPrefix(frame, "mcphub-linux-procfs-v1:"), "\n")
	if payload != "settled" && payload != "live" && !strings.HasPrefix(payload, "failure:") {
		t.Fatalf("unexpected fixed-vocabulary frame %q", frame)
	}

	root := NewRootCmd()
	var help bytes.Buffer
	root.SetOut(&help)
	root.SetArgs([]string{"--help"})
	if err := root.Execute(); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(help.String(), processinternal.LinuxProcfsClassifierHelperCommand) {
		t.Fatalf("hidden helper leaked into root help:\n%s", help.String())
	}
}

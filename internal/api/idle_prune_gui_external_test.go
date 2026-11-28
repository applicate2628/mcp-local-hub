package api_test

import (
	"bytes"
	"context"
	"mcp-local-hub/internal/api"
	"mcp-local-hub/internal/gui"
	"reflect"
	"testing"
	"time"
)

func TestAutomaticPruneGUIRealPathPreservesWorkspaceIntent(t *testing.T) {
	f := api.NewPruneFixtureForExternalTest(t)
	beforeRegistry, beforeIntent := f.BytesForExternalTest(t)
	beforeParsed, err := api.ReadSupervisorIntent(f.IntentPath)
	if err != nil {
		t.Fatal(err)
	}
	beforeRows := api.NewRegistry(f.RegistryPath)
	if err := beforeRows.Load(); err != nil {
		t.Fatal(err)
	}
	s := gui.NewServer(gui.Config{})
	s.Broadcaster().DisableGUIEventLog = true
	t.Cleanup(s.Broadcaster().Close)
	got := s.SweepPruneWorkspaces(context.Background(), time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC))
	afterRegistry, afterIntent := f.BytesForExternalTest(t)
	afterParsed, err := api.ReadSupervisorIntent(f.IntentPath)
	if err != nil {
		t.Fatal(err)
	}
	afterRows := api.NewRegistry(f.RegistryPath)
	if err := afterRows.Load(); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(beforeParsed, afterParsed) || !reflect.DeepEqual(beforeRows.Workspaces, afterRows.Workspaces) {
		t.Error("parsed registry or intent changed")
	}
	if !bytes.Equal(beforeRegistry, afterRegistry) || !bytes.Equal(beforeIntent, afterIntent) {
		t.Error("automatic GUI path changed registry or saved intent")
	}
	if n := f.DestructiveEffectsForExternalTest(); n != 0 {
		t.Errorf("entered destructive effects: %d", n)
	}
	if got != 0 {
		t.Errorf("retained workspace counted as removed: %d", got)
	}
	marker, err := api.ReadDefaultWorkspace(f.StateDir)
	if err != nil || marker != f.Workspace {
		t.Errorf("default marker=%q err=%v", marker, err)
	}
	retained, pruned := 0, 0
	for _, event := range f.EventsForExternalTest(t) {
		if event["workspace"] != f.Workspace {
			continue
		}
		switch event["event"] {
		case "workspace-prune-retained":
			retained++
		case "workspace-pruned":
			pruned++
		}
	}
	if retained != 1 || pruned != 0 {
		t.Errorf("workspace events: retained=%d pruned=%d", retained, pruned)
	}
}

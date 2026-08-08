package cli

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestNewRootCmd_EveryVisibleCommandHasARegisteredGroup enforces the
// invariant root.go asserts in prose: every command cobra will actually
// RENDER must belong to a registered group.
//
// The prose reasoning is that hidden commands need no GroupID because
// cobra never renders an unavailable command. That is true today and it is
// exactly why it needs a probe: the property depends on a `Hidden: true`
// somewhere else in the package, so un-hiding any command — the one edit
// most likely to happen, since hiding decisions get revisited — silently
// drops it into cobra's generic "Additional Commands" catch-all. That
// catch-all is the undifferentiated listing the grouping exists to remove,
// and nothing else in the build would complain.
//
// Falsifiable by construction: delete a `Hidden: true` (or a group
// assignment) and this fails naming the offender.
func TestNewRootCmd_EveryVisibleCommandHasARegisteredGroup(t *testing.T) {
	root := NewRootCmd()

	registered := make(map[string]bool)
	for _, g := range root.Groups() {
		registered[g.ID] = true
	}
	if len(registered) == 0 {
		t.Fatal("no command groups registered on the root command; the grouping this test guards is gone")
	}

	visible := 0
	for _, c := range root.Commands() {
		if c.Hidden {
			continue
		}
		visible++
		switch {
		case c.GroupID == "":
			t.Errorf("visible command %q has no GroupID; cobra will render it under the generic "+
				"\"Additional Commands\" heading that the grouping exists to remove. Either assign a "+
				"group in NewRootCmd's addGrouped calls, or keep the command Hidden.", c.Name())
		case !registered[c.GroupID]:
			t.Errorf("visible command %q has GroupID %q, which is not registered via root.AddGroup; "+
				"cobra panics on an unknown GroupID at AddCommand time and would otherwise render it "+
				"under \"Additional Commands\"", c.Name(), c.GroupID)
		}
	}

	// Guard the guard: a NewRootCmd that stopped attaching commands (or a
	// future refactor that moves them behind a builder) would make every
	// assertion above vacuous and this test would pass while checking
	// nothing. The floor is deliberately far below the real count so it
	// tracks "commands are still being registered", not the exact listing.
	if visible < 10 {
		t.Fatalf("only %d visible commands found; expected the full operator listing, so this test "+
			"is no longer exercising the invariant it claims to guard", visible)
	}
}

func TestNewRootCmd_GUIOwnerUnknownConfirmationWorkerIsHiddenAndCallable(t *testing.T) {
	root := NewRootCmd()
	const name = "gui-owner-unknown-confirmation-worker"
	worker, _, err := root.Find([]string{name})
	if err != nil || worker == nil || worker.Name() != name {
		t.Fatalf("find hidden worker: command=%v err=%v", worker, err)
	}
	if !worker.Hidden || worker.RunE == nil {
		t.Fatalf("worker hidden=%v runE=%v", worker.Hidden, worker.RunE != nil)
	}
	var help bytes.Buffer
	root.SetOut(&help)
	if err := root.Help(); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(help.String(), name) {
		t.Fatalf("hidden worker leaked into root help: %s", help.String())
	}
	for _, command := range root.Commands() {
		if !command.Hidden && command.Name() == name {
			t.Fatal("hidden worker leaked into visible operator commands")
		}
	}

	stateDir := ensureAliveTestStateDir(t)
	observedAt := time.Now().UTC().Truncate(time.Microsecond)
	payload, err := json.Marshal(guiOwnerUnknownConfirmationWorkerRequest{Version: 1, StateDir: stateDir, ObservedAt: observedAt})
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	worker.SetIn(bytes.NewReader(payload))
	worker.SetOut(&out)
	if err := worker.RunE(worker, nil); err != nil {
		t.Fatal(err)
	}
	if raw, err := os.ReadFile(filepath.Join(stateDir, guiOwnerUnknownConfirmationFileLeaf)); err != nil || string(raw) != observedAt.Format(time.RFC3339Nano) {
		t.Fatalf("callable worker marker=%q err=%v", raw, err)
	}
}

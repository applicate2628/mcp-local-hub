package cli

import "testing"

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

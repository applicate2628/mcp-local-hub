package config

import (
	"reflect"
	"testing"
)

// TestExpandWorkspacePathTokens_SubstringMatch covers the canonical
// happy path: a composite arg with the token embedded inside, plus a
// standalone token entry, both correctly expanded to the workspace
// path. Plan §D.2.
func TestExpandWorkspacePathTokens_SubstringMatch(t *testing.T) {
	in := []string{"--project=${workspace.path}/src", "${workspace.path}"}
	want := []string{"--project=C:/work/alpha/src", "C:/work/alpha"}
	got := ExpandWorkspacePathTokens(in, "C:/work/alpha")
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %#v, want %#v", got, want)
	}
}

// TestExpandWorkspacePathTokens_NoTokenNoOp covers args that do not
// reference the token: every element is returned verbatim, length and
// order preserved. Plan §D.2.
func TestExpandWorkspacePathTokens_NoTokenNoOp(t *testing.T) {
	in := []string{"--context", "codex", "--readonly"}
	got := ExpandWorkspacePathTokens(in, "C:/work/alpha")
	want := []string{"--context", "codex", "--readonly"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %#v, want %#v", got, want)
	}
	// Verify the input slice is not mutated.
	if &in[0] == &got[0] {
		t.Fatalf("got slice aliases input: ExpandWorkspacePathTokens must return a fresh slice")
	}
}

// TestExpandWorkspacePathTokens_MultipleOccurrencesPerArg covers the
// `${workspace.path}/a:${workspace.path}/b` case from the plan test
// contract — both occurrences within a single arg are replaced. Plan
// §D.2.
func TestExpandWorkspacePathTokens_MultipleOccurrencesPerArg(t *testing.T) {
	in := []string{"${workspace.path}/a:${workspace.path}/b"}
	got := ExpandWorkspacePathTokens(in, "/abs/ws")
	want := []string{"/abs/ws/a:/abs/ws/b"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %#v, want %#v", got, want)
	}
}

// TestExpandWorkspacePathTokens_NilIn returns nil verbatim — caller
// passes a nil args slice on a daemon_template with an empty
// extra_args_template (impossible per validator D.1, but the helper
// is defensive).
func TestExpandWorkspacePathTokens_NilIn(t *testing.T) {
	got := ExpandWorkspacePathTokens(nil, "C:/work/alpha")
	if got != nil {
		t.Fatalf("got %#v, want nil", got)
	}
}

// TestExpandWorkspacePathTokens_EmptySliceIn returns a fresh empty
// slice (length 0, not nil) so downstream callers do not have to
// special-case nil vs empty.
func TestExpandWorkspacePathTokens_EmptySliceIn(t *testing.T) {
	in := []string{}
	got := ExpandWorkspacePathTokens(in, "C:/work/alpha")
	if got == nil {
		t.Fatalf("got nil, want empty slice")
	}
	if len(got) != 0 {
		t.Fatalf("got len=%d, want 0", len(got))
	}
}

// TestExpandWorkspacePathTokens_DoesNotMutateInput ensures the input
// slice's element values remain untouched after expansion. Phase D.2
// fan-out reuses the manifest's template slice across N workspaces;
// the helper must NOT alias or in-place mutate.
func TestExpandWorkspacePathTokens_DoesNotMutateInput(t *testing.T) {
	in := []string{"--project=${workspace.path}/src"}
	_ = ExpandWorkspacePathTokens(in, "C:/work/alpha")
	if in[0] != "--project=${workspace.path}/src" {
		t.Fatalf("input mutated: in[0]=%q, want unchanged template", in[0])
	}
}

package patchesapply

import "testing"

func TestR19ExpandedUnquotedOperandIsReclassified(t *testing.T) {
	env := &varEnv{values: map[string]serializedValue{}}
	env.setValue("MODE", serializedValue{text: "static"})
	got, unresolved := evalCondition("${MODE}", env)
	if got == TriTrue {
		t.Fatalf("expanded unquoted identifier evaluated true; unresolved=%v", unresolved)
	}
}

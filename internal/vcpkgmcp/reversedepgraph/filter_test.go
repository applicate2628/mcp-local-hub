package reversedepgraph

import (
	"reflect"
	"testing"
)

func TestDeclaredFilterIsSuperset(t *testing.T) {
	manifest := []byte(`{
  "name": "consumer",
  "version-string": "1",
  "dependencies": ["core-dep", {"name":"windows-dep", "platform":"windows"}],
  "features": {
    "tls": {"description":"tls", "dependencies":[{"name":"feature-dep", "default-features":false}]},
    "tools": {"description":"tools", "dependencies":[{"name":"host-dep", "host":true}]}
  }
}`)
	deps, inspectable := ScanDeclaredSuperset(manifest)
	want := []string{"core-dep", "feature-dep", "host-dep", "windows-dep"}
	if !inspectable || !reflect.DeepEqual(deps, want) {
		t.Fatalf("ScanDeclaredSuperset = %#v/%v, want %#v/true", deps, inspectable, want)
	}
	if _, inspectable := ScanDeclaredSuperset([]byte(`{"dependencies":"not-an-array"}`)); inspectable {
		t.Fatal("unrecognized shape was pruned instead of remaining potential")
	}
}

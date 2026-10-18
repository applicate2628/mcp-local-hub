package config

import (
	"path/filepath"
	"strings"
	"testing"
)

func TestParseManifestWithEnvironmentUsesCapturedLookup(t *testing.T) {
	cwd := t.TempDir()
	environment := map[string]string{"MCPHUB_CAPTURED_ARGUMENT": "captured", "MCPHUB_CAPTURED_CWD": filepath.Clean(cwd)}
	lookup := func(name string) (string, bool) { value, present := environment[name]; return value, present }
	raw := `name: captured
kind: global
transport: stdio-bridge
command: synthetic-command
base_args: ["${MCPHUB_CAPTURED_ARGUMENT}"]
env:
  CAPTURED: "${MCPHUB_CAPTURED_ARGUMENT}"
  REFERENCE: "${env:MCPHUB_CAPTURED_ARGUMENT}"
daemons:
  - name: default
    port: 9999
    cwd: "${MCPHUB_CAPTURED_CWD}"
`
	m, err := ParseManifestWithEnvironment(strings.NewReader(raw), lookup)
	if err != nil {
		t.Fatal(err)
	}
	if m.BaseArgs[0] != "captured" || m.Env["CAPTURED"] != "captured" || m.Env["REFERENCE"] != "${env:MCPHUB_CAPTURED_ARGUMENT}" || m.Daemons[0].Cwd != cwd {
		t.Fatal("captured expansion changed argument/environment/reference/cwd semantics")
	}
	delete(environment, "MCPHUB_CAPTURED_ARGUMENT")
	if _, err := ParseManifestWithEnvironment(strings.NewReader(raw), lookup); err == nil {
		t.Fatal("missing captured variable was accepted")
	}
	if _, err := ParseManifestWithEnvironment(strings.NewReader(raw), nil); err == nil {
		t.Fatal("missing captured lookup was accepted")
	}
}

package cli

import (
	"bytes"
	"testing"

	"mcp-local-hub/internal/api"
)

type fakeProfileUpdateCLIAPI struct {
	plan     *api.AdoptProfileUpdatePlan
	planErr  error
	execErr  error
	executed bool
}

func (f *fakeProfileUpdateCLIAPI) BuildAdoptProfileUpdatePlan(string, string) (*api.AdoptProfileUpdatePlan, error) {
	return f.plan, f.planErr
}
func (f *fakeProfileUpdateCLIAPI) ExecuteAdoptProfileUpdate(*api.AdoptProfileUpdatePlan, api.AdoptProfileUpdateOpts) error {
	f.executed = true
	return f.execErr
}

func TestAdoptProfileUpdateCLIDryRunAndYes(t *testing.T) {
	fake := &fakeProfileUpdateCLIAPI{plan: &api.AdoptProfileUpdatePlan{ManifestName: "codegraph", Profile: "stdio-http-legacy-2024-11-05", OldManifestHash: "old", NewManifestHash: "new", RestartRequired: true}}
	prior := newProfileUpdateCLIAPI
	newProfileUpdateCLIAPI = func() profileUpdateCLIAPI { return fake }
	t.Cleanup(func() { newProfileUpdateCLIAPI = prior })
	root := NewRootCmd()
	var out bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&out)
	root.SetArgs([]string{"adopt-provenance", "update-profile", "codegraph", "--mcp-protocol-compatibility-profile", "stdio-http-legacy-2024-11-05"})
	if err := root.Execute(); err != nil {
		t.Fatal(err)
	}
	if fake.executed || !bytes.Contains(out.Bytes(), []byte("dry_run=true")) {
		t.Fatalf("dry run=%q executed=%t", out.String(), fake.executed)
	}
	root = NewRootCmd()
	out.Reset()
	root.SetOut(&out)
	root.SetErr(&out)
	root.SetArgs([]string{"adopt-provenance", "update-profile", "codegraph", "--mcp-protocol-compatibility-profile", "stdio-http-legacy-2024-11-05", "--yes"})
	if err := root.Execute(); err != nil {
		t.Fatal(err)
	}
	if !fake.executed {
		t.Fatal("--yes did not execute")
	}
}

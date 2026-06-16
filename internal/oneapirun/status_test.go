package oneapirun

import (
	"encoding/json"
	"reflect"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// newStatusTestServer builds an OneAPIRunServer with the three detection seams
// stubbed to fixed values, so the read-only status probe is exercised without
// the real ~1-3s setvars subprocess or a real oneAPI install. Mirrors
// newTestServer but also wires the detectRoot seam the status tool reads.
func newStatusTestServer(root string, rootOK bool, vsOK bool, dllDirs []string) *OneAPIRunServer {
	return &OneAPIRunServer{
		captureVSEnv:  func() ([]string, bool) { return nil, vsOK },
		oneAPIDLLDirs: func() []string { return dllDirs },
		detectRoot:    func() (string, bool) { return root, rootOK },
	}
}

// decodeStatusResult extracts the structured statusResult JSON from a tool
// result's TextContent, failing the test on any shape mismatch.
func decodeStatusResult(t *testing.T, result *mcp.CallToolResult) statusResult {
	t.Helper()
	if result.IsError {
		text := ""
		if len(result.Content) > 0 {
			if tc, ok := result.Content[0].(*mcp.TextContent); ok {
				text = tc.Text
			}
		}
		t.Fatalf("status tool returned IsError=true: %s", text)
	}
	if len(result.Content) == 0 {
		t.Fatal("empty Content")
	}
	tc, ok := result.Content[0].(*mcp.TextContent)
	if !ok {
		t.Fatalf("Content[0] is not TextContent: %T", result.Content[0])
	}
	var res statusResult
	if err := json.Unmarshal([]byte(tc.Text), &res); err != nil {
		t.Fatalf("result is not valid statusResult JSON: %v\nbody: %s", err, tc.Text)
	}
	return res
}

// emptyStatusArgs is a no-property request body (oneapi_env_status takes no
// arguments).
func emptyStatusArgs(t *testing.T) *mcp.CallToolRequest {
	t.Helper()
	raw, _ := json.Marshal(map[string]any{})
	return (&mockCallToolRequest{Arguments: raw}).toReal()
}

// ---------------------------------------------------------------------------
// oneapi_env_status — env_source labelling + presence + components.
// ---------------------------------------------------------------------------

func TestOneAPIEnvStatus_SetvarsSourceWhenCapturable(t *testing.T) {
	// setvars captured → env_source "setvars", oneapi_present true. Root and
	// components reported from the detection seams.
	root := `C:\Program Files (x86)\Intel\oneAPI`
	dllDirs := []string{
		root + `\mkl\latest\bin`,
		root + `\tbb\latest\bin`,
		root + `\compiler\latest\bin`,
	}
	rs := newStatusTestServer(root, true /*rootOK*/, true /*vsOK*/, dllDirs)

	result, err := rs.oneAPIEnvStatusTool(t.Context(), emptyStatusArgs(t))
	if err != nil {
		t.Fatalf("oneAPIEnvStatusTool: %v", err)
	}
	res := decodeStatusResult(t, result)

	if res.EnvSource != envSourceSetvars {
		t.Errorf("env_source = %q, want %q", res.EnvSource, envSourceSetvars)
	}
	if !res.OneAPIPresent {
		t.Error("oneapi_present = false, want true when setvars capturable")
	}
	if res.Root != root {
		t.Errorf("root = %q, want %q", res.Root, root)
	}
	want := []string{"mkl", "tbb", "compiler"}
	if !reflect.DeepEqual(res.Components, want) {
		t.Errorf("components = %v, want %v", res.Components, want)
	}
}

func TestOneAPIEnvStatus_OneAPIOnlyWhenSetvarsMissingButDirsPresent(t *testing.T) {
	// setvars NOT capturable, but DLL dirs found → "oneapi-only", still present.
	root := `C:\oneAPI`
	dllDirs := []string{root + `\mkl\latest\bin`}
	rs := newStatusTestServer(root, true, false /*vsOK*/, dllDirs)

	result, err := rs.oneAPIEnvStatusTool(t.Context(), emptyStatusArgs(t))
	if err != nil {
		t.Fatalf("oneAPIEnvStatusTool: %v", err)
	}
	res := decodeStatusResult(t, result)

	if res.EnvSource != envSourceOneAPIOnly {
		t.Errorf("env_source = %q, want %q", res.EnvSource, envSourceOneAPIOnly)
	}
	if !res.OneAPIPresent {
		t.Error("oneapi_present = false, want true when DLL dirs present")
	}
	if want := []string{"mkl"}; !reflect.DeepEqual(res.Components, want) {
		t.Errorf("components = %v, want %v", res.Components, want)
	}
}

func TestOneAPIEnvStatus_PlainWhenNothingPresent(t *testing.T) {
	// Neither setvars nor DLL dirs nor a root → "plain", not present, no
	// components, empty root.
	rs := newStatusTestServer("" /*root*/, false /*rootOK*/, false /*vsOK*/, nil)

	result, err := rs.oneAPIEnvStatusTool(t.Context(), emptyStatusArgs(t))
	if err != nil {
		t.Fatalf("oneAPIEnvStatusTool: %v", err)
	}
	res := decodeStatusResult(t, result)

	if res.EnvSource != envSourcePlain {
		t.Errorf("env_source = %q, want %q", res.EnvSource, envSourcePlain)
	}
	if res.OneAPIPresent {
		t.Error("oneapi_present = true, want false when nothing present")
	}
	if res.Root != "" {
		t.Errorf("root = %q, want empty", res.Root)
	}
	if res.Components != nil {
		t.Errorf("components = %v, want nil", res.Components)
	}
}

func TestOneAPIEnvStatus_RootReportedEvenInOneAPIOnly(t *testing.T) {
	// Operator visibility: a root detected via DetectRoot is reported even when
	// setvars itself could not be captured (env_source "oneapi-only").
	root := `D:\Intel\oneAPI`
	rs := newStatusTestServer(root, true, false, []string{root + `\tbb\latest\bin`})

	result, err := rs.oneAPIEnvStatusTool(t.Context(), emptyStatusArgs(t))
	if err != nil {
		t.Fatalf("oneAPIEnvStatusTool: %v", err)
	}
	res := decodeStatusResult(t, result)
	if res.Root != root {
		t.Errorf("root = %q, want %q", res.Root, root)
	}
}

func TestOneAPIEnvStatus_ComponentsOmittedWhenEmpty(t *testing.T) {
	// With a root but no DLL dirs, the JSON must omit `components` entirely
	// (omitempty) so a consumer can distinguish "no components" cleanly.
	rs := newStatusTestServer(`C:\oneAPI`, true, true, nil)

	result, err := rs.oneAPIEnvStatusTool(t.Context(), emptyStatusArgs(t))
	if err != nil {
		t.Fatalf("oneAPIEnvStatusTool: %v", err)
	}
	if len(result.Content) == 0 {
		t.Fatal("empty Content")
	}
	tc := result.Content[0].(*mcp.TextContent)
	var raw map[string]any
	if err := json.Unmarshal([]byte(tc.Text), &raw); err != nil {
		t.Fatalf("bad JSON: %v", err)
	}
	if _, has := raw["components"]; has {
		t.Errorf("components key present despite no DLL dirs: %s", tc.Text)
	}
}

func TestOneAPIEnvStatus_PanicRecoveredToStructuredJSON(t *testing.T) {
	// A panic from a detection seam must be recovered into a structured,
	// non-empty statusResult JSON (never a crash / empty result).
	rs := &OneAPIRunServer{
		captureVSEnv:  func() ([]string, bool) { return nil, false },
		oneAPIDLLDirs: func() []string { return nil },
		detectRoot:    func() (string, bool) { panic("simulated detect fault") },
	}
	result, err := rs.oneAPIEnvStatusTool(t.Context(), emptyStatusArgs(t))
	if err != nil {
		t.Fatalf("handler returned a Go error instead of recovering: %v", err)
	}
	if result == nil {
		t.Fatal("handler returned nil result on panic")
	}
	res := decodeStatusResult(t, result)
	if res.EnvSource != envSourcePlain {
		t.Errorf("recovered env_source = %q, want %q", res.EnvSource, envSourcePlain)
	}
	if res.OneAPIPresent {
		t.Error("recovered oneapi_present = true, want false")
	}
}

// TestRegisterTools_StatusToolRegisteredWithoutUnsafeOptIn asserts the
// read-only status probe is registered even when the unsafe run gate is OFF:
// registerTools must call registerStatusTool BEFORE its early return. A
// successful run also proves the status tool's InputSchema is well-formed
// (mcp.Server.AddTool panics on a malformed schema), so the safe consistency
// probe is always exposed on a daemon that withholds the arbitrary-command tool.
func TestRegisterTools_StatusToolRegisteredWithoutUnsafeOptIn(t *testing.T) {
	t.Setenv(enableUnsafeOneAPIRunEnv, "") // unsafe run tool withheld

	rs := newStatusTestServer(`C:\oneAPI`, true, true, []string{`C:\oneAPI\mkl\latest\bin`})
	rs.server = mcp.NewServer(&mcp.Implementation{Name: "oneapi-run", Version: "test"}, nil)

	// Must not panic — the status tool registers (valid schema) ahead of the
	// gate; the run tool is silently withheld because the opt-in is absent.
	registerTools(rs)
}

// ---------------------------------------------------------------------------
// envSourceFor — the shared three-way precedence owner.
// ---------------------------------------------------------------------------

func TestEnvSourceFor(t *testing.T) {
	cases := []struct {
		vs, dirs bool
		want     string
	}{
		{true, true, envSourceSetvars},     // setvars wins even with dirs
		{true, false, envSourceSetvars},    // setvars alone
		{false, true, envSourceOneAPIOnly}, // dirs only
		{false, false, envSourcePlain},     // neither
	}
	for _, c := range cases {
		if got := envSourceFor(c.vs, c.dirs); got != c.want {
			t.Errorf("envSourceFor(vs=%v, dirs=%v) = %q, want %q", c.vs, c.dirs, got, c.want)
		}
	}
}

// ---------------------------------------------------------------------------
// componentsFromDLLDirs / componentOf — component-name derivation.
// ---------------------------------------------------------------------------

func TestComponentsFromDLLDirs_WindowsAndPosixLayouts(t *testing.T) {
	in := []string{
		`C:\Program Files (x86)\Intel\oneAPI\mkl\latest\bin`,
		`C:\Program Files (x86)\Intel\oneAPI\tbb\latest\bin`,
		`/opt/intel/oneapi/compiler/latest/bin`,
	}
	got := componentsFromDLLDirs(in)
	want := []string{"mkl", "tbb", "compiler"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("componentsFromDLLDirs = %v, want %v", got, want)
	}
}

func TestComponentsFromDLLDirs_EmptyIsNil(t *testing.T) {
	if got := componentsFromDLLDirs(nil); got != nil {
		t.Errorf("componentsFromDLLDirs(nil) = %v, want nil", got)
	}
}

func TestComponentsFromDLLDirs_SkipsUnexpectedShape(t *testing.T) {
	// A path that does not end in "<component>/latest/bin" is skipped (no
	// guessed component) rather than emitting garbage.
	in := []string{
		`C:\oneAPI\mkl\latest\bin`, // good
		`C:\weird\path`,            // too short / no "latest" tail → skipped
		`/x/y/notlatest/bin`,       // second-to-last != "latest" → skipped
	}
	got := componentsFromDLLDirs(in)
	want := []string{"mkl"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("componentsFromDLLDirs = %v, want %v (unexpected shapes skipped)", got, want)
	}
}

func TestComponentOf(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{`C:\oneAPI\mkl\latest\bin`, "mkl"},
		{`/opt/intel/oneapi/tbb/latest/bin`, "tbb"},
		{`C:\oneAPI\compiler\LATEST\bin`, "compiler"}, // "latest" matched case-insensitively
		{`too\short`, ""},
		{`/a/b/notlatest/bin`, ""},
	}
	for _, c := range cases {
		if got := componentOf(c.in); got != c.want {
			t.Errorf("componentOf(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

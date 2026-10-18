package clients

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
)

func TestConfiguredStdioViewRefusesUnissuedAndHTTP(t *testing.T) {
	for _, profile := range []*CodexConfiguredProjectProfile{nil, {}, {owner: &lockingClient{}}} {
		if _, err := profile.ConfiguredStdioLaunch(); err == nil {
			t.Fatal("unissued source yielded launch view")
		}
	}
	configured, err := decodeCodexConfiguredMCP([]byte(configuredHTTPFixture), "graphify")
	if err != nil {
		t.Fatal(err)
	}
	profile := &CodexConfiguredProjectProfile{owner: &lockingClient{}, fingerprint: "synthetic-unit-profile", configured: configured}
	if _, err := profile.ConfiguredStdioLaunch(); err == nil {
		t.Fatal("HTTP yielded STDIO view")
	}
}

func TestConfiguredStdioViewPreservesAndClonesObservation(t *testing.T) {
	raw := strings.Replace(configuredStdioFixture, `"args":[]`, `"args":["--graph","relative.graph"]`, 1)
	raw = strings.Replace(raw, `"env":null`, `"env":{"INNOCUOUS":"synthetic-private-sentinel"}`, 1)
	raw = strings.Replace(raw, `"env_vars":[]`, `"env_vars":["LOCAL",{"name":"LOCAL_OBJECT","source":"local"},{"name":"REMOTE_OBJECT","source":"remote"}]`, 1)
	raw = strings.Replace(raw, `"cwd":null`, `"cwd":""`, 1)
	configured, err := decodeCodexConfiguredMCP([]byte(raw), "graphify")
	if err != nil {
		t.Fatal(err)
	}
	profile := &CodexConfiguredProjectProfile{owner: &lockingClient{}, fingerprint: "synthetic-unit-profile", configured: configured, payload: []byte(raw)}
	view, err := profile.ConfiguredStdioLaunch()
	if err != nil {
		t.Fatal(err)
	}
	args := view.Args()
	args[0] = "changed"
	env, isNull := view.Environment()
	if isNull || env["INNOCUOUS"] != "synthetic-private-sentinel" {
		t.Fatal("inline value/null distinction lost")
	}
	env["INNOCUOUS"] = "changed"
	cwd := view.ConfiguredCWD()
	if cwd == nil || *cwd != "" {
		t.Fatal("explicit empty cwd lost")
	}
	*cwd = "changed"
	refs := view.EnvironmentReferences()
	if len(refs) != 3 || !refs[0].LegacyString() || refs[0].Source() != "local" || refs[1].LegacyString() || refs[2].Source() != "remote" {
		t.Fatal("native reference distinction lost")
	}
	refs[0] = CodexConfiguredEnvironmentReference{}
	envAgain, _ := view.Environment()
	if view.Args()[0] != "--graph" || envAgain["INNOCUOUS"] != "synthetic-private-sentinel" || *view.ConfiguredCWD() != "" || view.EnvironmentReferences()[0].Name() != "LOCAL" || string(profile.payload) != raw || string(profile.configured.fields["startup_timeout_sec"]) != "30.0" {
		t.Fatal("getter clone mutated original observation")
	}
	privateCause := errors.New("synthetic-private-cause")
	profile.projectRead.PrimaryError = privateCause
	for _, value := range []any{view, profile} {
		serialized, err := json.Marshal(value)
		if err != nil || string(serialized) != "{}" {
			t.Fatal("private source serialized")
		}
	}
	for _, row := range []struct {
		marker string
		values []any
	}{
		{"configured STDIO observation (private)", []any{view, *view, CodexConfiguredStdioLaunch{}}},
		{"configured project observation (private)", []any{profile, *profile, CodexConfiguredProjectProfile{}}},
	} {
		for _, value := range row.values {
			for _, verb := range []string{"%v", "%+v", "%#v", "%s"} {
				if fmt.Sprintf(verb, value) != row.marker {
					t.Fatal("whole pointer/value emitted a reversible private representation")
				}
			}
		}
	}
	var nilView *CodexConfiguredStdioLaunch
	var nilProfile *CodexConfiguredProjectProfile
	for _, value := range []any{nilView, nilProfile} {
		for _, verb := range []string{"%v", "%+v", "%#v", "%s"} {
			if fmt.Sprintf(verb, value) != "<nil>" {
				t.Fatal("typed-nil private observation formatting unsafe")
			}
		}
	}
	envAfter, _ := view.Environment()
	if profile.projectRead.PrimaryError != privateCause || string(profile.payload) != raw || envAfter["INNOCUOUS"] != "synthetic-private-sentinel" || view.source != profile {
		t.Fatal("formatting erased private data/cause/source binding")
	}
	configured, err = decodeCodexConfiguredMCP([]byte(configuredStdioFixture), "graphify")
	if err != nil {
		t.Fatal(err)
	}
	profile.configured = configured
	view, err = profile.ConfiguredStdioLaunch()
	if err != nil {
		t.Fatal(err)
	}
	env, isNull = view.Environment()
	if env != nil || !isNull || view.Args() == nil || len(view.Args()) != 0 || view.EnvironmentReferences() == nil || view.ConfiguredCWD() != nil {
		t.Fatal("null/explicit empty representations collapsed")
	}
}

// Strings are synthetic. Keys/null/empty/numeric spelling follow the original
// received native0.162 row01 and row03 specimens; no credential capture is copied.
const configuredStdioFixture = `{"name":"graphify","enabled":false,"disabled_reason":null,"transport":{"type":"stdio","command":"synthetic-reader","args":[],"env":null,"env_vars":[],"cwd":null},"enabled_tools":null,"disabled_tools":null,"startup_timeout_sec":30.0,"tool_timeout_sec":null}`
const configuredHTTPFixture = `{"name":"graphify","enabled":false,"disabled_reason":null,"transport":{"type":"streamable_http","url":"http://127.0.0.1:9999/mcp","bearer_token_env_var":null,"http_headers":null,"env_http_headers":null,"http_headers_helper":null},"enabled_tools":null,"disabled_tools":null,"startup_timeout_sec":30.0,"tool_timeout_sec":null}`

func TestCodexConfiguredMCPPreservesNativeStates(t *testing.T) {
	for _, raw := range []string{configuredStdioFixture, configuredHTTPFixture} {
		value, err := decodeCodexConfiguredMCP([]byte(raw), "graphify")
		if err != nil || value.enabled || string(value.fields["startup_timeout_sec"]) != "30.0" || string(value.fields["enabled_tools"]) != "null" {
			t.Fatalf("native-shaped DTO/policy states were not retained: %v", err)
		}
		if bytes.Contains([]byte(raw), []byte(`"type":"stdio"`)) && (string(value.transport["args"]) != "[]" || string(value.transport["cwd"]) != "null" || string(value.transport["env"]) != "null") {
			t.Fatal("native absent-launch and empty-array states changed")
		}
	}
	emptyMap := strings.Replace(configuredStdioFixture, `"env":null`, `"env":{}`, 1)
	value, err := decodeCodexConfiguredMCP([]byte(emptyMap), "graphify")
	if err != nil || string(value.transport["env"]) != "{}" {
		t.Fatal("empty map was collapsed into null")
	}
}

func TestCodexConfiguredMCPRefusesAmbiguousProtocol(t *testing.T) {
	cases := map[string]string{
		"duplicate":            strings.Replace(configuredStdioFixture, `"enabled":false`, `"enabled":false,"enabled":true`, 1),
		"case":                 strings.Replace(configuredStdioFixture, `"enabled":false`, `"Enabled":false`, 1),
		"missing":              strings.Replace(configuredStdioFixture, `"enabled":false,`, ``, 1),
		"null enabled":         strings.Replace(configuredStdioFixture, `"enabled":false`, `"enabled":null`, 1),
		"extra":                strings.Replace(configuredStdioFixture, `"enabled":false`, `"enabled":false,"extra":null`, 1),
		"trailing":             configuredStdioFixture + `{}`,
		"transport null array": strings.Replace(configuredStdioFixture, `"args":[]`, `"args":null`, 1),
		"transport duplicate":  strings.Replace(configuredStdioFixture, `"env":null`, `"env":null,"env":{}`, 1),
		"bound name":           strings.Replace(configuredStdioFixture, `"name":"graphify"`, `"name":"other"`, 1),
	}
	for name, raw := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := decodeCodexConfiguredMCP([]byte(raw), "graphify"); err == nil {
				t.Fatal("ambiguous/native-invalid DTO was accepted")
			}
		})
	}
}

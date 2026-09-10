package config

import (
	"strings"
	"testing"
)

func TestServerManifestEnvForwardLocalRejectsInvalidOrAmbiguousNames(t *testing.T) {
	valid := func() *ServerManifest {
		return &ServerManifest{
			Name:      "forwarded-env",
			Kind:      KindGlobal,
			Transport: TransportStdioBridge,
			Command:   "provider-tool",
			Env:       map[string]string{"STATIC_MODE": "read"},
			Daemons:   []DaemonSpec{{Name: "default", Port: 9431}},
		}
	}
	cases := []struct {
		name    string
		forward []string
		want    string
	}{
		{name: "empty", forward: []string{""}, want: "invalid name"},
		{name: "nul", forward: []string{"TOKEN\x00VALUE"}, want: "invalid name"},
		{name: "equals", forward: []string{"TOKEN=VALUE"}, want: "invalid name"},
		{name: "duplicate", forward: []string{"TOKEN", "TOKEN"}, want: "duplicate"},
		{name: "static env collision", forward: []string{"STATIC_MODE"}, want: "collides with env"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			manifest := valid()
			manifest.EnvForwardLocal = tc.forward
			err := manifest.Validate()
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("Validate() error = %v, want substring %q", err, tc.want)
			}
		})
	}
}

func TestParseManifestEnvForwardLocalPreservesNamesAndOrdinaryStaticEnvSemantics(t *testing.T) {
	t.Setenv("MCPHUB_FORWARD_STATIC_TEST", "expanded-value")
	manifest, err := ParseManifest(strings.NewReader(`
name: forwarded-env
kind: global
transport: stdio-bridge
command: provider-tool
env:
  EXPANDED: "${MCPHUB_FORWARD_STATIC_TEST}"
  SECRET_REF: "secret:provider_token"
  FILE_REF: "file:provider-token.txt"
env_forward_local:
  - OPTIONAL_TOKEN
  - OPTIONAL_REGION
daemons:
  - name: default
    port: 9432
`))
	if err != nil {
		t.Fatalf("ParseManifest: %v", err)
	}
	if got := strings.Join(manifest.EnvForwardLocal, ","); got != "OPTIONAL_TOKEN,OPTIONAL_REGION" {
		t.Fatalf("EnvForwardLocal = %q, want names-only round-trip", got)
	}
	if got := manifest.Env["EXPANDED"]; got != "expanded-value" {
		t.Fatalf("Env[EXPANDED] = %q, want expanded-value", got)
	}
	if got := manifest.Env["SECRET_REF"]; got != "secret:provider_token" {
		t.Fatalf("Env[SECRET_REF] = %q, want preserved secret reference", got)
	}
	if got := manifest.Env["FILE_REF"]; got != "file:provider-token.txt" {
		t.Fatalf("Env[FILE_REF] = %q, want preserved file reference", got)
	}
}

func TestRemoteHTTPManifestRejectsEnvForwardLocal(t *testing.T) {
	for _, field := range []string{
		"env_forward_local: []",
		"env_forward_local: null",
		"env_forward_local: [OPTIONAL_TOKEN]",
	} {
		t.Run(field, func(t *testing.T) {
			raw := "name: remote\nkind: global\ntransport: remote-http\nurl: https://example.invalid/mcp\n" + field + "\n"
			_, err := ParseManifest(strings.NewReader(raw))
			if err == nil || !strings.Contains(err.Error(), "env_forward_local") {
				t.Fatalf("ParseManifest() error = %v, want env_forward_local refusal", err)
			}
		})
	}

	manifest := &ServerManifest{
		Name:            "remote",
		Kind:            KindGlobal,
		Transport:       TransportRemoteHTTP,
		URL:             "https://example.invalid/mcp",
		EnvForwardLocal: []string{"OPTIONAL_TOKEN"},
	}
	if err := manifest.Validate(); err == nil || !strings.Contains(err.Error(), "env_forward_local") {
		t.Fatalf("Validate() error = %v, want env_forward_local refusal", err)
	}
}

package config

import (
	"runtime"
	"strings"
	"testing"
)

func TestServerManifestEnvForwardLocalMatchesRuntimeEnvironmentKeyContract(t *testing.T) {
	valid := func() *ServerManifest {
		return &ServerManifest{
			Name:      "forwarded-env",
			Kind:      KindGlobal,
			Transport: TransportStdioBridge,
			Command:   "provider-tool",
			Daemons:   []DaemonSpec{{Name: "default", Port: 9431}},
		}
	}
	for _, tc := range []struct {
		name    string
		env     map[string]string
		forward []string
	}{
		{name: "static env collision", env: map[string]string{"STATIC_TOKEN": "configured"}, forward: []string{"static_token"}},
		{name: "forwarded duplicate", forward: []string{"FORWARD_TOKEN", "forward_token"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			manifest := valid()
			manifest.Env = tc.env
			manifest.EnvForwardLocal = tc.forward
			err := manifest.Validate()
			if runtime.GOOS == "windows" {
				if err == nil || !strings.Contains(err.Error(), "env_forward_local") {
					t.Fatalf("Validate() error = %v, want Windows environment-key refusal", err)
				}
			} else if err != nil {
				t.Fatalf("Validate() error = %v, want POSIX case-sensitive acceptance", err)
			}
		})
	}
}

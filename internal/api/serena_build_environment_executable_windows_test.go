//go:build windows

package api

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"mcp-local-hub/internal/config"
	"mcp-local-hub/internal/secrets"
)

func TestSerenaBuildEnvironmentWindowsExecutablePolicy(t *testing.T) {
	// Runtime and host settings must not broaden package preparation.
	t.Setenv("PATHEXT", ".BAT;.CMD")
	t.Setenv("GIT_CONFIG_COUNT", "2")
	t.Setenv("GIT_CONFIG_KEY_0", "core.longpaths")
	t.Setenv("GIT_CONFIG_VALUE_0", "false")
	t.Setenv("GIT_CONFIG_KEY_1", "core.abbrev")
	t.Setenv("GIT_CONFIG_VALUE_1", "4")
	keys := SerenaBuildBootstrapDeclarationKeys()
	if !reflect.DeepEqual(keys, []string{"SYSTEMROOT", "WINDIR", "COMSPEC", "PATH"}) {
		t.Fatalf("caller bootstrap contract changed: %v", keys)
	}
	bootstrap := map[string]string{}
	for _, key := range keys {
		bootstrap[key] = os.Getenv(key)
		if bootstrap[key] == "" {
			t.Fatalf("required bootstrap input unavailable: %s", key)
		}
	}
	// The real native source qualifies this physical command directory; no
	// process, test publisher or synthetic source issuer is created.
	bootstrap["PATH"] = filepath.Dir(bootstrap["COMSPEC"])
	raw, err := NewAPI().CatalogManifestGet("serena")
	if err != nil {
		t.Fatal(err)
	}
	manifest, err := config.ParseManifest(strings.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	var baselineSHA string
	for _, tc := range []struct {
		name         string
		declarations map[string]string
	}{
		{"ambient", nil},
		{"declared", map[string]string{"PATHEXT": ".BAT;.CMD"}},
		{"mixed-case-declared", map[string]string{"pAtHeXt": ".BAT;.CMD"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for key := range tc.declarations {
				if IsSerenaPreparationBuildDeclaration(key) {
					t.Fatalf("runtime extension declaration became a caller build input: %s", key)
				}
			}
			source, err := NewSerenaPreparationEnvironmentSource(ctx, manifest, secrets.NewResolver(nil, nil), tc.declarations, nil, bootstrap)
			if err != nil {
				t.Fatal(err)
			}
			projection, execution, err := BuildSerenaBuildEnvironmentProjection(ctx, source, DefaultSerenaPreparationPolicy())
			if err != nil {
				t.Fatal(err)
			}
			values := preparedSerenaEnvironmentMap(execution.values)
			if err := ValidatePreparedSerenaEnvironment(values); err != nil {
				t.Fatal("fixed subprocess policy conflicts with prepared environment", err)
			}
			for key := range values {
				if strings.HasPrefix(key, "GIT_") && key != "GIT_CONFIG_COUNT" && key != "GIT_CONFIG_KEY_0" && key != "GIT_CONFIG_VALUE_0" {
					t.Fatalf("ambient Git configuration entered package preparation: %s", key)
				}
			}
			identity, err := projection.SHA256()
			if err != nil {
				t.Fatal(err)
			}
			if baselineSHA == "" {
				baselineSHA = identity
			} else if identity != baselineSHA {
				t.Fatal("runtime extension declarations changed package build identity")
			}
			for _, policy := range []struct{ key, value, rule, different string }{
				{"PATHEXT", ".EXE", "serena-windows-executable-rule-v1:pathext=.EXE", ".BAT;.CMD"},
				{"GIT_CONFIG_COUNT", "1", "serena-windows-git-longpaths-rule-v1:core.longpaths=true", "2"},
				{"GIT_CONFIG_KEY_0", "core.longpaths", "serena-windows-git-longpaths-rule-v1:core.longpaths=true", "core.ignorecase"},
				{"GIT_CONFIG_VALUE_0", "true", "serena-windows-git-longpaths-rule-v1:core.longpaths=true", "false"},
			} {
				if values[policy.key] != policy.value {
					t.Fatalf("package subprocess policy %s = %q, want %q", policy.key, values[policy.key], policy.value)
				}
				want := SerenaBuildEnvironmentEntryV1{
					Key: policy.key, ValueKind: "policy", SourceKind: "hub-policy",
					SourceRefSHA256: preparedSerenaHash([]byte(policy.rule)),
					ValueSHA256:     preparedSerenaHash([]byte(values[policy.key])),
				}
				entryIndex := -1
				for i, entry := range projection.Entries {
					if entry.Key == policy.key {
						if entryIndex >= 0 || entry != want {
							t.Fatalf("subprocess policy provenance differs: %+v", entry)
						}
						entryIndex = i
					}
				}
				if entryIndex < 0 {
					t.Fatalf("subprocess policy %s is absent from the build identity", policy.key)
				}
				// Each fixed field changes the existing identity codec. No native
				// authority is manufactured when comparing these value records.
				omitted := projection
				omitted.Entries = append([]SerenaBuildEnvironmentEntryV1(nil), projection.Entries[:entryIndex]...)
				omitted.Entries = append(omitted.Entries, projection.Entries[entryIndex+1:]...)
				oldIdentity, err := omitted.SHA256()
				if err != nil || oldIdentity == identity {
					t.Fatalf("omitted %s policy collided with current identity: %v", policy.key, err)
				}
				altered := projection
				altered.Entries = append([]SerenaBuildEnvironmentEntryV1(nil), projection.Entries...)
				altered.Entries[entryIndex].ValueSHA256 = preparedSerenaHash([]byte(policy.different))
				alteredIdentity, err := altered.SHA256()
				if err != nil || alteredIdentity == identity {
					t.Fatalf("different %s policy collided with current identity: %v", policy.key, err)
				}
			}
		})
	}

	for _, tc := range []struct{ key, value string }{
		{"GIT_CONFIG_COUNT", "1"},
		{"GIT_CONFIG_KEY_0", "core.longpaths"},
		{"GIT_CONFIG_VALUE_0", "true"},
		{"gIt_CoNfIg_CoUnT", "1"},
		{"gIt_CoNfIg_KeY_0", "core.longpaths"},
		{"gIt_CoNfIg_VaLuE_0", "true"},
	} {
		t.Run("caller-refused-"+tc.key, func(t *testing.T) {
			if !IsSerenaPreparationBuildDeclaration(tc.key) {
				t.Fatal("declared Git setting was silently classified as runtime-only")
			}
			source, err := NewSerenaPreparationEnvironmentSource(ctx, manifest, secrets.NewResolver(nil, nil), map[string]string{tc.key: tc.value}, nil, bootstrap)
			if err != nil {
				t.Fatal(err)
			}
			_, _, err = BuildSerenaBuildEnvironmentProjection(ctx, source, DefaultSerenaPreparationPolicy())
			if err == nil || !strings.Contains(err.Error(), "SERENA_BUILD_INPUT_UNSUPPORTED") {
				t.Fatalf("caller Git policy was not refused: %v", err)
			}
		})
	}
}

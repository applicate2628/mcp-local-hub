//go:build windows

package api

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"mcp-local-hub/internal/config"
	"mcp-local-hub/internal/secrets"
)

func TestSerenaBuildEnvironmentWindowsBuildConstraintPolicy(t *testing.T) {
	t.Setenv("UV_BUILD_CONSTRAINT", "untrusted-host-constraint.txt")
	bootstrap := map[string]string{}
	for _, key := range SerenaBuildBootstrapDeclarationKeys() {
		bootstrap[key] = os.Getenv(key)
		if bootstrap[key] == "" {
			t.Fatalf("required bootstrap input unavailable: %s", key)
		}
	}
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
	source, err := NewSerenaPreparationEnvironmentSource(ctx, manifest, secrets.NewResolver(nil, nil), nil, nil, bootstrap)
	if err != nil {
		t.Fatal(err)
	}
	projection, execution, err := BuildSerenaBuildEnvironmentProjection(ctx, source, DefaultSerenaPreparationPolicy())
	if err != nil {
		t.Fatal(err)
	}
	t.Run("source-owned-content-and-identity", func(t *testing.T) {
		if !execution.buildConstraint {
			t.Fatal("fixed constraint policy did not reach acquisition")
		}
		if _, present := preparedSerenaEnvironmentMap(execution.values)["UV_BUILD_CONSTRAINT"]; present {
			t.Fatal("constraint content or ambient path leaked into execution environment")
		}
		want := SerenaBuildEnvironmentEntryV1{
			Key: "UV_BUILD_CONSTRAINT", ValueKind: "policy", SourceKind: "hub-policy",
			SourceRefSHA256: preparedSerenaHash([]byte("serena-windows-build-constraint-rule-v1:setuptools<84")),
			ValueSHA256:     preparedSerenaHash([]byte("setuptools<84\n")),
		}
		index := -1
		for i, entry := range projection.Entries {
			if entry.Key == want.Key {
				if index >= 0 || entry != want {
					t.Fatalf("fixed build constraint provenance differs: %+v", entry)
				}
				index = i
			}
		}
		if index < 0 {
			t.Fatal("fixed build constraint is absent from package identity")
		}
		role := -1
		for i, value := range projection.OwnedPathRoles {
			if value == "build-constraint" {
				if role >= 0 {
					t.Fatal("build constraint role is duplicated")
				}
				role = i
			}
		}
		if role < 0 {
			t.Fatal("owned build constraint path role is absent")
		}
		identity, err := projection.SHA256()
		if err != nil {
			t.Fatal(err)
		}
		old := projection
		old.Entries = append([]SerenaBuildEnvironmentEntryV1(nil), projection.Entries[:index]...)
		old.Entries = append(old.Entries, projection.Entries[index+1:]...)
		old.OwnedPathRoles = append([]string(nil), projection.OwnedPathRoles[:role]...)
		old.OwnedPathRoles = append(old.OwnedPathRoles, projection.OwnedPathRoles[role+1:]...)
		omitted, err := old.SHA256()
		if err != nil || omitted == identity {
			t.Fatal("old unconstrained environment collided with current identity", err)
		}
		altered := projection
		altered.Entries = append([]SerenaBuildEnvironmentEntryV1(nil), projection.Entries...)
		altered.Entries[index].ValueSHA256 = preparedSerenaHash([]byte("setuptools<85\n"))
		changed, err := altered.SHA256()
		if err != nil || changed == identity {
			t.Fatal("different constraint content collided with current identity", err)
		}
	})
	for _, key := range []string{"UV_BUILD_CONSTRAINT", "uV_bUiLd_CoNsTrAiNt"} {
		t.Run("caller-declaration-refused-"+key, func(t *testing.T) {
			if !IsSerenaPreparationBuildDeclaration(key) {
				t.Fatal("unqualified caller constraint was silently ignored")
			}
			source, err := NewSerenaPreparationEnvironmentSource(ctx, manifest, secrets.NewResolver(nil, nil), map[string]string{key: "caller-constraint.txt"}, nil, bootstrap)
			if err != nil {
				t.Fatal(err)
			}
			_, _, err = BuildSerenaBuildEnvironmentProjection(ctx, source, DefaultSerenaPreparationPolicy())
			if err == nil || !strings.Contains(err.Error(), "SERENA_BUILD_ARTIFACT_SOURCE_UNQUALIFIED") {
				t.Fatalf("caller constraint was not refused: %v", err)
			}
		})
	}
	t.Run("caller-install-setting-refused", func(t *testing.T) {
		_, err := preparedSerenaInstallSettings([]string{"--build-constraint", "caller-constraint.txt"}, execution.networkPolicy)
		if err == nil || !strings.Contains(err.Error(), "SERENA_PREPARATION_BUILD_ARTIFACT_SOURCE_UNQUALIFIED") {
			t.Fatalf("caller install constraint was not refused: %v", err)
		}
	})
}

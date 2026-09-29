package api

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"mcp-local-hub/internal/clients"
)

type adoptTargetTestClient struct {
	clients.Client
	name string
}

func (c *adoptTargetTestClient) Name() string { return c.name }

func TestAdoptClientConfigTargetV1JSONShape(t *testing.T) {
	target := AdoptClientConfigTargetV1{
		Kind:                  "codex_project",
		ProjectRoot:           `C:\workspace\demo`,
		ProjectIdentitySHA256: strings.Repeat("a", 64),
	}
	encoded, err := json.Marshal(target)
	if err != nil {
		t.Fatal(err)
	}
	const want = `{"kind":"codex_project","project_root":"C:\\workspace\\demo","project_identity_sha256":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}`
	if string(encoded) != want {
		t.Fatalf("JSON = %s, want %s", encoded, want)
	}
}

func TestAdoptClientTargetResolverUsesGlobalFactoryOnlyForLegacyRef(t *testing.T) {
	wantClient := &adoptTargetTestClient{name: "codex-cli"}
	globalCalls, projectCalls := 0, 0
	resolver := adoptClientTargetResolver{
		globalCatalogFactory: func() map[string]clients.Client {
			globalCalls++
			return map[string]clients.Client{"codex-cli": wantClient}
		},
		projectClientFactory: func(string, string) (clients.Client, error) {
			projectCalls++
			return nil, errors.New("project factory must not run")
		},
	}

	resolved, err := resolver.Resolve(adoptClientTargetRef{clientName: "codex-cli", targetEntryName: "demo"})
	if err != nil {
		t.Fatalf("Resolve(global): %v", err)
	}
	if resolved.client != wantClient || resolved.targetEntryName != "demo" || resolved.projectSource != nil || globalCalls != 1 || projectCalls != 0 {
		t.Fatalf("resolved=%+v calls global=%d project=%d", resolved, globalCalls, projectCalls)
	}
}

func TestAdoptClientTargetResolverRejectsInvalidProjectRefBeforeFactories(t *testing.T) {
	validRoot := t.TempDir()
	validFingerprint, err := clients.CodexProjectIdentitySHA256ForRoot(validRoot)
	if err != nil {
		t.Fatal(err)
	}
	planSource, err := extractCodexProjectAdoptSource(writeProjectAdoptConfig(t, validProjectAdoptConfig), "demo")
	if err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct {
		name string
		ref  adoptClientTargetRef
	}{
		{name: "empty target", ref: adoptClientTargetRef{clientName: "codex-cli", targetEntryName: "demo", configTarget: &AdoptClientConfigTargetV1{}}},
		{name: "unknown kind", ref: adoptClientTargetRef{clientName: "codex-cli", targetEntryName: "demo", configTarget: &AdoptClientConfigTargetV1{Kind: "other", ProjectRoot: validRoot, ProjectIdentitySHA256: validFingerprint}}},
		{name: "non-Codex client", ref: adoptClientTargetRef{clientName: "claude-code", targetEntryName: "demo", configTarget: &AdoptClientConfigTargetV1{Kind: "codex_project", ProjectRoot: validRoot, ProjectIdentitySHA256: validFingerprint}}},
		{name: "empty entry", ref: adoptClientTargetRef{clientName: "codex-cli", configTarget: &AdoptClientConfigTargetV1{Kind: "codex_project", ProjectRoot: validRoot, ProjectIdentitySHA256: validFingerprint}}},
		{name: "relative root", ref: adoptClientTargetRef{clientName: "codex-cli", targetEntryName: "demo", configTarget: &AdoptClientConfigTargetV1{Kind: "codex_project", ProjectRoot: "relative", ProjectIdentitySHA256: validFingerprint}}},
		{name: "uppercase fingerprint", ref: adoptClientTargetRef{clientName: "codex-cli", targetEntryName: "demo", configTarget: &AdoptClientConfigTargetV1{Kind: "codex_project", ProjectRoot: validRoot, ProjectIdentitySHA256: strings.ToUpper(validFingerprint)}}},
		{name: "mismatched fingerprint", ref: adoptClientTargetRef{clientName: "codex-cli", targetEntryName: "demo", configTarget: &AdoptClientConfigTargetV1{Kind: "codex_project", ProjectRoot: validRoot, ProjectIdentitySHA256: strings.Repeat("0", 64)}}},
		{name: "dual project modes", ref: adoptClientTargetRef{clientName: "codex-cli", targetEntryName: "demo", configTarget: &AdoptClientConfigTargetV1{Kind: "codex_project", ProjectRoot: validRoot, ProjectIdentitySHA256: validFingerprint}, projectSource: planSource}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			globalCalls, projectCalls := 0, 0
			resolver := adoptClientTargetResolver{
				globalCatalogFactory: func() map[string]clients.Client {
					globalCalls++
					return nil
				},
				projectClientFactory: func(string, string) (clients.Client, error) {
					projectCalls++
					return nil, nil
				},
			}
			_, err := resolver.Resolve(tc.ref)
			if err == nil || !errors.Is(err, errProjectAdoptConfigTargetInvalid) {
				t.Fatalf("Resolve error = %v, want PROJECT_ADOPT_CONFIG_TARGET_INVALID", err)
			}
			if globalCalls != 0 || projectCalls != 0 {
				t.Fatalf("factory calls global=%d project=%d, want zero", globalCalls, projectCalls)
			}
		})
	}
}

func TestAdoptClientTargetResolverUsesOnlyProjectFactory(t *testing.T) {
	t.Run("persisted", func(t *testing.T) {
		root := t.TempDir()
		fingerprint, err := clients.CodexProjectIdentitySHA256ForRoot(root)
		if err != nil {
			t.Fatal(err)
		}
		wantClient := &adoptTargetTestClient{name: "codex-cli"}
		globalCalls, projectCalls := 0, 0
		resolver := adoptClientTargetResolver{
			globalCatalogFactory: func() map[string]clients.Client {
				globalCalls++
				return nil
			},
			projectClientFactory: func(projectRoot, logicalEntry string) (clients.Client, error) {
				projectCalls++
				if projectRoot != root || logicalEntry != "demo" {
					t.Fatalf("project factory args root=%q entry=%q", projectRoot, logicalEntry)
				}
				return wantClient, nil
			},
		}
		resolved, err := resolver.Resolve(adoptClientTargetRef{
			clientName:      "codex-cli",
			targetEntryName: "demo",
			configTarget: &AdoptClientConfigTargetV1{
				Kind:                  "codex_project",
				ProjectRoot:           root,
				ProjectIdentitySHA256: fingerprint,
			},
		})
		if err != nil {
			t.Fatalf("Resolve(persisted): %v", err)
		}
		if resolved.client != wantClient || resolved.targetEntryName != "demo" || resolved.projectSource != nil || globalCalls != 0 || projectCalls != 1 {
			t.Fatalf("resolved=%+v calls global=%d project=%d", resolved, globalCalls, projectCalls)
		}
	})

	t.Run("plan snapshot", func(t *testing.T) {
		root := writeProjectAdoptConfig(t, validProjectAdoptConfig)
		source, err := extractCodexProjectAdoptSource(root, "demo")
		if err != nil {
			t.Fatal(err)
		}
		wantClient := &adoptTargetTestClient{name: "codex-cli"}
		globalCalls, projectCalls := 0, 0
		resolver := adoptClientTargetResolver{
			globalCatalogFactory: func() map[string]clients.Client {
				globalCalls++
				return nil
			},
			projectClientFactory: func(projectRoot, logicalEntry string) (clients.Client, error) {
				projectCalls++
				if projectRoot != root || logicalEntry != "demo" {
					t.Fatalf("project factory args root=%q entry=%q", projectRoot, logicalEntry)
				}
				return wantClient, nil
			},
		}
		resolved, err := resolver.Resolve(adoptClientTargetRef{clientName: "codex-cli", targetEntryName: "demo", projectSource: source})
		if err != nil {
			t.Fatalf("Resolve(plan): %v", err)
		}
		if resolved.client != wantClient || resolved.projectSource != source || globalCalls != 0 || projectCalls != 1 {
			t.Fatalf("resolved=%+v calls global=%d project=%d", resolved, globalCalls, projectCalls)
		}
	})
}

func TestAdoptClientTargetResolverKeepsSameNameProjectsDistinct(t *testing.T) {
	rootA, rootB := t.TempDir(), t.TempDir()
	hashA, err := clients.CodexProjectIdentitySHA256ForRoot(rootA)
	if err != nil {
		t.Fatal(err)
	}
	hashB, err := clients.CodexProjectIdentitySHA256ForRoot(rootB)
	if err != nil {
		t.Fatal(err)
	}
	if hashA == hashB {
		t.Fatalf("different roots produced equal identity %q", hashA)
	}
	var roots []string
	resolver := adoptClientTargetResolver{
		globalCatalogFactory: func() map[string]clients.Client {
			t.Fatal("global catalog must not run")
			return nil
		},
		projectClientFactory: func(root, logicalEntry string) (clients.Client, error) {
			roots = append(roots, root)
			return &adoptTargetTestClient{name: root}, nil
		},
	}
	for _, target := range []AdoptClientConfigTargetV1{
		{Kind: "codex_project", ProjectRoot: rootA, ProjectIdentitySHA256: hashA},
		{Kind: "codex_project", ProjectRoot: rootB, ProjectIdentitySHA256: hashB},
	} {
		if _, err := resolver.Resolve(adoptClientTargetRef{clientName: "codex-cli", targetEntryName: "graphify", configTarget: &target}); err != nil {
			t.Fatal(err)
		}
	}
	if len(roots) != 2 || roots[0] != rootA || roots[1] != rootB {
		t.Fatalf("resolved roots = %v", roots)
	}
}

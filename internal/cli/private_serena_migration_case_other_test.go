//go:build !windows

package cli

import (
	"context"
	"io"
	"os"
	"testing"
)

type privateSerenaMigrationCase struct{}

func beginPrivateSerenaMigrationCase(t *testing.T) (*privateSerenaMigrationCase, bool) {
	t.Helper()
	for _, key := range []string{"MCPHUB_PRIVATE_SERENA_MIGRATION_INPUT", "MCPHUB_PRIVATE_SERENA_MIGRATION_INPUT_SHA256", "MCPHUB_PRIVATE_SERENA_MIGRATION_CASE_CONFIG"} {
		if _, present := os.LookupEnv(key); present {
			t.Fatal("private Serena migration publisher receiving requires Windows")
		}
	}
	return nil, false
}

func (*privateSerenaMigrationCase) Context() context.Context {
	return context.Background()
}

func (*privateSerenaMigrationCase) TempDir(t *testing.T) string {
	t.Helper()
	return t.TempDir()
}

func (*privateSerenaMigrationCase) Env(t *testing.T, opts ...migrateSerenaTestEnvOptions) (string, string) {
	t.Helper()
	return migrateSerenaTestEnv(t, opts...)
}

func (*privateSerenaMigrationCase) SeedManifest(t *testing.T, directory, original string) string {
	t.Helper()
	return seedSerenaManifest(t, directory, original)
}

func (*privateSerenaMigrationCase) Run(t *testing.T, output io.Writer) error {
	t.Helper()
	return runMigrateSerenaDynamicPool(context.Background(), output)
}

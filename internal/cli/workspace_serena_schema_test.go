package cli

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"mcp-local-hub/internal/api"
)

func TestWorkspaceBootstrap_MigrateSerenaSchema_IsExplicitAndNonDestructive(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	projectDir := filepath.Join(root, ".serena")
	if err := os.MkdirAll(projectDir, 0o700); err != nil {
		t.Fatal(err)
	}
	project := filepath.Join(projectDir, "project.yml")
	before := "# retain this comment\nproject_name: preserved\nlanguages: [python]\ncustom: keep\n"
	if err := os.WriteFile(project, []byte(before), 0o600); err != nil {
		t.Fatal(err)
	}

	cmd := newWorkspaceBootstrapCmd()
	var out, errOut bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&errOut)
	cmd.SetArgs([]string{root, "--migrate-serena-schema"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("workspace bootstrap --migrate-serena-schema error = %v (stderr %s)", err, errOut.String())
	}
	after, err := os.ReadFile(project)
	if err != nil {
		t.Fatal(err)
	}
	got := string(after)
	for _, want := range []string{"# retain this comment", "project_name: preserved", "language_servers:", "- python", "custom: keep"} {
		if !strings.Contains(got, want) {
			t.Fatalf("migrated project.yml missing %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "languages:") {
		t.Fatalf("migrated project.yml retained compatibility key:\n%s", got)
	}
}

func TestWorkspaceRegister_CompatibilitySchemaPrintsMigrationWarning(t *testing.T) {
	withSerenaManifest(t, 9121, 9123)
	withStateDir(t)
	workspace := makeWorkspaceDir(t, t.TempDir(), []string{"cpp"}) // helper writes `languages`, the compatibility form.

	out, err := runWorkspaceCmd(t, "register", workspace)
	if err != nil {
		t.Fatalf("register compatibility schema: %v\n%s", err, out)
	}
	for _, want := range []string{
		"warning [SERENA_PROJECT_SCHEMA_COMPAT]",
		"uses `languages`",
		"mcphub workspace bootstrap \"" + workspace + "\" --migrate-serena-schema",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("compatibility registration warning missing %q:\n%s", want, out)
		}
	}
}

func TestMigrateSerenaProjectSchema_ReceiptBackupAndCompatibilityForms(t *testing.T) {
	for _, tc := range []struct {
		name   string
		before string
		want   []string
		absent []string
	}{
		{
			name:   "plural aliases and excluded dirs",
			before: "# retain\nlanguages: [ python ]\nexcluded_dirs: [node_modules, target/]\ncustom: keep\n",
			want:   []string{"# retain", "language_servers:", "- python", "ignored_paths:", "- node_modules/**", "- target/**", "custom: keep"},
			absent: []string{"languages:", "excluded_dirs:"},
		},
		{
			name:   "singular alias",
			before: "language: cpp\n",
			want:   []string{"language_servers:", "- cpp"},
			absent: []string{"language:"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "project.yml")
			before := []byte(tc.before)
			if err := os.WriteFile(path, before, 0o600); err != nil {
				t.Fatal(err)
			}
			receipt, err := migrateSerenaProjectSchema(path, "project")
			if err != nil {
				t.Fatalf("migrate: %v", err)
			}
			if !receipt.Changed || receipt.Preimage != sha256.Sum256(before) {
				t.Fatalf("receipt = %+v, want changed and exact preimage digest", receipt)
			}
			backup, err := os.ReadFile(receipt.BackupPath)
			if err != nil || !bytes.Equal(backup, before) {
				t.Fatalf("backup = %q err=%v, want exact %q", backup, err, before)
			}
			after, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if receipt.Postimage != sha256.Sum256(after) {
				t.Fatalf("postimage digest mismatch: got %x want %x", receipt.Postimage, sha256.Sum256(after))
			}
			if _, err := api.ReadSerenaProjectSchema(context.Background(), path); err != nil {
				t.Fatalf("migrated readback: %v", err)
			}
			body := string(after)
			for _, want := range tc.want {
				if !strings.Contains(body, want) {
					t.Fatalf("migrated body missing %q:\n%s", want, body)
				}
			}
			for _, absent := range tc.absent {
				if strings.Contains(body, absent) {
					t.Fatalf("migrated body retained %q:\n%s", absent, body)
				}
			}
		})
	}
}

func TestMigrateSerenaProjectSchema_ConflictNoopAndFailureSettlement(t *testing.T) {
	t.Run("conflict leaves bytes untouched", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "project.yml")
		before := []byte("language_servers: [python]\nlanguages: [go]\n")
		if err := os.WriteFile(path, before, 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := migrateSerenaProjectSchema(path, "project"); !errors.Is(err, api.ErrSerenaProjectSchemaConflict) {
			t.Fatalf("conflict error = %v", err)
		}
		after, _ := os.ReadFile(path)
		if !bytes.Equal(after, before) {
			t.Fatalf("conflict changed live bytes: %q", after)
		}
	})
	t.Run("current form is explicit no-op", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "project.yml")
		before := []byte("language_servers: [python]\n")
		if err := os.WriteFile(path, before, 0o600); err != nil {
			t.Fatal(err)
		}
		receipt, err := migrateSerenaProjectSchema(path, "project")
		if err != nil || receipt.Changed || receipt.BackupPath != "" {
			t.Fatalf("no-op receipt=%+v err=%v", receipt, err)
		}
		after, _ := os.ReadFile(path)
		if !bytes.Equal(after, before) {
			t.Fatalf("no-op changed bytes: %q", after)
		}
	})
	t.Run("backup write failure preserves live bytes", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "project.yml")
		before := []byte("languages: [python]\n")
		if err := os.WriteFile(path, before, 0o600); err != nil {
			t.Fatal(err)
		}
		originalWriter := writeSerenaProjectSchemaBytesAtomic
		writeSerenaProjectSchemaBytesAtomic = func(string, []byte) error { return errors.New("backup write failed") }
		t.Cleanup(func() { writeSerenaProjectSchemaBytesAtomic = originalWriter })
		if _, err := migrateSerenaProjectSchema(path, "project"); err == nil {
			t.Fatal("write failure must return error")
		}
		after, _ := os.ReadFile(path)
		if !bytes.Equal(after, before) {
			t.Fatalf("write failure changed live bytes: %q", after)
		}
	})
	t.Run("readback failure returns committed receipt", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "project.yml")
		before := []byte("languages: [python]\n")
		if err := os.WriteFile(path, before, 0o600); err != nil {
			t.Fatal(err)
		}
		originalReader := readSerenaProjectSchemaForMigration
		calls := 0
		readSerenaProjectSchemaForMigration = func(ctx context.Context, p string) (api.SerenaProjectSchema, error) {
			calls++
			if calls == 2 {
				return api.SerenaProjectSchema{}, errors.New("readback failed")
			}
			return originalReader(ctx, p)
		}
		t.Cleanup(func() { readSerenaProjectSchemaForMigration = originalReader })
		receipt, err := migrateSerenaProjectSchema(path, "project")
		if err == nil || !receipt.Changed || receipt.BackupPath == "" {
			t.Fatalf("readback result receipt=%+v err=%v", receipt, err)
		}
		if receipt.Postimage == ([sha256.Size]byte{}) {
			t.Fatal("committed readback failure must carry postimage digest")
		}
	})
}

func TestWorkspaceList_LabelsProxyPortAsInternal(t *testing.T) {
	t.Parallel()
	var out bytes.Buffer
	if err := printWorkspaceTable(&out, nil, ""); err != nil {
		t.Fatal(err)
	}
	if got := out.String(); !strings.Contains(got, "WORKSPACE_PROXY") || strings.Contains(got, " PORT ") {
		t.Fatalf("workspace list header = %q, want explicit internal workspace proxy label", got)
	}
}

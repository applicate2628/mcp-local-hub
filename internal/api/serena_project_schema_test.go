package api

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func writeSerenaProjectYMLForSchemaTest(t *testing.T, body string) string {
	t.Helper()
	root := t.TempDir()
	dir := filepath.Join(root, ".serena")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "project.yml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestReadSerenaProjectSchema_AcceptsCurrentAndCompatibilityForms(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name          string
		body          string
		wantLanguages []string
		wantForm      SerenaProjectSchemaForm
		wantCompat    bool
	}{
		{"current", "language_servers:\n  - python\n  - typescript\n", []string{"python", "typescript"}, SerenaProjectSchemaFormLanguageServers, false},
		{"plural compatibility", "languages: [ python, typescript ]\n", []string{"python", "typescript"}, SerenaProjectSchemaFormLanguages, true},
		{"singular compatibility", "language: python\n", []string{"python"}, SerenaProjectSchemaFormLanguage, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ReadSerenaProjectSchema(context.Background(), writeSerenaProjectYMLForSchemaTest(t, tc.body))
			if err != nil {
				t.Fatalf("ReadSerenaProjectSchema() error = %v", err)
			}
			if !reflect.DeepEqual(got.Languages, tc.wantLanguages) || got.Form != tc.wantForm || got.Compatibility != tc.wantCompat {
				t.Fatalf("ReadSerenaProjectSchema() = %#v, want languages=%#v form=%q compatibility=%t", got, tc.wantLanguages, tc.wantForm, tc.wantCompat)
			}
		})
	}
}

func TestReadSerenaProjectSchema_FailsClosedForInvalidForms(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		body string
		want error
	}{
		{"conflicting plurals", "language_servers: [python]\nlanguages: [go]\n", ErrSerenaProjectSchemaConflict},
		{"missing", "project_name: sample\nlanguage_servers: []\n", ErrSerenaProjectSchemaLanguagesMissing},
		{"wrong value type", "language_servers: python\n", ErrSerenaProjectSchemaParseFailed},
		{"malformed YAML", "language_servers: [python\n", ErrSerenaProjectSchemaParseFailed},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := ReadSerenaProjectSchema(context.Background(), writeSerenaProjectYMLForSchemaTest(t, tc.body))
			if !errors.Is(err, tc.want) {
				t.Fatalf("ReadSerenaProjectSchema() error = %v, want errors.Is(_, %v)", err, tc.want)
			}
		})
	}
}

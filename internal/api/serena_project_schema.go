package api

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"gopkg.in/yaml.v3"
)

var (
	ErrSerenaProjectSchemaConflict         = errors.New("SERENA_PROJECT_SCHEMA_CONFLICT")
	ErrSerenaProjectSchemaLanguagesMissing = errors.New("SERENA_PROJECT_SCHEMA_LANGUAGES_MISSING")
	ErrSerenaProjectSchemaParseFailed      = errors.New("SERENA_PROJECT_SCHEMA_PARSE_FAILED")
)

type SerenaProjectSchemaForm string

const (
	SerenaProjectSchemaFormLanguageServers SerenaProjectSchemaForm = "language_servers"
	SerenaProjectSchemaFormLanguages       SerenaProjectSchemaForm = "languages"
	SerenaProjectSchemaFormLanguage        SerenaProjectSchemaForm = "language"
)

// SerenaProjectSchema is the normalized read-only project-file projection
// consumed by both explicit registration and router auto-registration.
type SerenaProjectSchema struct {
	Languages     []string
	Form          SerenaProjectSchemaForm
	Compatibility bool
}

// ReadSerenaProjectSchema is the single v1.7 Serena project-schema parser.
// It accepts language_servers as current plus the documented compatibility
// aliases, without rewriting unknown upstream-owned fields.
func ReadSerenaProjectSchema(ctx context.Context, path string) (SerenaProjectSchema, error) {
	raw, err := ReadUntrustedSerenaProjectYML(ctx, path)
	if err != nil {
		return SerenaProjectSchema{}, err
	}
	var doc struct {
		LanguageServers *[]string `yaml:"language_servers"`
		Languages       *[]string `yaml:"languages"`
		Language        *string   `yaml:"language"`
	}
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		return SerenaProjectSchema{}, fmt.Errorf("%w: parse %s: %v", ErrSerenaProjectSchemaParseFailed, path, err)
	}
	current, currentOK := normalizeSerenaLanguageList(doc.LanguageServers)
	compat, compatOK := normalizeSerenaLanguageList(doc.Languages)
	legacy, legacyOK := normalizeSerenaLanguage(doc.Language)
	if (doc.LanguageServers != nil && !currentOK) || (doc.Languages != nil && !compatOK) || (doc.Language != nil && !legacyOK) {
		return SerenaProjectSchema{}, fmt.Errorf("%w: %s has no non-empty Serena language-server list", ErrSerenaProjectSchemaLanguagesMissing, path)
	}
	if doc.LanguageServers != nil && doc.Languages != nil && !sameStrings(current, compat) {
		return SerenaProjectSchema{}, fmt.Errorf("%w: %s defines language_servers and languages with different values", ErrSerenaProjectSchemaConflict, path)
	}
	if doc.LanguageServers != nil {
		return SerenaProjectSchema{Languages: current, Form: SerenaProjectSchemaFormLanguageServers}, nil
	}
	if doc.Languages != nil {
		return SerenaProjectSchema{Languages: compat, Form: SerenaProjectSchemaFormLanguages, Compatibility: true}, nil
	}
	if doc.Language != nil {
		return SerenaProjectSchema{Languages: legacy, Form: SerenaProjectSchemaFormLanguage, Compatibility: true}, nil
	}
	return SerenaProjectSchema{}, fmt.Errorf("%w: %s has no Serena language-server key", ErrSerenaProjectSchemaLanguagesMissing, path)
}

func normalizeSerenaLanguageList(values *[]string) ([]string, bool) {
	if values == nil {
		return nil, false
	}
	out := make([]string, 0, len(*values))
	for _, value := range *values {
		if clean := strings.TrimSpace(value); clean != "" {
			out = append(out, clean)
		}
	}
	return out, len(out) != 0
}

func normalizeSerenaLanguage(value *string) ([]string, bool) {
	if value == nil || strings.TrimSpace(*value) == "" {
		return nil, false
	}
	return []string{strings.TrimSpace(*value)}, true
}

func sameStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

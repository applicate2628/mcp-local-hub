package api

import (
	"errors"
	"strings"
	"testing"
)

// fakeSecretLookup builds a SecretLookup from a map. Missing keys
// return an error matching the production "secret not found" shape.
func fakeSecretLookup(secrets map[string]string) SecretLookup {
	return func(key string) (string, error) {
		v, ok := secrets[key]
		if !ok {
			return "", errors.New("secret " + key + " not found")
		}
		return v, nil
	}
}

func TestExpandSecrets_HappyPath(t *testing.T) {
	got, err := ExpandSecrets("Bearer ${secret:CONTEXT7_TOKEN}", fakeSecretLookup(map[string]string{
		"CONTEXT7_TOKEN": "abc123",
	}))
	if err != nil {
		t.Fatalf("ExpandSecrets: %v", err)
	}
	if got != "Bearer abc123" {
		t.Errorf("got %q, want %q", got, "Bearer abc123")
	}
}

func TestExpandSecrets_MultiplePlaceholders(t *testing.T) {
	got, err := ExpandSecrets(
		"Bearer ${secret:TOKEN}, X-Tenant: ${secret:TENANT}",
		fakeSecretLookup(map[string]string{"TOKEN": "tk", "TENANT": "acme"}),
	)
	if err != nil {
		t.Fatalf("ExpandSecrets: %v", err)
	}
	if got != "Bearer tk, X-Tenant: acme" {
		t.Errorf("got %q", got)
	}
}

func TestExpandSecrets_NoPlaceholdersIsPassthrough(t *testing.T) {
	// Should not call the lookup at all.
	calls := 0
	lookup := func(key string) (string, error) {
		calls++
		return "x", nil
	}
	got, err := ExpandSecrets("Bearer literal-token", lookup)
	if err != nil {
		t.Fatalf("ExpandSecrets: %v", err)
	}
	if got != "Bearer literal-token" {
		t.Errorf("got %q", got)
	}
	if calls != 0 {
		t.Errorf("lookup called %d times for literal string; want 0", calls)
	}
}

func TestExpandSecrets_MissingKeyErrors(t *testing.T) {
	_, err := ExpandSecrets("Bearer ${secret:MISSING}", fakeSecretLookup(nil))
	if err == nil {
		t.Fatal("expected error for missing key; got nil")
	}
	if !strings.Contains(err.Error(), "MISSING") {
		t.Errorf("error must name the missing key for operator forensics; got %v", err)
	}
}

func TestExpandSecrets_CRLFRejected(t *testing.T) {
	for _, tc := range []struct {
		name string
		val  string
	}{
		{"contains LF", "abc\ndef"},
		{"contains CR", "abc\rdef"},
		{"contains CRLF", "abc\r\ndef"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := ExpandSecrets("Bearer ${secret:HOSTILE}", fakeSecretLookup(map[string]string{
				"HOSTILE": tc.val,
			}))
			if err == nil {
				t.Fatal("expected CRLF-injection rejection; got nil")
			}
			if !strings.Contains(err.Error(), "HOSTILE") {
				t.Errorf("error must name the key for forensics; got %v", err)
			}
			// CRITICAL: error message must NOT leak the cleartext value.
			if strings.Contains(err.Error(), tc.val) {
				t.Errorf("error leaks cleartext value: %v", err)
			}
		})
	}
}

func TestExpandSecretsMap_HappyPath(t *testing.T) {
	got, err := ExpandSecretsMap(
		map[string]string{
			"Authorization": "Bearer ${secret:TOKEN}",
			"X-Tenant":      "${secret:TENANT}",
			"Static-Header": "no-placeholder",
		},
		fakeSecretLookup(map[string]string{"TOKEN": "tk", "TENANT": "acme"}),
	)
	if err != nil {
		t.Fatalf("ExpandSecretsMap: %v", err)
	}
	if got["Authorization"] != "Bearer tk" {
		t.Errorf("Authorization = %q", got["Authorization"])
	}
	if got["X-Tenant"] != "acme" {
		t.Errorf("X-Tenant = %q", got["X-Tenant"])
	}
	if got["Static-Header"] != "no-placeholder" {
		t.Errorf("Static-Header = %q", got["Static-Header"])
	}
}

func TestExpandSecretsMap_DoesNotMutateInput(t *testing.T) {
	input := map[string]string{"Auth": "${secret:T}"}
	_, err := ExpandSecretsMap(input, fakeSecretLookup(map[string]string{"T": "expanded"}))
	if err != nil {
		t.Fatalf("ExpandSecretsMap: %v", err)
	}
	if input["Auth"] != "${secret:T}" {
		t.Errorf("input map mutated; pre-call %q, post-call %q", "${secret:T}", input["Auth"])
	}
}

func TestExpandSecretsMap_MissingKeyErrorNamesHeader(t *testing.T) {
	_, err := ExpandSecretsMap(
		map[string]string{"Authorization": "Bearer ${secret:NOPE}"},
		fakeSecretLookup(nil),
	)
	if err == nil {
		t.Fatal("expected error; got nil")
	}
	if !strings.Contains(err.Error(), "Authorization") {
		t.Errorf("error must name the failing HEADER for operator forensics; got %v", err)
	}
}

func TestExpandSecretsMap_EmptyIsBenign(t *testing.T) {
	got, err := ExpandSecretsMap(nil, nil)
	if err != nil {
		t.Fatalf("empty map err: %v", err)
	}
	if got != nil {
		t.Errorf("nil-in should pass through as nil; got %v", got)
	}
}

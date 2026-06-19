package secrets

import (
	"os"
	"path/filepath"
	"testing"
)

func TestResolver_Secret(t *testing.T) {
	dir := t.TempDir()
	keyPath := filepath.Join(dir, ".age-key")
	vaultPath := filepath.Join(dir, "secrets.age")
	_ = InitVault(keyPath, vaultPath)
	v, _ := OpenVault(keyPath, vaultPath)
	v.Set("API_KEY", "xyz123")

	r := NewResolver(v, nil)
	got, err := r.Resolve("secret:API_KEY")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if got != "xyz123" {
		t.Errorf("Resolve = %q, want xyz123", got)
	}
}

func TestResolver_File(t *testing.T) {
	local := map[string]string{"email": "user@example.com"}
	r := NewResolver(nil, local)
	got, err := r.Resolve("file:email")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if got != "user@example.com" {
		t.Errorf("Resolve = %q, want user@example.com", got)
	}
}

func TestResolver_Env(t *testing.T) {
	t.Setenv("MCP_TEST_VAR", "env-value")
	r := NewResolver(nil, nil)
	got, err := r.Resolve("$MCP_TEST_VAR")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if got != "env-value" {
		t.Errorf("Resolve = %q, want env-value", got)
	}
}

func TestResolver_Literal(t *testing.T) {
	r := NewResolver(nil, nil)
	got, err := r.Resolve("plain-text")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if got != "plain-text" {
		t.Errorf("Resolve = %q, want plain-text", got)
	}
}

func TestResolver_SecretMissing(t *testing.T) {
	dir := t.TempDir()
	keyPath := filepath.Join(dir, ".age-key")
	vaultPath := filepath.Join(dir, "secrets.age")
	_ = InitVault(keyPath, vaultPath)
	v, _ := OpenVault(keyPath, vaultPath)

	r := NewResolver(v, nil)
	if _, err := r.Resolve("secret:NONEXISTENT"); err == nil {
		t.Error("expected error for missing secret, got nil")
	}
}

func TestResolver_EnvMissing(t *testing.T) {
	// Ensure variable is not set
	os.Unsetenv("MCP_DEFINITELY_NOT_SET")
	r := NewResolver(nil, nil)
	if _, err := r.Resolve("$MCP_DEFINITELY_NOT_SET"); err == nil {
		t.Error("expected error for missing env var, got nil")
	}
}

func TestResolveMapBestEffort_OmitsMissingSecret(t *testing.T) {
	// nil vault → any secret: ref is unresolvable. A literal resolves.
	r := NewResolver(nil, nil)
	resolved, omitted, err := r.ResolveMapBestEffort(map[string]string{
		"LITERAL": "hello",
		"WOLFRAM": "secret:wolfram_app_id",
	})
	if err != nil {
		t.Fatalf("secret-only omission must not error: %v", err)
	}
	if resolved["LITERAL"] != "hello" {
		t.Errorf("literal not resolved: %v", resolved)
	}
	if _, present := resolved["WOLFRAM"]; present {
		t.Errorf("missing secret was NOT omitted: %v", resolved)
	}
	if omitted["WOLFRAM"] != "secret:wolfram_app_id" {
		t.Errorf("omitted map missing WOLFRAM ref: %v", omitted)
	}
}

func TestResolveMapBestEffort_NeverErrorsAllResolvable(t *testing.T) {
	r := NewResolver(nil, nil)
	resolved, omitted, err := r.ResolveMapBestEffort(map[string]string{"A": "x", "B": "y"})
	if err != nil {
		t.Fatalf("all-resolvable must not error: %v", err)
	}
	if len(resolved) != 2 || len(omitted) != 0 {
		t.Errorf("resolved=%v omitted=%v; want 2 resolved, 0 omitted", resolved, omitted)
	}
}

func TestResolveMapBestEffort_NonSecretRefStaysFatal(t *testing.T) {
	r := NewResolver(nil, nil)
	// A missing $VAR is a documented REQUIRED input — it must stay fail-fast,
	// not be silently omitted like an optional secret (Codex #377).
	_, _, err := r.ResolveMapBestEffort(map[string]string{"TOK": "$DEFINITELY_UNSET_VAR_ZZZ"})
	if err == nil {
		t.Fatal("missing $VAR ref must return an error (non-secret refs are required)")
	}
}

func TestHasSecretRef(t *testing.T) {
	if !HasSecretRef(map[string]string{"A": "literal", "B": "secret:k"}) {
		t.Error("HasSecretRef missed a secret: ref")
	}
	if HasSecretRef(map[string]string{"A": "literal", "B": "$VAR", "C": "file:k"}) {
		t.Error("HasSecretRef false-positive on non-secret refs")
	}
}

func TestOpenVaultOptional_AbsentIsNilNil(t *testing.T) {
	dir := t.TempDir()
	// No vault file at this path → absent → (nil, nil), secrets optional.
	v, err := OpenVaultOptional(dir+"/nokey", dir+"/novault")
	if err != nil {
		t.Errorf("absent vault must be (nil,nil); got err=%v", err)
	}
	if v != nil {
		t.Errorf("absent vault must return nil vault; got %v", v)
	}
}

func TestOpenVaultOptional_UnreadableIsError(t *testing.T) {
	// The branch OpenVaultOptional exists for: a vault that is PRESENT but can
	// no longer be decrypted/parsed must return an ERROR (fatal for a
	// secret-using manifest), NOT be silently treated as absent → (nil,nil)
	// (Codex #377 r5 / merge-gate P1). Without this distinction a corrupt vault
	// would silently omit every secret.
	dir := t.TempDir()
	keyPath := filepath.Join(dir, ".age-key")
	vaultPath := filepath.Join(dir, "secrets.age")
	if err := InitVault(keyPath, vaultPath); err != nil {
		t.Fatalf("InitVault: %v", err)
	}
	// Corrupt the vault file: it still EXISTS (os.Stat succeeds, so the
	// absent-branch is NOT taken) but the ciphertext is now garbage.
	if err := os.WriteFile(vaultPath, []byte("not a valid age ciphertext"), 0o600); err != nil {
		t.Fatalf("corrupt vault: %v", err)
	}
	v, err := OpenVaultOptional(keyPath, vaultPath)
	if err == nil {
		t.Fatal("present-but-unreadable vault must return an error, not (nil,nil)")
	}
	if v != nil {
		t.Errorf("unreadable vault must return nil vault; got %v", v)
	}
}

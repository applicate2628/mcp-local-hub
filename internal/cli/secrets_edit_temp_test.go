package cli

import (
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// swapSecretsEditTempCreate replaces the owner-only secure-create seam
// (secureCreateOwnerOnlyFileFn) with fn for the duration of the test and
// restores it on cleanup, so a test can spy on how `mcphub secrets edit`
// creates its decrypted-vault temp.
func swapSecretsEditTempCreate(t *testing.T, fn func(path string, contents []byte) (bool, error)) {
	t.Helper()
	orig := secureCreateOwnerOnlyFileFn
	secureCreateOwnerOnlyFileFn = fn
	t.Cleanup(func() { secureCreateOwnerOnlyFileFn = orig })
}

// The hardened path names its temp mcp-secrets-<32 hex>.yaml (128-bit
// crypto/rand suffix). os.CreateTemp("mcp-secrets-*.yaml") would instead
// produce a variable-length random-DIGIT name, so this pattern doubles as
// a negative control against a regression to os.CreateTemp.
var secretsEditTempNameRe = regexp.MustCompile(`^mcp-secrets-[0-9a-f]{32}\.yaml$`)

// TestSecureCreateSecretsEditTemp_RoutesThroughSecureCreatePrimitive
// proves the decrypted-vault temp is written through the owner-only
// secure-create seam (never os.CreateTemp, which on Windows inherits the
// parent %LOCALAPPDATA% ACL). Negative controls: (1) if the code used
// os.CreateTemp the spy would never fire (len(calls)==0 fails); (2) the
// name would not match the hardened 32-hex pattern.
func TestSecureCreateSecretsEditTemp_RoutesThroughSecureCreatePrimitive(t *testing.T) {
	dir := t.TempDir()
	payload := []byte("API_KEY: sk-live-DEADBEEF\nDB_URL: postgres://u:p@h/db\n")

	type call struct {
		path     string
		contents []byte
	}
	var calls []call
	swapSecretsEditTempCreate(t, func(path string, contents []byte) (bool, error) {
		// Record, then actually create the owner-only file so the caller
		// gets a real path. Mode 0600 mirrors the primitive's POSIX
		// posture; the real Windows DACL is asserted by the Windows-tagged
		// test.
		calls = append(calls, call{path: path, contents: append([]byte(nil), contents...)})
		if err := os.WriteFile(path, contents, 0o600); err != nil {
			return false, err
		}
		return true, nil
	})

	got, err := secureCreateSecretsEditTemp(dir, payload)
	if err != nil {
		t.Fatalf("secureCreateSecretsEditTemp: %v", err)
	}

	if len(calls) != 1 {
		t.Fatalf("expected exactly 1 secure-create seam call (routing negative control), got %d", len(calls))
	}
	if calls[0].path != got {
		t.Fatalf("returned path %q != seam path %q", got, calls[0].path)
	}
	if string(calls[0].contents) != string(payload) {
		t.Fatalf("seam received contents %q, want the decrypted vault %q", calls[0].contents, payload)
	}

	base := filepath.Base(got)
	if !secretsEditTempNameRe.MatchString(base) {
		t.Fatalf("edit temp name %q does not match hardened pattern %s (naming negative control)", base, secretsEditTempNameRe)
	}
	if filepath.Clean(filepath.Dir(got)) != filepath.Clean(dir) {
		t.Fatalf("edit temp %q not under editDir %q", got, dir)
	}

	onDisk, err := os.ReadFile(got)
	if err != nil {
		t.Fatalf("read edit temp: %v", err)
	}
	if string(onDisk) != string(payload) {
		t.Fatalf("edit temp content = %q, want %q", onDisk, payload)
	}

	// Exactly the hardened temp exists under editDir — no stray
	// os.CreateTemp artifact alongside it.
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("readdir: %v", err)
	}
	if len(entries) != 1 || entries[0].Name() != base {
		var names []string
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Fatalf("expected exactly the hardened temp %q under editDir, got %v", base, names)
	}
}

// TestSecureCreateSecretsEditTemp_OExclRetryOnNameCollision covers the
// create-if-missing retry: a name already taken (created=false, err=nil)
// must retry with a fresh, DISTINCT random name rather than reuse a file
// it did not write.
func TestSecureCreateSecretsEditTemp_OExclRetryOnNameCollision(t *testing.T) {
	dir := t.TempDir()
	payload := []byte("K: v\n")

	var paths []string
	var seenContents [][]byte
	swapSecretsEditTempCreate(t, func(path string, contents []byte) (bool, error) {
		paths = append(paths, path)
		seenContents = append(seenContents, append([]byte(nil), contents...))
		if len(paths) < 3 {
			return false, nil // first two names "taken"
		}
		if err := os.WriteFile(path, contents, 0o600); err != nil {
			return false, err
		}
		return true, nil
	})

	got, err := secureCreateSecretsEditTemp(dir, payload)
	if err != nil {
		t.Fatalf("secureCreateSecretsEditTemp: %v", err)
	}
	if len(paths) != 3 {
		t.Fatalf("expected 3 name attempts, got %d (%v)", len(paths), paths)
	}
	if got != paths[2] {
		t.Fatalf("returned %q, want the 3rd attempt %q", got, paths[2])
	}
	uniq := map[string]bool{}
	for _, p := range paths {
		if uniq[p] {
			t.Fatalf("name reused across retries: %q in %v", p, paths)
		}
		uniq[p] = true
	}
	for i, c := range seenContents {
		if string(c) != string(payload) {
			t.Fatalf("attempt %d contents = %q, want %q", i, c, payload)
		}
	}
}

// TestSecureCreateSecretsEditTemp_ExhaustsUniqueNameAttempts asserts the
// retry loop fails loud (never spins) when every name is "taken".
func TestSecureCreateSecretsEditTemp_ExhaustsUniqueNameAttempts(t *testing.T) {
	dir := t.TempDir()
	calls := 0
	swapSecretsEditTempCreate(t, func(path string, contents []byte) (bool, error) {
		calls++
		return false, nil // always "name taken"
	})
	got, err := secureCreateSecretsEditTemp(dir, []byte("x"))
	if err == nil {
		t.Fatalf("expected error on exhausted attempts, got nil (path %q)", got)
	}
	if got != "" {
		t.Fatalf("expected empty path on failure, got %q", got)
	}
	if calls != secretsEditTempMaxNameAttempts {
		t.Fatalf("expected %d attempts, got %d", secretsEditTempMaxNameAttempts, calls)
	}
	if !strings.Contains(err.Error(), "exhausted") {
		t.Fatalf("error %q should mention exhaustion", err)
	}
}

// TestSecureCreateSecretsEditTemp_SurfacesHardErrorWithoutRetry asserts a
// hard secure-create failure (symlink refusal, pipeline error) is
// surfaced immediately and NOT retried under a different name.
func TestSecureCreateSecretsEditTemp_SurfacesHardErrorWithoutRetry(t *testing.T) {
	dir := t.TempDir()
	sentinel := errors.New("secure create: refuse to initialize through symlink")
	calls := 0
	swapSecretsEditTempCreate(t, func(path string, contents []byte) (bool, error) {
		calls++
		return false, sentinel
	})
	got, err := secureCreateSecretsEditTemp(dir, []byte("x"))
	if err == nil {
		t.Fatalf("expected hard error, got nil (path %q)", got)
	}
	if !errors.Is(err, sentinel) {
		t.Fatalf("error %v should wrap the sentinel refusal", err)
	}
	if calls != 1 {
		t.Fatalf("hard error must NOT retry; got %d seam calls", calls)
	}
	if got != "" {
		t.Fatalf("expected empty path on failure, got %q", got)
	}
}

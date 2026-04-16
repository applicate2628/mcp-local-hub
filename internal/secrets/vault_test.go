package secrets

import (
	"os"
	"path/filepath"
	"testing"
)

func TestVault_InitSetGet(t *testing.T) {
	dir := t.TempDir()
	keyPath := filepath.Join(dir, ".age-key")
	vaultPath := filepath.Join(dir, "secrets.age")

	if err := InitVault(keyPath, vaultPath); err != nil {
		t.Fatalf("InitVault: %v", err)
	}
	if _, err := os.Stat(keyPath); err != nil {
		t.Fatalf("identity file missing: %v", err)
	}
	if _, err := os.Stat(vaultPath); err != nil {
		t.Fatalf("vault file missing: %v", err)
	}

	v, err := OpenVault(keyPath, vaultPath)
	if err != nil {
		t.Fatalf("OpenVault: %v", err)
	}
	if err := v.Set("API_KEY", "super-secret-value"); err != nil {
		t.Fatalf("Set: %v", err)
	}
	got, err := v.Get("API_KEY")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got != "super-secret-value" {
		t.Errorf("Get = %q, want super-secret-value", got)
	}

	// Reopen vault with same identity — value should persist.
	v2, err := OpenVault(keyPath, vaultPath)
	if err != nil {
		t.Fatalf("OpenVault reopen: %v", err)
	}
	got2, err := v2.Get("API_KEY")
	if err != nil {
		t.Fatalf("Get reopen: %v", err)
	}
	if got2 != "super-secret-value" {
		t.Errorf("persisted value = %q, want super-secret-value", got2)
	}
}

func TestVault_List(t *testing.T) {
	dir := t.TempDir()
	keyPath := filepath.Join(dir, ".age-key")
	vaultPath := filepath.Join(dir, "secrets.age")
	_ = InitVault(keyPath, vaultPath)

	v, _ := OpenVault(keyPath, vaultPath)
	v.Set("A", "1")
	v.Set("B", "2")
	v.Set("C", "3")

	keys := v.List()
	if len(keys) != 3 {
		t.Fatalf("List = %v (len %d), want 3 keys", keys, len(keys))
	}
}

func TestVault_Delete(t *testing.T) {
	dir := t.TempDir()
	keyPath := filepath.Join(dir, ".age-key")
	vaultPath := filepath.Join(dir, "secrets.age")
	_ = InitVault(keyPath, vaultPath)

	v, _ := OpenVault(keyPath, vaultPath)
	v.Set("A", "1")
	if err := v.Delete("A"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, err := v.Get("A"); err == nil {
		t.Error("expected error for deleted key, got nil")
	}
}

func TestVault_WrongIdentity(t *testing.T) {
	dir := t.TempDir()
	keyPath := filepath.Join(dir, ".age-key")
	vaultPath := filepath.Join(dir, "secrets.age")
	_ = InitVault(keyPath, vaultPath)

	v, _ := OpenVault(keyPath, vaultPath)
	v.Set("X", "1")

	// Create a second identity and try to open the vault with it.
	wrongKey := filepath.Join(dir, ".age-key-wrong")
	wrongVault := filepath.Join(dir, "wrong.age")
	_ = InitVault(wrongKey, wrongVault)

	if _, err := OpenVault(wrongKey, vaultPath); err == nil {
		t.Error("OpenVault with wrong identity should fail, got nil error")
	}
}

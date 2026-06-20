package secrets

import (
	"bytes"
	"errors"
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

func TestVaultSaveAtomicRenameFailurePreservesExistingVault(t *testing.T) {
	dir := t.TempDir()
	keyPath := filepath.Join(dir, ".age-key")
	vaultPath := filepath.Join(dir, "secrets.age")
	if err := InitVault(keyPath, vaultPath); err != nil {
		t.Fatalf("InitVault: %v", err)
	}
	v, err := OpenVault(keyPath, vaultPath)
	if err != nil {
		t.Fatalf("OpenVault: %v", err)
	}
	before, err := os.ReadFile(vaultPath)
	if err != nil {
		t.Fatalf("read existing vault: %v", err)
	}

	sentinel := errors.New("synthetic rename failure")
	previousRename := vaultAtomicRenameFile
	var tempPath string
	vaultAtomicRenameFile = func(src, dst string) error {
		if dst != vaultPath {
			return previousRename(src, dst)
		}
		tempPath = src
		if filepath.Dir(src) != dir {
			t.Fatalf("atomic temp dir = %q, want sibling dir %q", filepath.Dir(src), dir)
		}
		if src == vaultPath {
			t.Fatalf("atomic temp path reused destination %q", vaultPath)
		}
		if _, statErr := os.Stat(src); statErr != nil {
			t.Fatalf("atomic temp missing before rename: %v", statErr)
		}
		return sentinel
	}
	t.Cleanup(func() { vaultAtomicRenameFile = previousRename })

	err = v.Set("API_KEY", "new-value")
	if !errors.Is(err, sentinel) {
		t.Fatalf("Set err = %v, want synthetic rename failure", err)
	}
	if tempPath == "" {
		t.Fatalf("vault save did not attempt sibling temp rename")
	}
	after, err := os.ReadFile(vaultPath)
	if err != nil {
		t.Fatalf("read vault after failed save: %v", err)
	}
	if !bytes.Equal(after, before) {
		t.Fatalf("vault destination changed after failed atomic rename")
	}
	if _, statErr := os.Stat(tempPath); !os.IsNotExist(statErr) {
		t.Fatalf("atomic temp %q was not cleaned up after failed rename: %v", tempPath, statErr)
	}
}

func TestInitVaultIdentityWriteUsesAtomicSiblingTemp(t *testing.T) {
	dir := t.TempDir()
	keyPath := filepath.Join(dir, ".age-key")
	vaultPath := filepath.Join(dir, "secrets.age")

	sentinel := errors.New("synthetic identity rename failure")
	previousRename := vaultAtomicRenameFile
	var identityTemp string
	vaultAtomicRenameFile = func(src, dst string) error {
		if dst != keyPath {
			return previousRename(src, dst)
		}
		identityTemp = src
		if filepath.Dir(src) != dir {
			t.Fatalf("identity temp dir = %q, want sibling dir %q", filepath.Dir(src), dir)
		}
		if src == keyPath {
			t.Fatalf("identity temp path reused destination %q", keyPath)
		}
		if _, statErr := os.Stat(src); statErr != nil {
			t.Fatalf("identity temp missing before rename: %v", statErr)
		}
		return sentinel
	}
	t.Cleanup(func() { vaultAtomicRenameFile = previousRename })

	err := InitVault(keyPath, vaultPath)
	if !errors.Is(err, sentinel) {
		t.Fatalf("InitVault err = %v, want synthetic identity rename failure", err)
	}
	if identityTemp == "" {
		t.Fatalf("identity write did not attempt sibling temp rename")
	}
	if _, statErr := os.Stat(keyPath); !os.IsNotExist(statErr) {
		t.Fatalf("identity destination exists after failed atomic rename: %v", statErr)
	}
	if _, statErr := os.Stat(vaultPath); !os.IsNotExist(statErr) {
		t.Fatalf("vault destination exists even though identity write failed first: %v", statErr)
	}
	if _, statErr := os.Stat(identityTemp); !os.IsNotExist(statErr) {
		t.Fatalf("identity temp %q was not cleaned up after failed rename: %v", identityTemp, statErr)
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

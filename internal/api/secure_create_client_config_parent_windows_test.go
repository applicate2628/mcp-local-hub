//go:build windows

package api

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestSecureCreateClientConfigParentDir_WindowsRefusesVerifiedComponentJunctionSwap(t *testing.T) {
	home := hardenedTempDir(t)
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)

	attackerTarget := filepath.Join(t.TempDir(), "junction-target")
	if err := os.Mkdir(attackerTarget, 0o700); err != nil {
		t.Fatalf("mkdir attacker target: %v", err)
	}

	swapped := filepath.Join(home, ".swappable")
	sawHook := false
	origHook := secureCreateClientConfigParentDirAfterVerifyHook
	secureCreateClientConfigParentDirAfterVerifyHook = func(verifiedPath string) error {
		if filepath.Clean(verifiedPath) != filepath.Clean(swapped) {
			return nil
		}
		sawHook = true
		if err := os.Remove(verifiedPath); err != nil {
			return fmt.Errorf("remove verified component for junction swap: %w", err)
		}
		if err := createJunctionForTest(verifiedPath, attackerTarget); err != nil {
			return fmt.Errorf("create junction swap: %w", err)
		}
		return nil
	}
	t.Cleanup(func() { secureCreateClientConfigParentDirAfterVerifyHook = origHook })

	cfg := filepath.Join(swapped, "User", "mcp.json")
	err := SecureCreateClientConfigParentDir(cfg)
	if !sawHook {
		t.Fatal("test hook did not observe the verified component")
	}
	if err == nil {
		t.Fatal("expected refusal after verified component junction swap, got nil")
	}
	if _, statErr := os.Stat(filepath.Join(attackerTarget, "User")); statErr == nil {
		t.Fatalf("junction target received child directory despite refusal: %s", filepath.Join(attackerTarget, "User"))
	} else if !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("stat attacker target child: %v", statErr)
	}
}

func TestSecureCreateClientConfigParentDir_WindowsStrictRefusesBroadenedExistingPrefix(t *testing.T) {
	home := hardenedTempDir(t)
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)

	broadenedPrefix := filepath.Join(home, ".broadened")
	if err := os.Mkdir(broadenedPrefix, 0o700); err != nil {
		t.Fatalf("mkdir broadened prefix: %v", err)
	}
	synthesizeDirWithInheritableAuthUsersReadACE(t, broadenedPrefix)

	cfg := filepath.Join(broadenedPrefix, "User", "mcp.json")
	err := SecureCreateClientConfigParentDir(cfg)
	if err == nil {
		t.Fatal("expected strict DACL refusal for broadened existing prefix, got nil")
	}
	if !errors.Is(err, ErrSecureWriteParentInsecure) {
		t.Fatalf("expected ErrSecureWriteParentInsecure, got %v", err)
	}
	if _, statErr := os.Stat(filepath.Join(broadenedPrefix, "User")); statErr == nil {
		t.Fatalf("strict prefix-DACL refusal still created child directory: %s", filepath.Join(broadenedPrefix, "User"))
	} else if !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("stat child under broadened prefix: %v", statErr)
	}
}

func createJunctionForTest(link, target string) error {
	cmd := exec.Command("cmd", "/c", "mklink", "/J", link, target)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("mklink /J: %w: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

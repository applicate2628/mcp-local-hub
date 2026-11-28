// Package api — config bundle export. Memo D11.
package api

import (
	"archive/zip"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

var mcphubVersionForBundle = func() string { return "dev" }

// PreflightExportBundle checks that the data and state directories can be
// resolved before the handler commits stream headers. Returns nil if both
// resolve (even if missing — bundle composition gracefully skips missing
// optional files); returns an error only if the path resolution itself fails.
// Memo D11.
func PreflightExportBundle() (warnings []string, err error) {
	if _, err := dataDirForBundle(); err != nil {
		return nil, fmt.Errorf("locate data dir: %w", err)
	}
	if _, err := stateDirForBundle(); err != nil {
		return nil, fmt.Errorf("locate state dir: %w", err)
	}
	return nil, nil
}

// WriteConfigBundle writes a .zip stream to w containing all config
// artifacts (memo D11). Returns nil on success; partial writes propagate
// the underlying io.Writer error.
func WriteConfigBundle(w io.Writer) error {
	zw := zip.NewWriter(w)
	defer zw.Close()

	dataDir, err := dataDirForBundle()
	if err != nil {
		return fmt.Errorf("locate data dir: %w", err)
	}
	stateDir, err := stateDirForBundle()
	if err != nil {
		return fmt.Errorf("locate state dir: %w", err)
	}

	// servers/<name>/manifest.yaml — walk the servers folder.
	serversRoot := filepath.Join(dataDir, "servers")
	if err := addDirGlob(zw, serversRoot, "servers", "manifest.yaml"); err != nil {
		return fmt.Errorf("add servers: %w", err)
	}
	// Top-level data files. The encrypted vault is `secrets.age` (per
	// internal/secrets/paths.go DefaultVaultPath), NOT `secrets.json`.
	// PR #51 originally referenced secrets.json — the file never existed,
	// so the bundle silently shipped without secrets. Fixed via Codex
	// bot PR #93 finding (closed in favor of this in-PR fix).
	if err := addFileIfExists(zw, filepath.Join(dataDir, "secrets.age"), "secrets.age"); err != nil {
		return fmt.Errorf("add secrets.age: %w", err)
	}
	if err := addPortableSettingsIfExists(zw, filepath.Join(dataDir, "gui-preferences.yaml")); err != nil {
		return fmt.Errorf("add gui-preferences.yaml: %w", err)
	}
	// State-dir files.
	if err := addFileIfExists(zw, filepath.Join(stateDir, "workspaces.yaml"), "workspaces.yaml"); err != nil {
		return fmt.Errorf("add workspaces.yaml: %w", err)
	}
	// bundle-meta.json
	meta := map[string]string{
		"export_time":    time.Now().UTC().Format(time.RFC3339),
		"mcphub_version": mcphubVersionForBundle(),
		"platform":       runtime.GOOS + "/" + runtime.GOARCH,
		"hostname":       "redacted", // Memo D11: literal string, not <host>, not omitted.
	}
	metaBytes, _ := json.MarshalIndent(meta, "", "  ")
	fh, err := zw.Create("bundle-meta.json")
	if err != nil {
		return err
	}
	if _, err := fh.Write(metaBytes); err != nil {
		return err
	}
	return nil
}

func addFileIfExists(zw *zip.Writer, src, dstName string) error {
	if strings.Contains(filepath.Base(src), ".bak.") {
		return nil
	}
	data, err := os.ReadFile(src)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	return addBundleFileBytes(zw, dstName, data)
}

func addPortableSettingsIfExists(zw *zip.Writer, path string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	raw, err := decodeSingleSettingsDocument(data)
	if err != nil {
		return err
	}
	if _, exists := raw[hubProjectAuthoritySettingsKey]; exists {
		delete(raw, hubProjectAuthoritySettingsKey)
		data, err = yaml.Marshal(raw)
		if err != nil {
			return errors.New("cannot encode portable settings content")
		}
	}
	return addBundleFileBytes(zw, filepath.Base(path), data)
}

func addBundleFileBytes(zw *zip.Writer, dstName string, data []byte) error {
	fh, err := zw.Create(dstName)
	if err != nil {
		return err
	}
	_, err = fh.Write(data)
	return err
}

func addDirGlob(zw *zip.Writer, srcRoot, dstPrefix, fileName string) error {
	if _, err := os.Stat(srcRoot); err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	return filepath.Walk(srcRoot, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return err
		}
		if filepath.Base(path) != fileName {
			return nil
		}
		if strings.Contains(path, ".bak.") {
			return nil
		}
		rel, err := filepath.Rel(srcRoot, path)
		if err != nil {
			return err
		}
		dstName := dstPrefix + "/" + filepath.ToSlash(rel)
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		fh, err := zw.Create(dstName)
		if err != nil {
			return err
		}
		_, err = fh.Write(data)
		return err
	})
}

func dataDirForBundle() (string, error) {
	if runtime.GOOS == "windows" {
		if v := os.Getenv("LOCALAPPDATA"); v != "" {
			return filepath.Join(v, "mcp-local-hub"), nil
		}
	}
	if v := os.Getenv("XDG_DATA_HOME"); v != "" {
		return filepath.Join(v, "mcp-local-hub"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".local", "share", "mcp-local-hub"), nil
}

// stateDirForBundle returns the directory containing state files such as
// workspaces.yaml. XDG_STATE_HOME is checked first on all platforms so that
// tests can redirect the state dir via that env var even on Windows (where
// LOCALAPPDATA redirects the data dir). In production on Windows, XDG_STATE_HOME
// is not set, so the LOCALAPPDATA branch fires as the effective fallback.
func stateDirForBundle() (string, error) {
	if v := os.Getenv("XDG_STATE_HOME"); v != "" {
		return filepath.Join(v, "mcp-local-hub"), nil
	}
	if runtime.GOOS == "windows" {
		if v := os.Getenv("LOCALAPPDATA"); v != "" {
			return filepath.Join(v, "mcp-local-hub"), nil
		}
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".local", "state", "mcp-local-hub"), nil
}

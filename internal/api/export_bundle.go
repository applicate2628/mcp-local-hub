// Package api — config bundle export. Memo D11.
package api

import (
	"archive/zip"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

var mcphubVersionForBundle = func() string { return "dev" }

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
	// Top-level data files.
	for _, item := range []struct{ src, name string }{
		{filepath.Join(dataDir, "secrets.json"), "secrets.json"},
		{filepath.Join(dataDir, "gui-preferences.yaml"), "gui-preferences.yaml"},
	} {
		if err := addFileIfExists(zw, item.src, item.name); err != nil {
			return fmt.Errorf("add %s: %w", item.name, err)
		}
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

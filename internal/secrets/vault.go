package secrets

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"time"

	"filippo.io/age"
	"gopkg.in/yaml.v3"
)

// Vault is an age-encrypted key/value store persisted in a single file.
// The on-disk format is JSON, age-encrypted with the user's identity.
// In-memory state is kept in `data` and flushed to disk on every mutation.
type Vault struct {
	identity  age.Identity
	recipient age.Recipient
	vaultPath string
	data      map[string]string
}

// InitVault generates a new X25519 identity at keyPath and writes an empty
// encrypted vault to vaultPath. Fails if either file already exists.
func InitVault(keyPath, vaultPath string) error {
	if _, err := os.Stat(keyPath); err == nil {
		return fmt.Errorf("identity file already exists: %s", keyPath)
	}
	if _, err := os.Stat(vaultPath); err == nil {
		return fmt.Errorf("vault file already exists: %s", vaultPath)
	}
	id, err := age.GenerateX25519Identity()
	if err != nil {
		return fmt.Errorf("generate identity: %w", err)
	}
	// Write identity file — plain text so launcher can read it.
	// Filesystem permissions are the only protection in MVP.
	if err := writeVaultFileAtomic(keyPath, []byte(id.String()+"\n"), 0600); err != nil {
		return fmt.Errorf("write identity: %w", err)
	}
	v := &Vault{
		identity:  id,
		recipient: id.Recipient(),
		vaultPath: vaultPath,
		data:      map[string]string{},
	}
	return v.save()
}

// OpenVault reads the identity from keyPath and opens the encrypted vault at vaultPath.
func OpenVault(keyPath, vaultPath string) (*Vault, error) {
	keyBytes, err := os.ReadFile(keyPath)
	if err != nil {
		return nil, fmt.Errorf("read identity: %w", err)
	}
	ids, err := age.ParseIdentities(bytes.NewReader(keyBytes))
	if err != nil {
		return nil, fmt.Errorf("parse identity: %w", err)
	}
	if len(ids) == 0 {
		return nil, fmt.Errorf("no identity in %s", keyPath)
	}
	id := ids[0]

	x25519, ok := id.(*age.X25519Identity)
	if !ok {
		return nil, fmt.Errorf("identity is not X25519 (unsupported)")
	}
	v := &Vault{
		identity:  id,
		recipient: x25519.Recipient(),
		vaultPath: vaultPath,
		data:      map[string]string{},
	}
	if err := v.load(); err != nil {
		return nil, err
	}
	return v, nil
}

// Set stores value under key and persists the vault to disk.
func (v *Vault) Set(key, value string) error {
	v.data[key] = value
	return v.save()
}

// Get returns the value for key or an error if not present.
func (v *Vault) Get(key string) (string, error) {
	val, ok := v.data[key]
	if !ok {
		return "", fmt.Errorf("secret %q not found", key)
	}
	return val, nil
}

// Delete removes key and persists the vault to disk.
func (v *Vault) Delete(key string) error {
	if _, ok := v.data[key]; !ok {
		return fmt.Errorf("secret %q not found", key)
	}
	delete(v.data, key)
	return v.save()
}

// List returns all secret keys, sorted alphabetically.
func (v *Vault) List() []string {
	keys := make([]string, 0, len(v.data))
	for k := range v.data {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// save encrypts v.data as JSON and atomically writes to v.vaultPath.
func (v *Vault) save() error {
	raw, err := json.Marshal(v.data)
	if err != nil {
		return fmt.Errorf("marshal vault: %w", err)
	}
	var buf bytes.Buffer
	w, err := age.Encrypt(&buf, v.recipient)
	if err != nil {
		return fmt.Errorf("age encrypt init: %w", err)
	}
	if _, err := w.Write(raw); err != nil {
		return fmt.Errorf("age encrypt write: %w", err)
	}
	if err := w.Close(); err != nil {
		return fmt.Errorf("age encrypt close: %w", err)
	}
	return writeVaultFileAtomic(v.vaultPath, buf.Bytes(), 0600)
}

var (
	vaultAtomicRenameFile    = os.Rename
	vaultAtomicSyncParentDir = syncParentDir
)

func writeVaultFileAtomic(path string, data []byte, perm os.FileMode) error {
	dir := filepath.Dir(path)
	base := filepath.Base(path)
	tempName := filepath.Join(dir, fmt.Sprintf(".%s.tmp-%d-%d", base, os.Getpid(), time.Now().UnixNano()))
	tempFile, err := os.OpenFile(tempName, os.O_WRONLY|os.O_CREATE|os.O_EXCL, perm)
	if err != nil {
		return fmt.Errorf("open temp: %w", err)
	}
	tempOpen := true
	cleanupTemp := func() {
		if tempOpen {
			_ = tempFile.Close()
		}
		_ = os.Remove(tempName)
	}

	if _, err := tempFile.Write(data); err != nil {
		cleanupTemp()
		return fmt.Errorf("write temp: %w", err)
	}
	if err := tempFile.Sync(); err != nil {
		cleanupTemp()
		return fmt.Errorf("sync temp: %w", err)
	}
	if err := tempFile.Close(); err != nil {
		tempOpen = false
		_ = os.Remove(tempName)
		return fmt.Errorf("close temp: %w", err)
	}
	tempOpen = false
	if err := os.Chmod(tempName, perm); err != nil {
		_ = os.Remove(tempName)
		return fmt.Errorf("chmod temp: %w", err)
	}
	if err := vaultAtomicRenameFile(tempName, path); err != nil {
		_ = os.Remove(tempName)
		return fmt.Errorf("rename temp: %w", err)
	}
	if err := vaultAtomicSyncParentDir(dir); err != nil {
		return fmt.Errorf("sync parent dir: %w", err)
	}
	return nil
}

// load reads v.vaultPath, decrypts with v.identity, unmarshals JSON into v.data.
func (v *Vault) load() error {
	raw, err := os.ReadFile(v.vaultPath)
	if err != nil {
		return fmt.Errorf("read vault: %w", err)
	}
	if len(raw) == 0 {
		return nil // empty vault after init
	}
	r, err := age.Decrypt(bytes.NewReader(raw), v.identity)
	if err != nil {
		return fmt.Errorf("age decrypt: %w", err)
	}
	plaintext, err := io.ReadAll(r)
	if err != nil {
		return fmt.Errorf("read decrypted: %w", err)
	}
	if len(plaintext) == 0 {
		return nil
	}
	if err := json.Unmarshal(plaintext, &v.data); err != nil {
		return fmt.Errorf("unmarshal vault: %w", err)
	}
	return nil
}

// ExportYAML returns the current vault contents as YAML bytes (for editor workflow).
func (v *Vault) ExportYAML() ([]byte, error) {
	return yaml.Marshal(v.data)
}

// ImportYAML replaces the entire vault with the contents of raw (YAML key/value map).
// The backing file is re-encrypted with the current identity.
func (v *Vault) ImportYAML(raw []byte) error {
	var m map[string]string
	if err := yaml.Unmarshal(raw, &m); err != nil {
		return fmt.Errorf("import yaml: %w", err)
	}
	if m == nil {
		m = map[string]string{}
	}
	v.data = m
	return v.save()
}

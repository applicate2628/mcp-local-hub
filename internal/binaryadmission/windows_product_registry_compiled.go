package binaryadmission

import (
	"encoding/base64"
	"errors"
)

// Set by the normal builder from the exact validated registry UTF-8 bytes.
// This immutable compiler input is correlation data, never native authority.
var compiledWindowsProductRegistryBase64 string

// ErrCompiledWindowsProductRegistryAbsent denotes only a missing linker input.
// Invalid current input is a distinct terminal error.
var ErrCompiledWindowsProductRegistryAbsent = errors.New("compiled current Windows Product registry is missing")

// CompiledWindowsProductRegistry lazily receives the normal current registry.
// DATA-only tools may use the ordinary codecs without a compiled native input.
func CompiledWindowsProductRegistry() (WindowsProductRegistryV4, error) {
	return parseCompiledWindowsProductRegistry(compiledWindowsProductRegistryBase64)
}

func parseCompiledWindowsProductRegistry(encoded string) (WindowsProductRegistryV4, error) {
	var zero WindowsProductRegistryV4
	if encoded == "" {
		return zero, ErrCompiledWindowsProductRegistryAbsent
	}
	raw, err := base64.StdEncoding.Strict().DecodeString(encoded)
	if err != nil || base64.StdEncoding.EncodeToString(raw) != encoded {
		return zero, errors.Join(errors.New("compiled current Windows Product registry base64 is invalid"), err)
	}
	registry, err := ParseWindowsProductRegistryV4(raw)
	if err != nil {
		return zero, err
	}
	if registry.SchemaVersion != 4 || registry.IdentityManifest.Schema != "windows-product-identity-v2" {
		return zero, errors.New("compiled Windows Product registry requires the current domain")
	}
	return registry, nil
}

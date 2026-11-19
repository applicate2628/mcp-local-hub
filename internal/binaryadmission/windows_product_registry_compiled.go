package binaryadmission

import (
	"encoding/base64"
	"errors"
)

// Set by the normal builder from the exact validated registry UTF-8 bytes.
// This immutable compiler input is correlation data, never native authority.
var compiledWindowsProductRegistryBase64 string

// CompiledWindowsProductRegistry lazily receives the normal current registry.
// DATA-only tools may use the ordinary codecs without a compiled native input.
func CompiledWindowsProductRegistry() (WindowsProductRegistryV4, error) {
	return parseCompiledWindowsProductRegistry(compiledWindowsProductRegistryBase64)
}

func parseCompiledWindowsProductRegistry(encoded string) (WindowsProductRegistryV4, error) {
	var zero WindowsProductRegistryV4
	if encoded == "" {
		return zero, errors.New("compiled current Windows Product registry is missing")
	}
	raw, err := base64.StdEncoding.Strict().DecodeString(encoded)
	if err != nil || base64.StdEncoding.EncodeToString(raw) != encoded {
		return zero, errors.Join(errors.New("compiled current Windows Product registry base64 is invalid"), err)
	}
	registry, err := parseWindowsProductRegistryV4(raw)
	if err != nil {
		return zero, err
	}
	if registry.SchemaVersion != 4 || registry.IdentityManifest.Schema != "windows-product-identity-v2" {
		return zero, errors.New("compiled Windows Product registry requires the current domain")
	}
	return registry, nil
}

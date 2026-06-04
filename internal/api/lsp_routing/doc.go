// Package lsp_routing implements path-to-workspace inference primitives for
// future per-language LSP auto-registration.
//
// The package is intentionally write-free: ResolveByPath only infers the
// canonical workspace root and reports whether an LSP registry row for the
// requested language already exists. Auto-registration and request forwarding
// belong to the future router layer.
package lsp_routing

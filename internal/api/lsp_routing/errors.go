package lsp_routing

import "errors"

// ErrWorkspaceNotFound is returned when no language project marker or fallback
// VCS root can be found for an absolute file path.
var ErrWorkspaceNotFound = errors.New("lsp_routing: workspace not found")

// ErrInvalidPath is returned when ResolveByPath receives an empty, relative, or
// otherwise unsafe path, or when the requested language is empty.
var ErrInvalidPath = errors.New("lsp_routing: invalid path")

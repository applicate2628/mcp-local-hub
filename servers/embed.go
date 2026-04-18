// Package servers exposes all shipped MCP server manifests as an
// embed.FS. This lets the mcphub binary find manifests regardless of
// where the .exe is installed — canonical ~/.local/bin/, a dev
// checkout, or anywhere on PATH — removing the earlier dependency on
// daemon.go finding <exeDir>/servers/ or <exeDir>/../servers/ on disk.
//
// Usage:
//
//	f, err := servers.Manifests.Open("perftools/manifest.yaml")
//
// The embed pattern `*/manifest.yaml` picks up every server directory
// that has a manifest.yaml; adding a new server is zero-config.
package servers

import "embed"

//go:embed */manifest.yaml
var Manifests embed.FS

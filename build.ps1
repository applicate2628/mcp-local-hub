#Requires -Version 5.1
# Build script for mcp-local-hub on Windows.
#
# Produces mcp.exe with:
#   * Windows version resource (FileDescription, CompanyName, etc.) embedded
#     via cmd/mcp/resource.syso (regenerated from versioninfo.json).
#   * Build metadata (version, commit, build date) embedded via ldflags -X;
#     visible via `./mcp.exe version`.
#   * Reproducible output via -trimpath.
#
# Prerequisites: Go 1.22+, git. Goversioninfo downloads on the fly via
# `go run github.com/josephspurrier/goversioninfo/cmd/goversioninfo@latest`.
#
# Note on antivirus: unsigned Go binaries can trip Windows Defender's ML
# heuristic (Wacatac.B!ml and friends). If mcp.exe disappears after build,
# add D:\path\to\mcp-local-hub to Defender exclusions. See INSTALL.md.

$ErrorActionPreference = "Stop"

$version = "0.1.0"
try { $commit = (git rev-parse --short HEAD 2>$null) } catch { $commit = "unknown" }
if ([string]::IsNullOrWhiteSpace($commit)) { $commit = "unknown" }
$buildDate = (Get-Date).ToUniversalTime().ToString("yyyy-MM-ddTHH:mm:ssZ")

Write-Host "==> Generating Windows version resource (cmd/mcp/resource.syso)"
go generate ./cmd/mcp
if ($LASTEXITCODE -ne 0) { exit $LASTEXITCODE }

$ldflags = "-X main.version=$version -X main.commit=$commit -X main.buildDate=$buildDate -H windowsgui"

Write-Host "==> Building mcp.exe (version=$version commit=$commit)"
go build -trimpath -ldflags $ldflags -o mcp.exe ./cmd/mcp
if ($LASTEXITCODE -ne 0) { exit $LASTEXITCODE }

if (Test-Path mcp.exe) {
    Write-Host "==> Metadata embedded:"
    (Get-Item mcp.exe).VersionInfo | Format-List FileVersion,ProductName,FileDescription,CompanyName,LegalCopyright,Comments
    Write-Host "==> Done. Run './mcp.exe version' to print build info."
} else {
    Write-Error "mcp.exe missing after build \u2014 check Defender exclusions (see INSTALL.md)."
    exit 1
}

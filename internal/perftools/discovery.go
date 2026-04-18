package perftools

import (
	"fmt"
	"os/exec"
	"strings"
)

// ToolInfo describes whether a perf tool is available on this host,
// plus the version string scraped from its `--version` output.
// Clients use the JSON rendering of this struct (via resource://tools)
// to decide whether to call a given tool handler or skip it cleanly.
type ToolInfo struct {
	Installed bool   `json:"installed"`
	Path      string `json:"path,omitempty"`
	Version   string `json:"version,omitempty"`
	Error     string `json:"error,omitempty"`
}

// ToolCatalog is the typed record of all four perf-tools the server
// advertises. Constructed once at startup by DetectTools() so per-call
// handler dispatch is a cheap pointer read.
type ToolCatalog struct {
	ClangTidy   *ToolInfo `json:"clang-tidy"`
	Hyperfine   *ToolInfo `json:"hyperfine"`
	LLVMObjdump *ToolInfo `json:"llvm-objdump"`
	IWYU        *ToolInfo `json:"include-what-you-use"`
}

// AsMap exposes the catalog for iteration in tests and JSON rendering.
// The keys match the upstream CLI names (include-what-you-use, not
// iwyu) so users reading the JSON aren't surprised.
func (c *ToolCatalog) AsMap() map[string]*ToolInfo {
	return map[string]*ToolInfo{
		"clang-tidy":           c.ClangTidy,
		"hyperfine":            c.Hyperfine,
		"llvm-objdump":         c.LLVMObjdump,
		"include-what-you-use": c.IWYU,
	}
}

// DetectTools probes PATH for each supported tool and records its
// version. Missing tools are reported as Installed=false with an Error
// string — the server still starts and advertises all four slots so
// clients see a stable catalog.
func DetectTools() *ToolCatalog {
	return &ToolCatalog{
		ClangTidy:   probe("clang-tidy", firstLine),
		Hyperfine:   probe("hyperfine", firstLine),
		LLVMObjdump: probe("llvm-objdump", firstLine),
		IWYU:        probe("include-what-you-use", firstLine),
	}
}

// probe runs `<bin> --version` and extracts the version via versionExtract.
// Returns Installed=false with Error on any failure.
func probe(bin string, versionExtract func(string) string) *ToolInfo {
	path, err := exec.LookPath(bin)
	if err != nil {
		return &ToolInfo{Installed: false, Error: fmt.Sprintf("not on PATH: %v", err)}
	}
	out, err := exec.Command(bin, "--version").CombinedOutput()
	if err != nil {
		return &ToolInfo{
			Installed: false,
			Path:      path,
			Error:     fmt.Sprintf("--version failed: %v", err),
		}
	}
	return &ToolInfo{
		Installed: true,
		Path:      path,
		Version:   versionExtract(string(out)),
	}
}

// firstLine trims the tool's --version output to a single user-friendly
// line. Most tools emit "<bin> x.y.z\n<more noise>" — we keep the first
// non-empty line.
func firstLine(s string) string {
	for _, line := range strings.Split(s, "\n") {
		line = strings.TrimSpace(line)
		if line != "" {
			return line
		}
	}
	return ""
}

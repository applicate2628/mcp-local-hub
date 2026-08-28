package reversedepgraph

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"

	"mcp-local-hub/internal/process"
	"mcp-local-hub/internal/vcpkgmcp/portname"
)

var tripletPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9_.-]*$`)

func validTriplet(value string) bool { return tripletPattern.MatchString(value) }

func ValidateArgs(ctx context.Context, args Args) error {
	if ctx == nil {
		return fmt.Errorf("context is nil")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if _, err := portname.Parse(args.Port); err != nil {
		return fmt.Errorf("port: %w", err)
	}
	if len(args.Port) > 255 || len(args.Triplet) > 255 || len(args.HostTriplet) > 255 {
		return fmt.Errorf("port and triplet names must not exceed 255 bytes")
	}
	if !validTriplet(args.Triplet) {
		return fmt.Errorf("triplet is not a legal explicit triplet name")
	}
	if !validTriplet(args.HostTriplet) {
		return fmt.Errorf("host_triplet is not a legal explicit triplet name")
	}
	if len(args.OverlayPorts) > MaxOverlayRoots || len(args.OverlayTriplets) > MaxOverlayRoots {
		return fmt.Errorf("overlay roots exceed %d", MaxOverlayRoots)
	}
	if args.TimeoutMS != 0 && (args.TimeoutMS < MinTimeoutMS || args.TimeoutMS > MaxTimeoutMS) {
		return fmt.Errorf("timeout_ms must be %d..%d", MinTimeoutMS, MaxTimeoutMS)
	}
	if args.VcpkgRoot == "" || !filepath.IsAbs(args.VcpkgRoot) {
		return fmt.Errorf("vcpkg_root must be absolute")
	}
	if args.ScratchRoot == "" || !filepath.IsAbs(args.ScratchRoot) {
		return fmt.Errorf("scratch_root must be absolute")
	}
	inputs := []string{args.VcpkgRoot}
	for name, roots := range map[string][]string{"overlay_ports": args.OverlayPorts, "overlay_triplets": args.OverlayTriplets} {
		for _, root := range roots {
			if root == "" || !filepath.IsAbs(root) {
				return fmt.Errorf("%s entries must be absolute", name)
			}
			inputs = append(inputs, root)
		}
	}
	if args.ManifestRoot != "" {
		if !filepath.IsAbs(args.ManifestRoot) {
			return fmt.Errorf("manifest_root must be absolute")
		}
		inputs = append(inputs, args.ManifestRoot)
	}
	for _, input := range append(inputs, args.ScratchRoot) {
		if len(input) > 32768 {
			return fmt.Errorf("path input exceeds 32768 bytes")
		}
		lower := strings.ToLower(input)
		if strings.Contains(lower, "://") || strings.Contains(input, "?") || strings.Contains(lower, "token=") || strings.Contains(lower, "password=") {
			return fmt.Errorf("credential/query-bearing path input refused")
		}
	}
	for _, input := range inputs {
		if pathsOverlap(input, args.ScratchRoot) {
			return fmt.Errorf("scratch_root overlaps input root")
		}
	}
	return nil
}

// BindTrustedRoot verifies that the request confirms the operator-controlled
// VCPKG_ROOT and returns only the strictly resolved operator-owned path for
// downstream execution and evidence. The request spelling is never retained.
func BindTrustedRoot(requested, trusted string) (string, error) {
	if requested == "" || !filepath.IsAbs(requested) {
		return "", fmt.Errorf("vcpkg_root must be an absolute confirmation of the daemon's trusted VCPKG_ROOT")
	}
	if trusted == "" || !filepath.IsAbs(trusted) {
		return "", fmt.Errorf("vcpkg execution is disabled: daemon VCPKG_ROOT is not an absolute trusted root")
	}
	canonicalTrusted, err := process.CanonicalizePathStrict(trusted)
	if err != nil {
		return "", fmt.Errorf("vcpkg execution is disabled: daemon VCPKG_ROOT cannot be resolved: %w", err)
	}
	trustedInfo, err := os.Stat(canonicalTrusted)
	if err != nil || !trustedInfo.IsDir() {
		return "", fmt.Errorf("vcpkg execution is disabled: daemon VCPKG_ROOT is not an existing directory")
	}
	canonicalRequested, err := process.CanonicalizePathStrict(requested)
	if err != nil {
		return "", fmt.Errorf("vcpkg_root cannot be resolved: %w", err)
	}
	requestedInfo, err := os.Stat(canonicalRequested)
	if err != nil || !requestedInfo.IsDir() {
		return "", fmt.Errorf("vcpkg_root is not an existing directory")
	}
	if !os.SameFile(requestedInfo, trustedInfo) {
		return "", fmt.Errorf("vcpkg_root must match the daemon's trusted VCPKG_ROOT")
	}
	return canonicalTrusted, nil
}

func pathsOverlap(left, right string) bool {
	left = canonicalForComparison(left)
	right = canonicalForComparison(right)
	return pathContains(left, right) || pathContains(right, left)
}

func canonicalForComparison(path string) string {
	cleaned, err := filepath.Abs(filepath.Clean(path))
	if err != nil {
		cleaned = filepath.Clean(path)
	}
	if resolved, err := filepath.EvalSymlinks(cleaned); err == nil {
		cleaned = resolved
	}
	if runtime.GOOS == "windows" {
		cleaned = strings.ToLower(cleaned)
	}
	return filepath.Clean(cleaned)
}

func pathContains(parent, child string) bool {
	relative, err := filepath.Rel(parent, child)
	if err != nil {
		return false
	}
	return relative == "." || (relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator)) && !filepath.IsAbs(relative))
}

func vcpkgExecutable(root string) string {
	name := "vcpkg"
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	return filepath.Join(root, name)
}

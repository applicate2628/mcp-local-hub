package reversedepgraph

import (
	"path/filepath"
	"runtime"
	"sort"
	"strings"
)

func DependInfoCommand(args Args, candidate, format, scratch string) Command {
	return DependInfoBatchCommand(args, []string{candidate}, format, scratch, 0)
}

func DependInfoBatchCommand(args Args, candidates []string, format, scratch string, batchIndex int) Command {
	candidates = append([]string(nil), candidates...)
	sort.Strings(candidates)
	argv := append([]string{"depend-info"}, candidates...)
	argv = append(argv, "--format="+format)
	if format == "list" {
		argv = append(argv, "--show-depth")
	}
	argv = append(argv,
		"--triplet="+args.Triplet,
		"--host-triplet="+args.HostTriplet,
	)
	for _, overlay := range args.OverlayPorts {
		argv = append(argv, "--overlay-ports="+overlay)
	}
	for _, overlay := range args.OverlayTriplets {
		argv = append(argv, "--overlay-triplets="+overlay)
	}
	argv = append(argv,
		"--vcpkg-root="+args.VcpkgRoot,
		"--x-buildtrees-root="+filepath.Join(scratch, "buildtrees"),
		"--x-install-root="+filepath.Join(scratch, "installed"),
		"--downloads-root="+filepath.Join(scratch, "downloads"),
		"--x-packages-root="+filepath.Join(scratch, "packages"),
		"--binarysource=clear",
		"--x-asset-sources=clear",
	)
	dir := scratch
	if args.ManifestRoot == "" {
		argv = append(argv, "--classic")
	} else {
		dir = args.ManifestRoot
	}
	return Command{
		Executable: vcpkgExecutable(args.VcpkgRoot),
		Args:       argv,
		Dir:        dir,
		Env:        allowedEnvironment(scratch),
		Stage:      "depend_info",
		Candidate:  strings.Join(candidates, ","),
		Candidates: candidates,
		BatchIndex: batchIndex,
		Format:     format,
	}
}

func allowedEnvironment(scratch string) []string {
	environment := []string{
		"VCPKG_DISABLE_METRICS=1",
		"TEMP=" + scratch,
		"TMP=" + scratch,
		"HOME=" + scratch,
		"APPDATA=" + scratch,
		"LOCALAPPDATA=" + scratch,
	}
	for _, key := range []string{"SystemRoot", "WINDIR", "PATH", "PATHEXT", "COMSPEC"} {
		if value, ok := lookupSafeEnvironment(key); ok {
			environment = append(environment, key+"="+value)
		}
	}
	if runtime.GOOS != "windows" {
		for _, key := range []string{"LANG", "LC_ALL"} {
			if value, ok := lookupSafeEnvironment(key); ok {
				environment = append(environment, key+"="+value)
			}
		}
	}
	sort.Strings(environment)
	return environment
}

package main

import (
	"fmt"
	"io"
	"os"

	"mcp-local-hub/internal/binaryadmission"
)

var admitWindowsRoleFn = binaryadmission.AdmitWindowsRole

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

func run(args []string, stdout, stderr io.Writer) int {
	if len(args) != 2 && len(args) != 5 {
		fmt.Fprintln(stderr, "usage: mcphub-pe-admit <cli|windowless|upgrade-prior> <candidate-path> [<expected-version> <expected-commit> <expected-build-date>]")
		return 2
	}
	role, path := args[0], args[1]
	var expected *binaryadmission.WindowsArtifact
	if len(args) == 5 {
		expected = &binaryadmission.WindowsArtifact{Version: args[2], Commit: args[3], BuildDate: args[4]}
	}
	var subsystem uint16
	var admitted binaryadmission.WindowsArtifact
	var err error
	switch role {
	case string(binaryadmission.WindowsArtifactRoleCLI):
		subsystem = binaryadmission.WindowsCUISubsystem
		admitted, err = admitWindowsRoleFn(binaryadmission.WindowsArtifact{Path: path, Role: binaryadmission.WindowsArtifactRoleCLI})
	case string(binaryadmission.WindowsArtifactRoleWindowless):
		subsystem = binaryadmission.WindowsGUISubsystem
		admitted, err = admitWindowsRoleFn(binaryadmission.WindowsArtifact{Path: path, Role: binaryadmission.WindowsArtifactRoleWindowless})
	case "upgrade-prior":
		if expected != nil {
			fmt.Fprintln(stderr, "expected build identity is supported only for cli and windowless roles")
			return 2
		}
		err = binaryadmission.AdmitWindowsUpgradePrior(path)
		if err == nil {
			f, openErr := os.Open(path)
			if openErr != nil {
				err = openErr
			} else {
				info, statErr := f.Stat()
				if statErr != nil {
					err = statErr
				} else {
					subsystem, err = binaryadmission.ReadWindowsPESubsystem(f, info.Size())
				}
				_ = f.Close()
			}
		}
	default:
		fmt.Fprintf(stderr, "usage: mcphub-pe-admit <cli|windowless|upgrade-prior> <candidate-path> (unknown role %q)\n", role)
		return 2
	}
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	if expected != nil {
		for _, field := range []struct {
			name     string
			actual   string
			expected string
		}{
			{"version", admitted.Version, expected.Version},
			{"commit", admitted.Commit, expected.Commit},
			{"build date", admitted.BuildDate, expected.BuildDate},
		} {
			if field.actual != field.expected {
				fmt.Fprintf(stderr, "VERSIONINFO %s mismatch: actual=%q expected=%q\n", field.name, field.actual, field.expected)
				return 1
			}
		}
	}
	fmt.Fprintf(stdout, "%s: role %s: PE subsystem %d\n", path, role, subsystem)
	return 0
}

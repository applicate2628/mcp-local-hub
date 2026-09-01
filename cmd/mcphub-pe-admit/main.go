package main

import (
	"fmt"
	"io"
	"os"

	"mcp-local-hub/internal/binaryadmission"
)

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

func run(args []string, stdout, stderr io.Writer) int {
	if len(args) != 2 {
		fmt.Fprintln(stderr, "usage: mcphub-pe-admit <cli|windowless|upgrade-prior> <candidate-path>")
		return 2
	}
	role, path := args[0], args[1]
	var subsystem uint16
	var err error
	switch role {
	case string(binaryadmission.WindowsArtifactRoleCLI):
		subsystem = binaryadmission.WindowsCUISubsystem
		_, err = binaryadmission.AdmitWindowsRole(binaryadmission.WindowsArtifact{Path: path, Role: binaryadmission.WindowsArtifactRoleCLI})
	case string(binaryadmission.WindowsArtifactRoleWindowless):
		subsystem = binaryadmission.WindowsGUISubsystem
		_, err = binaryadmission.AdmitWindowsRole(binaryadmission.WindowsArtifact{Path: path, Role: binaryadmission.WindowsArtifactRoleWindowless})
	case "upgrade-prior":
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
	fmt.Fprintf(stdout, "%s: role %s: PE subsystem %d\n", path, role, subsystem)
	return 0
}

package main

import (
	"fmt"
	"os"

	"mcp-local-hub/internal/binaryadmission"
)

func main() {
	if len(os.Args) != 2 {
		fmt.Fprintln(os.Stderr, "usage: mcphub-pe-admit <candidate-path>")
		os.Exit(2)
	}
	if err := binaryadmission.AdmitWindowsGUI(os.Args[1]); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	fmt.Printf("%s: PE subsystem %d (WINDOWS_GUI)\n", os.Args[1], binaryadmission.WindowsGUISubsystem)
}

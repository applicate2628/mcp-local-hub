package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"mcp-local-hub/internal/binaryadmission"
)

const productFilename = "mcphub.exe"

type validatorOptions struct {
	candidate         string
	expectedVersion   string
	expectedCommit    string
	expectedBuildDate string
}

type productIdentity struct {
	Schema    string                              `json:"schema"`
	Role      binaryadmission.WindowsArtifactRole `json:"role"`
	Filename  string                              `json:"filename"`
	Subsystem uint16                              `json:"subsystem"`
	Machine   uint16                              `json:"machine"`
	Version   string                              `json:"version"`
	Commit    string                              `json:"commit"`
	BuildDate string                              `json:"build_date"`
	SHA256    string                              `json:"sha256"`
}

func parseOptions(args []string, stderr io.Writer) (validatorOptions, bool) {
	var options validatorOptions
	flags := flag.NewFlagSet("windows-product-validate", flag.ContinueOnError)
	flags.SetOutput(stderr)
	flags.StringVar(&options.candidate, "candidate", "", "path to the candidate mcphub.exe")
	flags.StringVar(&options.expectedVersion, "expected-version", "", "expected product version")
	flags.StringVar(&options.expectedCommit, "expected-commit", "", "expected source commit")
	flags.StringVar(&options.expectedBuildDate, "expected-build-date", "", "expected UTC build date")
	flags.Usage = func() {
		fmt.Fprintln(stderr, "Usage: windows-product-validate --candidate <mcphub.exe> --expected-version <version> --expected-commit <commit> --expected-build-date <UTC>")
	}
	if err := flags.Parse(args); err != nil {
		flags.Usage()
		return validatorOptions{}, false
	}
	if flags.NArg() != 0 || options.candidate == "" || options.expectedVersion == "" || options.expectedCommit == "" || options.expectedBuildDate == "" {
		fmt.Fprintln(stderr, "all four named flags are required and positional arguments are not accepted")
		flags.Usage()
		return validatorOptions{}, false
	}
	if !validExpectedCommit(options.expectedCommit) {
		fmt.Fprintln(stderr, "--expected-commit must be exactly 40 lowercase hexadecimal characters")
		flags.Usage()
		return validatorOptions{}, false
	}
	return options, true
}

func validExpectedCommit(value string) bool {
	if len(value) != 40 {
		return false
	}
	for _, char := range value {
		if (char < '0' || char > '9') && (char < 'a' || char > 'f') {
			return false
		}
	}
	return true
}

func run(args []string, stdout, stderr io.Writer) int {
	options, ok := parseOptions(args, stderr)
	if !ok {
		return 2
	}
	if filepath.Base(options.candidate) != productFilename {
		fmt.Fprintf(stderr, "windows product validation failed: candidate filename must be %s\n", productFilename)
		return 1
	}

	admitted, err := binaryadmission.AdmitWindowsRole(binaryadmission.WindowsArtifact{
		Path: options.candidate,
		Role: binaryadmission.WindowsArtifactRoleProduct,
	})
	if err != nil {
		fmt.Fprintf(stderr, "windows product validation failed: %v\n", err)
		return 1
	}
	if admitted.Version != options.expectedVersion || admitted.Commit != options.expectedCommit || admitted.BuildDate != options.expectedBuildDate {
		fmt.Fprintf(
			stderr,
			"windows product validation failed: build identity mismatch: expected version=%q commit=%q build_date=%q, actual version=%q commit=%q build_date=%q\n",
			options.expectedVersion,
			options.expectedCommit,
			options.expectedBuildDate,
			admitted.Version,
			admitted.Commit,
			admitted.BuildDate,
		)
		return 1
	}

	identity := productIdentity{
		Schema:    "windows-product-identity-v1",
		Role:      admitted.Role,
		Filename:  productFilename,
		Subsystem: admitted.Subsystem,
		Machine:   admitted.Machine,
		Version:   admitted.Version,
		Commit:    admitted.Commit,
		BuildDate: admitted.BuildDate,
		SHA256:    admitted.SHA256,
	}
	if err := json.NewEncoder(stdout).Encode(identity); err != nil {
		fmt.Fprintf(stderr, "windows product validation failed: write identity: %v\n", err)
		return 1
	}
	return 0
}

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

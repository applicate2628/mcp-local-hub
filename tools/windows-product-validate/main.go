package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"strings"

	"mcp-local-hub/internal/binaryadmission"
)

const productFilename = "mcphub.exe"

type validatorOptions struct {
	candidate            string
	expectedVersion      string
	expectedCommit       string
	expectedBuildDate    string
	expectedSourceEpoch  string
	sourceSnapshot       string
	sourceRoot           string
	prepareSource        string
	sourceCheckout       string
	sourceSnapshotOutput string
	generatedSource      []string
}

type generatedSourceFlags []string

func (v *generatedSourceFlags) String() string         { return strings.Join(*v, ",") }
func (v *generatedSourceFlags) Set(value string) error { *v = append(*v, value); return nil }

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
	flags.StringVar(&options.expectedSourceEpoch, "expected-source-epoch", "", "optional builder-derived source epoch")
	flags.StringVar(&options.sourceSnapshot, "source-snapshot", "", "existing Root-qualified source snapshot")
	flags.StringVar(&options.sourceRoot, "source-root", "", "isolated Root-qualified source copy")
	flags.StringVar(&options.prepareSource, "prepare-source", "", "fresh absolute source export directory")
	flags.StringVar(&options.sourceCheckout, "source-checkout", "", "actual absolute Git checkout directory")
	flags.StringVar(&options.sourceSnapshotOutput, "source-snapshot-output", "", "fresh absolute outside-export snapshot file")
	flags.Var((*generatedSourceFlags)(&options.generatedSource), "generated-source", "literal repository-relative additional generated file (repeatable)")
	flags.Usage = func() {
		fmt.Fprintln(stderr, "Usage: windows-product-validate --candidate <mcphub.exe> --expected-version <version> --expected-commit <commit> --expected-build-date <UTC>")
		fmt.Fprintln(stderr, "       windows-product-validate --source-snapshot <snapshot> --source-root <source>")
		fmt.Fprintln(stderr, "       windows-product-validate --prepare-source <export> --source-checkout <checkout> --source-snapshot-output <snapshot> --expected-commit <commit> [--generated-source <file> ...]")
	}
	if err := flags.Parse(args); err != nil {
		flags.Usage()
		return validatorOptions{}, false
	}
	present := map[string]bool{}
	flags.Visit(func(f *flag.Flag) { present[f.Name] = true })
	if present["prepare-source"] || present["source-checkout"] || present["source-snapshot-output"] || present["generated-source"] {
		if flags.NArg() != 0 || options.prepareSource == "" || options.sourceCheckout == "" || options.sourceSnapshotOutput == "" || !validExpectedCommit(options.expectedCommit) || present["candidate"] || present["expected-version"] || present["expected-build-date"] || present["expected-source-epoch"] || present["source-root"] || present["source-snapshot"] {
			fmt.Fprintln(stderr, "PUBLISHER_SOURCE_TABLE_INVALID: preparation requires complete exclusive operands and a full expected commit")
			return validatorOptions{}, false
		}
		return options, true
	}
	if present["source-snapshot"] || present["source-root"] {
		if flags.NArg() != 0 || options.sourceSnapshot == "" || options.sourceRoot == "" || present["candidate"] || present["expected-version"] || present["expected-commit"] || present["expected-build-date"] || present["expected-source-epoch"] {
			fmt.Fprintln(stderr, "PUBLISHER_SOURCE_TABLE_INVALID: source-only flags are mutually exclusive")
			return validatorOptions{}, false
		}
		return options, true
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
	if present["expected-source-epoch"] && !validExpectedSourceEpoch(options.expectedSourceEpoch) {
		fmt.Fprintln(stderr, "PUBLISHER_SOURCE_EPOCH_MISMATCH: expected epoch must be 64 lowercase hexadecimal characters")
		return validatorOptions{}, false
	}
	return options, true
}

func validExpectedSourceEpoch(value string) bool {
	if len(value) != 64 {
		return false
	}
	for _, c := range value {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
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
	if options.prepareSource != "" {
		ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
		defer stop()
		err := binaryadmission.PreparePublisherSource(ctx, binaryadmission.PublisherSourcePreparationRequest{Checkout: options.sourceCheckout, ExpectedCommit: options.expectedCommit, Export: options.prepareSource, Snapshot: options.sourceSnapshotOutput, GeneratedSource: options.generatedSource}, stdout)
		if err != nil {
			fmt.Fprintf(stderr, "publisher source preparation failed: %v\n", err)
			return 1
		}
		return 0
	}
	if options.sourceSnapshot != "" {
		return runSourceVerification(options, stdout, stderr)
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

	if options.expectedSourceEpoch != "" {
		raw, err := os.ReadFile(options.candidate)
		if err != nil {
			fmt.Fprintln(stderr, err)
			return 1
		}
		i, err := binaryadmission.ParseWindowsPublisherSourceIdentity(raw)
		if err != nil {
			fmt.Fprintln(stderr, err)
			return 1
		}
		h := sha256.Sum256(raw)
		if i.SourceEpoch != options.expectedSourceEpoch || i.Version != admitted.Version || i.Commit != admitted.Commit || i.BuildDate != admitted.BuildDate || i.Role != admitted.Role || i.Machine != admitted.Machine || i.Subsystem != admitted.Subsystem || hex.EncodeToString(h[:]) != admitted.SHA256 {
			fmt.Fprintln(stderr, "PUBLISHER_SOURCE_EPOCH_MISMATCH")
			return 1
		}
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

func runSourceVerification(options validatorOptions, stdout, stderr io.Writer) int {
	root, err := filepath.Abs(options.sourceRoot)
	if err != nil {
		fmt.Fprintln(stderr, "PUBLISHER_SOURCE_TABLE_INVALID")
		return 1
	}
	snapshot, err := filepath.Abs(options.sourceSnapshot)
	if err != nil {
		fmt.Fprintln(stderr, "PUBLISHER_SOURCE_TABLE_INVALID")
		return 1
	}
	rel, err := filepath.Rel(root, snapshot)
	if err != nil && strings.EqualFold(filepath.VolumeName(root), filepath.VolumeName(snapshot)) || err == nil && (rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))) {
		fmt.Fprintln(stderr, "PUBLISHER_SOURCE_TABLE_INVALID: snapshot must be outside source")
		return 1
	}
	info, err := os.Lstat(snapshot)
	if err != nil || !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
		fmt.Fprintln(stderr, "PUBLISHER_SOURCE_TABLE_INVALID: snapshot not ordinary")
		return 1
	}
	f, err := os.Open(snapshot)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	opened, statErr := f.Stat()
	if statErr == nil && !os.SameFile(info, opened) {
		statErr = binaryadmission.ErrPublisherSourceTableInvalid
	}
	var raw []byte
	if statErr == nil {
		raw, statErr = io.ReadAll(f)
	}
	closeErr := f.Close()
	if err := errors.Join(statErr, closeErr); err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	input, err := binaryadmission.VerifyPublisherSourceSnapshot(context.Background(), root, raw)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	row := struct {
		Schema               string `json:"schema"`
		SourceSnapshotSHA256 string `json:"source_snapshot_sha256"`
		SourceEpoch          string `json:"source_epoch"`
		FileCount            int    `json:"file_count"`
	}{"windows-publisher-source-input-v1", input.SourceSnapshotSHA256, input.SourceEpoch, input.FileCount}
	if err := json.NewEncoder(stdout).Encode(row); err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	return 0
}

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

//go:build windows

// test-serena-publisher builds one source-issued Serena integration test image.
// It neither builds an installable product nor runs a test or package installer.
package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"

	"mcp-local-hub/internal/binaryadmission"
	"mcp-local-hub/internal/process"
)

const qualificationVersion = "0.0.0-serena-qualification"

type options struct {
	checkout, commit, goPath, goRoot, moduleCache, root string
	timeout                                             time.Duration
}

func main() {
	var o options
	flag.StringVar(&o.checkout, "checkout", "", "absolute clean source checkout")
	flag.StringVar(&o.commit, "expected-commit", "", "exact 40-character lowercase Git commit")
	flag.StringVar(&o.goPath, "go", "", "absolute existing native go.exe")
	flag.StringVar(&o.goRoot, "go-root", "", "absolute existing compiler and standard-library tree")
	flag.StringVar(&o.moduleCache, "module-cache", "", "absolute existing module cache; network is disabled")
	flag.StringVar(&o.root, "work-root", "", "fresh absolute private work directory; all outputs remain here")
	flag.DurationVar(&o.timeout, "timeout", 10*time.Minute, "finite build deadline, at most 10m")
	flag.Parse()
	if err := validateOptions(o, flag.NArg()); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
	if err := build(o); err != nil {
		fmt.Fprintln(os.Stderr, "Serena qualification builder:", err)
		os.Exit(1)
	}
}

func validateOptions(o options, extra int) error {
	if extra != 0 || o.timeout <= 0 || o.timeout > 10*time.Minute ||
		len(o.commit) != 40 || strings.Trim(o.commit, "0123456789abcdef") != "" {
		return errors.New("exact commit and finite deadline are required")
	}
	for _, p := range []string{o.checkout, o.goPath, o.goRoot, o.moduleCache, o.root} {
		volume := filepath.VolumeName(p)
		if !filepath.IsAbs(p) || filepath.Clean(p) != p || len(volume) != 2 || volume[1] != ':' ||
			strings.ContainsAny(strings.TrimPrefix(p, volume), ":\x00\r\n") {
			return errors.New("paths must be clean absolute local-drive paths")
		}
	}
	for _, p := range []string{o.checkout, o.moduleCache, o.goPath, o.goRoot} {
		if within(o.root, p) || within(p, o.root) {
			return errors.New("work root must be disjoint from checkout, tool and module cache")
		}
	}
	return nil
}

func within(root, path string) bool {
	relative, err := filepath.Rel(root, path)
	return err == nil && relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator))
}

func build(o options) (returned error) {
	ctx, cancel := context.WithTimeout(context.Background(), o.timeout)
	defer cancel()
	var handles []windows.Handle
	closeHandles := func() (closeErr error) {
		owned := handles
		handles = nil // a failed close is never retried with a recycled handle value
		for i := len(owned) - 1; i >= 0; i-- {
			closeErr = errors.Join(closeErr, windows.CloseHandle(owned[i]))
		}
		return closeErr
	}
	defer func() { returned = errors.Join(returned, closeHandles()) }()
	for _, entry := range []struct {
		path      string
		directory bool
	}{
		{o.checkout, true}, {o.moduleCache, true}, {filepath.Join(o.moduleCache, "cache", "download"), true},
		{o.goPath, false}, {o.goRoot, true}, {filepath.Dir(o.root), true},
	} {
		held, err := pinOrdinaryPath(entry.path, entry.directory)
		if err != nil {
			return err
		}
		handles = append(handles, held...)
	}
	if err := createPrivateRoot(o.root); err != nil {
		return err
	}
	held, err := pinOrdinaryPath(o.root, true)
	if err != nil {
		return err
	}
	handles = append(handles, held...)
	for _, leaf := range []string{"bin", "proof", "temp", "gocache", "gopath", "appdata", "localappdata", "trash"} {
		if err := os.Mkdir(filepath.Join(o.root, leaf), 0700); err != nil {
			return err
		}
	}
	source, snapshot := filepath.Join(o.root, "source"), filepath.Join(o.root, "source-snapshot.json")
	var preparation bytes.Buffer
	if err := binaryadmission.PreparePublisherSource(ctx, binaryadmission.PublisherSourcePreparationRequest{
		Checkout: o.checkout, ExpectedCommit: o.commit, Export: source, Snapshot: snapshot,
	}, &preparation); err != nil {
		return err
	}
	if err := writeFresh(filepath.Join(o.root, "proof", "source-preparation.json"), preparation.Bytes()); err != nil {
		return err
	}
	var prepared struct {
		GitCommit           string
		DirtySelectedSource bool
	}
	// Decode the existing owner's response without assigning caller-chosen hashes.
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(preparation.Bytes(), &fields); err != nil {
		return err
	}
	if err := json.Unmarshal(fields["git_commit"], &prepared.GitCommit); err != nil {
		return err
	}
	if err := json.Unmarshal(fields["dirty_selected_source"], &prepared.DirtySelectedSource); err != nil {
		return err
	}
	if prepared.GitCommit != o.commit || prepared.DirtySelectedSource {
		return errors.New("qualification source must be committed; selected bytes differ from expected commit")
	}
	rawSnapshot, err := os.ReadFile(snapshot)
	if err != nil {
		return err
	}
	sourceInput, err := binaryadmission.VerifyPublisherSourceSnapshot(ctx, source, rawSnapshot)
	if err != nil {
		return err
	}
	buildDate := time.Now().UTC().Format("2006-01-02T15:04:05Z")
	resourcePath := filepath.Join(source, "internal", "cli", "zz_serena_qualification_versioninfo.syso")
	if _, err := os.Lstat(resourcePath); !errors.Is(err, os.ErrNotExist) {
		return errors.New("qualification resource output must not already exist")
	}
	config := filepath.Join(o.root, "qualification-versioninfo.json")
	if err := writeJSON(config, resourceConfig(o.commit, buildDate, sourceInput.SourceEpoch)); err != nil {
		return err
	}
	env := buildEnvironment(o)
	cmd := exec.Command(o.goPath, "telemetry", "off")
	cmd.Dir, cmd.Env = source, env
	if err := runStage(ctx, o.root, "private-telemetry-off", cmd); err != nil {
		return err
	}
	cmd = exec.Command(o.goPath, "version")
	cmd.Dir, cmd.Env = source, env
	if err := runStage(ctx, o.root, "go-version", cmd); err != nil {
		return err
	}
	// A build-only resource is never part of the verified source table. Preserve
	// it in our private trash before verifying the entire source closure again.
	resourceMoved := false
	defer func() {
		if !resourceMoved {
			returned = errors.Join(returned, moveResourceToTrash(resourcePath, o.root))
		}
	}()
	cmd = exec.Command(o.goPath, "run", "github.com/josephspurrier/goversioninfo/cmd/goversioninfo@v1.5.0",
		"-64", "-o", resourcePath, config)
	cmd.Dir, cmd.Env = source, env
	if runtime.GOARCH != "amd64" {
		return errors.New("this qualification builder currently requires native windows/amd64")
	}
	if err := runStage(ctx, o.root, "version-resource", cmd); err != nil {
		return err
	}
	output := filepath.Join(o.root, "bin", "serena-qualification.test.exe")
	ldflags := "-H=windowsgui -X mcp-local-hub/internal/buildinfo.version=" + qualificationVersion +
		" -X mcp-local-hub/internal/buildinfo.commit=" + o.commit +
		" -X mcp-local-hub/internal/buildinfo.date=" + buildDate +
		" -X mcp-local-hub/internal/buildinfo.publisherSourceEpoch=" + sourceInput.SourceEpoch
	cmd = exec.Command(o.goPath, "test", "-c", "-mod=readonly", "-trimpath",
		"-tags=test_state_path_env,serena_qualification", "-ldflags="+ldflags, "-o", output, "./internal/cli")
	cmd.Dir, cmd.Env = source, env
	if err := runStage(ctx, o.root, "compile", cmd); err != nil {
		return err
	}
	if err := moveResourceToTrash(resourcePath, o.root); err != nil {
		return err
	}
	resourceMoved = true
	finalSnapshot, err := os.ReadFile(snapshot)
	if err != nil {
		return err
	}
	if !bytes.Equal(rawSnapshot, finalSnapshot) {
		return errors.New("source snapshot changed during build")
	}
	after, err := binaryadmission.VerifyPublisherSourceSnapshot(ctx, source, finalSnapshot)
	if err != nil {
		return err
	}
	if after != sourceInput {
		return errors.New("verified source closure changed during build")
	}
	binaryHandles, err := pinOrdinaryPath(output, false)
	if err != nil {
		return err
	}
	handles = append(handles, binaryHandles...)
	raw, err := os.ReadFile(output)
	if err != nil {
		return err
	}
	identity, err := binaryadmission.ParseWindowsPublisherSourceIdentity(raw)
	if err != nil {
		return err
	}
	if identity.Role != binaryadmission.WindowsArtifactRoleSerenaQualification ||
		identity.SourceEpoch != sourceInput.SourceEpoch || identity.Commit != o.commit ||
		identity.Version != qualificationVersion || identity.BuildDate != buildDate ||
		identity.Machine != 0x8664 || identity.Subsystem != binaryadmission.WindowsGUISubsystem {
		return errors.New("compiled qualification image disagrees with verified build inputs")
	}
	if _, err := binaryadmission.AdmitWindowsRole(binaryadmission.WindowsArtifact{
		Path: output, Role: binaryadmission.WindowsArtifactRoleProduct,
	}); err == nil {
		return errors.New("qualification image unexpectedly admitted as Product")
	}
	hash := sha256.Sum256(raw)
	// Open the exact fresh receipt while ancestor holds still prevent redirection.
	// Only that retained file receives success bytes after input handles close.
	proof, err := os.OpenFile(filepath.Join(o.root, "proof", "publisher.json"), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	defer func() {
		if proof != nil {
			returned = errors.Join(returned, proof.Close())
		}
	}()
	if err := errors.Join(closeHandles(), ctx.Err()); err != nil {
		return err
	}
	if err := json.NewEncoder(proof).Encode(map[string]any{
		"schema": "serena-qualification-publisher-v1", "source_input": sourceInput,
		"artifact": "bin/serena-qualification.test.exe", "sha256": hex.EncodeToString(hash[:]),
		"role": identity.Role, "version": identity.Version, "commit": identity.Commit,
		"build_date": identity.BuildDate, "machine": identity.Machine, "subsystem": identity.Subsystem,
		"ordinary_product_admission": false, "tests_executed": false, "retained_input_handles_closed": true,
	}); err != nil {
		return err
	}
	if err := proof.Sync(); err != nil {
		return err
	}
	proofCloseErr := proof.Close()
	proof = nil
	if proofCloseErr != nil {
		return proofCloseErr
	}
	fmt.Println("Source-issued Serena qualification publisher built; proof retained under", o.root)
	return nil
}

func moveResourceToTrash(path, root string) error {
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return errors.New("generated resource is not an ordinary file; retained for inspection")
	}
	return os.Rename(path, filepath.Join(root, "trash", "zz_serena_qualification_versioninfo.syso"))
}

func resourceConfig(commit, date, epoch string) map[string]any {
	zeroVersion := map[string]int{"Major": 0, "Minor": 0, "Patch": 0, "Build": 0}
	return map[string]any{
		"FixedFileInfo": map[string]any{
			"FileVersion": zeroVersion, "ProductVersion": zeroVersion,
			"FileFlagsMask": "3f", "FileFlags": "00", "FileOS": "40004", "FileType": "01", "FileSubType": "00",
		},
		"StringFileInfo": map[string]string{
			"ProductName": "mcp-local-hub", "ProductVersion": qualificationVersion,
			"FileVersion": "0.0.0.0", "FileDescription": "Source-issued Serena integration qualification",
			"InternalName": "mcphub-serena-qualification", "OriginalFilename": "serena-qualification.test.exe",
			"PrivateBuild": "mcphub-build-v1;commit=" + commit + ";build_date=" + date,
			"SpecialBuild": "mcphub-role-v1;role=serena-qualification",
			"Comments":     "Serena integration qualification\nmcphub-source-v1;sha256=" + epoch,
		},
		"VarFileInfo": map[string]any{"Translation": map[string]string{"LangID": "0409", "CharsetID": "04B0"}},
	}
}

func buildEnvironment(o options) []string {
	// Versioned go run also queries deprecation metadata. Serve that lookup
	// from the existing download cache with no network or direct fallback.
	proxyURL := (&url.URL{Scheme: "file", Path: "/" + filepath.ToSlash(filepath.Join(o.moduleCache, "cache", "download"))}).String()
	// A comma is valid in a URL path but separates entries in GOPROXY.
	proxyURL = strings.ReplaceAll(proxyURL, ",", "%2C")
	values := map[string]string{
		"GOENV": "off", "GOTOOLCHAIN": "local", "GOPROXY": proxyURL, "GOSUMDB": "off", "GOVCS": "*:off",
		"GOPRIVATE": "", "GONOPROXY": "none", "GONOSUMDB": "none",
		"GOWORK": "off", "GOFLAGS": "", "CGO_ENABLED": "0", "GOOS": "windows", "GOARCH": "amd64",
		"GOROOT": o.goRoot, "GOCACHEPROG": "", "GO111MODULE": "on", "GOEXPERIMENT": "",
		"GOMODCACHE": o.moduleCache, "GOCACHE": filepath.Join(o.root, "gocache"), "GOPATH": filepath.Join(o.root, "gopath"),
		"TEMP": filepath.Join(o.root, "temp"), "TMP": filepath.Join(o.root, "temp"), "TMPDIR": filepath.Join(o.root, "temp"),
		"GOTMPDIR": filepath.Join(o.root, "temp"),
		"APPDATA":  filepath.Join(o.root, "appdata"), "LOCALAPPDATA": filepath.Join(o.root, "localappdata"),
	}
	env := make([]string, 0, len(os.Environ())+len(values))
	for _, entry := range os.Environ() {
		key, _, _ := strings.Cut(entry, "=")
		key = strings.ToUpper(key)
		if strings.HasPrefix(key, "MCPHUB_") {
			continue
		}
		if _, replaced := values[key]; !replaced {
			env = append(env, entry)
		}
	}
	for key, value := range values {
		env = append(env, key+"="+value)
	}
	return env
}

func runStage(ctx context.Context, root, stage string, cmd *exec.Cmd) (returned error) {
	stdout, err := os.OpenFile(filepath.Join(root, "proof", stage+".stdout.log"), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	defer func() { returned = errors.Join(returned, stdout.Close()) }()
	stderr, err := os.OpenFile(filepath.Join(root, "proof", stage+".stderr.log"), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	defer func() { returned = errors.Join(returned, stderr.Close()) }()
	deadline, ok := ctx.Deadline()
	if !ok {
		return errors.New("build stage requires a finite deadline")
	}
	process.NoConsole(cmd)
	receipt, runErr := process.RunContainedStreamObserved(ctx, cmd, process.ContainedStreamOptions{
		CleanupTimeout: 15 * time.Second, AbsoluteCleanupDeadline: deadline.Add(15 * time.Second),
		StdoutByteLimit: 16 << 20, StderrByteLimit: 16 << 20, Stderr: stderr,
	}, func(reader io.Reader) error { _, err := io.Copy(stdout, reader); return err })
	complete := receipt.NativeSettlement != nil && receipt.NativeSettlement.Complete() && receipt.IOSettlement.Complete()
	var native process.ContainedNativeSnapshot
	if receipt.NativeSettlement != nil {
		native = receipt.NativeSettlement.Snapshot()
	}
	failure := ""
	if runErr != nil {
		failure = runErr.Error()
	}
	proofErr := writeJSON(filepath.Join(root, "proof", stage+".settlement.json"), map[string]any{
		"stage": stage, "root": receipt.RootIdentity, "native": native, "io": receipt.IOSettlement,
		"process_and_io_complete": complete, "root_wait_known": receipt.RootWaitKnown, "exit_code": receipt.RootExitCode, "failure": failure,
	})
	if !complete || !receipt.RootWaitKnown || receipt.RootExitCode != 0 {
		runErr = errors.Join(runErr, errors.New(stage+": process and IO settlement did not prove successful completion"))
	}
	return errors.Join(runErr, proofErr, stdout.Sync(), stderr.Sync())
}

func createPrivateRoot(path string) error {
	user, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		return err
	}
	sid := user.User.Sid.String()
	sd, err := windows.SecurityDescriptorFromString("O:" + sid + "D:P(A;OICI;FA;;;" + sid + ")(A;OICI;FA;;;SY)(A;OICI;FA;;;BA)")
	if err != nil {
		return err
	}
	name, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return err
	}
	security := &windows.SecurityAttributes{Length: uint32(unsafe.Sizeof(windows.SecurityAttributes{})), SecurityDescriptor: sd}
	err = windows.CreateDirectory(name, security)
	runtime.KeepAlive(sd)
	return err
}

func pinOrdinaryPath(path string, directory bool) (held []windows.Handle, returned error) {
	defer func() {
		if returned != nil {
			for i := len(held) - 1; i >= 0; i-- {
				returned = errors.Join(returned, windows.CloseHandle(held[i]))
			}
			held = nil
		}
	}()
	parts := []string{filepath.VolumeName(path) + "\\"}
	for _, part := range strings.Split(strings.TrimPrefix(path, parts[0]), "\\") {
		if part != "" {
			parts = append(parts, filepath.Join(parts[len(parts)-1], part))
		}
	}
	for i, part := range parts {
		name, err := windows.UTF16PtrFromString(part)
		if err != nil {
			return held, err
		}
		leaf := i == len(parts)-1
		access := uint32(windows.READ_CONTROL | windows.FILE_READ_ATTRIBUTES)
		if leaf && !directory {
			access |= windows.GENERIC_READ
		}
		handle, err := windows.CreateFile(name, access, windows.FILE_SHARE_READ, nil, windows.OPEN_EXISTING,
			windows.FILE_FLAG_OPEN_REPARSE_POINT|windows.FILE_FLAG_BACKUP_SEMANTICS, 0)
		if err != nil {
			return held, err
		}
		held = append(held, handle)
		var info windows.ByHandleFileInformation
		if err := windows.GetFileInformationByHandle(handle, &info); err != nil {
			return held, err
		}
		if info.FileAttributes&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0 ||
			(!leaf || directory) != (info.FileAttributes&windows.FILE_ATTRIBUTE_DIRECTORY != 0) {
			return held, errors.New("reparse or unexpected file type in qualification builder path")
		}
	}
	return held, nil
}

func writeFresh(path string, raw []byte) (returned error) {
	file, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	defer func() { returned = errors.Join(returned, file.Close()) }()
	if _, err := file.Write(raw); err != nil {
		return err
	}
	return file.Sync()
}

func writeJSON(path string, value any) error {
	raw, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	return writeFresh(path, append(raw, '\n'))
}

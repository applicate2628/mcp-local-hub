//go:build windows

// test-recovery-namespace runs only the three native recovery namespace tests.
// Both compilation and execution use the production contained-process owner.
package main

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"

	"mcp-local-hub/internal/process"
)

const selectedTests = "^TestSecretsInit_RecoveryNamespace(Boundary|RetainedDirectory|FailurePaths)$"

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "recovery namespace fixture:", err)
		os.Exit(1)
	}
}

func run() (returned error) {
	repoFlag := flag.String("repo", "", "absolute repository worktree")
	goFlag := flag.String("go", "", "absolute existing go.exe (no downloads)")
	pwshFlag := flag.String("pwsh", "", "absolute existing PowerShell 7 executable")
	parentFlag := flag.String("parent", os.TempDir(), "existing absolute parent for a fresh private fixture root")
	buildTimeout := flag.Duration("build-timeout", 5*time.Minute, "finite API test compilation deadline")
	testTimeout := flag.Duration("test-timeout", 3*time.Minute, "finite Go test deadline")
	flag.Parse()
	if flag.NArg() != 0 || *buildTimeout <= 0 || *testTimeout <= 0 || *buildTimeout > 10*time.Minute || *testTimeout > 10*time.Minute {
		return errors.New("invalid arguments or deadline")
	}
	repo, err := localPath(*repoFlag)
	if err != nil {
		return err
	}
	goPath, err := localPath(*goFlag)
	if err != nil {
		return err
	}
	pwsh, err := localPath(*pwshFlag)
	if err != nil {
		return err
	}
	parent, err := localPath(*parentFlag)
	if err != nil {
		return err
	}
	var pins []windows.Handle
	defer func() {
		for i := len(pins) - 1; i >= 0; i-- {
			returned = errors.Join(returned, windows.CloseHandle(pins[i]))
		}
	}()
	for _, entry := range []struct {
		path      string
		directory bool
	}{{repo, true}, {parent, true}, {goPath, false}, {pwsh, false}} {
		held, err := pinOrdinaryPath(entry.path, entry.directory)
		if err != nil {
			return err
		}
		pins = append(pins, held...)
	}
	var nonce [16]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return err
	}
	root := filepath.Join(parent, "mcphub-recovery-namespace-"+hex.EncodeToString(nonce[:]))
	tokenUser, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		return err
	}
	sddl := "O:" + tokenUser.User.Sid.String() + "D:P(A;OICI;FA;;;" + tokenUser.User.Sid.String() + ")(A;OICI;FA;;;SY)(A;OICI;FA;;;BA)"
	sd, err := windows.SecurityDescriptorFromString(sddl)
	if err != nil {
		return err
	}
	for _, dir := range []string{root, filepath.Join(root, "temp"), filepath.Join(root, "proof"), filepath.Join(root, "bin")} {
		path, err := windows.UTF16PtrFromString(dir)
		if err != nil {
			return err
		}
		security := &windows.SecurityAttributes{Length: uint32(unsafe.Sizeof(windows.SecurityAttributes{})), SecurityDescriptor: sd}
		if err := windows.CreateDirectory(path, security); err != nil {
			return fmt.Errorf("create fresh private directory: %w", err)
		}
		held, err := pinOrdinaryPath(dir, true)
		if err != nil {
			return err
		}
		pins = append(pins, held...)
	}
	runtime.KeepAlive(sd)
	fmt.Println("Private evidence root:", root)

	sourceLeaves := []string{"go.mod", "go.sum", "internal/api/secrets_init_recovery_test.go", "internal/api/secrets_init_recovery_directory_windows.go", "tools/test-recovery-namespace/main_windows.go", "scripts/test-recovery-namespace/helper.ps1"}
	sourceHashes := make(map[string]string)
	var helper []byte
	for _, leaf := range sourceLeaves {
		path := filepath.Join(repo, filepath.FromSlash(leaf))
		held, err := pinOrdinaryPath(path, false)
		if err != nil {
			return err
		}
		pins = append(pins, held...)
		raw, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		digest := sha256.Sum256(raw)
		sourceHashes[leaf] = hex.EncodeToString(digest[:])
		if leaf == "scripts/test-recovery-namespace/helper.ps1" {
			helper = raw
		}
	}
	helperPath := filepath.Join(root, "helper.ps1")
	if err := writeFresh(helperPath, helper); err != nil {
		return err
	}
	helperPins, err := pinOrdinaryPath(helperPath, false)
	if err != nil {
		return err
	}
	pins = append(pins, helperPins...)
	if err := writeJSON(filepath.Join(root, "proof", "inputs.json"), struct {
		Repository           string            `json:"repository"`
		SelectedSourceSHA256 map[string]string `json:"selected_source_sha256"`
		Go                   string            `json:"go"`
		PowerShell           string            `json:"powershell"`
		Tests                string            `json:"tests"`
	}{repo, sourceHashes, goPath, pwsh, selectedTests}); err != nil {
		return err
	}

	env := fixtureEnvironment(map[string]string{
		"GOTOOLCHAIN": "local", "GOPROXY": "off", "GOSUMDB": "off", "GOFLAGS": "", "GOWORK": "off",
		"GOOS": "windows", "GOARCH": runtime.GOARCH,
		"TEMP": filepath.Join(root, "temp"), "TMP": filepath.Join(root, "temp"), "TMPDIR": filepath.Join(root, "temp"),
		"GOTMPDIR":                                 filepath.Join(root, "temp"),
		"MCPHUB_RECOVERY_NAMESPACE_ROOT":           root,
		"MCPHUB_RECOVERY_NAMESPACE_FIXTURE_HELPER": helperPath,
		"MCPHUB_RECOVERY_NAMESPACE_FIXTURE_SHA256": sourceHashes["scripts/test-recovery-namespace/helper.ps1"],
		"MCPHUB_RECOVERY_NAMESPACE_PWSH":           pwsh,
	})
	binary := filepath.Join(root, "bin", "api.test.exe")
	build := exec.Command(goPath, "test", "-mod=readonly", "-c", "-o", binary, "./internal/api")
	build.Dir, build.Env = repo, env
	fmt.Println("Compiling API test binary with existing local dependencies.")
	if _, err := runStage(root, "build", build, *buildTimeout); err != nil {
		return err
	}
	held, err := pinOrdinaryPath(binary, false)
	if err != nil {
		return err
	}
	pins = append(pins, held...)
	binaryFile, err := os.Open(binary)
	if err != nil {
		return err
	}
	binaryDigest := sha256.New()
	_, hashErr := io.Copy(binaryDigest, binaryFile)
	if err := errors.Join(hashErr, binaryFile.Close()); err != nil {
		return err
	}
	if err := writeJSON(filepath.Join(root, "proof", "binary.json"), map[string]string{"path": binary, "sha256": hex.EncodeToString(binaryDigest.Sum(nil))}); err != nil {
		return err
	}

	cmd := exec.Command(binary, "-test.run="+selectedTests, "-test.count=1", "-test.timeout="+testTimeout.String(), "-test.v")
	cmd.Dir, cmd.Env = repo, env
	fmt.Println("Running the three recovery namespace tests in a contained process.")
	receipt, runErr := runStage(root, "tests", cmd, *testTimeout+10*time.Second)
	if runErr != nil {
		return runErr
	}
	if err := verifyObservations(root, binary, receipt.RootIdentity); err != nil {
		return err
	}
	fmt.Println("Namespace tests and original-process measurement bindings passed. Evidence retained:", root)
	return nil
}

func runStage(root, stage string, cmd *exec.Cmd, timeout time.Duration) (receipt process.ContainedStreamReceiptV1, returned error) {
	var empty process.ContainedStreamReceiptV1
	stdout, err := os.OpenFile(filepath.Join(root, "proof", stage+".stdout.log"), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return empty, err
	}
	defer func() { returned = errors.Join(returned, stdout.Close()) }()
	stderr, err := os.OpenFile(filepath.Join(root, "proof", stage+".stderr.log"), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return empty, err
	}
	defer func() { returned = errors.Join(returned, stderr.Close()) }()
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	deadline, _ := ctx.Deadline()
	process.NoConsole(cmd)
	receipt, runErr := process.RunContainedStreamObserved(ctx, cmd, process.ContainedStreamOptions{
		CleanupTimeout: 10 * time.Second, AbsoluteCleanupDeadline: deadline.Add(10 * time.Second),
		StdoutByteLimit: 16 << 20, StderrByteLimit: 16 << 20, Stderr: stderr,
	}, func(reader io.Reader) error { _, err := io.Copy(stdout, reader); return err })
	complete := receipt.NativeSettlement != nil && receipt.NativeSettlement.Complete() && receipt.IOSettlement.Complete()
	var native process.ContainedNativeSnapshot
	if receipt.NativeSettlement != nil {
		native = receipt.NativeSettlement.Snapshot()
	}
	proofErr := writeJSON(filepath.Join(root, "proof", stage+".settlement.json"), struct {
		Stage    string                          `json:"stage"`
		Root     process.ContainedRootIdentity   `json:"root"`
		Native   process.ContainedNativeSnapshot `json:"native"`
		IO       process.ContainedIOSettlement   `json:"io"`
		Complete bool                            `json:"complete"`
		Failure  string                          `json:"failure,omitempty"`
	}{stage, receipt.RootIdentity, native, receipt.IOSettlement, complete, errorText(runErr)})
	if runErr != nil || proofErr != nil {
		return receipt, errors.Join(runErr, proofErr)
	}
	if !complete || !receipt.RootWaitKnown || receipt.RootExitCode != 0 {
		return receipt, errors.New(stage + ": positive process/IO settlement is incomplete")
	}
	return receipt, errors.Join(stdout.Sync(), stderr.Sync())
}

func verifyObservations(root, binary string, identity process.ContainedRootIdentity) error {
	if !identity.Known || identity.PID <= 0 || identity.NativeBirthFILETIME == 0 {
		return errors.New("original actor identity was not observed")
	}
	paths, err := filepath.Glob(filepath.Join(root, "proof", "handles-*.json"))
	if err != nil {
		return err
	}
	if len(paths) != 4 {
		return fmt.Errorf("expected four real handle observations from two test boundaries, received %d", len(paths))
	}
	for _, path := range paths {
		raw, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		var observed struct {
			ActorPID uint32 `json:"ActorPid"`
			Birth    uint64 `json:"CreationTimeFILETIME"`
			Image    string `json:"ExecutablePath"`
			Count    uint32 `json:"HandleCount"`
		}
		if err := json.Unmarshal(raw, &observed); err != nil {
			return err
		}
		if observed.ActorPID != uint32(identity.PID) || observed.Birth != identity.NativeBirthFILETIME || !strings.EqualFold(filepath.Clean(observed.Image), binary) || observed.Count == 0 {
			return errors.New("handle measurement does not match the original contained actor")
		}
	}
	raw, err := os.ReadFile(filepath.Join(root, "proof", "wrong-owner-attempt.json"))
	if err != nil {
		return err
	}
	var owner struct {
		Assigned                                bool
		Failure                                 string
		Attempts                                int
		OwnerBefore, OwnerAfter, RequestedOwner string
	}
	if err := json.Unmarshal(raw, &owner); err != nil {
		return err
	}
	if owner.Attempts != 1 || owner.OwnerBefore == "" || owner.RequestedOwner != "S-1-5-32-545" {
		return errors.New("wrong-owner attempt evidence is incomplete")
	}
	if owner.Assigned {
		if owner.Failure != "" || owner.OwnerAfter != owner.RequestedOwner {
			return errors.New("wrong-owner success did not read back")
		}
		fmt.Println("Wrong-owner rejection exercised after one successful private owner assignment.")
	} else {
		if owner.Failure == "" || owner.OwnerAfter != owner.OwnerBefore {
			return errors.New("wrong-owner refusal evidence is inconsistent")
		}
		fmt.Println("UNVERIFIED actual wrong-owner receiving: ordinary-token assignment was refused; see private proof.")
	}
	return nil
}

func localPath(path string) (string, error) {
	clean := filepath.Clean(path)
	if !filepath.IsAbs(path) || len(clean) < 3 || clean[1] != ':' || clean[2] != '\\' || !((clean[0] >= 'A' && clean[0] <= 'Z') || (clean[0] >= 'a' && clean[0] <= 'z')) {
		return "", errors.New("an absolute local drive path is required")
	}
	for _, part := range strings.Split(clean[3:], `\`) {
		if strings.HasSuffix(part, ".") || strings.HasSuffix(part, " ") || strings.ContainsAny(part, `:"<>|?*`) {
			return "", errors.New("ambiguous path component refused")
		}
	}
	return clean, nil
}

// Retain every ordinary ancestor without write/delete sharing, preventing both
// replacement and in-place reparse changes. Source bytes stay bound as well.
func pinOrdinaryPath(path string, directory bool) (held []windows.Handle, returned error) {
	defer func() {
		if returned != nil {
			for _, handle := range held {
				_ = windows.CloseHandle(handle)
			}
			held = nil
		}
	}()
	parts := []string{filepath.VolumeName(path) + `\`}
	for _, part := range strings.Split(strings.TrimPrefix(path, parts[0]), `\`) {
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
		access, share := uint32(windows.READ_CONTROL|windows.FILE_READ_ATTRIBUTES), uint32(windows.FILE_SHARE_READ)
		if leaf && !directory {
			access |= windows.GENERIC_READ
			share = windows.FILE_SHARE_READ
		}
		handle, err := windows.CreateFile(name, access, share, nil, windows.OPEN_EXISTING, windows.FILE_FLAG_OPEN_REPARSE_POINT|windows.FILE_FLAG_BACKUP_SEMANTICS, 0)
		if err != nil {
			return held, err
		}
		held = append(held, handle)
		var info windows.ByHandleFileInformation
		if err := windows.GetFileInformationByHandle(handle, &info); err != nil {
			return held, err
		}
		if info.FileAttributes&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0 || (!leaf || directory) != (info.FileAttributes&windows.FILE_ATTRIBUTE_DIRECTORY != 0) {
			return held, errors.New("reparse or unexpected file type in fixture path")
		}
	}
	return held, nil
}

func fixtureEnvironment(values map[string]string) []string {
	env := make([]string, 0, len(os.Environ())+len(values))
	for _, entry := range os.Environ() {
		key, _, _ := strings.Cut(entry, "=")
		// API TestMain has child-process sentinels before its sandbox setup.
		// A stale inherited sentinel must never select an external state root.
		if strings.HasPrefix(strings.ToUpper(key), "MCPHUB_") {
			continue
		}
		if _, replaced := values[strings.ToUpper(key)]; !replaced {
			env = append(env, entry)
		}
	}
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		env = append(env, key+"="+values[key])
	}
	return env
}

func writeFresh(path string, raw []byte) (err error) {
	file, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, file.Close()) }()
	if _, err = file.Write(raw); err != nil {
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

func errorText(err error) string {
	if err != nil {
		return err.Error()
	}
	return ""
}

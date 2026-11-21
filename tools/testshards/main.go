// Command testshards provides an optional Go-test coverage diagnostic.
// It does not change or replace the repository's mandatory unsharded gate.
package main

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

type shard struct {
	Index    int      `json:"index"`
	Roots    []string `json:"roots"`
	Selector string   `json:"selector"`
}

type manifest struct {
	Commit                          string  `json:"commit"`
	GoVersion                       string  `json:"goVersion"`
	Package                         string  `json:"package"`
	Tags                            string  `json:"tags"`
	TestMainDeclarations            int     `json:"testMainDeclarations"`
	BenchmarksNotRun                int     `json:"benchmarksNotRun"`
	RootCount                       int     `json:"rootCount"`
	MandatoryUnshardedGateUnchanged bool    `json:"mandatoryUnshardedGateUnchanged"`
	EquivalentToSingleProcess       bool    `json:"equivalentToSingleProcess"`
	Shards                          []shard `json:"shards"`
}

type outcome struct {
	Index       int      `json:"index"`
	LogSHA256   string   `json:"logSHA256"`
	Expected    int      `json:"expected"`
	Completed   int      `json:"completed"`
	Missing     []string `json:"missing"`
	Problems    []string `json:"problems"`
	Skipped     []string `json:"skipped"`
	ExitCode    int      `json:"exitCode"`
	PackagePass bool     `json:"packagePass"`
	Valid       bool     `json:"valid"`
}

var testName = regexp.MustCompile(`^(Test[A-Za-z0-9_]+|Fuzz[A-Za-z0-9_]+|Example[A-Za-z0-9_]*)$`)
var benchmarkName = regexp.MustCompile(`^Benchmark[A-Za-z0-9_]+$`)

func parseInventory(raw []byte) ([]string, int, error) {
	var names []string
	seen := make(map[string]bool)
	benchmarks := 0
	scan := bufio.NewScanner(bytes.NewReader(raw))
	scan.Buffer(make([]byte, 4096), 1<<20)
	for scan.Scan() {
		line := strings.TrimSpace(scan.Text())
		switch {
		case testName.MatchString(line):
			if seen[line] {
				return nil, 0, fmt.Errorf("duplicate root %q", line)
			}
			seen[line] = true
			names = append(names, line)
		case benchmarkName.MatchString(line):
			benchmarks++
		case strings.HasPrefix(line, "Test"), strings.HasPrefix(line, "Example"),
			strings.HasPrefix(line, "Fuzz"), strings.HasPrefix(line, "Benchmark"):
			return nil, 0, fmt.Errorf("unrecognized Go test name %q", line)
		}
	}
	if err := scan.Err(); err != nil {
		return nil, 0, err
	}
	if len(names) == 0 {
		return nil, 0, errors.New("no compiled Test/Example/Fuzz roots")
	}
	return names, benchmarks, nil
}

func partition(names []string, maximum int) ([]shard, error) {
	if maximum < 1 || maximum > 150 || len(names) == 0 {
		return nil, errors.New("invalid shard limit or empty test inventory")
	}
	sorted := append([]string(nil), names...)
	sort.Strings(sorted)
	for i, name := range sorted {
		if !testName.MatchString(name) {
			return nil, fmt.Errorf("invalid root %q", name)
		}
		if i > 0 && sorted[i-1] == name {
			return nil, fmt.Errorf("duplicate root %q", name)
		}
	}
	var plan []shard
	claimed := make(map[string]bool, len(sorted))
	for offset := 0; offset < len(sorted); offset += maximum {
		end := offset + maximum
		if end > len(sorted) {
			end = len(sorted)
		}
		roots := append([]string(nil), sorted[offset:end]...)
		patterns := make([]string, len(roots))
		for i, root := range roots {
			if claimed[root] {
				return nil, fmt.Errorf("overlapping root %q", root)
			}
			claimed[root] = true
			patterns[i] = regexp.QuoteMeta(root)
		}
		selector := "^(" + strings.Join(patterns, "|") + ")$"
		if len(selector) > 14000 {
			return nil, errors.New("shard selector exceeds safe command length")
		}
		plan = append(plan, shard{Index: len(plan), Roots: roots, Selector: selector})
	}
	if len(claimed) != len(names) {
		return nil, errors.New("incomplete root coverage")
	}
	return plan, nil
}

func countTestMain(repo, pkg string) (int, error) {
	path := filepath.Join(repo, filepath.FromSlash(strings.TrimPrefix(pkg, "./")))
	files, err := os.ReadDir(path)
	if err != nil {
		return 0, err
	}
	rule := regexp.MustCompile("(?m)^func TestMain[(]")
	count := 0
	for _, file := range files {
		if file.IsDir() || !strings.HasSuffix(file.Name(), "_test.go") {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(path, file.Name()))
		if err != nil {
			return 0, err
		}
		count += len(rule.FindAll(raw, -1))
	}
	return count, nil
}

func isolatedEnv(root string) ([]string, error) {
	paths := map[string]string{}
	for _, key := range []string{"TMP", "HOME", "APPDATA", "LOCALAPPDATA"} {
		path := filepath.Join(root, strings.ToLower(key))
		if err := os.MkdirAll(path, 0700); err != nil {
			return nil, err
		}
		paths[key] = path
	}
	overrides := map[string]string{
		"TEMP": paths["TMP"], "TMP": paths["TMP"], "TMPDIR": paths["TMP"],
		"GOTMPDIR": paths["TMP"], "HOME": paths["HOME"], "USERPROFILE": paths["HOME"],
		"APPDATA": paths["APPDATA"], "LOCALAPPDATA": paths["LOCALAPPDATA"],
		"GOENV": "off", "GOPROXY": "off", "GOSUMDB": "off", "GOVCS": "*:off",
		"GOTOOLCHAIN": "local", "GOWORK": "off", "GOFLAGS": "", "MCPHUB_E2E_SCHEDULER": "none",
	}
	env := make([]string, 0, len(os.Environ())+len(overrides))
	for _, entry := range os.Environ() {
		key, _, _ := strings.Cut(entry, "=")
		if _, replaced := overrides[strings.ToUpper(key)]; !replaced {
			env = append(env, entry)
		}
	}
	for key, value := range overrides {
		env = append(env, key+"="+value)
	}
	return env, nil
}

func jsonWrite(path string, value any) error {
	bytes, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(bytes, byte(10)), 0600)
}

type event struct {
	Action string `json:"Action"`
	Test   string `json:"Test"`
}

func analyzeLog(raw []byte, s shard, code int) outcome {
	result := outcome{Index: s.Index, Expected: len(s.Roots), ExitCode: code,
		Missing: []string{}, Problems: []string{}, Skipped: []string{}}
	expected := make(map[string]bool, len(s.Roots))
	for _, root := range s.Roots {
		expected[root] = true
	}
	seen := make(map[string]bool, len(s.Roots))
	scan := bufio.NewScanner(bytes.NewReader(raw))
	scan.Buffer(make([]byte, 16*1024), 16<<20)
	for scan.Scan() {
		var e event
		if err := json.Unmarshal(scan.Bytes(), &e); err != nil {
			result.Problems = append(result.Problems, "non-JSON test output")
			continue
		}
		if e.Action == "pass" && e.Test == "" {
			result.PackagePass = true
		}
		if e.Action == "skip" && e.Test != "" {
			result.Skipped = append(result.Skipped, e.Test)
		}
		if e.Test == "" || strings.Contains(e.Test, "/") ||
			(e.Action != "pass" && e.Action != "fail" && e.Action != "skip") {
			continue
		}
		if !expected[e.Test] || seen[e.Test] {
			result.Problems = append(result.Problems, "unexpected/repeated root: "+e.Test)
		}
		seen[e.Test] = true
		if e.Action != "pass" {
			result.Problems = append(result.Problems, "not passed: "+e.Test)
		}
	}
	if err := scan.Err(); err != nil {
		result.Problems = append(result.Problems, err.Error())
	}
	result.Completed = len(seen)
	for _, root := range s.Roots {
		if !seen[root] {
			result.Missing = append(result.Missing, root)
		}
	}
	result.Valid = code == 0 && result.PackagePass && len(result.Missing) == 0 &&
		len(result.Problems) == 0 && len(result.Skipped) == 0
	return result
}

func runGo(goBinary, repo string, env, args []string, log string) (int, error) {
	cmd := exec.Command(goBinary, args...)
	cmd.Dir, cmd.Env = repo, env
	file, err := os.Create(log)
	if err != nil {
		return -1, err
	}
	cmd.Stdout, cmd.Stderr = file, file
	runErr := cmd.Run()
	closeErr := file.Close()
	if closeErr != nil {
		return -1, closeErr
	}
	if runErr == nil {
		return 0, nil
	}
	var exit *exec.ExitError
	if errors.As(runErr, &exit) {
		return exit.ExitCode(), nil
	}
	return -1, runErr
}

func gitInfo(repo string) (string, error) {
	head := exec.Command("git", "-C", repo, "rev-parse", "HEAD")
	raw, err := head.Output()
	if err != nil {
		return "", err
	}
	status := exec.Command("git", "-C", repo, "status", "--porcelain=v1", "--untracked-files=all")
	dirty, err := status.Output()
	if err != nil {
		return "", err
	}
	if len(bytes.TrimSpace(dirty)) != 0 {
		return "", errors.New("source must be clean")
	}
	return strings.TrimSpace(string(raw)), nil
}

func main() {
	if err := execute(); err != nil {
		fmt.Fprintln(os.Stderr, "testshards:", err)
		os.Exit(1)
	}
}
func execute() error {
	repoFlag := flag.String("repo", "", "absolute path to a clean source checkout")
	outFlag := flag.String("evidence", "", "new, separate evidence directory")
	pkg := flag.String("package", "./internal/api", "exact Go package: api or cli")
	tags := flag.String("tags", "", "optional Go test build tags")
	goBinary := flag.String("go", "", "absolute path to already installed Go executable")
	maximum := flag.Int("max-roots", 80, "maximum compiled root tests per shard (1..150)")
	run := flag.Bool("execute", false, "execute planned shards, not a replacement for the main gate")
	selected := flag.Int("shard", -1, "diagnose only one shard index, with -execute")
	flag.Parse()
	if flag.NArg() != 0 {
		return errors.New("unexpected positional arguments")
	}
	if *pkg != "./internal/api" && *pkg != "./internal/cli" {
		return errors.New("only the API and CLI gate packages are supported")
	}
	if *selected >= 0 && !*run {
		return errors.New("-shard requires -execute")
	}
	if *selected < -1 {
		return errors.New("invalid shard index")
	}
	for _, required := range []string{*repoFlag, *outFlag, *goBinary} {
		if !filepath.IsAbs(required) {
			return errors.New("repo/evidence/go must be absolute paths")
		}
	}
	repo, err := filepath.Abs(*repoFlag)
	if err != nil {
		return err
	}
	out, err := filepath.Abs(*outFlag)
	if err != nil {
		return err
	}
	relative, err := filepath.Rel(repo, out)
	if err != nil {
		return err
	}
	if relative == "." || (relative != ".." && !strings.HasPrefix(relative, ".."+string(os.PathSeparator))) {
		return errors.New("evidence directory must remain outside the source checkout")
	}
	if _, err := os.Stat(out); err == nil {
		return errors.New("evidence directory already exists")
	} else if !os.IsNotExist(err) {
		return err
	}
	for _, key := range []string{"GOROOT", "GOCACHE", "GOMODCACHE"} {
		if !filepath.IsAbs(os.Getenv(key)) {
			return fmt.Errorf("explicit absolute %s required", key)
		}
	}
	if _, err := os.Stat(*goBinary); err != nil {
		return err
	}
	head, err := gitInfo(repo)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(out, 0700); err != nil {
		return err
	}
	env, err := isolatedEnv(filepath.Join(out, "inventory"))
	if err != nil {
		return err
	}
	args := []string{"test", "-list", ".", "-p=1"}
	if *tags != "" {
		args = append(args, "-tags="+*tags)
	}
	args = append(args, *pkg)
	inventoryLog := filepath.Join(out, "inventory.txt")
	code, err := runGo(*goBinary, repo, env, args, inventoryLog)
	if err != nil {
		return err
	}
	raw, err := os.ReadFile(inventoryLog)
	if err != nil {
		return err
	}
	if code != 0 {
		return fmt.Errorf("go test -list failed (exit %d); see %s", code, inventoryLog)
	}
	names, benchmarks, err := parseInventory(raw)
	if err != nil {
		return err
	}
	shards, err := partition(names, *maximum)
	if err != nil {
		return err
	}
	testMainCount, err := countTestMain(repo, *pkg)
	if err != nil {
		return err
	}
	if testMainCount != 1 {
		return fmt.Errorf("expected one test process TestMain, found %d", testMainCount)
	}
	listedHead, err := gitInfo(repo)
	if err != nil || listedHead != head {
		return errors.New("source changed during test inventory")
	}
	version, err := exec.Command(*goBinary, "version").Output()
	if err != nil {
		return fmt.Errorf("read Go version: %w", err)
	}
	m := manifest{Commit: head, GoVersion: strings.TrimSpace(string(version)), Package: *pkg, Tags: *tags, RootCount: len(names),
		BenchmarksNotRun: benchmarks, TestMainDeclarations: testMainCount,
		MandatoryUnshardedGateUnchanged: true, EquivalentToSingleProcess: false, Shards: shards}
	if err := jsonWrite(filepath.Join(out, "plan.json"), m); err != nil {
		return err
	}
	fmt.Printf("PLAN HEAD=%s PACKAGE=%s ROOTS=%d SHARDS=%d TESTMAIN=%d BENCHMARKS_NOT_RUN=%d\n",
		head, *pkg, len(names), len(shards), testMainCount, benchmarks)
	if !*run {
		return nil
	}
	if *selected >= len(shards) {
		return errors.New("shard index out of range")
	}
	var results []outcome
	for _, part := range shards {
		if *selected >= 0 && *selected != part.Index {
			continue
		}
		dir := filepath.Join(out, fmt.Sprintf("shard-%03d", part.Index))
		childEnv, err := isolatedEnv(dir)
		if err != nil {
			return err
		}
		command := []string{"test", "-json", "-count=1", "-p=1", "-timeout=5m", "-run", part.Selector}
		if *tags != "" {
			command = append(command, "-tags="+*tags)
		}
		command = append(command, *pkg)
		log := filepath.Join(dir, "go-test.jsonl")
		code, err := runGo(*goBinary, repo, childEnv, command, log)
		if err != nil {
			return err
		}
		info, err := os.Stat(log)
		if err != nil {
			return err
		}
		if info.Size() > 64<<20 {
			return errors.New("test log exceeds diagnostic bound; preserve original log")
		}
		raw, err := os.ReadFile(log)
		if err != nil {
			return err
		}
		result := analyzeLog(raw, part, code)
		digest := sha256.Sum256(raw)
		result.LogSHA256 = hex.EncodeToString(digest[:])
		results = append(results, result)
		if err := jsonWrite(filepath.Join(out, "results.json"), results); err != nil {
			return err
		}
		fmt.Printf("SHARD=%d PASS=%t ROOTS=%d/%d SKIPS=%d EXIT=%d\n", part.Index,
			result.Valid, result.Completed, result.Expected, len(result.Skipped), code)
		if !result.Valid {
			return fmt.Errorf("shard %d failed; see %s", part.Index, log)
		}
		newHead, err := gitInfo(repo)
		if err != nil || newHead != head {
			return errors.New("source changed during verification")
		}
	}
	fmt.Printf("DIAGNOSTIC_SHARDS_EXECUTED=%d/%d; ORIGINAL_UNSHARDED_GATE_NOT_REPLACED\n", len(results), len(shards))
	return nil
}

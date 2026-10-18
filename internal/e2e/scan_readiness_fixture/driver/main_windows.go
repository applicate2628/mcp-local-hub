//go:build windows

// Guest-only receiving driver. It observes an already installed fixture;
// it never launches Windows Sandbox or creates a Product manifest/install.
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
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

const installedProductSHA256 = "7AE08112EC2929AA5508009BFEEC34BE06ACDC3253B1CBEF7270D9F4018D1556"
const installedCommit = "995ab3ec5ffe1b90485992b7dacd5d68939bc503"
const maxReceiptFiles = 16
const maxReceiptBytes = 4 << 20
const maxCLIBytes = 1 << 20

var errNoListener = errors.New("no loopback listener")

type limitedBuffer struct {
	bytes.Buffer
	limit int
}

func (b *limitedBuffer) Write(p []byte) (int, error) {
	if b.Len()+len(p) > b.limit {
		return 0, fmt.Errorf("command output exceeds %d bytes", b.limit)
	}
	return b.Buffer.Write(p)
}

type guestRun struct {
	product      string
	fixture      string
	manifest     string
	receiptDir   string
	port         int
	client       *http.Client
	written      int
	bytesWritten int
}

func (g *guestRun) command(parent context.Context, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(parent, 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, g.product, args...)
	stdout := &limitedBuffer{limit: maxCLIBytes}
	stderr := &limitedBuffer{limit: 64 << 10}
	cmd.Stdout, cmd.Stderr = stdout, stderr
	cmd.WaitDelay = time.Second
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("mcphub %s: %w; stderr=%q", strings.Join(args, " "), err, stderr.String())
	}
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	return stdout.Bytes(), nil
}

func (g *guestRun) write(name string, data []byte) error {
	if strings.ContainsAny(name, `/\:`) || name == "." || name == ".." {
		return fmt.Errorf("invalid receipt name")
	}
	if g.written+1 > maxReceiptFiles || g.bytesWritten+len(data) > maxReceiptBytes {
		return fmt.Errorf("receipt cap exceeded")
	}
	path := filepath.Join(g.receiptDir, name)
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return err
	}
	_, writeErr := f.Write(data)
	if writeErr == nil {
		writeErr = f.Sync()
	}
	closeErr := f.Close()
	if writeErr != nil {
		return writeErr
	}
	if closeErr != nil {
		return closeErr
	}
	readback, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	if !bytes.Equal(readback, data) {
		return fmt.Errorf("receipt readback mismatch: %s", name)
	}
	g.written++
	g.bytesWritten += len(data)
	return nil
}

func checkEmptyReceiptDir(path string) error {
	if !filepath.IsAbs(path) {
		return fmt.Errorf("receipt directory must be absolute")
	}
	clean := filepath.Clean(path)
	for cur := clean; ; cur = filepath.Dir(cur) {
		info, err := os.Lstat(cur)
		if err != nil {
			return err
		}
		if info.Mode()&os.ModeSymlink != 0 || info.Mode()&os.ModeIrregular != 0 {
			return fmt.Errorf("reparse/irregular receipt path component: %s", cur)
		}
		name, err := syscall.UTF16PtrFromString(cur)
		if err != nil {
			return err
		}
		attributes, err := syscall.GetFileAttributes(name)
		if err != nil {
			return err
		}
		if attributes&syscall.FILE_ATTRIBUTE_REPARSE_POINT != 0 {
			return fmt.Errorf("reparse receipt path component: %s", cur)
		}
		if cur == filepath.Dir(cur) {
			break
		}
	}
	entries, err := os.ReadDir(clean)
	if err != nil {
		return err
	}
	if len(entries) != 0 {
		return fmt.Errorf("receipt directory is not empty")
	}
	return nil
}

func fileSHA256(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return strings.ToUpper(hex.EncodeToString(h.Sum(nil))), nil
}

func processStartID(pid int) (string, error) {
	handle, err := syscall.OpenProcess(syscall.PROCESS_QUERY_INFORMATION, false, uint32(pid))
	if err != nil {
		return "", err
	}
	defer syscall.CloseHandle(handle)
	var created, exited, kernel, user syscall.Filetime
	if err := syscall.GetProcessTimes(handle, &created, &exited, &kernel, &user); err != nil {
		return "", err
	}
	return strconv.FormatInt(created.Nanoseconds(), 10), nil
}

func portOwner(ctx context.Context, port int) (int, error) {
	// The numeric port is validated before interpolation. This is a guest-local
	// OS socket census, not a Product or fixture mutation.
	script := fmt.Sprintf("@(Get-NetTCPConnection -LocalAddress 127.0.0.1 -LocalPort %d -State Listen -ErrorAction SilentlyContinue | Select-Object -ExpandProperty OwningProcess) -join ','", port)
	probeCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	command := exec.CommandContext(probeCtx, "powershell.exe", "-NoProfile", "-NonInteractive", "-Command", script)
	out, err := command.Output()
	if err != nil {
		return 0, fmt.Errorf("port owner %d: %w", port, err)
	}
	if strings.TrimSpace(string(out)) == "" {
		return 0, errNoListener
	}
	owner, err := strconv.Atoi(strings.TrimSpace(string(out)))
	if err != nil || owner <= 0 {
		return 0, fmt.Errorf("port %d has no unique loopback owner: %q", port, out)
	}
	return owner, nil
}

func (g *guestRun) observedPair(ctx context.Context, recovered bool) (pair, []byte, []byte, error) {
	status, err := g.command(ctx, "status", "--json")
	if err != nil {
		return pair{}, nil, nil, err
	}
	scan, err := g.command(ctx, "scan", "--json")
	if err != nil {
		return pair{}, nil, nil, err
	}
	p, err := parsePair(status, scan, recovered)
	if err != nil {
		return pair{}, nil, nil, err
	}
	if p.Port != g.port {
		return pair{}, nil, nil, fmt.Errorf("unexpected fixture port %d", p.Port)
	}
	owner, err := portOwner(ctx, p.Port)
	if err != nil {
		return pair{}, nil, nil, err
	}
	if owner != p.PID {
		return pair{}, nil, nil, fmt.Errorf("port owner %d != status PID %d", owner, p.PID)
	}
	p.StartID, err = processStartID(p.PID)
	if err != nil {
		return pair{}, nil, nil, err
	}
	statusRow, err := focusStatus(status)
	if err != nil {
		return pair{}, nil, nil, err
	}
	scanEntry, err := focusScan(scan)
	return p, statusRow, scanEntry, err
}

func (g *guestRun) awaitPair(ctx context.Context, recovered bool) (pair, []byte, []byte, error) {
	var last error
	for ctx.Err() == nil {
		p, status, scan, err := g.observedPair(ctx, recovered)
		if err == nil {
			return p, status, scan, nil
		}
		last = err
		timer := time.NewTimer(2 * time.Second)
		select {
		case <-ctx.Done():
			timer.Stop()
		case <-timer.C:
		}
	}
	return pair{}, nil, nil, fmt.Errorf("fixture state not observed by deadline: %v: %w", last, ctx.Err())
}

func (g *guestRun) fixtureURL() string { return "http://127.0.0.1:" + strconv.Itoa(g.port+10000) }

func (g *guestRun) events(ctx context.Context) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, g.fixtureURL()+"/fixture/events", nil)
	if err != nil {
		return nil, err
	}
	resp, err := g.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("fixture events HTTP %d", resp.StatusCode)
	}
	return io.ReadAll(io.LimitReader(resp.Body, 64<<10))
}

func (g *guestRun) release(ctx context.Context) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, g.fixtureURL()+"/fixture/release", nil)
	if err != nil {
		return err
	}
	resp, err := g.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		return fmt.Errorf("fixture release HTTP %d", resp.StatusCode)
	}
	return nil
}

func (g *guestRun) releaseAfterWitness(ctx context.Context, raw []byte) error {
	var witness struct {
		Released    bool `json:"released"`
		Held        int  `json:"held"`
		Canceled    int  `json:"canceled"`
		Releases    int  `json:"releases"`
		Lists       int  `json:"lists"`
		Deletes     int  `json:"deletes"`
		Outstanding int  `json:"outstanding"`
		Dropped     int  `json:"dropped_events"`
		Events      []struct {
			Kind    string `json:"kind"`
			Session string `json:"session,omitempty"`
		} `json:"events"`
	}
	if err := json.Unmarshal(raw, &witness); err != nil {
		return err
	}
	var held, canceled bool
	for _, event := range witness.Events {
		if event.Kind == "initialize_held" {
			held = true
		}
		if event.Kind == "initialize_canceled" {
			canceled = true
		}
	}
	if witness.Released || witness.Releases != 0 || witness.Held < 1 || witness.Canceled < 1 || witness.Outstanding != 0 || len(witness.Events) > 64 || !held || !canceled {
		return fmt.Errorf("failure not tied to closed canceled initialize")
	}
	allowlisted, err := json.Marshal(witness)
	if err != nil {
		return err
	}
	if err := g.write("closed-events.json", allowlisted); err != nil {
		return err
	}
	return g.release(ctx)
}

func validateEvents(raw []byte) error {
	var e struct {
		Held        int               `json:"held"`
		Canceled    int               `json:"canceled"`
		Releases    int               `json:"releases"`
		Lists       int               `json:"lists"`
		Deletes     int               `json:"deletes"`
		Outstanding int               `json:"outstanding"`
		Events      []json.RawMessage `json:"events"`
	}
	if err := json.Unmarshal(raw, &e); err != nil {
		return err
	}
	if e.Held < 1 || e.Canceled < 1 || e.Releases != 1 || e.Lists < 1 || e.Deletes < 1 || e.Outstanding != 0 || len(e.Events) > 64 {
		return fmt.Errorf("fixture lifecycle events incomplete: held=%d canceled=%d releases=%d lists=%d deletes=%d outstanding=%d", e.Held, e.Canceled, e.Releases, e.Lists, e.Deletes, e.Outstanding)
	}
	return nil
}

func (g *guestRun) cleanup() error {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	var problems []error
	if _, err := g.command(ctx, "stop", "--server", fixtureName); err != nil {
		problems = append(problems, err)
	}
	if _, err := g.command(ctx, "uninstall", "--server", fixtureName); err != nil {
		problems = append(problems, err)
	}
	for _, port := range []int{g.port, g.port + 10000} {
		owner, err := portOwner(ctx, port)
		if err == nil {
			problems = append(problems, fmt.Errorf("fixture port %d still owned by PID %d", port, owner))
		} else if !errors.Is(err, errNoListener) {
			problems = append(problems, err)
		}
	}
	if raw, err := g.command(ctx, "status", "--json"); err != nil {
		problems = append(problems, err)
	} else {
		var rows []daemonRow
		if err := json.Unmarshal(raw, &rows); err != nil {
			problems = append(problems, err)
		} else {
			for _, row := range rows {
				if row.Server == fixtureName {
					problems = append(problems, fmt.Errorf("fixture status row remains after uninstall"))
				}
			}
		}
	}
	return errors.Join(problems...)
}

func (g *guestRun) run(ctx context.Context) (runErr error) {
	active, cancel := context.WithTimeout(ctx, 200*time.Second)
	defer cancel()
	defer func() {
		cleanupErr := g.cleanup()
		cleanupRecord, _ := json.Marshal(map[string]any{"ok": cleanupErr == nil})
		runErr = errors.Join(runErr, cleanupErr, g.write("cleanup.json", cleanupRecord))
	}()
	failed, status, scan, err := g.awaitPair(active, false)
	if err != nil {
		return err
	}
	if err := g.write("failed-status-row.json", status); err != nil {
		return err
	}
	if err := g.write("failed-scan-entry.json", scan); err != nil {
		return err
	}
	before, err := g.events(active)
	if err != nil {
		return err
	}
	if err := g.releaseAfterWitness(active, before); err != nil {
		return err
	}
	recovered, status, scan, err := g.awaitPair(active, true)
	if err != nil {
		return err
	}
	if err := sameGeneration(failed, recovered); err != nil {
		return err
	}
	if err := g.write("recovered-status-row.json", status); err != nil {
		return err
	}
	if err := g.write("recovered-scan-entry.json", scan); err != nil {
		return err
	}
	after, err := g.events(active)
	if err != nil {
		return err
	}
	if err := validateEvents(after); err != nil {
		return err
	}
	if err := g.write("fixture-events.json", after); err != nil {
		return err
	}
	identity, _ := json.Marshal(map[string]any{"failed": failed, "recovered": recovered})
	if err := g.write("identity.json", identity); err != nil {
		return err
	}
	return nil
}

func main() {
	product := flag.String("product", "", "absolute guest-private installed mcphub executable")
	fixture := flag.String("fixture", "", "absolute guest-private fixture executable")
	manifest := flag.String("manifest", "", "absolute guest-private manifest")
	expectedFixtureHash := flag.String("fixture-sha256", "", "reviewed fixture executable SHA-256")
	receipt := flag.String("receipt", "", "pre-created empty guest receipt export leaf")
	port := flag.Int("port", 0, "fixture external port")
	flag.Parse()
	if flag.NArg() != 0 || !filepath.IsAbs(*product) || !filepath.IsAbs(*fixture) || !filepath.IsAbs(*manifest) || !filepath.IsAbs(*receipt) || len(*expectedFixtureHash) != 64 || *port < 1 || *port > 55535 {
		fmt.Fprintln(os.Stderr, "guest driver requires exact absolute paths, reviewed fixture hash and valid port")
		os.Exit(2)
	}
	if err := checkEmptyReceiptDir(*receipt); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
	g := &guestRun{product: *product, fixture: *fixture, manifest: *manifest, receiptDir: *receipt, port: *port, client: &http.Client{Timeout: 3 * time.Second}}
	productHash, err := fileSHA256(*product)
	if err == nil && productHash != installedProductSHA256 {
		err = fmt.Errorf("Product hash mismatch")
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
	fixtureHash, err := fileSHA256(*fixture)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
	if !strings.EqualFold(fixtureHash, *expectedFixtureHash) {
		fmt.Fprintln(os.Stderr, "fixture hash mismatch")
		os.Exit(2)
	}
	manifestHash, err := fileSHA256(*manifest)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 240*time.Second)
	defer cancel()
	version, err := g.command(ctx, "version")
	if err != nil || !bytes.Contains(version, []byte(installedCommit)) {
		fmt.Fprintln(os.Stderr, "installed Product version/commit mismatch", err)
		os.Exit(2)
	}
	shownManifest, err := g.command(ctx, "manifest", "show", fixtureName)
	if err != nil {
		fmt.Fprintln(os.Stderr, "guest fixture manifest missing", err)
		os.Exit(2)
	}
	shownHash := sha256.Sum256(shownManifest)
	if strings.ToUpper(hex.EncodeToString(shownHash[:])) != manifestHash {
		fmt.Fprintln(os.Stderr, "guest fixture manifest mismatch")
		os.Exit(2)
	}
	hashes, _ := json.Marshal(map[string]string{"product_sha256": productHash, "fixture_sha256": fixtureHash, "manifest_sha256": manifestHash})
	if err := g.write("input-hashes.json", hashes); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
	err = g.run(ctx)
	completion, _ := json.Marshal(map[string]any{"ok": err == nil})
	if writeErr := g.write("completion.json", completion); writeErr != nil {
		err = errors.Join(err, writeErr)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

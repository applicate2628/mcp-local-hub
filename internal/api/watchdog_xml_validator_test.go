package api

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/user"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// ---------------------------------------------------------------------------
// Test infrastructure
// ---------------------------------------------------------------------------

// installTestSchtasksFn patches the package-level schtasks-XML query seam.
// Tests use this to inject deterministic XML payloads (or simulated errors,
// timeouts, oversize buffers) without touching the host's real Task
// Scheduler.
func installTestSchtasksFn(t *testing.T, fn func(ctx context.Context, taskName string) ([]byte, error)) {
	t.Helper()
	orig := schtasksQueryXMLFn
	schtasksQueryXMLFn = fn
	t.Cleanup(func() { schtasksQueryXMLFn = orig })
}

// installTestCanonicalMcphubPath patches the canonical-mcphub-path seam used
// by the validator's Command field assertion.
func installTestCanonicalMcphubPath(t *testing.T, path string) {
	t.Helper()
	orig := canonicalMcphubPathFn
	canonicalMcphubPathFn = func() (string, error) { return path, nil }
	t.Cleanup(func() { canonicalMcphubPathFn = orig })
}

// installTestCurrentWindowsUser patches the current-user seam used by the
// principal field assertion.
func installTestCurrentWindowsUser(t *testing.T, name string) {
	t.Helper()
	orig := currentWindowsUserFn
	currentWindowsUserFn = func() (string, error) { return name, nil }
	t.Cleanup(func() { currentWindowsUserFn = orig })
}

// makeXML produces a minimal Task Scheduler XML payload with the given
// (Command, Arguments, UserId, RunLevel, LogonType) fields. Test helpers
// flip individual fields to drive the negation matrix.
//
// The test fixture omits the XML declaration so encoding negotiation is
// not needed — the validator's decoder runs with CharsetReader=nil
// (entity bombs defended-against), which rejects unknown charset
// declarations. Real schtasks output is UTF-16 LE; production callers
// are expected to either pre-decode or feed UTF-8 bytes that the
// decoder accepts via the default UTF-8 path.
func makeXML(cmd, args, userID, runLevel, logonType string) string {
	return fmt.Sprintf(`<Task version="1.4" xmlns="http://schemas.microsoft.com/windows/2004/02/mit/task">
  <RegistrationInfo><Description>test</Description></RegistrationInfo>
  <Principals>
    <Principal id="Author">
      <UserId>%s</UserId>
      <LogonType>%s</LogonType>
      <RunLevel>%s</RunLevel>
    </Principal>
  </Principals>
  <Settings><Enabled>true</Enabled></Settings>
  <Actions Context="Author">
    <Exec>
      <Command>%s</Command>
      <Arguments>%s</Arguments>
    </Exec>
  </Actions>
</Task>`, userID, logonType, runLevel, cmd, args)
}

// xmlFixture is shared across positive-path tests: a known-good XML pointing
// at /test/mcphub.exe with the canonical daemon-running args, our injected
// user, the canonical RunLevel/LogonType.
type xmlFixture struct {
	command   string
	user      string
	runLevel  string
	logonType string
	taskName  string
	args      string
}

func defaultFixture(t *testing.T) xmlFixture {
	t.Helper()
	exe := filepath.Join(t.TempDir(), "mcphub.exe")
	if err := os.WriteFile(exe, []byte("stub"), 0o600); err != nil {
		t.Fatalf("write stub mcphub: %v", err)
	}
	return xmlFixture{
		command:   exe,
		user:      "TESTUSER",
		runLevel:  "LeastPrivilege",
		logonType: "InteractiveToken",
		taskName:  "\\mcp-local-hub-time-default",
		args:      "daemon --server time --daemon default --port 9100",
	}
}

func (f xmlFixture) install(t *testing.T) {
	t.Helper()
	installTestCanonicalMcphubPath(t, f.command)
	installTestCurrentWindowsUser(t, f.user)
	installTestSchtasksFn(t, func(ctx context.Context, taskName string) ([]byte, error) {
		if taskName != f.taskName {
			t.Errorf("schtasksQueryXMLFn: taskName=%q, want %q", taskName, f.taskName)
		}
		return []byte(makeXML(f.command, f.args, f.user, f.runLevel, f.logonType)), nil
	})
}

// snapshotWith builds an OwnershipSnapshot containing the named global
// (server, daemon) pair only.
func snapshotWith(server, daemon string) OwnershipSnapshot {
	return OwnershipSnapshot{
		ManifestServers: map[string]bool{server: true},
		ManifestDaemons: map[string]map[string]bool{
			server: {daemon: true},
		},
		WorkspaceTasksByKey: map[string]string{},
		PortMap:             map[string]int{},
		SnapshottedAt:       time.Now().UTC(),
	}
}

// snapshotWithLazyProxy builds a snapshot containing one workspace registry
// entry mapping the given (wskey, lang) pair to taskName.
func snapshotWithLazyProxy(wskey, lang, taskName string) OwnershipSnapshot {
	return OwnershipSnapshot{
		ManifestServers:     map[string]bool{},
		ManifestDaemons:     map[string]map[string]bool{},
		WorkspaceTasksByKey: map[string]string{wskey + "-" + lang: taskName},
		PortMap:             map[string]int{},
		SnapshottedAt:       time.Now().UTC(),
	}
}

// ---------------------------------------------------------------------------
// classifyOwnedTaskName
// ---------------------------------------------------------------------------

func TestClassifyOwnedTaskName_NotOwned(t *testing.T) {
	cases := []string{
		"\\some-other-task",
		"random-task",
		"",
		"\\mcp-local-hub-",
		"mcp-local-hub-time-default", // missing leading backslash
	}
	for _, name := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := classifyOwnedTaskName(name)
			if !errors.Is(err, ErrNotOwnedTask) {
				t.Errorf("classifyOwnedTaskName(%q): err=%v, want ErrNotOwnedTask", name, err)
			}
		})
	}
}

func TestClassifyOwnedTaskName_Global(t *testing.T) {
	o, err := classifyOwnedTaskName("\\mcp-local-hub-time-default")
	if err != nil {
		t.Fatalf("classifyOwnedTaskName: %v", err)
	}
	if o.Kind != ownershipGlobal {
		t.Errorf("Kind=%q, want %q", o.Kind, ownershipGlobal)
	}
	if o.Server != "time" || o.Daemon != "default" {
		t.Errorf("Server/Daemon=%q/%q, want time/default", o.Server, o.Daemon)
	}
}

func TestClassifyOwnedTaskName_LazyProxy(t *testing.T) {
	o, err := classifyOwnedTaskName("\\mcp-local-hub-lsp-abcd1234-go")
	if err != nil {
		t.Fatalf("classifyOwnedTaskName: %v", err)
	}
	if o.Kind != ownershipLazyProxy {
		t.Errorf("Kind=%q, want %q", o.Kind, ownershipLazyProxy)
	}
	if o.WorkspaceKey != "abcd1234" {
		t.Errorf("WorkspaceKey=%q, want abcd1234", o.WorkspaceKey)
	}
	if o.Language != "go" {
		t.Errorf("Language=%q, want go", o.Language)
	}
}

func TestClassifyOwnedTaskName_MaintenanceWatchdog(t *testing.T) {
	o, err := classifyOwnedTaskName("\\mcp-local-hub-watchdog")
	if err != nil {
		t.Fatalf("classifyOwnedTaskName: %v", err)
	}
	if o.Kind != ownershipMaintenance {
		t.Errorf("Kind=%q, want %q", o.Kind, ownershipMaintenance)
	}
	if o.MaintenanceKind != "watchdog" {
		t.Errorf("MaintenanceKind=%q, want watchdog", o.MaintenanceKind)
	}
}

func TestClassifyOwnedTaskName_MaintenanceWeeklyRefresh(t *testing.T) {
	o, err := classifyOwnedTaskName("\\mcp-local-hub-workspace-weekly-refresh")
	if err != nil {
		t.Fatalf("classifyOwnedTaskName: %v", err)
	}
	if o.Kind != ownershipMaintenance {
		t.Errorf("Kind=%q, want %q", o.Kind, ownershipMaintenance)
	}
}

// ---------------------------------------------------------------------------
// ValidateOwnedTaskXML — name / not-owned
// ---------------------------------------------------------------------------

func TestValidate_NotOwnedTaskName(t *testing.T) {
	v := newPlainValidatorForTest(t)
	err := v.validate("\\some-foreign-task")
	if !errors.Is(err, ErrNotOwnedTask) {
		t.Fatalf("err=%v, want ErrNotOwnedTask", err)
	}
}

// ---------------------------------------------------------------------------
// ValidateOwnedTaskXML — XML hardening
// ---------------------------------------------------------------------------

func TestValidate_DOCTYPERejected(t *testing.T) {
	f := defaultFixture(t)
	installTestCanonicalMcphubPath(t, f.command)
	installTestCurrentWindowsUser(t, f.user)
	xmlBytes := []byte(`<?xml version="1.0"?><!DOCTYPE foo [<!ENTITY x "y">]>` + makeXML(f.command, f.args, f.user, f.runLevel, f.logonType))
	installTestSchtasksFn(t, func(ctx context.Context, taskName string) ([]byte, error) {
		return xmlBytes, nil
	})
	v := newPlainValidatorForTest(t)
	err := v.validate(f.taskName)
	if !errors.Is(err, ErrXMLDoctypeRejected) {
		t.Fatalf("err=%v, want ErrXMLDoctypeRejected", err)
	}
}

func TestValidate_BillionLaughsRejectedAsDoctype(t *testing.T) {
	f := defaultFixture(t)
	installTestCanonicalMcphubPath(t, f.command)
	installTestCurrentWindowsUser(t, f.user)
	bombXML := `<?xml version="1.0"?>
<!DOCTYPE lolz [
 <!ENTITY lol "lol">
 <!ENTITY lol2 "&lol;&lol;&lol;&lol;&lol;&lol;&lol;&lol;&lol;&lol;">
 <!ENTITY lol3 "&lol2;&lol2;&lol2;&lol2;&lol2;&lol2;&lol2;&lol2;&lol2;&lol2;">
]>
<Task version="1.4"><Actions><Exec><Command>x</Command></Exec></Actions></Task>`
	installTestSchtasksFn(t, func(ctx context.Context, taskName string) ([]byte, error) {
		return []byte(bombXML), nil
	})
	v := newPlainValidatorForTest(t)
	err := v.validate(f.taskName)
	// Either DOCTYPE rejection (preferred — caught at the byte-level scan
	// before any parsing) or malformed (defense-in-depth at the parser).
	if !errors.Is(err, ErrXMLDoctypeRejected) && !errors.Is(err, ErrXMLMalformed) {
		t.Fatalf("err=%v, want ErrXMLDoctypeRejected or ErrXMLMalformed", err)
	}
}

func TestValidate_OversizeXML(t *testing.T) {
	f := defaultFixture(t)
	installTestCanonicalMcphubPath(t, f.command)
	installTestCurrentWindowsUser(t, f.user)
	// 65 KiB payload — limit is 64 KiB (xmlSizeLimit). The validator reads
	// xmlSizeLimit+1 bytes; a length of 65*1024 trips the limit.
	big := strings.Repeat("a", 65*1024)
	installTestSchtasksFn(t, func(ctx context.Context, taskName string) ([]byte, error) {
		return []byte(big), nil
	})
	v := newPlainValidatorForTest(t)
	err := v.validate(f.taskName)
	if !errors.Is(err, ErrXMLOversize) {
		t.Fatalf("err=%v, want ErrXMLOversize", err)
	}
}

func TestValidate_DeepNested(t *testing.T) {
	f := defaultFixture(t)
	installTestCanonicalMcphubPath(t, f.command)
	installTestCurrentWindowsUser(t, f.user)
	// Build nesting depth > 32. Each <a> increases depth by 1.
	var b strings.Builder
	b.WriteString(`<?xml version="1.0"?>`)
	depth := 40
	for i := 0; i < depth; i++ {
		b.WriteString("<a>")
	}
	b.WriteString("x")
	for i := 0; i < depth; i++ {
		b.WriteString("</a>")
	}
	installTestSchtasksFn(t, func(ctx context.Context, taskName string) ([]byte, error) {
		return []byte(b.String()), nil
	})
	v := newPlainValidatorForTest(t)
	err := v.validate(f.taskName)
	if !errors.Is(err, ErrXMLTooDeep) && !errors.Is(err, ErrXMLMalformed) {
		t.Fatalf("err=%v, want ErrXMLTooDeep or ErrXMLMalformed", err)
	}
}

func TestValidate_MalformedXML(t *testing.T) {
	f := defaultFixture(t)
	installTestCanonicalMcphubPath(t, f.command)
	installTestCurrentWindowsUser(t, f.user)
	installTestSchtasksFn(t, func(ctx context.Context, taskName string) ([]byte, error) {
		return []byte(`<?xml version="1.0"?><Task><Actions><Exec><Command>foo</Command><<<garbage tail`), nil
	})
	v := newPlainValidatorForTest(t)
	err := v.validate(f.taskName)
	if !errors.Is(err, ErrXMLMalformed) {
		t.Fatalf("err=%v, want ErrXMLMalformed", err)
	}
}

func TestValidate_SchtasksTimeout(t *testing.T) {
	f := defaultFixture(t)
	installTestCanonicalMcphubPath(t, f.command)
	installTestCurrentWindowsUser(t, f.user)
	installTestSchtasksFn(t, func(ctx context.Context, taskName string) ([]byte, error) {
		<-ctx.Done()
		return nil, ctx.Err()
	})
	// Drop the 2s production deadline to a few ms so the test stays fast
	// without changing the production constant.
	orig := schtasksTimeoutForTest
	schtasksTimeoutForTest = 50 * time.Millisecond
	t.Cleanup(func() { schtasksTimeoutForTest = orig })

	v := newPlainValidatorForTest(t)
	err := v.validate(f.taskName)
	if !errors.Is(err, ErrSchtasksTimeout) {
		t.Fatalf("err=%v, want ErrSchtasksTimeout", err)
	}
}

// ---------------------------------------------------------------------------
// ValidateOwnedTaskXML — field assertions
// ---------------------------------------------------------------------------

func TestValidate_CommandMismatch(t *testing.T) {
	f := defaultFixture(t)
	f.install(t)
	// Override the canonical path to point at a different file the XML
	// won't reference.
	other := filepath.Join(t.TempDir(), "other.exe")
	if err := os.WriteFile(other, []byte("stub-other"), 0o600); err != nil {
		t.Fatalf("write other: %v", err)
	}
	installTestCanonicalMcphubPath(t, other)
	v := newPlainValidatorForTest(t)
	err := v.validate(f.taskName)
	if !errors.Is(err, ErrCommandMismatch) {
		t.Fatalf("err=%v, want ErrCommandMismatch", err)
	}
}

func TestValidate_PrincipalMismatch(t *testing.T) {
	f := defaultFixture(t)
	f.install(t)
	installTestCurrentWindowsUser(t, "OTHERUSER")
	v := newPlainValidatorForTest(t)
	err := v.validate(f.taskName)
	if !errors.Is(err, ErrPrincipalMismatch) {
		t.Fatalf("err=%v, want ErrPrincipalMismatch", err)
	}
}

func TestValidate_UnexpectedRunLevel(t *testing.T) {
	f := defaultFixture(t)
	installTestCanonicalMcphubPath(t, f.command)
	installTestCurrentWindowsUser(t, f.user)
	installTestSchtasksFn(t, func(ctx context.Context, taskName string) ([]byte, error) {
		return []byte(makeXML(f.command, f.args, f.user, "HighestAvailable", f.logonType)), nil
	})
	v := newPlainValidatorForTest(t)
	err := v.validate(f.taskName)
	if !errors.Is(err, ErrUnexpectedRunLevel) {
		t.Fatalf("err=%v, want ErrUnexpectedRunLevel", err)
	}
}

func TestValidate_UnexpectedLogonType(t *testing.T) {
	f := defaultFixture(t)
	installTestCanonicalMcphubPath(t, f.command)
	installTestCurrentWindowsUser(t, f.user)
	installTestSchtasksFn(t, func(ctx context.Context, taskName string) ([]byte, error) {
		return []byte(makeXML(f.command, f.args, f.user, f.runLevel, "S4U")), nil
	})
	v := newPlainValidatorForTest(t)
	err := v.validate(f.taskName)
	if !errors.Is(err, ErrUnexpectedLogonType) {
		t.Fatalf("err=%v, want ErrUnexpectedLogonType", err)
	}
}

// ---------------------------------------------------------------------------
// ValidateOwnedTaskXML — args structural assertions
// ---------------------------------------------------------------------------

func TestValidate_GlobalArgsMissingDaemonPrefix(t *testing.T) {
	f := defaultFixture(t)
	installTestCanonicalMcphubPath(t, f.command)
	installTestCurrentWindowsUser(t, f.user)
	// "watchdog" instead of "daemon" — wrong shape.
	installTestSchtasksFn(t, func(ctx context.Context, taskName string) ([]byte, error) {
		return []byte(makeXML(f.command,
			"watchdog --server time --daemon default --port 9100",
			f.user, f.runLevel, f.logonType)), nil
	})
	v := newPlainValidatorForTest(t)
	err := v.validate(f.taskName)
	if !errors.Is(err, ErrArgsMismatch) {
		t.Fatalf("err=%v, want ErrArgsMismatch", err)
	}
}

func TestValidate_GlobalArgsServerMismatch(t *testing.T) {
	f := defaultFixture(t)
	installTestCanonicalMcphubPath(t, f.command)
	installTestCurrentWindowsUser(t, f.user)
	// Args claim --server other --daemon default but task name says
	// time/default → ErrArgsMismatch.
	installTestSchtasksFn(t, func(ctx context.Context, taskName string) ([]byte, error) {
		return []byte(makeXML(f.command,
			"daemon --server other --daemon default --port 9100",
			f.user, f.runLevel, f.logonType)), nil
	})
	v := newPlainValidatorForTest(t)
	err := v.validate(f.taskName)
	if !errors.Is(err, ErrArgsMismatch) {
		t.Fatalf("err=%v, want ErrArgsMismatch", err)
	}
}

func TestValidate_LazyProxyArgsMissing(t *testing.T) {
	f := defaultFixture(t)
	taskName := "\\mcp-local-hub-lsp-abc-go"
	installTestCanonicalMcphubPath(t, f.command)
	installTestCurrentWindowsUser(t, f.user)
	installTestSchtasksFn(t, func(ctx context.Context, tn string) ([]byte, error) {
		// Missing 'daemon workspace-proxy' shape.
		return []byte(makeXML(f.command, "daemon some-other-subcommand", f.user, f.runLevel, f.logonType)), nil
	})
	snap := snapshotWithLazyProxy("abc", "go", taskName)
	v := NewOwnedXMLValidatorFromSnapshot(snap).(*ownedXMLValidator)
	err := v.IsOwnedAndValidErr(taskName)
	if !errors.Is(err, ErrArgsMismatch) {
		t.Fatalf("err=%v, want ErrArgsMismatch", err)
	}
}

func TestValidate_MaintenanceWatchdogArgs(t *testing.T) {
	f := defaultFixture(t)
	installTestCanonicalMcphubPath(t, f.command)
	installTestCurrentWindowsUser(t, f.user)
	installTestSchtasksFn(t, func(ctx context.Context, taskName string) ([]byte, error) {
		// Wrong: should be "watchdog --once" not "daemon ..."
		return []byte(makeXML(f.command, "daemon --server x --daemon y", f.user, f.runLevel, f.logonType)), nil
	})
	v := newPlainValidatorForTest(t)
	err := v.validate("\\mcp-local-hub-watchdog")
	if !errors.Is(err, ErrArgsMismatch) {
		t.Fatalf("err=%v, want ErrArgsMismatch", err)
	}
}

// ---------------------------------------------------------------------------
// ValidateOwnedTaskXML — structural ownership (snapshot path)
// ---------------------------------------------------------------------------

// TestValidate_GlobalUnstructured_AdversaryFakeServer covers the Code Review
// #6 / Plan §5 v7-8 attack scenario: an adversary registers
// \mcp-local-hub-fake-default as a real Task Scheduler entry pointing at
// attacker.exe. The classifier passes (matches global pattern) but the
// snapshot's ManifestServers / ManifestDaemons does NOT contain
// (fake, default) → ErrUnstructuredOwnership.
func TestValidate_GlobalUnstructured_AdversaryFakeServer(t *testing.T) {
	f := defaultFixture(t)
	installTestCanonicalMcphubPath(t, f.command)
	installTestCurrentWindowsUser(t, f.user)
	installTestSchtasksFn(t, func(ctx context.Context, taskName string) ([]byte, error) {
		return []byte(makeXML(f.command, "daemon --server fake --daemon default", f.user, f.runLevel, f.logonType)), nil
	})
	// Snapshot contains ONLY (time, default). (fake, default) is
	// foreign — must be rejected.
	snap := snapshotWith("time", "default")
	v := NewOwnedXMLValidatorFromSnapshot(snap).(*ownedXMLValidator)
	err := v.IsOwnedAndValidErr("\\mcp-local-hub-fake-default")
	if !errors.Is(err, ErrUnstructuredOwnership) {
		t.Fatalf("err=%v, want ErrUnstructuredOwnership", err)
	}
}

func TestValidate_GlobalUnstructured_DaemonNotInManifest(t *testing.T) {
	f := defaultFixture(t)
	installTestCanonicalMcphubPath(t, f.command)
	installTestCurrentWindowsUser(t, f.user)
	installTestSchtasksFn(t, func(ctx context.Context, taskName string) ([]byte, error) {
		return []byte(makeXML(f.command, "daemon --server time --daemon foo", f.user, f.runLevel, f.logonType)), nil
	})
	// Snapshot has server=time but only daemon=default; daemon=foo is
	// NOT present → ErrUnstructuredOwnership.
	snap := snapshotWith("time", "default")
	v := NewOwnedXMLValidatorFromSnapshot(snap).(*ownedXMLValidator)
	err := v.IsOwnedAndValidErr("\\mcp-local-hub-time-foo")
	if !errors.Is(err, ErrUnstructuredOwnership) {
		t.Fatalf("err=%v, want ErrUnstructuredOwnership", err)
	}
}

func TestValidate_LazyProxyUnstructured_KeyNotInRegistry(t *testing.T) {
	f := defaultFixture(t)
	installTestCanonicalMcphubPath(t, f.command)
	installTestCurrentWindowsUser(t, f.user)
	taskName := "\\mcp-local-hub-lsp-zzz-go"
	installTestSchtasksFn(t, func(ctx context.Context, tn string) ([]byte, error) {
		return []byte(makeXML(f.command, "daemon workspace-proxy --workspace foo", f.user, f.runLevel, f.logonType)), nil
	})
	// Snapshot has ONLY (abc-go) registered; (zzz-go) is foreign.
	snap := snapshotWithLazyProxy("abc", "go", "\\mcp-local-hub-lsp-abc-go")
	v := NewOwnedXMLValidatorFromSnapshot(snap).(*ownedXMLValidator)
	err := v.IsOwnedAndValidErr(taskName)
	if !errors.Is(err, ErrUnstructuredOwnership) {
		t.Fatalf("err=%v, want ErrUnstructuredOwnership", err)
	}
}

func TestValidate_LazyProxyUnstructured_TaskNameByteMismatch(t *testing.T) {
	f := defaultFixture(t)
	installTestCanonicalMcphubPath(t, f.command)
	installTestCurrentWindowsUser(t, f.user)
	requestedTask := "\\mcp-local-hub-lsp-abc-go"
	installTestSchtasksFn(t, func(ctx context.Context, tn string) ([]byte, error) {
		return []byte(makeXML(f.command, "daemon workspace-proxy --workspace foo", f.user, f.runLevel, f.logonType)), nil
	})
	// Registry's TaskName for (abc, go) does NOT match the requested
	// task name byte-for-byte.
	snap := snapshotWithLazyProxy("abc", "go", "\\mcp-local-hub-lsp-abc-different")
	v := NewOwnedXMLValidatorFromSnapshot(snap).(*ownedXMLValidator)
	err := v.IsOwnedAndValidErr(requestedTask)
	if !errors.Is(err, ErrUnstructuredOwnership) {
		t.Fatalf("err=%v, want ErrUnstructuredOwnership", err)
	}
}

// ---------------------------------------------------------------------------
// ValidateOwnedTaskXML — happy path
// ---------------------------------------------------------------------------

func TestValidate_HappyPath_Global(t *testing.T) {
	f := defaultFixture(t)
	f.install(t)
	snap := snapshotWith("time", "default")
	v := NewOwnedXMLValidatorFromSnapshot(snap)
	concrete := v.(*ownedXMLValidator)
	if err := concrete.IsOwnedAndValidErr(f.taskName); err != nil {
		t.Fatalf("validate: %v", err)
	}
	if !v.IsOwnedAndValid(f.taskName) {
		t.Errorf("IsOwnedAndValid: false, want true on happy path")
	}
}

func TestValidate_HappyPath_LazyProxy(t *testing.T) {
	f := defaultFixture(t)
	taskName := "\\mcp-local-hub-lsp-abc-go"
	installTestCanonicalMcphubPath(t, f.command)
	installTestCurrentWindowsUser(t, f.user)
	installTestSchtasksFn(t, func(ctx context.Context, tn string) ([]byte, error) {
		return []byte(makeXML(f.command,
			"daemon workspace-proxy --workspace abc --language go --port 9555",
			f.user, f.runLevel, f.logonType)), nil
	})
	snap := snapshotWithLazyProxy("abc", "go", taskName)
	v := NewOwnedXMLValidatorFromSnapshot(snap).(*ownedXMLValidator)
	if err := v.IsOwnedAndValidErr(taskName); err != nil {
		t.Fatalf("validate: %v", err)
	}
}

func TestValidate_HappyPath_MaintenanceWatchdog(t *testing.T) {
	f := defaultFixture(t)
	installTestCanonicalMcphubPath(t, f.command)
	installTestCurrentWindowsUser(t, f.user)
	installTestSchtasksFn(t, func(ctx context.Context, tn string) ([]byte, error) {
		return []byte(makeXML(f.command, "watchdog --once", f.user, f.runLevel, f.logonType)), nil
	})
	v := newPlainValidatorForTest(t)
	if err := v.validate("\\mcp-local-hub-watchdog"); err != nil {
		t.Fatalf("validate watchdog: %v", err)
	}
}

func TestValidate_HappyPath_MaintenanceWeeklyRefresh(t *testing.T) {
	f := defaultFixture(t)
	installTestCanonicalMcphubPath(t, f.command)
	installTestCurrentWindowsUser(t, f.user)
	installTestSchtasksFn(t, func(ctx context.Context, tn string) ([]byte, error) {
		return []byte(makeXML(f.command,
			"daemon workspace-weekly-refresh",
			f.user, f.runLevel, f.logonType)), nil
	})
	v := newPlainValidatorForTest(t)
	if err := v.validate("\\mcp-local-hub-workspace-weekly-refresh"); err != nil {
		t.Fatalf("validate weekly-refresh: %v", err)
	}
}

// ---------------------------------------------------------------------------
// Snapshot-bound validator behaviour (tick-stable)
// ---------------------------------------------------------------------------

// TestNewOwnedXMLValidatorFromSnapshot_TickStable verifies that the
// snapshot-bound validator does NOT consult any state outside the
// snapshot. Two validators built at T0 and T+1 from different snapshots
// see different ownership universes; the same validator answering twice
// remains consistent with its captured snapshot, even if a fresh
// LoadOwnershipSnapshot would now return different data.
func TestNewOwnedXMLValidatorFromSnapshot_TickStable(t *testing.T) {
	f := defaultFixture(t)
	installTestCanonicalMcphubPath(t, f.command)
	installTestCurrentWindowsUser(t, f.user)
	installTestSchtasksFn(t, func(ctx context.Context, tn string) ([]byte, error) {
		return []byte(makeXML(f.command, "daemon --server time --daemon default", f.user, f.runLevel, f.logonType)), nil
	})
	// T0 snapshot has (time, default). Validator built from it accepts
	// the time-default task.
	snap0 := snapshotWith("time", "default")
	v0 := NewOwnedXMLValidatorFromSnapshot(snap0).(*ownedXMLValidator)
	if err := v0.IsOwnedAndValidErr("\\mcp-local-hub-time-default"); err != nil {
		t.Fatalf("v0 should accept time/default: %v", err)
	}
	// Mutate the snap0 map AFTER constructing the validator. The
	// validator must NOT see the mutation. (Note: NewOwnedXMLValidatorFromSnapshot
	// captures by reference per its docstring, so callers are required not to
	// mutate after passing — LoadOwnershipSnapshot returns a defensive copy.
	// We test the per-call stability of the captured-snapshot view here.)
	v0Snap := v0.snap
	if !v0Snap.ManifestDaemons["time"]["default"] {
		t.Fatalf("setup: snap0 should still have (time, default) before mutation")
	}
	// A new validator built from a NEW snapshot lacking (time, default)
	// rejects, proving structural checks consult the validator-bound
	// snapshot only.
	snap1 := snapshotWith("time", "different")
	v1 := NewOwnedXMLValidatorFromSnapshot(snap1).(*ownedXMLValidator)
	if err := v1.IsOwnedAndValidErr("\\mcp-local-hub-time-default"); !errors.Is(err, ErrUnstructuredOwnership) {
		t.Errorf("v1 (no time/default) should reject: %v", err)
	}
	// And v0 must continue to accept (proves cross-validator isolation).
	if err := v0.IsOwnedAndValidErr("\\mcp-local-hub-time-default"); err != nil {
		t.Errorf("v0 must remain stable while v1 sees a different snap: %v", err)
	}
}

// ---------------------------------------------------------------------------
// Plain (non-snapshot) constructor smoke
// ---------------------------------------------------------------------------

func TestNewOwnedXMLValidator_PlainSmoke(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("plain validator reads manifest fresh; non-Windows test environments are noisy")
	}
	v := NewOwnedXMLValidator()
	if v == nil {
		t.Fatal("NewOwnedXMLValidator: nil")
	}
	// No assertion on result — manifest contents vary; we just smoke
	// the constructor + interface satisfaction.
	_ = v.IsOwnedAndValid("\\mcp-local-hub-nonexistent")
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

// newPlainValidatorForTest returns a plain (non-snapshot) validator with
// manifest reads bypassed by the snapshot fallback. Used by tests that
// only exercise XML hardening / field assertions / ownership classification
// where structural lookups are not the focus.
func newPlainValidatorForTest(t *testing.T) *ownedXMLValidator {
	t.Helper()
	// Provide a permissive snapshot so structural checks pass; the test
	// is targeting other layers.
	snap := OwnershipSnapshot{
		ManifestServers: map[string]bool{"time": true},
		ManifestDaemons: map[string]map[string]bool{
			"time": {"default": true},
		},
		WorkspaceTasksByKey: map[string]string{},
		PortMap:             map[string]int{},
		SnapshottedAt:       time.Now().UTC(),
	}
	return &ownedXMLValidator{snap: snap}
}

// TestRealCurrentUser is a smoke test of the production currentWindowsUserFn
// default — exercises the real user lookup so a packaging regression
// (broken user.Current) is loud.
func TestRealCurrentUser(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("user-name semantics differ on non-Windows; the validator is Windows-only")
	}
	got, err := defaultCurrentWindowsUser()
	if err != nil {
		t.Fatalf("defaultCurrentWindowsUser: %v", err)
	}
	if got == "" {
		t.Fatal("got empty user name")
	}
	// Compare to user.Current as a smoke check; allow domain-trimming.
	cu, err := user.Current()
	if err != nil {
		t.Skipf("user.Current unavailable: %v", err)
	}
	want := cu.Username
	if i := strings.LastIndex(want, "\\"); i >= 0 {
		want = want[i+1:]
	}
	if !strings.EqualFold(got, want) {
		t.Errorf("default user = %q, want (case-insensitive) %q", got, want)
	}
}

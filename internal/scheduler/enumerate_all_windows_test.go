//go:build windows

package scheduler

import (
	"errors"
	"strings"
	"testing"
)

// schtasks /Query /XML ONE /FO LIST emits one XML document per task,
// each preceded by its own <?xml version="1.0" encoding="UTF-16"?>
// processing instruction and a fully-typed <Task> root element.
// The documents are concatenated back-to-back into a single stream
// (the "ONE" in `/XML ONE` means "single file" — not "single wrapped
// root"). The parser must split on the leading PI marker and decode
// each <Task> independently, otherwise encoding/xml chokes on
// multiple top-level <?xml ...?> declarations.
//
// Each test below feeds a synthetic stream through parseEnumerateXML
// — the pure XML-string-to-[]TaskStatus function the production
// EnumerateAllMcphubTasks() shell calls after invoking schtasks. The
// pure-parser indirection is the safety boundary that lets the
// scheduler unit tests run without ever touching the host's real
// Task Scheduler state. See AGENTS.md "Verification and decision
// discipline" + repo CLAUDE.md kosyak
// feedback_kosyak_full_test_sweep_affects_real_scheduler.md.

// fixtureTask renders a synthetic single-task XML document mirroring
// the shape `schtasks /Query /XML ONE` emits. Each test composes one
// or more of these into a `\n` (newline)-separated stream.
func fixtureTask(uri, userID string) string {
	return `<?xml version="1.0" encoding="UTF-16"?>
<Task version="1.4" xmlns="http://schemas.microsoft.com/windows/2004/02/mit/task">
  <RegistrationInfo>
    <URI>` + uri + `</URI>
    <Author>` + userID + `</Author>
  </RegistrationInfo>
  <Triggers>
    <LogonTrigger>
      <Enabled>true</Enabled>
    </LogonTrigger>
  </Triggers>
  <Principals>
    <Principal id="Author">
      <UserId>` + userID + `</UserId>
      <LogonType>InteractiveToken</LogonType>
      <RunLevel>LeastPrivilege</RunLevel>
    </Principal>
  </Principals>
  <Settings>
    <Enabled>true</Enabled>
  </Settings>
  <Actions Context="Author">
    <Exec>
      <Command>C:\path\mcphub.exe</Command>
      <Arguments>daemon --server memory --daemon claude</Arguments>
    </Exec>
  </Actions>
</Task>
`
}

// TestEnumerateAllMcphubTasks_ParsesMixedOwners is the load-bearing
// migration case: a v0.4.x host with one task installed under the
// current user and another installed under a different account (a
// historical RunAs setup the operator hasn't cleaned up). Both must
// appear in the result. List() filters the second one out via
// sameWindowsUser; EnumerateAllMcphubTasks() must NOT.
func TestEnumerateAllMcphubTasks_ParsesMixedOwners(t *testing.T) {
	stream := fixtureTask(`\mcp-local-hub-memory-claude`, `dima_`) +
		fixtureTask(`\mcp-local-hub-foo-claude`, `SomeOtherUser`)

	tasks, err := parseEnumerateXML(stream)
	if err != nil {
		t.Fatalf("parseEnumerateXML returned error: %v", err)
	}
	if len(tasks) != 2 {
		t.Fatalf("expected 2 tasks, got %d: %+v", len(tasks), tasks)
	}

	byName := make(map[string]TaskStatus, len(tasks))
	for _, ts := range tasks {
		byName[ts.Name] = ts
	}

	memory, ok := byName[`\mcp-local-hub-memory-claude`]
	if !ok {
		t.Fatalf("expected \\mcp-local-hub-memory-claude in result; got %+v", tasks)
	}
	if !strings.EqualFold(memory.Owner, `dima_`) {
		t.Errorf("expected Owner=dima_ for memory-claude, got %q", memory.Owner)
	}

	foo, ok := byName[`\mcp-local-hub-foo-claude`]
	if !ok {
		t.Fatalf("expected \\mcp-local-hub-foo-claude in result; got %+v", tasks)
	}
	if !strings.EqualFold(foo.Owner, `SomeOtherUser`) {
		t.Errorf("expected Owner=SomeOtherUser for foo-claude, got %q", foo.Owner)
	}
}

// TestEnumerateAllMcphubTasks_FiltersByMcphubPrefix asserts that
// only tasks whose URI begins with `\mcp-local-hub-` are returned.
// The host's task store typically contains hundreds of unrelated
// Windows / vendor tasks; the migration classifier only cares
// about ours.
func TestEnumerateAllMcphubTasks_FiltersByMcphubPrefix(t *testing.T) {
	stream := fixtureTask(`\mcp-local-hub-memory-claude`, `dima_`) +
		fixtureTask(`\Microsoft\Windows\UpdateOrchestrator\Refresh`, `SYSTEM`) +
		fixtureTask(`\unrelated-task`, `dima_`)

	tasks, err := parseEnumerateXML(stream)
	if err != nil {
		t.Fatalf("parseEnumerateXML returned error: %v", err)
	}
	if len(tasks) != 1 {
		t.Fatalf("expected 1 task (only the mcp-local-hub one), got %d: %+v", len(tasks), tasks)
	}
	if tasks[0].Name != `\mcp-local-hub-memory-claude` {
		t.Errorf("expected only \\mcp-local-hub-memory-claude, got %q", tasks[0].Name)
	}
}

// TestEnumerateAllMcphubTasks_EmptyTaskStore covers the fresh-install
// path: no `\mcp-local-hub-*` tasks installed yet. parseEnumerateXML
// must return an empty (nil-or-zero-length) slice and a nil error so
// the migration driver can no-op cleanly.
func TestEnumerateAllMcphubTasks_EmptyTaskStore(t *testing.T) {
	// schtasks /Query /XML ONE with no tasks emits an empty body, but
	// callers also accept a body containing only non-mcp-local-hub
	// tasks (the migration driver doesn't care which is which).
	tests := []struct {
		name   string
		stream string
	}{
		{name: "empty body", stream: ""},
		{name: "whitespace only", stream: "\r\n\r\n\t\r\n"},
		{name: "non-mcp tasks only", stream: fixtureTask(`\Microsoft\Windows\Defender\Scan`, `SYSTEM`)},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			tasks, err := parseEnumerateXML(tc.stream)
			if err != nil {
				t.Fatalf("parseEnumerateXML returned error: %v", err)
			}
			if len(tasks) != 0 {
				t.Errorf("expected 0 tasks, got %d: %+v", len(tasks), tasks)
			}
		})
	}
}

// TestEnumerateAllMcphubTasks_MalformedXML asserts the parser returns
// a clear non-panicking error when the input is garbage. Production
// caller (migration driver) treats this as a hard failure that
// aborts the migration journal — better to abort than to operate on
// a partial enumeration.
func TestEnumerateAllMcphubTasks_MalformedXML(t *testing.T) {
	// Truncated mid-element: parser must trip xml.SyntaxError without
	// panicking.
	broken := `<?xml version="1.0" encoding="UTF-16"?>
<Task version="1.4" xmlns="http://schemas.microsoft.com/windows/2004/02/mit/task">
  <RegistrationInfo>
    <URI>\mcp-local-hub-memory-claude</URI`
	_, err := parseEnumerateXML(broken)
	if err == nil {
		t.Fatal("expected error for truncated XML, got nil")
	}
	// Sanity: the error message should mention "xml" or "parse" so an
	// operator sees what failed, not a bare exit code.
	msg := strings.ToLower(err.Error())
	if !strings.Contains(msg, "xml") && !strings.Contains(msg, "parse") && !strings.Contains(msg, "task") {
		t.Errorf("expected error to mention xml/parse/task; got %q", err.Error())
	}
}

// TestEnumerateAllMcphubTasks_ExtractsOwner asserts the Principal
// UserId is carried through to TaskStatus.Owner verbatim, even when
// it's a different user than the running process. This is the core
// migration use case: classify v0.4.x tasks created under a
// different RunAs account.
func TestEnumerateAllMcphubTasks_ExtractsOwner(t *testing.T) {
	stream := fixtureTask(`\mcp-local-hub-time-claude`, `MACHINE\OtherUser`)
	tasks, err := parseEnumerateXML(stream)
	if err != nil {
		t.Fatalf("parseEnumerateXML returned error: %v", err)
	}
	if len(tasks) != 1 {
		t.Fatalf("expected 1 task, got %d", len(tasks))
	}
	if tasks[0].Owner != `MACHINE\OtherUser` {
		t.Errorf("expected Owner=MACHINE\\OtherUser, got %q", tasks[0].Owner)
	}
	if tasks[0].Name != `\mcp-local-hub-time-claude` {
		t.Errorf("expected Name=\\mcp-local-hub-time-claude, got %q", tasks[0].Name)
	}
}

// TestEnumerateAllMcphubTasks_NoPanicOnNilInput is a defensive guard:
// the production path should never feed parseEnumerateXML nil, but a
// future caller (or a flaky schtasks return) might. The parser must
// degrade to "empty result" without panicking.
func TestEnumerateAllMcphubTasks_NoPanicOnNilInput(t *testing.T) {
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("parseEnumerateXML panicked on empty input: %v", r)
		}
	}()
	tasks, err := parseEnumerateXML("")
	if err != nil {
		t.Fatalf("parseEnumerateXML returned error on empty input: %v", err)
	}
	if len(tasks) != 0 {
		t.Errorf("expected 0 tasks on empty input, got %d", len(tasks))
	}
}

// TestEnumerateAllMcphubTasks_HandlesDOMAINPrefix asserts the parser
// preserves the DOMAIN\user form when present in <UserId>. Migration
// caller may need to strip DOMAIN itself; the enumerator is
// transport-only and preserves the raw schtasks string.
func TestEnumerateAllMcphubTasks_HandlesDOMAINPrefix(t *testing.T) {
	stream := fixtureTask(`\mcp-local-hub-wolfram-codex`, `WORKGROUP\dima_`)
	tasks, err := parseEnumerateXML(stream)
	if err != nil {
		t.Fatalf("parseEnumerateXML returned error: %v", err)
	}
	if len(tasks) != 1 {
		t.Fatalf("expected 1 task, got %d", len(tasks))
	}
	if tasks[0].Owner != `WORKGROUP\dima_` {
		t.Errorf("expected raw DOMAIN-prefixed owner preserved, got %q", tasks[0].Owner)
	}
}

// TestSplitConcatenatedTaskXML directly exercises the splitter
// helper. The function takes the concatenated `schtasks /Query /XML
// ONE` stream and yields one slice per individual XML document. It
// MUST tolerate (a) leading/trailing whitespace between docs, (b) a
// possibly-missing trailing newline at end-of-stream, and (c) an
// empty input.
func TestSplitConcatenatedTaskXML(t *testing.T) {
	a := fixtureTask(`\mcp-local-hub-a`, `dima_`)
	b := fixtureTask(`\mcp-local-hub-b`, `dima_`)

	tests := []struct {
		name     string
		input    string
		wantDocs int
	}{
		{name: "empty", input: "", wantDocs: 0},
		{name: "single doc", input: a, wantDocs: 1},
		{name: "two docs back-to-back", input: a + b, wantDocs: 2},
		{name: "two docs with extra whitespace", input: "\r\n" + a + "\r\n\r\n" + b + "\r\n", wantDocs: 2},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := splitConcatenatedTaskXML(tc.input)
			if len(got) != tc.wantDocs {
				t.Fatalf("splitConcatenatedTaskXML(%q): expected %d docs, got %d (%v)", tc.name, tc.wantDocs, len(got), got)
			}
			for i, doc := range got {
				if !strings.Contains(doc, "<Task") {
					t.Errorf("doc[%d] missing <Task tag: %q", i, doc)
				}
			}
		})
	}
}

// TestEnumerateAllMcphubTasks_BypassesSameUserFilter is a regression
// guard: the entire point of this helper vs List() is that the
// sameWindowsUser filter is bypassed. We can't test that the
// production schtasks call returns mixed-owner data (that would
// require touching the live scheduler), but we CAN assert via fixture
// composition that the parser does not silently drop non-current-user
// entries. The "dima_" + "SomeOtherUser" fixture composition in
// TestEnumerateAllMcphubTasks_ParsesMixedOwners already exercises
// this, but this test pins the intent explicitly so a future refactor
// that re-introduces a same-user filter at the parser layer fails
// loudly.
func TestEnumerateAllMcphubTasks_BypassesSameUserFilter(t *testing.T) {
	stream := fixtureTask(`\mcp-local-hub-paper-search-claude`, `CompletelyDifferentUser`)
	tasks, err := parseEnumerateXML(stream)
	if err != nil {
		t.Fatalf("parseEnumerateXML returned error: %v", err)
	}
	if len(tasks) != 1 {
		t.Fatalf("expected 1 task (parser must NOT same-user-filter), got %d", len(tasks))
	}
	if tasks[0].Owner != `CompletelyDifferentUser` {
		t.Errorf("parser must preserve foreign owner; got %q", tasks[0].Owner)
	}
}

// TestEnumerateAllMcphubTasks_ErrorIsTyped asserts the malformed-XML
// path returns a wrapped error that errors.Is or errors.As can be
// used against by a caller that needs to distinguish parser errors
// from schtasks invocation errors. This guards against accidentally
// returning a bare string error.
func TestEnumerateAllMcphubTasks_ErrorIsTyped(t *testing.T) {
	broken := `<?xml version="1.0"?><Task><<<<garbage`
	_, err := parseEnumerateXML(broken)
	if err == nil {
		t.Fatal("expected error for malformed XML")
	}
	// The wrapper should carry context. We don't pin the exact wrap
	// chain but assert that `errors.Unwrap` returns a non-nil error
	// (so the underlying xml.SyntaxError is reachable).
	if errors.Unwrap(err) == nil {
		t.Errorf("expected wrapped error chain (errors.Unwrap nonnil), got bare: %v", err)
	}
}

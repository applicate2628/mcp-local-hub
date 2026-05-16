//go:build windows

package scheduler

import (
	"bytes"
	"encoding/xml"
	"fmt"
	"io"
	"os/exec"
	"strings"

	"mcp-local-hub/internal/process"
)

// EnumerateAllMcphubTasks returns every Windows Scheduled Task whose
// URI begins with `\mcp-local-hub-`, REGARDLESS of which user account
// the task runs as. Migration (v0.5.0 phase 10) uses this to discover
// v0.4.x tasks owned by RunAs accounts other than the current user
// so the deviation-only classifier (Task 10.2) can decide whether to
// preserve, warn-and-preserve, or abort.
//
// Unlike (*windowsScheduler).List, this helper:
//
//   - is package-level — NOT a Scheduler interface method, so the POSIX
//     stubs (scheduler_linux.go, scheduler_darwin.go) do not grow a
//     no-op method. Per spec §"Package ownership", POSIX has no
//     schtasks; a function-shaped helper keeps the interface lean.
//   - uses `schtasks /Query /XML ONE /FO LIST` instead of the
//     human-readable LIST form. XML gives clean access to
//     Principal.UserId (the foreign-owner field migration cares about)
//     and is robust against locale variations in the schtasks LIST
//     headings (`Run As User:` vs localized strings on non-English
//     installs).
//   - omits the same-user filter sameWindowsUser(...). That filter is
//     the whole point of NOT routing through List.
//
// On non-elevated shells some tasks may be invisible (Windows ACL on
// the task store). The caller (migration driver, Task 10.4) handles
// partial-result surfacing — this helper just reports what schtasks
// exposes. Returns an error only if the schtasks invocation itself
// fails or the XML is unparseable.
//
// Production wires schtasksPath via resolveSchtasksPath() (same as
// the windowsScheduler constructor). Tests do NOT invoke this
// function directly — they exercise parseEnumerateXML against
// synthetic fixtures so the developer's real Task Scheduler state
// stays untouched (kosyak avoidance per repo
// CLAUDE.md/feedback_kosyak_full_test_sweep_affects_real_scheduler.md).
func EnumerateAllMcphubTasks() ([]TaskStatus, error) {
	schtasksPath, err := resolveSchtasksPath()
	if err != nil {
		return nil, fmt.Errorf("enumerate mcphub tasks: %w", err)
	}
	cmd := exec.Command(schtasksPath, "/Query", "/XML", "ONE", "/FO", "LIST")
	process.NoConsole(cmd)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("schtasks /Query /XML ONE: %w: %s", err, strings.TrimSpace(string(out)))
	}
	return parseEnumerateXML(string(out))
}

// parseEnumerateXML is the pure function form of
// EnumerateAllMcphubTasks. Splits the concatenated `schtasks /Query
// /XML ONE` output into per-task XML documents, parses each, and
// returns only tasks whose URI begins with `\mcp-local-hub-`.
//
// Kept as a separate function so unit tests can feed synthetic
// fixtures without invoking the real schtasks binary. This is the
// safety boundary that prevents the test suite from polluting the
// developer's installed Task Scheduler state.
func parseEnumerateXML(stream string) ([]TaskStatus, error) {
	docs := splitConcatenatedTaskXML(stream)
	results := make([]TaskStatus, 0, len(docs))
	for _, doc := range docs {
		ts, ok, err := decodeOneTask(doc)
		if err != nil {
			return nil, fmt.Errorf("parse task xml: %w", err)
		}
		if !ok {
			continue
		}
		if !strings.HasPrefix(ts.Name, mcphubURIPrefix) {
			continue
		}
		results = append(results, ts)
	}
	return results, nil
}

// mcphubURIPrefix is the leading byte sequence every legitimate
// mcp-local-hub-owned task URI begins with. Matches the prefix used
// by classifyOwnedTaskName in internal/api/watchdog_xml_validator.go
// so the two filters stay aligned; a future rename in either place
// must update both.
const mcphubURIPrefix = `\mcp-local-hub-`

// splitConcatenatedTaskXML splits a `schtasks /Query /XML ONE` output
// stream into per-task XML documents. The schtasks tool concatenates
// one fully-formed `<?xml ... ?>` + `<Task>...</Task>` document per
// task back-to-back; encoding/xml chokes on the multiple top-level
// processing instructions if fed the whole buffer at once, so we
// split on the `<?xml` marker and yield each chunk.
//
// Tolerates:
//   - leading/trailing whitespace between documents
//   - missing trailing newline at end-of-stream
//   - empty input (returns nil)
//   - inputs that begin without the `<?xml` PI (treated as one chunk
//     starting at the first `<Task` element, defensive for variant
//     schtasks builds that elide the PI on some Windows SKUs)
func splitConcatenatedTaskXML(stream string) []string {
	if stream == "" {
		return nil
	}
	// Trim ambient whitespace; the underlying tool sometimes prefixes
	// the stream with a stray `\r\n` on console-redirected stdout.
	trimmed := strings.TrimSpace(stream)
	if trimmed == "" {
		return nil
	}

	const piMarker = "<?xml"
	const taskMarker = "<Task"

	// Find every offset where a new document begins. Each document
	// starts at a `<?xml` PI; if the very first chunk lacks the PI
	// (defensive), treat the first `<Task` as the start.
	var starts []int
	for i := 0; i < len(trimmed); {
		idx := strings.Index(trimmed[i:], piMarker)
		if idx < 0 {
			break
		}
		starts = append(starts, i+idx)
		i = i + idx + len(piMarker)
	}
	if len(starts) == 0 {
		// No PI markers at all — fall back to the <Task element marker
		// for defensive single-doc inputs.
		if strings.Contains(trimmed, taskMarker) {
			return []string{trimmed}
		}
		return nil
	}

	out := make([]string, 0, len(starts))
	for i, s := range starts {
		var doc string
		if i+1 < len(starts) {
			doc = trimmed[s:starts[i+1]]
		} else {
			doc = trimmed[s:]
		}
		doc = strings.TrimSpace(doc)
		if doc != "" {
			out = append(out, doc)
		}
	}
	return out
}

// enumXMLTask is the minimal XML subset parseEnumerateXML extracts
// from each per-task document. RegistrationInfo.URI yields the task
// name; Principals.Principal[0].UserId yields the Owner. Other
// fields (Triggers, Settings, Actions) are intentionally NOT
// extracted here — the migration classifier (Task 10.2) operates on
// the raw XML body via its own decoder, so duplicating extraction
// would risk drift.
//
// Note: schtasks emits `<?xml version="1.0" encoding="UTF-16"?>` but
// the redirected stdout bytes are actually ASCII (Windows cmd quirk
// — the declaration mirrors the in-memory wide-char form, but the
// on-the-wire bytes are not UTF-16 wide chars). We strip the PI
// before decoding so encoding/xml's default UTF-8 reader handles
// the ASCII subset cleanly. This mirrors the api package's
// stripXMLDeclaration helper in watchdog_xml_validator.go.
type enumXMLTask struct {
	XMLName          xml.Name             `xml:"Task"`
	RegistrationInfo enumXMLRegInfo       `xml:"RegistrationInfo"`
	Principals       enumXMLPrincipalList `xml:"Principals"`
	Settings         enumXMLSettings      `xml:"Settings"`
}

type enumXMLRegInfo struct {
	URI string `xml:"URI"`
}

type enumXMLPrincipalList struct {
	Principal []enumXMLPrincipal `xml:"Principal"`
}

type enumXMLPrincipal struct {
	UserId string `xml:"UserId"`
}

type enumXMLSettings struct {
	Enabled string `xml:"Enabled"`
}

// decodeOneTask parses a single per-task XML document into a
// TaskStatus. Returns (TaskStatus{}, false, nil) if the document
// doesn't contain a parseable URI (e.g. a XML fragment for a
// non-task element schtasks might emit on edge cases) — caller
// treats this as "skip" not "error". Returns a non-nil error only
// if the XML itself is structurally malformed.
//
// The decoder is hardened in the same shape the api package's
// watchdog validator uses: strict mode, no entity expansion, no
// charset hooks. This is enumerate-only (no decision is made on
// these bytes — the migration classifier re-parses the raw XML
// itself), but treating untrusted input strictly is cheap and
// keeps the surface aligned with the validator's pattern.
func decodeOneTask(raw string) (TaskStatus, bool, error) {
	body := stripEnumerateXMLDeclaration(raw)
	if strings.TrimSpace(body) == "" {
		return TaskStatus{}, false, nil
	}
	dec := xml.NewDecoder(bytes.NewReader([]byte(body)))
	dec.Strict = true
	dec.Entity = nil
	dec.CharsetReader = nil

	var t enumXMLTask
	if err := dec.Decode(&t); err != nil && err != io.EOF {
		return TaskStatus{}, false, err
	}
	uri := strings.TrimSpace(t.RegistrationInfo.URI)
	if uri == "" {
		return TaskStatus{}, false, nil
	}
	status := TaskStatus{
		Name:       uri,
		LastResult: -1, // unknown — XML payload doesn't carry runtime stats
	}
	if len(t.Principals.Principal) > 0 {
		status.Owner = strings.TrimSpace(t.Principals.Principal[0].UserId)
	}
	// Best-effort State: schtasks XML carries <Settings><Enabled> as
	// the closest analog to the LIST form's `Status:` field. The
	// migration driver does not depend on this — it queries Status()
	// for the live runtime state — but populating something here
	// keeps the TaskStatus surface uniform between the two enumeration
	// paths.
	switch strings.ToLower(strings.TrimSpace(t.Settings.Enabled)) {
	case "true":
		status.State = "Ready"
	case "false":
		status.State = "Disabled"
	}
	return status, true, nil
}

// stripEnumerateXMLDeclaration drops a leading `<?xml ... ?>` PI (and
// any leading BOM / whitespace) so encoding/xml's default UTF-8
// reader handles the ASCII body. Mirrors api/watchdog_xml_validator.go
// stripXMLDeclaration; duplicated here so the scheduler package
// stays free of cross-package coupling for what is otherwise a
// 30-line helper.
//
// Safe because: (a) the body of legitimate Task Scheduler payloads is
// pure 7-bit ASCII (Command paths, args, names), and (b) callers of
// parseEnumerateXML do NOT use this decoded form for any
// security-sensitive decision — the migration classifier reads the
// raw XML through its own hardened decoder.
func stripEnumerateXMLDeclaration(raw string) string {
	// UTF-8 BOM.
	if len(raw) >= 3 && raw[0] == 0xEF && raw[1] == 0xBB && raw[2] == 0xBF {
		raw = raw[3:]
	}
	// Leading whitespace.
	i := 0
	for i < len(raw) && (raw[i] == ' ' || raw[i] == '\t' || raw[i] == '\r' || raw[i] == '\n') {
		i++
	}
	if i+5 > len(raw) || raw[i:i+5] != "<?xml" {
		return raw
	}
	end := strings.Index(raw[i:], "?>")
	if end < 0 {
		return raw
	}
	return raw[i+end+2:]
}
